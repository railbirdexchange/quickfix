# Heartbeat external-store ownership

This phase measures QuickFIX control-message processing and its synchronous
external-store handoff. Before this change, every external store receives an
owned `bytes.Clone` of the encoded output slot, even when the store consumes
the bytes synchronously. The new explicit `BorrowedMessageStore` capability
lets that store receive a read-only view for the duration of its save call.
Legacy stores and decorators that do not explicitly opt in keep owned bytes.

## Scope and ownership

```text
reused control message / borrowed application callback
    → sender reserves and encodes an output slot
    → synchronous borrowed-store call
        → store copies retained bytes into its own journal before return
    → pending/writer channel transfers the output slot
    → completed write into independent peer buffer
    → writer returns slot to vacant channel
    → benchmark observes completion and parses the peer buffer
```

The store borrows bytes on both success and error; it may never retain that
view or mutate it. Callback execution, persistence, sequence advancement,
completed writes and returned slots remain part of every measured operation.
Capacity exhaustion and persistence errors keep their existing behavior. This
capability does not add reservation or atomic-commit guarantees to an external
store; it preserves that store's existing save method semantics.

The three store configurations share the same bounded journal implementation:

- `bounded`: internal prepared journal path, unchanged control.
- `legacy_external`: decorator whose ordinary save override must run and may
  retain its argument.
- `borrowed_external`: explicit owner accepts borrowed input and copies into
  the same preallocated journal before returning.

All use the existing borrowed application adapter and `NullLog`. A fixed-size
independent peer buffer receives the completed writer output. These are
**completed-write component timings, not TCP or full-session latency**. No
application, Kafka, protobuf or telemetry work is claimed by this fixture.

`idle_heartbeat` calls the production heartbeat sender. `timer_heartbeat`
expires the actual reused `EventTimer`, receives its `sessionEvent` and invokes
the production state-machine `Timeout`. Resetting it to zero avoids including
the configured idle interval; no per-operation timer or goroutine is created.
`test_request` sends the production TestRequest and verifies `112=TEST`.

The existing fixture pre-fills 1,023 of 1,024 journal cells. Sixty-six completed
sends warm the final cell and all 33 output slots twice before timing. Each
operation checks one callback, exactly one sender-sequence increment, expected
message type/sequence/TestReqID, persisted bytes equal to the completed wire,
and all slots returned. Explicit `RetireBefore` advances the acknowledged
journal floor; new messages and completed writes do not evict retained data.

## Reproduction

Measured on Apple M4 Pro, darwin/arm64, Go 1.25.3, `-cpu=1`. Before runtime:
`ae90fb273d7c608058b614413e400ae0ae97f817`; exact measured source hashes are
recorded in `before-source.sha256`. Harness and runtime hashes identify the
candidate source separately from later documentation changes.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkHeartbeatStore$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkHeartbeatStore$/^idle_heartbeat$/^borrowed_external$' -benchmem -benchtime=100000x -count=1 -cpu=1 -memprofilerate=1 -memprofile=benchmarks/zero-allocation/phase-7/heartbeat-store/before.alloc.pprof -o benchmarks/zero-allocation/phase-7/heartbeat-store/quickfix-before.test
rtk proxy go tool pprof -top -alloc_objects -focus='prepareOutbound' benchmarks/zero-allocation/phase-7/heartbeat-store/quickfix-before.test benchmarks/zero-allocation/phase-7/heartbeat-store/before.alloc.pprof
```

The profiling run uses a fixed iteration count for attribution; its latency is
not used for the comparison. The before profile records 100,133 allocations at
`bytes.Clone` in `prepareOutbound` (100,000 measured iterations plus calibration
and warm-up). The sampled source listing was saved before runtime edits.
Compiled profiling executables are ignored; profiles, source hashes and raw
benchmark output are retained.

## Controlled result

All five samples agree on the allocation counts. Times below are sample
medians; the small timing differences do not establish a broad latency gain.
The demonstrated improvement is removal of one owned frame allocation from
every explicitly borrowed external-store handoff in these fixtures.

| Flow / store | Before B/op | Before allocs/op | After B/op | After allocs/op | Before ns/op | After ns/op |
|---|---:|---:|---:|---:|---:|---:|
| Idle heartbeat / bounded | 0 | 0 | 0 | 0 | 1906 | 1880 |
| Idle heartbeat / legacy external | 80 | 1 | 80 | 1 | 1663 | 1674 |
| Idle heartbeat / borrowed external | 80 | 1 | 0 | 0 | 1687 | 1641 |
| Timer heartbeat / bounded | 0 | 0 | 0 | 0 | 2526 | 2425 |
| Timer heartbeat / legacy external | 80 | 1 | 80 | 1 | 2222 | 2216 |
| Timer heartbeat / borrowed external | 80 | 1 | 0 | 0 | 2217 | 2166 |
| TestRequest / bounded | 0 | 0 | 0 | 0 | 2058 | 2103 |
| TestRequest / legacy external | 80 | 1 | 80 | 1 | 1858 | 1806 |
| TestRequest / borrowed external | 80 | 1 | 0 | 0 | 1792 | 1777 |

Raw [before](heartbeat-before.txt) and [after](heartbeat-after.txt) output
preserves all 45 samples each. The harness SHA-256 is identical in the
[before](before-source.sha256) and [after](after-source.sha256) records.
The internal bounded path performs prepared-journal admission checks that the
external-store wrapper does not, so compare each store with its own before
measurement instead of interpreting cross-store timings as equivalent work.

The mandatory [before](baseline-before.txt) and [after](baseline-after.txt)
allocation baselines pass with unchanged allocation rows: normal unsigned
decimal parsing, reused parsing (including dictionary/groups) and validation
remain 0 B/0 allocations; decimal overflow remains 168 B/5, convenience
building 152 B/6 and legacy frame reading 160 B/2.

The matched [after profile](profile-after-objects.txt) has no allocation
samples in `prepareOutbound`; the [byte profile](profile-after-bytes.txt)
agrees. The 100,000-iteration profiling run itself reports 0 B/0 allocations.
The after source-list command exits 2 with `no matches found for regexp:
prepareOutbound` because that allocating stack no longer appears; this is
recorded in [profile-after-source.txt](profile-after-source.txt), not treated
as a correctness-test failure. Whole-process profiles still include fixture
construction, test calibration and other untimed setup allocations.

## Correctness coverage

`heartbeat_store_test.go` exercises all control/store combinations, legacy
decorator overrides, explicit decorator re-opt-in, noncomparable store values
and returned owners, save failure with unchanged journal/sequence state and
returned output/control ownership, successful retry, disabled persistence,
and retained bytes surviving two full output-ring rotations. The borrowed
store inspects slot aliasing only during its synchronous call and retains an
owned copy; the legacy store retains its unchanged owned argument.

The focused [`TestHeartbeatStore` run](tests-after.txt) passes in 0.632 seconds.
The [Go 1.23.12 core/local-package run](core-go1.23.12.txt) passes: full core
31.319 seconds, plus internal timer helpers, dictionaries, memory/file stores
and composite/file loggers. The core run includes the existing real-TCP
session, ownership, protocol and exact-allocation tests.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^TestHeartbeatStore' -count=1 -timeout=120s
GOTOOLCHAIN=go1.23.12 rtk proxy go test -p=1 . ./internal ./datadictionary ./store/memory ./store/file ./log/composite ./log/file -count=1 -timeout=180s
GOTOOLCHAIN=go1.23.12 rtk proxy go test -race -p=1 . ./internal -count=1 -timeout=180s
GOTOOLCHAIN=go1.23.12 rtk proxy go vet . ./internal ./datadictionary
```

The separate [Go 1.23.12 race run](core-race-go1.23.12.txt) passes: core
36.384 seconds and internal timer helpers 1.482 seconds. Performance was
measured without race instrumentation. [Go 1.23.12 vet](vet-go1.23.12.txt)
for core/internal/datadictionary also passes (exit 0, no diagnostics).

No external Mongo/SQL integration or live environment is involved. The
previously reported `TestMongoLogTestSuite/TestMongoLog` and
`TestMongoLogTestSuite/TestMongoLogNoSession` teardown failures in `log/mongo`
were reproduced by `GOTOOLCHAIN=auto rtk proxy go test ./... -count=1` before
and after phase 1, with `MONGODB_TEST_CXN` absent; see the [separate failure
evidence](../../phase-1/README.md#existing-optional-mongo-logger-test-failure).
That command and those integration tests were not rerun by this phase; they
are not part of these core/local-package results.
