# Phase 6: outbound ownership and bounded retention

`BoundedOutbound=Y` adds an explicitly owned builder, fixed output slots, a
completed-write return channel and a strict in-memory resend journal. It requires
`BoundedInbound=Y`, `BorrowedOutboundApplication`, `BorrowedLog`, enabled message
persistence, and a concrete `BoundedStore` supplied by the store factory.

```mermaid
sequenceDiagram
    participant Handler as Borrowed inbound callback
    participant Session as Session owner
    participant Journal as Fixed resend journal
    participant Ready as Writer channel
    participant Writer as Socket writer
    participant Completed as Completed-slot channel
    Handler->>Session: BeginOutbound, typed mutations, Send
    Session->>Session: Reserve output slot and queue permit
    Session->>Journal: Reserve entry without changing committed state
    Session->>Session: Set prospective sequence, outbound callback, encode and check limits
    Session->>Journal: Copy bytes and commit sequence atomically
    Session->>Ready: Transfer exclusive output slot
    Ready->>Writer: Transfer slot; return queue permit
    Writer->>Writer: Complete every partial write, or record terminal failure
    Writer->>Completed: Transfer completed slot and result
    Completed->>Session: Reclaim slot
    Handler->>Journal: Explicit independently acknowledged RetireOutboundBefore
```

## Supported configuration

The reference profile uses the phase-5 defaults (16 KiB frames, 512 wire fields,
128 total group entries, depth 8, 32 inbound queue entries, 32 retained inbound
messages) with:

| Capacity or setting | Reference/default |
|---|---:|
| `BoundedOutboundQueueDepth` | 32 |
| Output slots | queue depth + 1 = 33 |
| `BoundedBuilderBytes` | 32,768 |
| Builder mutation records | twice `BoundedMaxFields` = 1,024 |
| `BoundedWriteTimeout` | 5s per complete frame |
| Explicit store factory | `NewBoundedStoreFactory(1024, 16384)` |
| Journal byte arena | 16 MiB |

The journal size is an explicit factory choice, not an implicit replacement for
another configured store. `BoundedStore` is in-memory: it supports reconnect and
resend within the running process, not crash durability. Existing durable stores
and the unbounded `MemoryStore` retain their APIs and are outside this profile.

Each builder owns its metadata and copies field values into its byte arena.
Overwrites, removals and `CopyFrom` do not reclaim mutation budgets; `Reset` does.
Byte capacity and mutation capacity are separate from final wire limits. Encoded
output is parsed with fixed scratch before commit/transfer to prove that wire
fits the same field/group/frame limits required for replay. Scalar setters and
typed groups support length-delimited binary values; `SetRawGroup` accepts
SOH-delimited text fields. Callers supply valid matching DATA length fields and
dictionary group structure. Wire fixtures cover scalar formatting, groups,
binary DATA, timestamps, logon and resend headers.

## Lifetime, exhaustion and recovery

Applications obtain the sole builder with `MessageSlot.BeginOutbound` inside
their borrowed inbound callback. `Send`, `Abort`, or callback return ends that
borrow. An unfinished builder is reclaimed automatically. Borrowed outbound
callbacks explicitly replace legacy `ToAdmin`/`ToApp` for this opt-in mode; their
views cannot be retained, reset, sent recursively or shared. Legacy external send
APIs return `ErrBoundedSendRequiresCallback` for bounded sessions before mutation.

When output capacity is exhausted, the session waits for writer completions;
the inbound channel and ring propagate backpressure. A finite whole-frame write
deadline bounds a stalled consumer, including partial-write loops. Every slot
returns only after terminal success/failure. Disconnect joins the writer and
reader and reclaims queued, completed and retained slots before reconnect reuse.
Normal disconnect drains pending writes. A terminal write failure preserves the
already committed sequence/journal because peer receipt may be uncertain.

Journal exhaustion returns `ErrJournalFull` before assigning/persisting another
sequence. Builder/frame/journal capacity errors similarly leave that send's
committed state unchanged. Reset Logon uses a staged journal reservation: old
bytes, sequences and creation time survive until the complete replacement Logon
is encoded successfully. Final outbound callback reset intent is honored. Failure
does not trigger a second send/reset that would conceal the original capacity
error. Operational failures preserve journal state even with `ResetOnDisconnect`.

Completed writes never retire retained messages. Only an explicit independently
acknowledged recovery boundary may call `RetireOutboundBefore`/`RetireBefore`.
There is no automatic eviction. An out-of-window resend request fails with
`ErrJournalRangeUnavailable` before replay/target advancement; retained gaps may
be represented by FIX gap fills. Full-journal resend needs only output capacity.
Replay preserves original body bytes/group order, sequence, original sending
time, PossDup, and callback header edits. Application vetoes retain gap-fill
semantics. The separate replay builder prevents a preceding gap fill from
overwriting approved resend headers. Store compatibility snapshots still allocate.

Existing raw logging and send-stage observations remain active. Borrowed logging
is synchronous; an allocating handler/logger/observer is outside the allocation
contract. Observer replacement publishes an immutable function atomically, so
the bounded send path does not acquire the old global observer read lock.

## Measurement boundary

Before source: `97d6e7d53bd2016186284b7724b7bc0561e24f6a` (phase 5).
The unchanged original harness is run before and after:

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkBounded(Build|OutboundOwnership)$' -benchmem -benchtime=1s -count=5 -cpu=1
```

The construction comparison repopulates the same order fields and appends the
same wire fixture using legacy setters versus the new fixed builder. It uses the
existing allocation-free append encoder on both sides, isolating this phase's
new construction reduction. The original existing-message legacy build row is
also retained as a compatibility reference.

`BenchmarkBoundedOutboundOwnership` uses the reference capacities, warms all 33
slots outside timing, and includes building, output reservation, encoded-limit
checking, journal commit, transfer, completed write, slot reclaim, peer parse and
explicit acknowledged retirement. Its fixed in-process connection copies all
bytes before reporting completion. It is a component measurement, not proof of
the real-network full-session path. Phase 7 supplies that qualification.

The initial component profile found an optional absent ResetSeqNumFlag lookup
constructing a reject error (120 B/op, 3 allocs/op). A `Has` guard removes that
allocation without suppressing invalid present-field checks. The diagnostic
profile summary is retained separately from controlled performance samples.

Measured October 15, 2026, Go 1.25.3, darwin/arm64, Apple M4 Pro:

| Warmed operation | B/op | allocs/op | Five-sample time range |
|---|---:|---:|---:|
| Legacy repopulate + caller-owned append | 376 | 34 | 963.5–1,002 ns |
| Bounded repopulate + caller-owned append | 0 | 0 | 1,710–1,753 ns |
| Bounded existing-message append | 0 | 0 | 192.5–194.4 ns |
| Completed-write ownership + journal + peer acknowledgment | 0 | 0 | 11,508–11,672 ns |

All five allocation samples agree: [raw bounded results](bounded-go1.25.3.txt).
The allocation reduction does not imply lower CPU time: constructing the small
fixture with these fixed capacities is slower than the legacy setter reference.
The construction benchmark uses 512 builder mutation records/32 KiB; the session
ownership benchmark uses the full reference profile, including 1,024 records.

The original controlled [before](before-go1.25.3.txt) and
[after](after-go1.25.3.txt) rows retain the parent allocation results: dictionary
and group parse 0/0; legacy validation 936 B/5; legacy existing-message build
152 B/6; legacy framed read 160 B/2. Harness blob:
`80bc8bf1fd51732a053235c53bdd76ab173cdabf`.

## Correctness evidence

```sh
GOTOOLCHAIN=go1.23.12+auto rtk proxy go test ./... -skip '^TestMongoLogTestSuite$' -count=1
GOTOOLCHAIN=go1.23.12+auto rtk proxy go vet ./...
GOTOOLCHAIN=auto rtk proxy go test -race . -count=1
```

[Go 1.23 package tests](tests-go1.23.12.txt),
[vet](vet-go1.23.12.txt), and [full core race tests](race.txt) passed. Performance
runs were isolated from correctness and race testing. Fixtures cover partial
writes, no progress, write/deadline errors, slow consumers, queue backpressure,
exact reclamation, atomic exhaustion, callback scope, reset staging/cancellation,
failed initiator logon teardown, full-journal replay, raw groups/binary fields,
veto/gap fills and retired-range rejection. Existing legacy suites still pass.

The known optional Mongo logging teardown failure is recorded separately in
[phase 1](../phase-1/README.md#existing-optional-mongo-logger-test-failure).
Its test remains explicitly excluded; Mongo store tests passed in this run.

Remaining recovery-event formatting and full-session qualification are phase-7
work; this phase does not claim the whole library or all session paths allocate
zero. Connection setup/teardown and malformed-message diagnostics are outside
these warmed component measurements.
