# Phase 6: ordinary outbound ownership and retained recovery

Ordinary `SendToTarget` prepares the original caller-owned `Message`
synchronously, encodes into a fixed session output slot, and transfers only
independent wire bytes to the writer. Existing `Application` callbacks and send
signatures remain in use. There is no alternate builder or outbound mode flag.

```mermaid
sequenceDiagram
    participant Caller as Caller / ordinary FromApp
    participant Sender as Existing send ownership
    participant Journal as Configured MessageStore
    participant Writer as Socket writer
    participant Vacant as Vacant slot channel
    Caller->>Sender: SendToTarget(original Message)
    Vacant->>Sender: Reserve output slot
    Sender->>Journal: Reserve bounded journal capacity, when supported
    Sender->>Caller: ToApp / ToAdmin(original Message)
    Sender->>Sender: Encode and retain caller-visible 9/10
    Sender->>Journal: Persist independent wire and advance sender sequence
    Sender-->>Caller: Return with caller Message still owned
    Sender->>Writer: Transfer fixed output slot
    Writer->>Writer: Complete all partial writes or record failure
    Writer->>Vacant: Return slot after terminal write result
```

The sender uses the existing send/resend locks. The writer returns slots directly
through a channel and never needs those locks or session event-loop dispatch.
Ordinary `FromApp -> SendToTarget` therefore retains synchronous preparation.
Socket completion is distinct from queue admission.

## Limits, retention and compatibility

| Setting or resource | Default / measured configuration |
|---|---:|
| `OutboundFrameQueueCapacity` | 32 pending frames |
| Fixed output slots | queue capacity + 1 = 33 |
| `BoundedMaxFrameBytes` | 16,384 bytes, shared inbound/output bound |
| `OutboundWriteTimeout` | 30 seconds per complete frame |
| `SocketOutboundBufferSize` | existing setting, capped at pending capacity; zero remains unbuffered |
| Measured store factory | `NewBoundedStoreFactory(1024, 16384)` |
| Fixed journal bytes | 16 MiB |
| Resend parse limits | 512 fields, 128 group entries, nesting depth 8 |

Frame/queue capacity is reserved before header sequence assignment, callbacks,
or persistence. An oversized frame returns `ErrBufferTooSmall`; a full output
ring returns `ErrOutboundCapacity`. The bounded journal reserves capacity before
mutation. These failures preserve committed sender/target and retained bytes;
they do not trigger a secondary Logout or `ResetOnDisconnect` reset. Logon
resets remain staged until encoding succeeds. A failed scheduled rollover keeps
its reset intent across ticks/disconnect and retries through an admitted Logon.
Before a prepared journal commit, capacity-only parsing of the encoded frame
also enforces the configured field/group/depth bounds. Semantic parse errors do
not introduce new outgoing dictionary validation. Valid retained frames therefore
fit the replay scratch limits as well as the output byte capacity.
Generic store failures retain the configured store's existing failure semantics;
the public store interface cannot make arbitrary Reset-plus-Save transactional.

`BoundedStore` is a fixed in-memory `MessageStore` implementation with a resend
journal, not crash-durable persistence. Existing durable stores
are never replaced. A direct bounded store accepts prepared commits; decorators,
including ones embedding it, continue through their public Save/Reset overrides.
Other stores receive owned bytes. Retaining stores such as the unbounded
`MemoryStore` remain compatible and allocate outside this measurement's guarantee.

Journal capacity bounds a sequence window beginning at the explicit recovery
floor, including gaps. Reservation and lookup use a fixed sequence-indexed ring;
entries are never evicted. Only independently acknowledged `RetireBefore` or an
explicit reset releases history. Socket completion alone is not acknowledgement.
Out-of-window replay fails before target advancement. The configured store's
iteration remains authoritative. Replay preserves original body/group ordering,
original sending time, PossDup, callback header edits and veto/gap-fill behavior.

Bounded-store sequence getters and independent target operations are atomic.
Sender/journal operations use existing engine serialization. Direct store
mutation, retirement and creation-time access require a quiescent owner;
external administrative resets must not race an inbound transaction.
`SetNextSenderMsgSeqNum` serializes with send/resend and must run outside send
callbacks. No new hot mutex, pool, reference count, dependency or slice growth
is introduced.

The caller's `Message` remains usable/mutable after Send returns; the writer
never holds it. Engine scalar byte views belong to that Message and may change
on its next send. `CopyInto` and owned string getters detach retained values.
Engine resend callbacks receive a scoped replay view with the same copying rule.
Ordinary body setters and fresh construction still allocate in this phase.

Logging finishes before slot transfer. Existing synchronous borrowed logging can
consume bytes directly; arbitrary legacy loggers receive owned copies. Logging,
callbacks, observers and persistence remain enabled. Disabled send observation
avoids clock reads and message-type string construction; enabled observers keep
the existing synchronous event contract.

Disconnect joins readers/writers and reclaims slots before reuse. Partial,
zero-progress and terminal write failures preserve committed journal state
because receipt is uncertain. Successful teardown keeps ResetOnDisconnect
behavior. Resend may exceed the ring depth: the writer returns capacity
independently, and writer failure propagates before target advancement.

## Controlled evidence

The isolated pair uses ordinary inbound parent `afbd0127`; later phase-5 latency
commits are deliberately excluded. Both sides use the same harness and inputs.
The superseded opt-in experiment's original files remain for provenance; its
builder measurements do not qualify the ordinary runtime described here.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkScopedOutboundSession$' -benchmem -benchtime=1s -count=5 -cpu=1
```

The ordinary harness includes reused Message preparation, ordinary ToApp,
encoding, persistence, completed write, slot return, peer parsing and explicit
retirement after peer processing. It preloads 1,023 of 1,024 journal entries and
warms all 33 output slots twice outside timing. The fixed peer sink copies every
byte before Write returns; each timed operation ends after write completion and
ownership return. It is a completed-write component measurement, not actual TCP,
the full inbound session, or round-trip latency. The group case preserves/parses
group wire data; it does not invoke allocating legacy GetGroup or build a
variable response body.

Before evidence: [original baseline](scoped-before-go1.25.3.txt),
[ordinary completed writes](scoped-session-before-go1.25.3.txt).
After evidence: [original baseline](scoped-after-go1.25.3.txt),
[ordinary completed writes](scoped-session-after-go1.25.3.txt).

Go 1.25.3, Apple M4 Pro, CPU=1, five repetitions:

| Ordinary completed-write flow | Before ns/op | Before B/op | Before allocs/op | After ns/op | After B/op | After allocs/op |
|---|---:|---:|---:|---:|---:|---:|
| NewOrderSingle | 2,958–3,038 | 343 | 20 | 1,950–2,081 | 0 | 0 |
| Repeating group | 3,959–4,075 | 551 | 22 | 3,061–3,321 | 0 | 0 |
| Long custom message type | 2,963–3,043 | 503 | 24 | 1,950–2,078 | 0 | 0 |

The mandatory baseline preserves the earlier allocation results. These cases
do not measure the new ordinary outbound ownership path:

| Baseline flow | Before ns/op | After ns/op | B/op before → after | allocs/op before → after |
|---|---:|---:|---:|---:|
| Decimal, short | 11.26–11.73 | 11.70–12.13 | 0 → 0 | 0 → 0 |
| Decimal, signed fraction | 27.52–28.67 | 29.12–29.51 | 0 → 0 | 0 → 0 |
| Decimal, maximum uint128 | 54.55–56.05 | 54.53–60.82 | 0 → 0 | 0 → 0 |
| Decimal, arbitrary-precision overflow | 320.2–327.4 | 321.5–333.2 | 168 → 168 | 5 → 5 |
| Parse, no dictionary | 370.4–380.3 | 370.5–376.3 | 0 → 0 | 0 → 0 |
| Parse with dictionary | 442.5–451.1 | 438.6–449.2 | 0 → 0 | 0 → 0 |
| Parse with repeating group | 869.6–895.5 | 859.5–873.6 | 0 → 0 | 0 → 0 |
| Validate NewOrderSingle | 868.2–879.2 | 855.1–876.5 | 0 → 0 | 0 → 0 |
| Legacy build NewOrderSingle | 746.8–769.8 | 753.3–916.1 | 152 → 152 | 6 → 6 |
| Legacy framed read | 109.5–110.7 | 108.6–109.9 | 160 → 160 | 2 → 2 |

Both complete after commands passed: mandatory baseline in 79.494 seconds and
ordinary completed writes in 20.541 seconds. CPU-heavy qualification work was
serialized. A prior baseline run was interrupted; its
[partial diagnostic output](scoped-diagnostic-interrupted-after-go1.25.3.txt.gz)
is excluded from every result above. The complete mandatory command was rerun
from its first case.

The unchanged harness SHA-256 values are:

| File | SHA-256 |
|---|---|
| `allocation_baseline_test.go` | `ad521a9f8acb7bea2afed51f7ae1de23d68dd95379fcbc7f2f5489d68b3dd78a` |
| `scoped_outbound_benchmark_test.go` | `9cadc954f11c1284760eec13eba8abd6b1f282a86ed46822561c4a9a5eaa23fb` |

The [qualified source manifest](scoped-qualified-source.sha256) records root Go
sources/tests and `config/bounded.go`; its
[post-measurement verification](scoped-qualified-source-check.txt) passed.
The current candidate retains the phase-5 `afbd0127` parent; later inbound
latency changes require separate combined qualification.

These completed-write results exceed one microsecond. Full inbound-to-completed-
write qualification and variable response construction remain phase-7 work;
the whole library is not claimed to be allocation-free.

## Correctness qualification

Fixtures cover partial/zero-progress writes, buffered/unbuffered completion,
slow-consumer ownership, full rings, journal bounds/aborts, reset atomicity,
callback pointer identity and tag34 edits, observable 9/10, stale encoded header
identity canonicalization, detached copies, retaining stores/loggers, decorators,
FromApp sending, reconnect, replay beyond ring capacity, short-range writer
failure, Logout/gap-fill errors, and scheduled reset retry/preservation.

```sh
GOTOOLCHAIN=go1.23.12+auto rtk proxy go test ./... -skip '^TestMongoLogTestSuite$' -count=1
GOTOOLCHAIN=go1.23.12+auto rtk proxy go vet ./...
GOTOOLCHAIN=auto rtk proxy go test -race . -count=1
```

The [Go 1.23.12 package suite](scoped-tests-go1.23.12.txt) passed, including the
core package in 8.026 seconds. [Go 1.23.12 vet](scoped-vet-go1.23.12.txt) exited
successfully without diagnostics. The [full core race run](scoped-race.txt)
passed in 18.506 seconds. Source was unchanged after those checks and through
both after measurements. `TestOrdinaryOutboundWarmedAllocations` exercises
100 completed ordinary sends per scenario after visiting every output slot.

The existing optional Mongo logger environment failure remains documented in
[phase 1](../phase-1/README.md#existing-optional-mongo-logger-test-failure). Only
that suite is skipped. Race/correctness checks are separate from performance.
