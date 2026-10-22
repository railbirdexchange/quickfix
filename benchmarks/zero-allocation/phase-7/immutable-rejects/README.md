# Immutable prepared missing-field errors

Configured missing-field getters now return immutable errors prepared during
session setup. Each error stores a tag value and its completed text; no getter,
reference read or repeated text read needs an allocation. This replaces the
[intermediate mutable-reference implementation](../missing-field-rejects/README.md)
in the same PR.

## API and ownership

`MessageRejectError.RefTagID()` changes from `*Tag` to `(Tag, bool)`. The boolean
distinguishes an absent reference from a present zero. Custom implementations
and callers must migrate. Generic constructors keep their optional `*Tag`
argument and snapshot it instead of retaining an alias. Error text, reject
reason 8, business classification, wire fields and logging remain intact.

```text
session setup -> dictionary tag union -> immutable error table + text
  -> shared by inbound parser, replay parser, control and outgoing messages
message owner -> absent configured field -> shared immutable error
  -> RefTagID: copied value + presence
  -> Error: already-owned immutable string
  -> retained error survives message/frame reuse and session teardown
reject wire -> existing output slot -> writer channel
  -> completed write -> returned output slot
```

All mutable message/frame/output storage keeps its existing exclusive owner.
The error table has no mutation after setup, mutex, pool or reference counting.
Unknown tags never enlarge it. Copies and ordinary callback materialization
share immutable error metadata while retaining their existing field ownership.
Reset/reparse restores the original slot table even after `CopyInto` replaced
the message's field maps.

## Controlled measurements

Before runtime: `32260f8fb8d00f22cb53e39617104cc624ed9642`, the preceding PR28
revision. After runtime: the implementation delivered with this directory.
Both captures use identical benchmark source, verified by the
[before](benchmarks-before.sha256) and [after](benchmarks-after.sha256) manifests.
Go 1.27.2, darwin/arm64, Apple M4 Pro, CPU 1; five one-second samples. Setup,
dictionary construction and 66 heartbeat warm-ups are outside timed intervals.
Builds, correctness tests and performance captures run serially.

| Operation | Before ns/op | After ns/op | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|---:|---:|
| Configured missing GetString(526) | 16.10 | 13.38 | 16 / 1 | 0 / 0 |
| Same lookup, read Error() twice | 60.99 | 15.46 | 112 / 3 | 0 / 0 |
| Completed heartbeat, three configured missing reads | 1,920 | 1,926 | 48 / 3 | 0 / 0 |
| Same heartbeat, read each error text twice | 2,063 | 1,930 | 336 / 9 | 0 / 0 |
| Unknown missing GetString(100001) | 16.20 | 46.13 | 16 / 1 | 72 / 2 |
| Same unknown lookup, read Error() twice | 59.73 | 51.34 | 112 / 3 | 72 / 2 |

Times are five-sample medians. Every sample agrees on byte/object counts.
The unrendered completed-heartbeat time is effectively unchanged; rendering
twice has a 6.4% lower median. These are QuickFIX component measurements with
a bounded store, null logger and copying connection fixture, not network or
whole FIX-server latency. Each operation waits for write completion, returned
output slots, independent peer parsing and explicit journal retirement.

The FIX44 fixture defines all three probed tags (11, 41, 526). The older FIX40
heartbeat fixture is preserved: its dictionary does not define 526, so that
lookup correctly uses the allocating fallback. No dictionary validation is
disabled or silently broadened to qualify a benchmark.

The fallback tradeoff is explicit: an unknown-tag error now eagerly owns cached
text, so an unrendered unknown lookup costs more than the intermediate lazy
version. Repeated text reads allocate nothing after construction. Direct public
constructor calls and standalone unconfigured field maps use that same fallback.
This phase does not make every error, convenience API or the whole library
allocation-free.

Raw component captures: [before](prepared-before.txt), [after](prepared-after.txt).
The required unchanged allocation baseline is captured
[before](baseline-before.txt) and [after](baseline-after.txt).
Every baseline byte/object row is unchanged: warmed plain, dictionary and group
parsing plus validation remain 0/0; legacy build remains 152 B/6 objects,
legacy framing 160 B/2 objects and decimal overflow 168 B/5 objects.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^Benchmark(PreparedMissingField|PreparedRejectHeartbeat)$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
```

## Correctness and verification

Fixtures cover all missing getter variants, dictionary unions, unknown and
signed-limit tags, constructor input snapshots, present-zero/absent references,
retained errors, concurrent readers, copies, arena transitions, slot reuse,
session/replay/control sharing and destination-session selection. Completed
FIX40/FIX44 reject tests check wire fields, text, sequence values, returned
output slots and retained logger arguments, including a prepared FIX44 error.

Exact allocation tests check total counters across 1,000 missing lookups plus
2,000 text reads, and separately 100 completed heartbeats with three missing
lookups and two text reads per error. They require zero added objects and bytes,
rather than accepting a rounded benchmark average.

| Verification | Result |
|---|---|
| [Focused API, lifetime, wire and exact-allocation fixtures](focused-tests.txt) | PASS |
| [Go 1.27.2 all packages](tests-go1.27.2.txt) | PASS, core 30.192 s |
| [Go 1.27.2 vet](vet-go1.27.2.txt) | PASS, empty successful output |
| [Separate Go 1.27.2 core/internal race](race-go1.27.2.txt) | PASS, core 34.190 s |

The all-package run excludes only `TestMongoLogTestSuite`, whose unrelated
teardown failure was reproduced before the original codec change. Store tests
remain enabled. Race testing runs separately from the performance captures.
Independent source review found and prompted a fix for error metadata after
`CopyInto` into a public slot; the regression fixture passes. The final source
review found no remaining actionable issue.

```sh
GOTOOLCHAIN=auto rtk proxy go test -p=1 ./... -skip '^TestMongoLogTestSuite$' -count=1 -timeout=5m
GOTOOLCHAIN=auto rtk proxy go vet ./...
GOTOOLCHAIN=auto rtk proxy go test -race -p=1 . ./internal -count=1 -timeout=5m
```

No deployment is part of this revision.
