// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package das

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/bits"

	"github.com/ethereum/go-ethereum/common"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/offchainlabs/nitro/arbstate"
	"github.com/offchainlabs/nitro/blsSignatures"
	flag "github.com/spf13/pflag"
)

type AggregatorConfig struct {
	// sequencer public key
	AssumedHonest      int    `koanf:"assumed-honest"`
	Backends           string `koanf:"backends"`
	StoreSignerAddress string `koanf:"store-signer-address"`
}

var DefaultAggregatorConfig = AggregatorConfig{
	StoreSignerAddress: "",
}

func AggregatorConfigAddOptions(prefix string, f *flag.FlagSet) {
	f.Int(prefix+".assumed-honest", DefaultAggregatorConfig.AssumedHonest, "Number of assumed honest backends (H). If there are N backends, K=N+1-H valid responses are required to consider an Store request to be successful.")
	f.String(prefix+".backends", DefaultAggregatorConfig.Backends, "JSON RPC backend configuration")
}

type Aggregator struct {
	config   AggregatorConfig
	services []ServiceDetails

	/// calculated fields
	requiredServicesForStore       int
	maxAllowedServiceStoreFailures int
	keysetHash                     [32]byte
	keysetBytes                    []byte
	storeSignerAddr                *common.Address
}

type ServiceDetails struct {
	service     DataAvailabilityService
	pubKey      blsSignatures.PublicKey
	signersMask uint64
}

func NewServiceDetails(service DataAvailabilityService, pubKey blsSignatures.PublicKey, signersMask uint64) (*ServiceDetails, error) {
	if bits.OnesCount64(signersMask) != 1 {
		return nil, fmt.Errorf("Tried to configure backend DAS %v with invalid signersMask %X", service, signersMask)
	}
	return &ServiceDetails{
		service:     service,
		pubKey:      pubKey,
		signersMask: signersMask,
	}, nil
}

func NewAggregator(config AggregatorConfig, services []ServiceDetails) (*Aggregator, error) {
	var aggSignersMask uint64
	pubKeys := []blsSignatures.PublicKey{}
	for _, d := range services {
		if bits.OnesCount64(d.signersMask) != 1 {
			return nil, fmt.Errorf("Tried to configure backend DAS %v with invalid signersMask %X", d.service, d.signersMask)
		}
		aggSignersMask |= d.signersMask
		pubKeys = append(pubKeys, d.pubKey)
	}
	if bits.OnesCount64(aggSignersMask) != len(services) {
		return nil, errors.New("At least two signers share a mask")
	}

	keyset := &arbstate.DataAvailabilityKeyset{
		AssumedHonest: uint64(config.AssumedHonest),
		PubKeys:       pubKeys,
	}
	ksBuf := bytes.NewBuffer([]byte{})
	if err := keyset.Serialize(ksBuf); err != nil {
		return nil, err
	}
	keysetHashBuf, err := keyset.Hash()
	if err != nil {
		return nil, err
	}
	var keysetHash [32]byte
	copy(keysetHash[:], keysetHashBuf)

	storeSignerAddr, err := StoreSignerAddressFromString(config.StoreSignerAddress)
	if err != nil {
		return nil, err
	}

	return &Aggregator{
		config:                         config,
		services:                       services,
		requiredServicesForStore:       len(services) + 1 - config.AssumedHonest,
		maxAllowedServiceStoreFailures: config.AssumedHonest - 1,
		keysetHash:                     keysetHash,
		keysetBytes:                    ksBuf.Bytes(),
		storeSignerAddr:                storeSignerAddr,
	}, nil
}

func (a *Aggregator) Retrieve(ctx context.Context, cert *arbstate.DataAvailabilityCertificate) ([]byte, error) {
	return a.retrieve(ctx, cert, a)
}

// retrieve calls  on each backend DAS in parallel and returns immediately on the
// first successful response where the data matches the requested hash. Otherwise
// if all requests fail or if its context is canceled (eg via TimeoutWrapper) then
// it returns an error.
func (a *Aggregator) retrieve(ctx context.Context, cert *arbstate.DataAvailabilityCertificate, dasReaer arbstate.DataAvailabilityServiceReader) ([]byte, error) {
	err := cert.VerifyNonPayloadParts(ctx, dasReaer)
	if err != nil {
		return nil, err
	}

	// Query all services, even those that didn't sign.
	// They may have been late in returning a response after storing the data,
	// or got the data by some other means.
	blobChan := make(chan []byte, len(a.services))
	errorChan := make(chan error, len(a.services))
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, d := range a.services {
		go func(ctx context.Context, d ServiceDetails) {
			blob, err := d.service.Retrieve(ctx, cert)
			if err != nil {
				errorChan <- err
				return
			}
			if bytes.Equal(crypto.Keccak256(blob), cert.DataHash[:]) {
				blobChan <- blob
			} else {
				errorChan <- fmt.Errorf("DAS (mask %X) returned data that doesn't match requested hash!", d.signersMask)
			}
		}(subCtx, d)
	}

	errorCount := 0
	var errorCollection []error
	for errorCount < len(a.services) {
		select {
		case blob := <-blobChan:
			return blob, nil
		case err = <-errorChan:
			errorCollection = append(errorCollection, err)
			log.Warn("Couldn't retrieve message from DAS", "err", err)
			errorCount++
		case <-ctx.Done():
			break
		}
	}

	return nil, fmt.Errorf("Data wasn't able to be retrieved from any DAS: %v", errorCollection)
}

type storeResponse struct {
	details ServiceDetails
	sig     blsSignatures.Signature
	err     error
}

// store calls Store on each backend DAS in parallel and collects responses.
// If there were at least K responses then it aggregates the signatures and
// signersMasks from each DAS together into the DataAvailabilityCertificate
// then Store returns immediately. If there were any backend Store subroutines
// that were still running when Aggregator.Store returns, they are allowed to
// continue running until the context is canceled (eg via TimeoutWrapper),
// with their results discarded.
//
// If Store gets enough errors that K successes is impossible, then it stops early
// and returns an error.
//
// If Store gets not enough successful responses by the time its context is canceled
// (eg via TimeoutWrapper) then it also returns an error.
func (a *Aggregator) Store(ctx context.Context, message []byte, timeout uint64, sig []byte) (*arbstate.DataAvailabilityCertificate, error) {
	if a.storeSignerAddr != nil {
		actualSigner, err := DasRecoverSigner(message, timeout, sig)
		if err != nil {
			return nil, err
		}
		if actualSigner != *a.storeSignerAddr {
			return nil, errors.New("store request not properly signed")
		}
	}

	responses := make(chan storeResponse, len(a.services))

	expectedHash := crypto.Keccak256(message)
	for _, d := range a.services {
		go func(ctx context.Context, d ServiceDetails) {
			cert, err := d.service.Store(ctx, message, timeout, sig)
			if err != nil {
				responses <- storeResponse{d, nil, err}
				return
			}

			verified, err := blsSignatures.VerifySignature(cert.Sig, serializeSignableFields(cert), d.pubKey)
			if err != nil {
				responses <- storeResponse{d, nil, err}
				return
			}
			if !verified {
				responses <- storeResponse{d, nil, errors.New("Signature verification failed.")}
				return
			}

			// SignersMask from backend DAS is ignored.

			if !bytes.Equal(cert.DataHash[:], expectedHash) {
				responses <- storeResponse{d, nil, errors.New("Hash verification failed.")}
				return
			}
			if cert.Timeout != timeout {
				responses <- storeResponse{d, nil, fmt.Errorf("Timeout was %d, expected %d", cert.Timeout, timeout)}
				return
			}

			responses <- storeResponse{d, cert.Sig, nil}
		}(ctx, d)
	}

	var pubKeys []blsSignatures.PublicKey
	var sigs []blsSignatures.Signature
	var aggCert arbstate.DataAvailabilityCertificate
	var aggSignersMask uint64
	var storeFailures, successfullyStoredCount int
	var errs []error
	for i := 0; i < len(a.services) && storeFailures <= a.maxAllowedServiceStoreFailures && successfullyStoredCount < a.requiredServicesForStore; i++ {
		select {
		case <-ctx.Done():
			break
		case r := <-responses:
			if r.err != nil {
				storeFailures++
				errs = append(errs, fmt.Errorf("Error from backend %v, with signer mask %d: %w", r.details.service, r.details.signersMask, r.err))
				continue
			}

			pubKeys = append(pubKeys, r.details.pubKey)
			sigs = append(sigs, r.sig)
			aggSignersMask |= r.details.signersMask
			successfullyStoredCount++
		}
	}

	if successfullyStoredCount < a.requiredServicesForStore {
		return nil, fmt.Errorf("Aggregator failed to store message to at least %d out of %d DASes (assuming %d are honest), errors received %d, %v", a.requiredServicesForStore, len(a.services), a.config.AssumedHonest, storeFailures, errs)
	}

	aggCert.Sig = blsSignatures.AggregateSignatures(sigs)
	aggPubKey := blsSignatures.AggregatePublicKeys(pubKeys)
	aggCert.SignersMask = aggSignersMask
	copy(aggCert.DataHash[:], expectedHash)
	aggCert.Timeout = timeout
	aggCert.KeysetHash = a.keysetHash

	verified, err := blsSignatures.VerifySignature(aggCert.Sig, serializeSignableFields(&aggCert), aggPubKey)
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, errors.New("Failed aggregate signature check")
	}
	return &aggCert, nil
}

func (a *Aggregator) KeysetFromHash(ctx context.Context, ksHash []byte) ([]byte, error) {
	if !bytes.Equal(ksHash, a.keysetHash[:]) {
		return nil, ErrDasKeysetNotFound
	}
	return a.keysetBytes, nil
}

func (a *Aggregator) CurrentKeysetBytes(ctx context.Context) ([]byte, error) {
	return a.keysetBytes, nil
}

func (a *Aggregator) String() string {
	var b bytes.Buffer
	b.WriteString("das.Aggregator{")
	first := true
	for _, d := range a.services {
		if !first {
			b.WriteString(",")
		}
		b.WriteString(fmt.Sprintf("signersMask(aggregator):%d,", d.signersMask))
		b.WriteString(d.service.String())
	}
	b.WriteString("}")
	return b.String()
}