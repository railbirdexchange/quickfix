# Missing-field reject allocation reduction

An absent field read through QuickFIX's public getters eagerly built formatted
error text, a separate reference-tag allocation and a generic reject object even
when the caller only checked the returned error. The saved PREPROD profile at
QuickFIX `5b4d3a87` identified this path under `FieldMap.GetString`.

`ConditionallyRequiredFieldMissing` now returns a small owned error containing
the original tag and an independently mutable reference tag. It formats text
only when `Error()` is called, using a fixed stack buffer and `strconv.AppendInt`.
Exported signatures, reject reason 8, business-reject classification, text,
reference-pointer lifetime and independent error ownership are unchanged. No
FIX-server, protobuf, Kafka, validation, persistence or logging changes are made.

## Ownership

```text
message owner -> field lookup -> absent field
  -> caller-owned reject {immutable original tag, mutable reference tag}
       -> RefTagID: stable pointer into this error, independently retainable
       -> Error: local fixed formatting buffer -> new caller-owned string
  -> message/frame reset does not invalidate the error, pointer or returned text

actual session reject -> wire text and log text -> existing outbound slot
  -> writer channel -> completed write -> returned slot
```

Each error has its own mutable reference tag. There is no shared error cache,
mutex, pool, reference counting, unsafe string view or new public API. Text keeps
the original tag even if a caller mutates `*RefTagID()`.

## Paired measurements

Before runtime: `5b4d3a87f8f3090acd2af893ddb61eb69bb690e8`, with the new test and
benchmark fixtures applied but production code unchanged. After runtime differs
only in `errors.go`. Go 1.27.2, darwin/arm64, Apple M4 Pro; one CPU, five one-second
samples. Captures, builds and tests ran serially. Setup and warm-up are untimed;
the completed-heartbeat fixture performs 66 warm-up operations. Returned errors
are retained through an interface sink, preserving the public owned lifetime.

| Flow | Before ns/op | After ns/op | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|---:|---:|
| Legacy missing `GetString(41)` | 103.6 | 15.22 | 120 / 3 | 16 / 1 |
| Fixed missing `GetString(41)` | 104.0 | 14.94 | 120 / 3 | 16 / 1 |
| Construct tag 41 reject, no text read | 96.19 | 8.122 | 120 / 3 | 16 / 1 |
| Construct tag 526 reject, no text read | 97.21 | 8.080 | 128 / 4 | 16 / 1 |
| Construct tag 41 reject, read text once | 97.85 | 23.21 | 120 / 3 | 64 / 2 |
| Construct tag 41 reject, read text twice | 99.64 | 38.96 | 120 / 3 | 112 / 3 |
| Construct tag 526 reject, read text once | 102.6 | 25.23 | 128 / 4 | 64 / 2 |
| Construct tag 526 reject, read text twice | 103.6 | 43.06 | 128 / 4 | 112 / 3 |
| Completed heartbeat, three absent field reads | 1,862 | 1,600 | 368 / 10 | 48 / 3 |

Times are five-sample medians; byte and allocation counts agree in every sample.
The completed-heartbeat component waits for connection-write completion, output-slot
return and independent peer parsing. Its callbacks read absent tags 11, 41 and
526. It uses the existing bounded external-store stand-in, null logger and
copying connection fixture. It measures QuickFIX work, not production network
latency, the whole FIX-server or a new PREPROD result. Its median falls 14.1%,
with 87.0% fewer allocated bytes and 70% fewer objects in this component.

Raw measurements: [before](missing-before.txt), [after](missing-after.txt).
The mandatory unchanged allocation-baseline capture is retained [before](baseline-before.txt)
and [after](baseline-after.txt). Fixture manifests [before](fixtures-before.sha256)
and [after](fixtures-after.sha256) record identical final fixture contents; the
additional wire/lifetime test was run on the original implementation after its
benchmark capture, and does not run in the benchmark interval.

All allocation-baseline byte/object rows are unchanged: reused plain, dictionary
and group parsing and validation remain 0/0; the legacy build is 152 B/6 objects,
legacy framed read 160 B/2 objects and arbitrary-precision overflow 168 B/5 objects.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^Benchmark(MissingFieldReject|HeartbeatMissingFieldReads)$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
```

## Limits and correctness

This path still allocates one owned error per missing lookup. Every `Error()` call
now creates an owned string; previously it returned the already-formatted text.
Repeated rendering of one retained error can therefore allocate more. The
twice-rendered rows cover the existing session behavior of reading text for both
the wire and log; session callback/log call counts are unchanged. There is no
whole-library zero-allocation claim and no production deployment in this phase.

The new parity tests pass [before](parity-before.txt) and [after](parity-after.txt).
They cover signed integer limits, exact text and classification, stable mutable
reference pointers, independent errors, retained errors across message reuse,
read-only concurrency and concurrent text reads with reference-tag mutation.
Completed FIX 4.0 and FIX 4.4 reject fixtures verify wire text/reasons/reference
fields, sequence numbers, returned output slots and retained logger arguments.

| Verification | Result |
|---|---|
| [Go 1.27.2 all packages](tests-go1.27.2.txt) | PASS, core 30.129 s |
| [Go 1.23.12 all packages](tests-go1.23.12.txt) | PASS, core 30.857 s |
| [Go 1.23.12 vet](vet-go1.23.12.txt) | PASS, empty successful output |
| [Separate Go 1.27.2 core/internal race](race-go1.27.2.txt) | PASS |

Both all-package runs skip only `TestMongoLogTestSuite`, whose unrelated logger
teardown failure was reproduced before the earlier codec change. This PR does
not alter that test or logger. Race testing ran separately after all performance
captures. An independent source review found no actionable correctness issue.

```sh
GOTOOLCHAIN=auto rtk proxy go test -p=1 ./... -skip '^TestMongoLogTestSuite$' -count=1 -timeout=5m
GOTOOLCHAIN=go1.23.12 rtk proxy go test -p=1 ./... -skip '^TestMongoLogTestSuite$' -count=1 -timeout=5m
GOTOOLCHAIN=go1.23.12 rtk proxy go vet ./...
GOTOOLCHAIN=auto rtk proxy go test -race -p=1 . ./internal -count=1 -timeout=5m
```
