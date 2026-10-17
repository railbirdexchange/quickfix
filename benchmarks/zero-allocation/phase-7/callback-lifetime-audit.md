# Resolved callback ownership boundaries

The ordinary engine keeps fixed internal storage and preserves default
`Application` lifetimes through owned delivery adapters. `BorrowApplication`
explicitly permits shorter lifetimes within that same engine. No feature flag,
alternate engine or send signature selects the allocation path.

This supersedes the earlier compatibility-gap audit of
`4d74075f8fbfcf4a0e8decd35999825cb54672da` (production `a2fb11b9`) against
original runtime `d0993b42089daed35711c252244147d1183fd398`. That candidate's
implicit borrowing was incompatible; documenting callback scope alone did not
repair it. The correction supplies the missing adapters and regression tests.
One microsecond is a stretch target, not a compatibility or merge gate.

## Current boundary contract

| Boundary | Ordinary `Application` | Explicit `BorrowApplication` |
|---|---|---|
| `FromApp` / `FromAdmin` | Owned message/frame materialized after fixed parsing; retained pointer, raw bytes, scalar/group views and shallow sections survive slot reuse | Message and views valid only during callback |
| Generated `ToAdmin` | Owned copy of private control cell; callback replacements are the representation actually encoded | Private control view valid only during callback |
| Replay `ToApp` | Owned message materialized after fixed parse and before resend header mutation; remains valid after replay and journal reuse | Fixed replay view valid only during callback |
| Caller-created outgoing message | Original pointer preserved; old engine-field getter slices survive subsequent sends | Original pointer preserved; field views may move or be overwritten on later mutation/send |
| Reused response construction | No automatic arena; a previous borrowed arena is detached before owned delivery | Fixed arena attaches on the second send, with configured limits |

Internal frame, parse, validation, control and output storage is shared by both
contracts. The owned boundary allocates. Explicit borrowed delivery omits those
copies; retaining data then requires an owned copying operation. Writer channels
transfer independent output slots, never the caller's message.

## Implementation and behavior

- `session_state.go` and `inbound_owned_message.go` materialize the successful
  fixed parse into an owned message before state dispatch. One copied raw frame
  and owned field metadata preserve the parsed representation and its raw/value
  aliases, including group ranges. `session.fromCallback` clears the internal
  frame token before ordinary application delivery. Callback mutations remain
  visible to subsequent protocol handling.
- `session.toAdmin` in `scoped_control.go` checks construction errors before
  copying the internal control cell. It returns the delivered representation,
  and send/gap-fill paths continue encoding that object. Section or whole-message
  replacement therefore reaches the wire and persistence. The retained object
  is never recycled with the private control cell.
- `scopedResend.process` materializes owned delivery immediately after fixed
  parse, before `session.resend` changes headers or calls `ToApp`. This placement
  was retained in merge `6c66e9fa34759187547d133b998362afd7e8c2c9`. Fixed replay
  scratch is still reset between frames and at request completion; owned
  delivery is independent. Original persisted body/group wire behavior remains.
- `prepareOutbound` detaches construction arenas before ordinary callbacks and
  permits automatic attachment only for explicitly borrowed delivery. Cold
  engine scalars own separate wire/getter regions in one exact allocation.
  Replacing a scalar does not overwrite an earlier getter. Capacity/construction
  failures precede committed sequence/store changes, with output/journal
  reservations released on failure.

## Regression evidence

The current [focused Go 1.23 run](callback-boundary/focused-third-go1.23.12.txt)
passed in 18.835 seconds. It includes all seven exact borrowed TCP allocation
intervals, public-constructor integration and generated-control ownership.
Final qualification also passed: all seven exact intervals (10.379 s), Go 1.23
package tests (core 29.302 s; only the existing Mongo logger teardown suite
excluded), vet, the separate complete core race run (32.751 s), and qualified
source verification. The [final callback-boundary evidence](callback-boundary/README.md)
records the complete benchmarks: 35 borrowed full-session rows and ten direct
rows at 0/0, with owned delivery still allocating. Ordinary/group batch medians
increased 3.6%/5.0%; the ownership correction makes no speed-gain claim.

The lifetime fixtures exercise actual retained references rather than copying
them in the test and thereby hiding reuse:

| Fixture | Behavior checked |
|---|---|
| `TestInboundLegacyCallbacksRetainMessageAndBytes` | Pointer, shallow sections, getters and raw bytes across more frames than ring capacity and reconnect |
| `TestInboundLegacyCallbackRetainsNestedGroups` / `TestInboundOwnedMaterializationMatchesParsedRepresentation` | Nested/header/trailer groups, parsed raw/field aliases and detached frame storage |
| `TestInboundLegacyFromAdminMutationChangesHeartbeat` | Incoming TestReqID mutation reaches the outgoing heartbeat |
| `TestInboundLegacyRetainedMessageConcurrentWithSlotReuse` | Retained reads while later frames reuse internal slots |
| `TestOutboundLegacyRetainedGettersSurviveRepeatedSends` | Original pointer and old sequence/time/369/BodyLength/checksum getters across second/third sends |
| `TestOutboundLegacyReplaySurvivesJournalReuseAndAnotherResend` | Retained replay pointer/raw/groups through completion, retirement, exact journal-cell reuse and another resend |
| `TestOrdinaryControlCallbacksRetainMessagesAndSections` | All seven generated control types retain distinct callback objects and shallow views |
| `TestOrdinaryControlCallbackReplacementReachesCompletedWriter` | Field, section and whole-message replacements reach persistence and completed wire |
| `TestOrdinaryControlConstructionOverflowReturnsError` | No callback, panic or sequence/store change on pre-delivery construction overflow |
| `TestOrdinaryOutboundDetachesPreviouslyBorrowedStorage` | Original pointer retained when moving from borrowed to owned delivery; later mutations leave saved owned getters intact |
| `TestApplicationBoundaryPublicConstructors` | Real acceptor/initiator TCP sessions for both contracts, dictionary validation, persistence, slot reuse, disconnect/reconnect and joined reader/writer teardown |

The earlier failing-before evidence remains in the
[outbound boundary qualification](../phase-6/callback-boundary/README.md): saved
sequence bytes changed on a later send, and replay raw bytes were reset at
request completion. Those failures are the reason the old zero-allocation
candidate is not a compatible allocation baseline.

## Original behavior preserved

At `d0993b42089daed35711c252244147d1183fd398`, `parser.go:159–161` allocated
and copied each frame, and `session_state.go:94–99` created a new message per
input. Parsed fields referenced the independently owned frame through
`tag_value.go`. Holding the incoming message kept its contents alive. The
original `Application` documentation did not declare callback expiration;
`GetBytes` being zero-copy did not imply that its frame could later be recycled.
Generated admin messages were separate instances, and outgoing messages remained
caller-owned.

Original resend processing reused one message within a resend request, so it
did not promise a distinct snapshot for every callback in that batch. It did
leave the last representation alive after the request. Current owned replay
delivery preserves that lifetime and gives each callback a distinct message.
The historical source comparison establishes those original behaviors; the new
retention fixtures execute the corrected boundaries. Earlier fixed-storage
measurements at `8033f27d` and predecessors retain their source identities and
do not qualify the new default ownership contract.
