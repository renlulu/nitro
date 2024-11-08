// Copyright 2024-2025, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package gethexec

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/arbitrum_types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/lru"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/offchainlabs/nitro/solgen/go/express_lane_auctiongen"
	"github.com/offchainlabs/nitro/timeboost"
	"github.com/offchainlabs/nitro/util/arbmath"
	"github.com/offchainlabs/nitro/util/stopwaiter"
	"github.com/pkg/errors"
)

type expressLaneControl struct {
	sequence   uint64
	controller common.Address
}

type expressLaneService struct {
	stopwaiter.StopWaiter
	sync.RWMutex
	auctionContractAddr      common.Address
	initialTimestamp         time.Time
	roundDuration            time.Duration
	auctionClosing           time.Duration
	chainConfig              *params.ChainConfig
	logs                     chan []*types.Log
	seqClient                *ethclient.Client
	auctionContract          *express_lane_auctiongen.ExpressLaneAuction
	roundControl             *lru.Cache[uint64, *expressLaneControl]
	messagesBySequenceNumber map[uint64]*timeboost.ExpressLaneSubmission
}

func newExpressLaneService(
	auctionContractAddr common.Address,
	sequencerClient *ethclient.Client,
	bc *core.BlockChain,
) (*expressLaneService, error) {
	chainConfig := bc.Config()
	auctionContract, err := express_lane_auctiongen.NewExpressLaneAuction(auctionContractAddr, sequencerClient)
	if err != nil {
		return nil, err
	}

	retries := 0
pending:
	var roundTimingInfo timeboost.RoundTimingInfo
	roundTimingInfo, err = auctionContract.RoundTimingInfo(&bind.CallOpts{})
	if err != nil {
		const maxRetries = 5
		if errors.Is(err, bind.ErrNoCode) && retries < maxRetries {
			wait := time.Millisecond * 250 * (1 << retries)
			log.Info("ExpressLaneAuction contract not ready, will retry afer wait", "err", err, "auctionContractAddr", auctionContractAddr, "wait", wait, "maxRetries", maxRetries)
			retries++
			time.Sleep(wait)
			goto pending
		}
		return nil, err
	}
	if err = roundTimingInfo.Validate(nil); err != nil {
		return nil, err
	}
	initialTimestamp := time.Unix(roundTimingInfo.OffsetTimestamp, 0)
	roundDuration := arbmath.SaturatingCast[time.Duration](roundTimingInfo.RoundDurationSeconds) * time.Second
	auctionClosingDuration := arbmath.SaturatingCast[time.Duration](roundTimingInfo.AuctionClosingSeconds) * time.Second
	return &expressLaneService{
		auctionContract:          auctionContract,
		chainConfig:              chainConfig,
		initialTimestamp:         initialTimestamp,
		auctionClosing:           auctionClosingDuration,
		roundControl:             lru.NewCache[uint64, *expressLaneControl](8), // Keep 8 rounds cached.
		auctionContractAddr:      auctionContractAddr,
		roundDuration:            roundDuration,
		seqClient:                sequencerClient,
		logs:                     make(chan []*types.Log, 10_000),
		messagesBySequenceNumber: make(map[uint64]*timeboost.ExpressLaneSubmission),
	}, nil
}

func (es *expressLaneService) Start(ctxIn context.Context) {
	es.StopWaiter.Start(ctxIn, es)

	// Log every new express lane auction round.
	es.LaunchThread(func(ctx context.Context) {
		log.Info("Watching for new express lane rounds")
		waitTime := timeboost.TimeTilNextRound(es.initialTimestamp, es.roundDuration)
		// Wait until the next round starts
		select {
		case <-ctx.Done():
			return
		case <-time.After(waitTime):
			// First tick happened, now set up regular ticks
		}

		ticker := time.NewTicker(es.roundDuration)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case t := <-ticker.C:
				round := timeboost.CurrentRound(es.initialTimestamp, es.roundDuration)
				log.Info(
					"New express lane auction round",
					"round", round,
					"timestamp", t,
				)
				es.Lock()
				// Reset the sequence numbers map for the new round.
				es.messagesBySequenceNumber = make(map[uint64]*timeboost.ExpressLaneSubmission)
				es.Unlock()
			}
		}
	})
	es.LaunchThread(func(ctx context.Context) {
		log.Info("Monitoring express lane auction contract")
		// Monitor for auction resolutions from the auction manager smart contract
		// and set the express lane controller for the upcoming round accordingly.
		latestBlock, err := es.seqClient.HeaderByNumber(ctx, nil)
		if err != nil {
			// TODO: Should not be a crit.
			log.Crit("Could not get latest header", "err", err)
		}
		fromBlock := latestBlock.Number.Uint64()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Millisecond * 250):
				latestBlock, err := es.seqClient.HeaderByNumber(ctx, nil)
				if err != nil {
					log.Crit("Could not get latest header", "err", err)
				}
				toBlock := latestBlock.Number.Uint64()
				if fromBlock == toBlock {
					continue
				}
				filterOpts := &bind.FilterOpts{
					Context: ctx,
					Start:   fromBlock,
					End:     &toBlock,
				}
				it, err := es.auctionContract.FilterAuctionResolved(filterOpts, nil, nil, nil)
				if err != nil {
					log.Error("Could not filter auction resolutions event", "error", err)
					continue
				}
				for it.Next() {
					log.Info(
						"New express lane controller assigned",
						"round", it.Event.Round,
						"controller", it.Event.FirstPriceExpressLaneController,
					)
					es.roundControl.Add(it.Event.Round, &expressLaneControl{
						controller: it.Event.FirstPriceExpressLaneController,
						sequence:   0,
					})
				}
				setExpressLaneIterator, err := es.auctionContract.FilterSetExpressLaneController(filterOpts, nil, nil, nil)
				if err != nil {
					log.Error("Could not filter express lane controller transfer event", "error", err)
					continue
				}
				for setExpressLaneIterator.Next() {
					round := setExpressLaneIterator.Event.Round
					roundInfo, ok := es.roundControl.Get(round)
					if !ok {
						log.Warn("Could not find round info for express lane controller transfer event", "round", round)
						continue
					}
					prevController := setExpressLaneIterator.Event.PreviousExpressLaneController
					if roundInfo.controller != prevController {
						log.Warn("New express lane controller did not match previous controller",
							"round", round,
							"previous", setExpressLaneIterator.Event.PreviousExpressLaneController,
							"new", setExpressLaneIterator.Event.NewExpressLaneController)
						continue
					}
					newController := setExpressLaneIterator.Event.NewExpressLaneController
					es.roundControl.Add(round, &expressLaneControl{
						controller: newController,
						sequence:   0,
					})
				}
				fromBlock = toBlock
			}
		}
	})
}

func (es *expressLaneService) currentRoundHasController() bool {
	es.Lock()
	defer es.Unlock()
	currRound := timeboost.CurrentRound(es.initialTimestamp, es.roundDuration)
	control, ok := es.roundControl.Get(currRound)
	if !ok {
		return false
	}
	return control.controller != (common.Address{})
}

func (es *expressLaneService) isWithinAuctionCloseWindow(arrivalTime time.Time) bool {
	// Calculate the next round start time
	elapsedTime := arrivalTime.Sub(es.initialTimestamp)
	elapsedRounds := elapsedTime / es.roundDuration
	nextRoundStart := es.initialTimestamp.Add((elapsedRounds + 1) * es.roundDuration)
	// Calculate the time to the next round
	timeToNextRound := nextRoundStart.Sub(arrivalTime)
	// Check if the arrival timestamp is within AUCTION_CLOSING_DURATION of TIME_TO_NEXT_ROUND
	return timeToNextRound <= es.auctionClosing
}

// Sequence express lane submission skips validation of the express lane message itself,
// as the core validator logic is handled in `validateExpressLaneTx“
func (es *expressLaneService) sequenceExpressLaneSubmission(
	ctx context.Context,
	msg *timeboost.ExpressLaneSubmission,
	publishTxFn func(
		parentCtx context.Context,
		tx *types.Transaction,
		options *arbitrum_types.ConditionalOptions,
		delay bool,
	) error,
) error {
	es.Lock()
	defer es.Unlock()
	control, ok := es.roundControl.Get(msg.Round)
	if !ok {
		return timeboost.ErrNoOnchainController
	}
	// Check if the submission nonce is too low.
	if msg.SequenceNumber < control.sequence {
		return timeboost.ErrSequenceNumberTooLow
	}
	// Check if a duplicate submission exists already, and reject if so.
	if _, exists := es.messagesBySequenceNumber[msg.SequenceNumber]; exists {
		return timeboost.ErrDuplicateSequenceNumber
	}
	// Log an informational warning if the message's sequence number is in the future.
	if msg.SequenceNumber > control.sequence {
		log.Warn("Received express lane submission with future sequence number", "SequenceNumber", msg.SequenceNumber)
	}
	// Put into the sequence number map.
	es.messagesBySequenceNumber[msg.SequenceNumber] = msg

	for {
		// Get the next message in the sequence.
		nextMsg, exists := es.messagesBySequenceNumber[control.sequence]
		if !exists {
			break
		}
		if err := publishTxFn(
			ctx,
			nextMsg.Transaction,
			msg.Options,
			false, /* no delay, as it should go through express lane */
		); err != nil {
			// If the tx failed, clear it from the sequence map.
			delete(es.messagesBySequenceNumber, msg.SequenceNumber)
			return err
		}
		// Increase the global round sequence number.
		control.sequence += 1
	}
	es.roundControl.Add(msg.Round, control)
	return nil
}

func (es *expressLaneService) validateExpressLaneTx(msg *timeboost.ExpressLaneSubmission) error {
	if msg == nil || msg.Transaction == nil || msg.Signature == nil {
		return timeboost.ErrMalformedData
	}
	if msg.ChainId.Cmp(es.chainConfig.ChainID) != 0 {
		return errors.Wrapf(timeboost.ErrWrongChainId, "express lane tx chain ID %d does not match current chain ID %d", msg.ChainId, es.chainConfig.ChainID)
	}
	if msg.AuctionContractAddress != es.auctionContractAddr {
		return errors.Wrapf(timeboost.ErrWrongAuctionContract, "msg auction contract address %s does not match sequencer auction contract address %s", msg.AuctionContractAddress, es.auctionContractAddr)
	}
	currentRound := timeboost.CurrentRound(es.initialTimestamp, es.roundDuration)
	if msg.Round != currentRound {
		return errors.Wrapf(timeboost.ErrBadRoundNumber, "express lane tx round %d does not match current round %d", msg.Round, currentRound)
	}
	if !es.currentRoundHasController() {
		return timeboost.ErrNoOnchainController
	}
	// Reconstruct the message being signed over and recover the sender address.
	signingMessage, err := msg.ToMessageBytes()
	if err != nil {
		return timeboost.ErrMalformedData
	}
	if len(msg.Signature) != 65 {
		return errors.Wrap(timeboost.ErrMalformedData, "signature length is not 65")
	}
	// Recover the public key.
	prefixed := crypto.Keccak256(append([]byte(fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(signingMessage))), signingMessage...))
	sigItem := make([]byte, len(msg.Signature))
	copy(sigItem, msg.Signature)

	// Signature verification expects the last byte of the signature to have 27 subtracted,
	// as it represents the recovery ID. If the last byte is greater than or equal to 27, it indicates a recovery ID that hasn't been adjusted yet,
	// it's needed for internal signature verification logic.
	if sigItem[len(sigItem)-1] >= 27 {
		sigItem[len(sigItem)-1] -= 27
	}
	pubkey, err := crypto.SigToPub(prefixed, sigItem)
	if err != nil {
		return timeboost.ErrMalformedData
	}
	sender := crypto.PubkeyToAddress(*pubkey)
	control, ok := es.roundControl.Get(msg.Round)
	if !ok {
		return timeboost.ErrNoOnchainController
	}
	if sender != control.controller {
		return timeboost.ErrNotExpressLaneController
	}
	return nil
}
