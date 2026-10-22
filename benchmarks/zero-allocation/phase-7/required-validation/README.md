# Reuse required-field validation errors

Private session validators now prepare immutable reason-1 errors for the finite
union of required header, body and trailer tags. Reject selection still uses
the original dictionary traversal; after selecting a missing tag, it returns
the prepared error instead of allocating another identical object.

This targets the missing-Side NewOrderSingle rejection seen in PREPROD. Its
generic error subtree accounted for 417,818 sampled objects / 26,740,320 bytes
in the previous candidate rejection window. Those are sampled window totals,
not exact allocations per message or live heap.

```text
session setup -> immutable required-tag metadata and owned error values
message owner -> unchanged dictionary traversal -> selected missing tag
  -> immutable error -> reject control -> existing output slot
  -> writer channel -> completed write -> returned slot
retained error -> independent of later message/scratch reuse
```

No public API, validation rule, reject text, reject reason, reference field,
business classification, logging or persistence is removed or changed. There
is no new hot-path mutex, pool, growth or mutable error. Public validators, early
missing-MsgType checks, recursive group-required checks and unknown tags retain
their existing constructor fallback; this is not a whole-library error claim.

## Controlled measurements

Before runtime: `2568ec7288d573ba92ea0c353267612e2034075e` (PR29). After runtime:
the production changes delivered with this directory. Both captures use the
identical `required_validation_benchmark_test.go`, verified by `harness.sha256`.
Go 1.27.2, darwin/arm64, Apple M4 Pro, CPU 1, five one-second samples. Setup,
dictionary preparation and 66 warm-ups are outside timing. Correctness, race
checks and performance captures run serially.

The completed-reject row includes validation, control construction, borrowed
callback, bounded persistence, encoding, completed writer copy, returned output
slot, peer parse and explicit journal retirement. It is a QuickFIX component
measurement, not TCP round-trip or whole FIX-server latency.

| Operation | Before ns/op | After ns/op | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|---:|---:|
| Valid session validation | 647.1 | 633.4 | 0 / 0 | 0 / 0 |
| Missing Side validation, two error text reads | 209.8 | 184.4 | 64 / 1 | 0 / 0 |
| Validation through completed reject write | 3,711 | 3,525 | 64 / 1 | 0 / 0 |

Missing-field validation is 12.1% faster at the median; the completed reject
is 5.0% faster. The valid control is 2.1% faster. All mandatory baseline
byte/object rows are unchanged. These are local component results, not a
deployed throughput or latency claim. Raw captures are
[before](required-before.txt), [after](required-after.txt),
[baseline before](baseline-before.txt) and [baseline after](baseline-after.txt).

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^(BenchmarkSessionRequiredValidation|BenchmarkSessionRequiredReject)$' -benchmem -benchtime=1s -count=5 -cpu=1
```

## Correctness

Fixtures preserve header/body/trailer precedence, FIXT admin/application
dictionary selection and valid input after rejection. They check finite table
size, unknown/nil fallback, retained errors during slot reuse and validator
detachment, and concurrent readers of immutable errors. Mutable validators and
messages remain single-owner objects.

Completed FIX 4.0 and FIX 4.4 fixtures verify version-specific text, `35=3`,
reason `373=1`, reference tag `371=54`, reference type `372=D` where supported,
reference sequence, sender advancement and output-slot return. Exact non-race
counters require zero objects and bytes across 1,000 FIX44 missing-Side
validations plus 2,000 error text reads, and 100 completed reject writes.
FIXT admin correctness is covered; its exact allocation count is not separately
measured by these fixtures.

Focused fixtures and exact allocation counters passed. All-package tests
passed (core 30.398s), vet passed, and separate core/internal race tests passed
(35.166s / 1.435s). The frozen benchmark harness digest was verified after the
after capture. Receipts are [focused](focused-tests.txt),
[all packages](tests-go1.27.2.txt), [vet](vet-go1.27.2.txt) and
[race](race-go1.27.2.txt). The all-package run excludes only the previously
reproduced unrelated `TestMongoLogTestSuite` teardown failure when Mongo is
absent; store tests remain enabled. Independent source review found no
actionable correctness issue. No deployment is claimed by this phase.
