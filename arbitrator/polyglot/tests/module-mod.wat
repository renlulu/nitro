;; Copyright 2022, Offchain Labs, Inc.
;; For license information, see https://github.com/nitro/blob/master/LICENSE

(module
    (import "test" "noop" (func))
    (func (export "void"))
    (func (export "more") (param i32 i64) (result f32)
        unreachable))
