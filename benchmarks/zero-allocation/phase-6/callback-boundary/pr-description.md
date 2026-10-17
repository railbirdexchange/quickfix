## Ordinary outbound slots with compatible callback ownership

Ordinary `SendToTarget` now prepares the original caller-owned message,
persists independent wire bytes and transfers a fixed output slot to the
writer. The writer returns the slot only after the complete write or a terminal
failure. This reduces allocations while preserving synchronous callbacks,
persistence, resend ordering and the existing send signatures.

The default callback contract remains owned. `ToApp` and `ToAdmin` receive the
original caller pointer, saved engine-field byte slices survive later sends,
and retained resend messages survive replay completion and journal reuse. Each
engine scalar copies its wire/getter bytes into disjoint regions of one owned
allocation. Replay materializes an owned message from the already bounded
parsed representation, preserving raw body/group wire behavior and callback
header edits. `BorrowApplication` explicitly changes callback lifetime only;
both contracts use the same engine, storage and writer.

This phase-6 PR stacks on the inbound ownership work in #20. Complete-session
and deployed FIX-server qualification remain separate from these component
measurements.

```mermaid
sequenceDiagram
    participant Caller as Caller owns original Message
    participant Send as Existing serialized send
    participant Slots as Vacant slot channel
    participant Store as Configured resend journal
    participant Writer as Socket writer
    Caller->>Send: SendToTarget(original Message)
    Slots->>Send: Reserve fixed output slot
    Send->>Store: Reserve journal capacity before state mutation
    Send->>Caller: ToApp / ToAdmin(original pointer)
    Send->>Send: Keep owned scalar views; encode independent wire
    Send->>Store: Persist wire and advance sender sequence
    Send-->>Caller: Return; caller retains Message and old field views
    Send->>Writer: Transfer output-slot ownership
    Writer->>Writer: Complete write or record terminal failure
    Writer->>Slots: Return output slot
    Store->>Send: Retained frame for bounded replay parse
    Send->>Caller: Owned replay Message; borrowed only if explicitly wrapped
```

### Allocation evidence

Compatible original phase-6 baseline versus the current ordinary delivery:

| Completed-write flow | Before B/op | Before allocs/op | Current owned B/op | Current owned allocs/op | Current owned ns/op |
|---|---:|---:|---:|---:|---:|
| NewOrderSingle | 343 | 20 | 88 | 4 | 1,867–1,946 |
| Repeating group | 551 | 22 | 96 | 4 | 2,962–3,018 |
| Long custom message type | 503 | 24 | 96 | 4 | 1,851–1,880 |

The explicit borrowed cases remain 0 B/op and 0 allocs/op, at 1,820–1,863 ns
for orders, 2,886–2,923 ns for groups and 1,780–1,838 ns for custom message
types. Go 1.25.3, Apple M4 Pro, CPU=1, five one-second repetitions.

The immediate pre-correction `c41682e9` candidate also reported zero allocations
but silently shortened callback lifetimes. Those results are incompatible and
are not used as the allocation-reduction baseline. The current owned boundary
adds four allocations relative to that candidate. The original compatible
baseline and current results are separate runs with intervening inbound work;
their timing difference does not isolate this callback correction's effect.

Each timed operation includes ordinary preparation/callback, encoding,
persistence, completed write, slot return, peer parsing and explicit journal
retirement. Setup and warming all 33 slots twice occur outside timing; the
journal starts with 1,023 of 1,024 entries retained. These are component
measurements with a fixed peer sink, not TCP or FIX-server round trips.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkScopedOutboundSession$' -benchmem -benchtime=1s -count=5 -cpu=1
```

Both commands ran before and after. Mandatory baseline allocations are unchanged:
decimal fast parsing, reused parsing with/without dictionary/groups and
NewOrderSingle validation remain 0/0; decimal overflow remains 168 B/5
allocations, legacy construction 152 B/6 and legacy framed read 160 B/2.
The current outbound harness distinguishes `/owned` and `/borrowed` delivery.

Full raw measurements, the historical compatible baseline, failing-before
regressions and current verification are linked in the
[callback ownership evidence](https://github.com/railbirdexchange/quickfix/blob/perf/bounded-outbound-ownership/benchmarks/zero-allocation/phase-6/callback-boundary/README.md).

### Correctness and limits

- New regression fixtures retain scalar views across second/third sends for
  both callback types, verify original pointer identity, and ensure mutations
  of old views cannot change current fields or encoded wire.
- Replay fixtures retain the message/raw bytes/groups through completed resend,
  retirement and reuse of the same journal entry, then a second resend. They
  verify persisted body behavior, BodyLength and checksum.
- Existing coverage preserves callback mutation/rejection, store/logger
  retention, partial and failed writes, slow consumers, full rings, reset
  atomicity, reconnect and replay beyond output capacity.
- Go 1.23.12 package tests and vet passed. The full core race run passed in
  19.389 seconds, separately from benchmarks. The previously reproduced Mongo
  logger teardown failure is documented in phase 1; only
  `TestMongoLogTestSuite` is excluded from the package run.

```sh
GOTOOLCHAIN=go1.23.12+auto rtk proxy go test ./... -skip '^TestMongoLogTestSuite$' -count=1
GOTOOLCHAIN=go1.23.12+auto rtk proxy go vet ./...
GOTOOLCHAIN=auto rtk proxy go test -race . -count=1
```

Measured limits are 32 pending frames plus one output slot, 16,384 bytes per
frame, a 30-second complete-frame write timeout, and a 1,024-entry/16-MiB
in-memory bounded journal. Replay permits 512 fields, 128 group entries and
depth 8. Capacity is reserved before sequence assignment and persistence;
exhaustion returns an error before committed sequence/store mutation. History
is never silently evicted, and socket completion alone does not retire it.

Existing stores/loggers remain supported, and durable stores are not replaced.
No new mutex, pool, reference counting, dependency or automatic hot-path slice
growth is introduced. Default ownership copies allocate; only the explicit
borrowed measured path is zero-allocation within these bounds. Unbounded
stores, allocating loggers, arbitrary-precision decimal overflow, fresh body
construction and legacy convenience APIs remain outside that claim. No
validation, persistence, resend handling or logging is disabled. One-microsecond
and complete FIX-server latency are not claimed by this PR.
