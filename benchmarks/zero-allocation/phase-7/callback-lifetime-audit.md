# Callback lifetime compatibility gap

Source audit: 2026-10-16. Original runtime
`d0993b42089daed35711c252244147d1183fd398`; candidate
`4d74075f8fbfcf4a0e8decd35999825cb54672da` (production code `a2fb11b9`).
This is source evidence, not an executed before/after regression fixture.

The candidate keeps method signatures but does **not** preserve the lifetime of
messages delivered to existing application callbacks. This remains a merge
blocker alongside the unmet latency target. The zero-allocation measurements
use synchronous callbacks that do not retain these views; they do not prove
compatibility for arbitrary existing applications.

## Concrete behavior

An ordinary `FromApp` callback can save its `*Message` in an application field,
return, and read its ClOrdID after session processing finishes. The original
runtime allocated a distinct message and copied frame for each input. Keeping
the message reachable kept its contents alive. The candidate clears the parsed
message after processing; the later getter can report a missing field. After
another input, the same pointer can represent that later message.

Saving a `GetBytes` result or `Message.Bytes()` instead retains a view into a
raw input slot. Its contents change when the slot is reused. A shallow message
or field-map copy also shares the recycled backing.

| Boundary | Original behavior | Candidate behavior |
| --- | --- | --- |
| Incoming `FromApp` / `FromAdmin` | Distinct message and copied frame per input | Session-owned message reset after processing; raw frame recycled |
| Engine-generated `ToAdmin` | Distinct control message instances | One control cell cleared on later acquisition |
| Replay `ToApp` | One message reused within each resend request | Reused within the request and also reset at request completion |
| Caller-created outgoing message | Caller retains its own message | Caller still retains its own message; writer owns independent encoded bytes |

Replay needs the distinction above: the original runtime already reused one
message within a resend batch. It did not provide a separate retained snapshot
for every replay callback. Resetting the last replay at request completion is
nevertheless an additional lifetime change.

## Source evidence

- Original `parser.go:159–161` allocated and copied each frame;
  `session_state.go:94–99` allocated a new message for each incoming frame.
  Parsed fields referenced that independently owned frame in `tag_value.go`.
- Original `application.go` did not declare callback-only borrowing.
  `GetBytes` was explicitly zero-copy, but no callback expiration was stated.
- Candidate `session.go:827–836` passes the reusable message directly to the
  application. `session_state.go:94–103` resets its slot after processing;
  `MessageSlot.Reset` clears the maps, parsed fields and frame references.
- Candidate `scoped_control.go` reuses the control cell and clears replay state
  at request completion. `scoped_control_test.go` explicitly checks control
  pointer identity and clearing.

Current ownership adapters preserve copied data: `CopyInto`, owned string
getters, detached `GetGroup` results and the legacy logger's automatic byte
copies. There is no equivalent automatic application-message adapter or
existing retention/release signal. `enableScopedCallbacks` allows convenience
mutations; it does not detach the delivered message.

Existing compatibility fixtures test caller-initiated copying. The fix-server
RFQ and audit fixtures verify its specific owned retained data. Neither proves
that an unchanged application saving a callback pointer remains compatible.
Documenting a new callback lifetime does not supply that compatibility.

## Required decision

The existing `Application` interface gives the library no way to distinguish a
callback that saves its message from one that consumes it synchronously. If
both receive the same reusable identity, recycling invalidates the saved one.
Without a release signal, backpressure also cannot determine when that saved
message is safe to reuse.

Internal frame, parser, validation and output storage can remain fixed in
either design. Automatically delivering owned messages preserves the old
callback behavior but allocates at that boundary. Callback-scoped delivery can
reuse storage without allocation but changes existing lifetime behavior. The
current measured candidate implements the latter; that change has not been
accepted as satisfying the plug-and-play compatibility requirement.
