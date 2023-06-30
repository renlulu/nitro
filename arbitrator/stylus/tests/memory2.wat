;; Copyright 2023, Offchain Labs, Inc.
;; For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE

(module
    (import "forward" "memory_grow" (func $memory_grow (param i32)))
    (func (export "arbitrum_main") (param $args_len i32) (result i32)
        (call $memory_grow (i32.const 0))
        (call $memory_grow (i32.sub (i32.const 0) (i32.const 1)))
        i32.const 0
    )
    (memory (export "memory") 0)
)
