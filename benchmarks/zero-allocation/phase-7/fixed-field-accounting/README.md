# Fixed-field lifecycle indexes and wire accounting

The deployed service profile attributed 1.81 seconds of sampled CPU to
`fixedFieldMap.length` and 0.68 seconds to `fixedFieldMap.total` in a 20-second
capture with 117.82 seconds of aggregate CPU samples. Both methods scanned every
reserved hash-table entry, including vacant capacity, when constructing owned
wire strings. The original [CPU summary](source-service-cpu.txt) is retained;
these are sampled CPU totals, not per-message timings.

This change reuses the existing touched-entry index for length and checksum
accounting. Removed entries remain in that bounded index until reset and are
still skipped. The index records each table slot only once, so repeated removal
and reinsertion cannot count a field twice. Lookup, probing, tag ordering, wire
encoding, validation and public ownership contracts are unchanged.

The tag and touched indexes now occupy disjoint, capacity-capped slices of one
fixed allocation. `Tag` is an integer type; the private touched slice stores
table indexes. Neither slice can extend into the other's storage. The existing
capacity checks also bound their combined allocation without a new size limit.
This removes one setup allocation per fixed map; it does not remove the owned
output string produced by `Message.String`.

```text
message slot / construction arena owns fixedFieldMap
  -> fixed entry table
  -> one fixed integer backing allocation
       -> tag slice [0:maxFields), independently capacity-capped
       -> touched-entry slice [maxFields:end), independently capacity-capped

first insertion into a vacant entry -> record its index once
  -> length/checksum inspect touched occupied entries
  -> removal clears entry references and leaves a tombstone
  -> reset clears every touched entry, then resets both index lengths

String callback -> independently owned string retained by caller
  -> encode/persist -> writer owns output slot
  -> write completes -> slot returns -> independent peer parses completed bytes
```

No mutex, pool, reference count, unsafe operation, dependency, public API or
automatic hot-path growth is added.

## Matched measurements

Baseline runtime: `549a129ac2ce277a3fe8d81cb325da525a8d14a0`.
Both captures use the same benchmark harness, Go 1.27.2, darwin/arm64,
Apple M4 Pro, one CPU, five one-second samples. Source and compiler receipts are
in [before hashes](source-before.sha256), [after hashes](source-after.sha256) and
[compiler](compiler.txt). Builds, tests and performance captures were serialized.

The following times are medians. Allocation counts and bytes agree across all
five samples in each row. Raw measurements:
[before](fixed-before.txt), [after](fixed-after.txt).

| Operation | Before ns/op | After ns/op | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|---:|---:|
| Setup, capacity 4 | 137.2 | 140.2 | 1,072 / 4 | 1,072 / 3 |
| Setup, capacity 64 | 832.7 | 739.7 | 15,184 / 4 | 15,184 / 3 |
| Setup, capacity 256 | 2,078 | 1,586 | 55,376 / 4 | 55,376 / 3 |
| Sparse length + checksum, 8 of 256 fields | 473.9 | 61.96 | 0 / 0 | 0 / 0 |
| Dense length + checksum, 256 of 256 fields | 2,377 | 1,746 | 0 / 0 | 0 / 0 |
| Grouped length + checksum, 8 groups | 554.0 | 126.4 | 0 / 0 | 0 / 0 |
| Tombstones, 256 touched / 8 occupied | 482.3 | 278.4 | 0 / 0 | 0 / 0 |
| Lookup, 8 populated | 2.079 | 2.060 | 0 / 0 | 0 / 0 |
| Lookup, 256 populated | 2.077 | 2.073 | 0 / 0 | 0 / 0 |
| Remove/reinsert/reset churn | 116.5 | 116.3 | 0 / 0 | 0 / 0 |
| Owned String callback plus completed write | 6,057 | 2,287 | 112 / 1 | 112 / 1 |

The measured allocation reduction is **one setup object per fixed map**.
Measured setup bytes are unchanged. Runtime layout remains 96 bytes per entry
and 72 bytes per map backing; the before/after correctness logs include the
native layout. The capacity-4 setup timing increased slightly, so this evidence
does not claim a universal setup-time improvement.

Accounting and lookup fixtures are warmed before timing. Setup rows deliberately
measure construction with retained results. The completed-write fixture warms
all 33 output slots twice, calls `Message.String` in the borrowed application's
outgoing callback, and waits for writer completion, slot return and independent
peer parsing on every operation. It includes the required owned wire string.
Its fixed copying connection, bounded store and null logger are component
fixtures; the 2.287 microseconds is not a PREPROD socket latency or a whole-service
throughput result.

## Matched real-TCP control

The existing `BenchmarkBoundedFullSession` ordinary and groups scenarios were
also measured unchanged, first at parent
`549a129ac2ce277a3fe8d81cb325da525a8d14a0`, then at candidate
`22aa04daf73729d407643ff45fd52da497de566d`. Both worktrees were clean before
measurement. The same Go 1.27.2 toolchain, one CPU and five one-second samples
were used serially, with no other local builds, tests or load clients running.
The three full-session fixture sources and `go.mod` have identical
[before](tcp-before-source.sha256) and [after](tcp-after-source.sha256) hashes.
Separate [before head](tcp-before-head.txt), [after head](tcp-after-head.txt),
[before compiler](tcp-before-toolchain.txt) and
[after compiler](tcp-after-toolchain.txt) receipts identify the captures.

One operation sends **32 NewOrderSingle requests**, reads and validates their
32 execution-report responses over real loopback TCP, then sends a heartbeat
acknowledgement and waits for the session-owner checkpoint. That checkpoint
verifies sequence progression, all 33 output slots returned, and zero retained
journal entries. Dictionary validation, optional groups, application handling,
variable-field response construction, persistence, socket writes, peer
validation and acknowledgement are timed. Setup and warm-up are excluded.

| Scenario | Before ns / 32-request batch | After ns / 32-request batch | Median change | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|---:|---:|---:|
| Ordinary | 248,605 | 249,291 | +0.28% | 0 / 0 | 0 / 0 |
| Groups | 279,054 | 282,042 | +1.07% | 0 / 0 | 0 / 0 |

All ten samples per head report 0 B/op and 0 allocs/op. Raw captures:
[before](tcp-before.txt), [after](tcp-after.txt), PASS in 51.229s and 51.953s.
The separate exact allocation assertions described below remain the proof
against per-operation rounding.

This control does **not demonstrate a TCP timing improvement**. Its callback
does not call `Message.String`, so it measures the existing bounded core flow,
separately from the owned-string component improved above. Dividing these batch
times by 32 gives amortized elapsed time per request, including shared
acknowledgement overhead; neither the batch times nor that normalization are
individual-message RTT, engine CPU time or PREPROD latency.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkBoundedFullSession$/^(ordinary|groups)$' -benchmem -benchtime=1s -count=5 -cpu=1 -timeout=120s
```

## Mandatory allocation baseline

The mandatory [baseline before](baseline-before.txt) and
[baseline after](baseline-after.txt) retain the standard five-sample capture.
Their allocation rows are unchanged: reused parsing with or without dictionary
and groups, validation, and non-overflow unsigned decimal reads remain 0 B/op
and 0 allocs/op. The owned build wrapper remains 144 B/op and 5 allocs/op,
the legacy framed reader 160 B/op and 2 allocs/op, and the unsigned-decimal
overflow fallback 168 B/op and 5 allocs/op.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkFixedField(Setup|Accounting|Lookup|Churn|CompletedString)$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
```

## Correctness

The baseline accounting tests passed before the runtime edit. Candidate focused
tests cover sparse/dense field values, grouped ranges, framing-tag exclusions,
scalar replacement of a group, remove/reinsert churn, shared FieldMap value
copies, disjoint index capacities, sorting, atomic capacity failure, reset and
clearing every retained field reference. Existing message-slot, String and
append fixtures run in the same focused check.

Receipts: [before correctness](correctness-before.txt),
[candidate correctness](correctness-after.txt).

Final checks passed with `GOTOOLCHAIN=auto` using Go 1.27.2:

| Check | Result and receipt |
|---|---|
| Focused accounting, scoped fields, message slots, String and append | PASS, [focused output](correctness-after.txt) |
| Mandatory baseline | PASS before / after, 78.938s / 77.750s |
| Focused benchmarks | PASS before / after, 83.249s / 78.664s |
| Core and affected local packages | PASS, root package 38.784s, [core output](core.txt) |
| Separate race check | PASS, root package 42.504s and internal 1.622s, [race output](race.txt) |
| Vet | Exit 0, no diagnostics, [vet output](vet.txt) |

The unfiltered core run includes `TestBoundedFullSessionExactAllocations`
(real TCP, completed writes) and the existing observed-heartbeat exact
allocation assertion. These remain separate from the in-memory String
component timing above. Tests with allocation assertions are excluded from
race builds by their existing build constraints.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^(TestFixedField|TestScopedFixed|TestMessageSlot|TestMessageString|TestMessageAppend)' -count=1 -v
GOTOOLCHAIN=auto rtk proxy go test -p=1 . ./internal ./datadictionary ./store/memory ./store/file ./log/composite ./log/file -count=1 -timeout=180s
GOTOOLCHAIN=auto rtk proxy go test -race -p=1 . ./internal -count=1 -timeout=180s
GOTOOLCHAIN=auto rtk proxy go vet . ./internal ./datadictionary
```

Source remains compatible with Go 1.23; this phase uses the current configured
toolchain only. The previously reported external Mongo logging teardown
failure is outside the changed flow and these local-package checks; it was not
rerun or claimed fixed.

No service deployment or new live measurement is part of this phase. Remaining
generated string/decimal getters and owned `Message.String` outputs retain their
existing owned storage; this change does not claim the whole library is
allocation-free.
