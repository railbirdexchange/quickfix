# Byte-oriented timestamp validation

Before measurements use unchanged production at
`0701bc4dac50b427ed05bdac8d6870c997af5bf8`
(`perf/configured-validation-rejects`).

The change is confined to boolean dictionary type validation for
`UTCTIMESTAMP` and `TIME`. Public `FIXUTCTimestamp.Read` retains its existing
receiver behavior and owned error details.

Go 1.27.2 accepts more than digit-only fractional fields: both period and comma
separators, leading plus, and leading minus when the fractional value is zero.
The validator normalizes these cases in at most 27 stack bytes
and reuses the existing canonical date/time checks. Invalid input returns
false without constructing a timestamp reader error.

This follows Go 1.27.2 `time/format.go`: fixed fractional fields call
`parseNanoseconds`, which uses signed `atoi` and rejects negative results.
Replacing the leading plus with zero preserves the field's width and numeric
value; replacing minus is valid only for all-zero remaining digits. Although
`stdHour` accepts one digit, the four library lengths and exact-width remaining
fields exclude that shorter form. A compensating extra character is rejected
as excess text. The independent oracle tests these constraints directly.

```text
message owner -> timestamp bytes
  -> canonical checks, or local [27]byte normalized copy
  -> boolean result -> existing validation reject
  -> control slot -> bounded persistence -> writer channel
  -> completed writer copy -> output slot returned
```

The stack copy is never returned or shared. Dictionary checks, reject reason,
reference fields, error text, logging and persistence remain in the flow.
The base already precomputes configured static validation rejects. This phase
removes only the timestamp reader's temporary errors inside boolean validation.
Public reader errors and unconfigured rejection paths retain their ownership.

The frozen fixtures compare acceptance with independent, length-selected
`time.Parse` calls. Cases cover all four lengths, comma and signed fractions,
negative zero, calendar boundaries including year zero, all byte replacements,
malformed lengths, and one-digit-hour forms padded to permitted total lengths.
The full validator fixture reads `Error()` twice. The completed-reject fixture
asserts message type, sequence, reference sequence/type/tag, reason, text,
persisted bytes, output-slot return and journal retirement.

The completed-reject benchmark uses the existing independent peer buffer and
writer completion notification. It is a component measurement, not TCP
round-trip or complete FIX-server latency. Setup and 66 warm-up operations are
outside timed loops.

Controlled commands (Go 1.27.2, darwin/arm64, Apple M4 Pro, five samples,
single CPU; medians below):

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' \
  -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' \
  -bench '^(BenchmarkTimestampValidationValue|BenchmarkSessionTimestampValidation|BenchmarkSessionTimestampReject|BenchmarkSessionRequiredReject)$' \
  -benchmem -benchtime=200ms -count=5 -cpu=1
```

| Flow | Before ns/op | After ns/op | Before B/op, allocs/op | After B/op, allocs/op |
|---|---:|---:|---:|---:|
| Boolean, seconds | 14.78 | 15.15 | 0, 0 | 0, 0 |
| Boolean, nanoseconds | 17.70 | 18.31 | 0, 0 | 0, 0 |
| Boolean, comma fraction | 74.10 | 18.06 | 0, 0 | 0, 0 |
| Boolean, plus fraction | 80.21 | 18.20 | 0, 0 | 0, 0 |
| Boolean, negative zero | 70.61 | 18.63 | 0, 0 | 0, 0 |
| Boolean, invalid calendar | 99.53 | 8.552 | 104, 2 | 0, 0 |
| Boolean, negative nonzero fraction | 111.6 | 4.267 | 136, 3 | 0, 0 |
| Boolean, invalid length | 27.62 | 2.260 | 80, 2 | 0, 0 |
| Full validation, valid | 634.7 | 635.8 | 0, 0 | 0, 0 |
| Full validation, comma plus fraction | 701.4 | 624.5 | 0, 0 | 0, 0 |
| Full validation, invalid calendar; two Error calls | 402.9 | 297.5 | 104, 2 | 0, 0 |
| Invalid calendar through completed reject | 3759 | 3670 | 104, 2 | 0, 0 |
| Unchanged missing-required-field completed reject control | 3435 | 3396 | 0, 0 | 0, 0 |

Canonical boolean checks increased by 0.37–0.61 ns in these samples. Full valid
validation remained effectively stable. The completed-reject timing improvement
is small; the allocation reduction is confirmed by exact counters, not inferred
from timing. These medians are per-operation benchmark costs, not network
latency percentiles.

The required baseline preserves every allocation row:

| Baseline flow | Before ns/op | After ns/op | B/op, allocs/op (both) |
|---|---:|---:|---:|
| Decimal, short | 10.94 | 11.04 | 0, 0 |
| Decimal, signed fraction | 27.16 | 27.48 | 0, 0 |
| Decimal, maximum uint128 | 58.31 | 57.15 | 0, 0 |
| Decimal, arbitrary-precision overflow | 309.0 | 315.2 | 168, 5 |
| Reused parse, no dictionary | 366.8 | 369.9 | 0, 0 |
| Reused parse, dictionary | 419.7 | 425.6 | 0, 0 |
| Reused parse, repeating groups | 800.3 | 795.2 | 0, 0 |
| Validate NewOrderSingle | 676.6 | 676.0 | 0, 0 |
| Build NewOrderSingle, legacy API | 749.1 | 761.3 | 144, 5 |
| Framed read, legacy API | 113.1 | 113.4 | 160, 2 |

The initial session fixture incorrectly changed only the parsed field map,
leaving its ordered validation walk unchanged. `fixture-development.txt` records
that fixture failure on unchanged production. Setup now reparses correctly
framed bytes before validation. The subsequent before correctness pass and
frozen fixture hashes are retained beside the benchmark receipts.

Correctness, race testing and benchmarks run serially with an exclusive local
CPU reservation. Frozen benchmark and parity fixture SHA-256 values are in
`fixture-sha256.txt`. Public `FIXUTCTimestamp.Read` is unchanged.

- Before correctness: PASS (`before-correctness.txt`).
- Before exact-counter regression: expected FAIL (`exact-before.txt`);
  100 completed calendar rejects allocate 200 objects / 10,400 bytes.
- After parity, retained reject, and exact counters: PASS
  (`after-correctness-exact.txt`); 1,000 calls in each of eight boolean cases
  and 100 completed calendar rejects each allocate zero objects / zero bytes.
- Independent time.Parse differential fuzz: PASS, 242,194 executions in 20s
  (`fuzz.txt`). This supplements the deterministic all-byte and calendar cases.
- Existing real-TCP full-session exact counters before: PASS
  (`full-session-before.txt`), seven scenarios with 100 completed cycles each
  and zero objects / zero bytes.
- Full core, internal, dictionary, and config packages: PASS (`core-tests.txt`).
  Core completed in 37.998s; its seven real-TCP allocation scenarios again
  report zero objects / zero bytes for 100 completed cycles each.
- Separate race checks: PASS (`race.txt`), core 47.464s plus internal and
  dictionary packages. Vet: PASS, exit 0 with empty output (`vet.txt`).

Check commands:

```sh
GOTOOLCHAIN=auto rtk proxy go test . \
  -run '^(TestTimestampValidation|TestSessionTimestampRejectExactAllocations|TestFIXUTCTimestampReadLegacyParity)' \
  -count=1 -v
GOTOOLCHAIN=auto rtk proxy go test . \
  -run '^TestBoundedFullSessionExactAllocations$' -count=1 -v
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' \
  -fuzz '^FuzzTimestampValidationLegacyParity$' -fuzztime=20s -parallel=1
GOTOOLCHAIN=auto rtk proxy go test . ./internal/... ./datadictionary/... ./config/... -count=1 -v
GOTOOLCHAIN=auto rtk proxy go test -race . ./internal/... ./datadictionary/... ./config/... -count=1
GOTOOLCHAIN=auto rtk proxy go vet . ./internal/... ./datadictionary/... ./config/...
```

The unrelated Mongo logging teardown failure documented in earlier phases was
not rerun; this phase changes no logger or Mongo code. No deployed service
qualification or whole-library zero-allocation claim is made here.
