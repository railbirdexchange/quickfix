# Configured validation rejects

The private session validator prepares immutable errors for configured fields
whose rejection contains only a tag, a reason and fixed text. Format, enum,
empty-value, duplicate, field-order, field-not-defined and group-count failures
select a prepared result after the existing validation traversal selects the
failure. Required children of nested groups also use prepared required-field
errors. Validation order, settings, dictionary rules and public APIs stay intact.
The parameter-free `InvalidMessageType`, `UnsupportedMessageType` and
`valueIsIncorrectNoTag` constructors also return prepared immutable interfaces,
preserving the same concrete `messageRejectError` value and all its fields.

Preparation follows dictionary applicability: enum errors require an enumerated
field; scalar format errors require a validated type; count errors require a
group root. Group roots also prepare integer-format errors because the existing
walker reads their count as `FIXInt`, even if a custom dictionary declares
another scalar type. Any configured field can be empty, duplicated, misplaced
after a trailer, or used in a message that does not define it.

```text
session setup -> immutable dictionaries -> finite tag/reason index
  -> one immutable compact error block
session owner -> unchanged validation traversal -> prepared error
  -> reject control -> bounded journal/output slot -> writer channel
  -> completed write -> returned output slot
retained error -> immutable block, independent of message/frame/scratch reuse
```

The index never changes after setup. Results carry no message, frame, scratch,
dictionary or session reference. Retaining one result can retain its compact
immutable block, as with the existing prepared missing-field errors.

## Measurement boundary

Before runtime: `842c3f3604bbfafdf1c9035026083731bb106f2c`, the PR34 documentation
head containing runtime `22aa04daf73729d407643ff45fd52da497de566d`.
The before harness was added and its correctness passed before runtime edits.
Both sides use the same frozen category, completed-write and setup benchmark
source. Go 1.27.2, darwin/arm64, Apple M4 Pro; five one-second samples, CPU 1.
Dictionary loading, session/message construction and 66 warm-ups are outside
timed loops except the explicitly labeled validator-setup benchmark.

Validation rows retain the returned error and read its text twice. The two
completed-reject rows include validation, control construction, borrowed
callback, sequence advancement, bounded journal persistence, encoding, completed
writer copy, returned output slot, independent peer parse and journal retirement.
They check reason, reference tag/type/sequence, text and persisted bytes.
These are QuickFIX component measurements, not TCP RTT or FIX-server throughput.

Raw before captures: [categories/completed/setup](configured-before.txt),
[mandatory baseline](baseline-before.txt), [focused parity](focused-before.txt),
[source hashes](source-before.sha256), [toolchain](toolchain.txt).

The additional parameter-free constructor and invalid-MsgType benchmarks were
frozen and measured before changing those three constructors, after the
configured-tag implementation had been written. The invalid-MsgType check
returns before configured-field validation, so these rows do not exercise the
already-changed tag/reason table. See [extra before](static-before.txt),
[pre-change parity](static-focused-before.txt) and
[extra before source hashes](static-source-before.sha256). A separate row
retains `GetString(35)` for the unconfigured long type `UNKNOWN.TYPE`; its owned
string allocation remains part of that row on both sides.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkConfigured(ValidationReject|CompletedReject|ValidationSetup)$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkStaticReject' -benchmem -benchtime=1s -count=5 -cpu=1
```

## Results

Median ns/op from five samples. B/op and allocs/op agree across all five samples.
Raw [configured after](configured-after.txt) and [after source hashes](source-after.sha256).

| Validation row | Before ns/op | After ns/op | B/op before → after | allocs/op before → after |
|---|---:|---:|---:|---:|
| Valid control | 618.2 | 613.4 | 0 → 0 | 0 → 0 |
| Scalar format | 333.1 | 303.4 | 64 → 0 | 1 → 0 |
| Enum | 301.6 | 274.9 | 64 → 0 | 1 → 0 |
| Empty value | 87.10 | 65.72 | 64 → 0 | 1 → 0 |
| Duplicate | 544.2 | 521.7 | 64 → 0 | 1 → 0 |
| Group count | 637.0 | 613.6 | 64 → 0 | 1 → 0 |
| Field order | 114.3 | 92.62 | 64 → 0 | 1 → 0 |
| Field not defined for message | 618.6 | 595.3 | 64 → 0 | 1 → 0 |
| Required group child | 412.3 | 399.6 | 64 → 0 | 1 → 0 |
| Unknown tag fallback | 339.7 | 340.6 | 64 → 64 | 1 → 1 |
| Malformed timestamp | 207.7 | 195.3 | 112 → 48 | 3 → 2 |
| Completed enum reject | 3,683 | 3,598 | 64 → 0 | 1 → 0 |
| Completed group-count reject | 4,049 | 4,010 | 64 → 0 | 1 → 0 |

The small completed-write timing differences are descriptive local medians,
not a claim of a significant transport-latency improvement.

The parameter-free constructor rows retain the result and read `Error()` twice.
Raw [static after](static-after.txt):

| Row | Before ns/op | After ns/op | B/op before → after | allocs/op before → after |
|---|---:|---:|---:|---:|
| `InvalidMessageType` | 24.20 | 2.475 | 64 → 0 | 1 → 0 |
| `UnsupportedMessageType` | 25.48 | 2.377 | 64 → 0 | 1 → 0 |
| `valueIsIncorrectNoTag` | 24.30 | 2.420 | 64 → 0 | 1 → 0 |
| Invalid-MsgType validation | 49.79 | 22.73 | 64 → 0 | 1 → 0 |
| Same validation + retained long `GetString(35)` | 70.00 | 48.36 | 80 → 16 | 2 → 1 |

| Setup row | Before ns/op | After ns/op | B/op before → after | allocs/op before → after |
|---|---:|---:|---:|---:|
| FIX44 session validator construction | 27,220 | 499,638 | 45,016 → 564,448 | 281 → 343 |

FIX44 prepares 4,629 applicable tag/reason pairs. Their compact immutable result
block contains 74,064 bytes, excluding the lookup and required-field metadata.
The setup total includes transient map allocation. It increases by 519,432 bytes
per construction; it must not be described as retained-heap size or steady-state
message cost.

The mandatory baseline retains every allocation count. Its local timing
medians are listed without attributing small differences in unchanged controls
to this change. Raw [before](baseline-before.txt) / [after](baseline-after.txt):

| Baseline row | Before ns/op | After ns/op | B/op on both | allocs/op on both |
|---|---:|---:|---:|---:|
| UDecimal short read | 10.74 | 11.05 | 0 | 0 |
| UDecimal signed fraction | 27.00 | 27.66 | 0 | 0 |
| UDecimal max uint128 | 56.95 | 59.33 | 0 | 0 |
| UDecimal overflow | 307.3 | 315.7 | 168 | 5 |
| Reused parse without dictionary | 363.9 | 370.6 | 0 | 0 |
| Reused parse with dictionary | 423.2 | 432.5 | 0 | 0 |
| Reused parse with group | 796.3 | 799.6 | 0 | 0 |
| Valid NewOrderSingle validation | 668.5 | 680.9 | 0 | 0 |
| Legacy NewOrderSingle build | 755.2 | 757.8 | 144 | 5 |
| Legacy framed read | 112.5 | 113.2 | 160 | 2 |

## Preserved fallbacks

Unknown tags never enlarge the table and retain the existing owned constructor.
Public validators also retain their tag-specific constructor path. Early missing
MsgType errors and dynamic convenience-API diagnostics are outside this change.

Malformed timestamp validation still calls the existing public timestamp reader
and discards its parser error; this phase removes only the outer static reject.
The parser still accepts its legacy fallback forms, including comma fractions.
No timestamp parser or error-string contract changes.

Ordinary `NUMINGROUP` format errors stop in byte validation before the group
walker. A custom group declared as `STRING` can instead reach `FIXInt.Read` and
its transient `errors.New("invalid format")` or `errors.New("empty bytes")`.
Those internal reader errors are not changed here. Integer overflow keeps its
existing wrapping behavior; the fixtures compare against the legacy validator
rather than introducing stricter parsing under an allocation change.

Preparing a finite table increases per-session setup and retained memory.
The setup benchmark measures total allocated bytes and objects, including
temporary map growth; it is not an exact retained-heap measurement. The metadata
fixture separately reports compact block size, excluding the lookup and existing
required-field metadata. This phase claims reduced per-rejection allocation,
not reduced startup heap or allocation-free arbitrary errors.

## Correctness and exact counters

[Focused after results](focused-after.txt) compare the complete reject contract:
text, reason, optional reference tag, business/session classification and business
reference ID. Fixtures cover paired-fault precedence, FIXT application/admin
messages, header/trailer groups, custom group types and legacy overflow behavior.
Returned errors survive later valid parsing, slot reset and validator teardown;
multiple goroutines can read the retained immutable errors.

Non-race whole-interval counters require exactly zero `Mallocs` and `TotalAlloc`
across 1,000 validations for each static configured category and all three
parameter-free constructors, plus invalid-MsgType full validation. Separate
intervals require exactly zero across 100 completed enum and group-count rejects.
The completed-wire tests verify all affected normal rejecting categories and a
missing required group child. The custom `STRING` group reader boundary reports
1,000 allocations/16,000 bytes across 1,000 invalid-count validations; ordinary
`NUMINGROUP` format rejection reports zero.

The package command deliberately excludes the previously reproduced unrelated
Mongo logging teardown suite, `TestMongoLogTestSuite`
([original diagnosis](../../phase-1/README.md#existing-optional-mongo-logger-test-failure),
[original before failure](../../phase-1/tests-before.txt)). It does not disable
validation, logging or persistence in the measured flows. Mongo store tests and
all other package tests are included; this does not establish a live Mongo
integration run.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^(TestConfigured|TestStaticReject|TestBoundedValidationPairedFaultPrecedence)' -count=1 -v -timeout=2m
GOTOOLCHAIN=auto rtk proxy go test -p=1 ./... -skip '^TestMongoLogTestSuite$' -count=1 -timeout=5m
GOTOOLCHAIN=auto rtk proxy go test -race -p=1 . ./internal -count=1 -timeout=5m
GOTOOLCHAIN=auto rtk proxy go vet ./...
```

[Package test output](packages-after.txt) passed, including the core session
fixtures and exact counters (core 38.833 s). [Separate race output](race-after.txt)
passed (core 46.275 s, internal 1.577 s). [Vet](vet-after.txt) exited 0 with empty
output. Performance runs completed before race testing.
Source syntax remains Go 1.23-compatible; this phase runs the repository-selected
[Go 1.27.2 toolchain](toolchain-after.txt) and adds no dependency or API change.
