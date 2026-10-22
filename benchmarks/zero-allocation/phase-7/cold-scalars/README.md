# Compact ownership for cold engine scalars

BEFORE is commit `aa38f33449de93729f2f8343c4dc958640e9a505`, measured in
[`../first-send/after-go1.25.3.txt`](../first-send/after-go1.25.3.txt). AFTER is
`runtime-change.diff` with exact sources pinned by `after-source.sha256`.
The benchmark harness is unchanged: this directory's `harness-after.sha256`
matches `../first-send/harness-after.sha256`.

Both captures use the same five-run, one-second, single-CPU command on Go 1.25.3,
darwin/arm64, Apple M4 Pro. Session setup and warmup remain outside measurement;
each operation includes a completed write and the returned output slot.

| Flow | Before ns/op range | After ns/op range | Before B/op | After B/op | Before allocs/op | After allocs/op |
|---|---:|---:|---:|---:|---:|---:|
| Fresh Message plus convenience parse | 2,970–3,074 | 2,910–3,041 | 2,823 | 2,760 | 30 | 24 |
| Same Message, convenience reparse | 2,571–2,600 | 2,476–2,569 | 151 | 88 | 10 | 4 |

Each flow saves 63 bytes and six allocations per operation. Cold parsed engine
fields now use one exact-size payload allocation each, with separate wire and
getter regions. Integer, boolean, and timestamp formatting use bounded stack
storage before copying into that allocation. Both region capacities equal their
lengths, preventing an append through one slice from crossing into the other.
Previously retained getter and wire bytes are never overwritten.

This change only replaces the cold engine-scalar fallback. It adds no Message
fields, shared session scratch, pool, or slice growth. New-field metadata can
still allocate. The existing reused-message arena, controls, parsed slots and
replay path keep their fixed reusable storage. Neither row above claims zero
allocation, network latency, or submicrosecond completion. Timing ranges alone
do not establish statistical significance.

```sh
GOTOOLCHAIN=go1.25.3 rtk proxy go test . -run '^$' -bench '^BenchmarkScopedOutboundFirstSend$' -benchmem -benchtime=1s -count=5 -cpu=1
```

`after-go1.25.3.txt` passes in 12.764 seconds. Focused Go 1.23.12 first-send,
control and arena fixtures pass in 0.626 seconds; those fixtures plus the
backpressure cases pass with the race detector in 2.924 seconds.
All seven full TCP scenarios pass 100 completed cycles each with zero additional
mallocs and allocated bytes, in 9.709 seconds.

New fixtures check retained previous values, independent getter/wire mutations,
append isolation, empty values, signed integer limits, all timestamp precisions,
unknown-precision fallback and years outside the usual four-digit range. They
also assert exactly one payload allocation when replacing each existing cold
engine scalar. Existing fixtures retain their output-slot recycling, copy,
reparse, and warmed completed-send zero-allocation assertions. Full TCP exact
qualification is recorded in `full-session-exact-go1.23.12.txt`.
