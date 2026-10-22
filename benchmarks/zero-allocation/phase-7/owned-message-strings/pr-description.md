Constructed `Message.String()` copied a complete temporary wire byte slice into
the returned string. Encode directly into one pre-sized local string builder,
sharing the existing field-sort helper with append encoding. Prepare the 256
valid checksum strings once while retaining `SetString` ownership and the
out-of-range formatting fallback.

```text
message owner -> cook stored tags 9/10 -> existing section locks/order
  -> local builder -> owned string retained by caller
startup checksum table -> existing SetString copy -> message-owned field value
parsed raw message -> unchanged owned string copy
```

Go 1.27.2, Apple M4 Pro, one CPU, five one-second samples; medians:

| Operation | ns/op before → after | B/op before → after | allocs/op before → after |
|---|---:|---:|---:|
| Ordinary order String | 848.5 → 809.2 | 264 → 144 | 7 → 5 |
| Ordinary reject String | 812.9 → 785.5 | 296 → 160 | 7 → 5 |
| Arena order String | 694.6 → 687.0 | 227 → 112 | 3 → 1 |
| Arena reject String | 689.1 → 673.5 | 259 → 128 | 3 → 1 |
| Baseline order byte build | 827.1 → 806.1 | 152 → 144 | 6 → 5 |

Raw String controls retain one output allocation; all other mandatory baseline
allocation counts are unchanged. The fixed checksum table costs 4 KiB of global
string entries plus one 768-byte startup backing allocation on this target.
Intermediate builder-only receipts are retained; timing variation does not
establish an isolated checksum latency improvement.

Golden fixtures cover exact BodyLength/checksum, timestamps, resend headers,
groups and nested groups. Ownership tests retain strings through message and
input-buffer reuse; checksum tests exhaust 0..255, fallback values and mutable
getter isolation. API signatures, validation, logging, persistence, Bytes and
resend body semantics are preserved. This is a library component improvement;
it does not claim whole-library zero allocation or a deployed latency result.

Final all-package tests and vet passed; separate full core/internal race tests
passed. Only the previously reproduced unrelated Mongo logging teardown test is
excluded; Mongo store tests remain enabled. The benchmark harness is identical
between before and after, with setup and warm-up outside timing.

Evidence and reproducible commands are in
`benchmarks/zero-allocation/phase-7/owned-message-strings/README.md`.
