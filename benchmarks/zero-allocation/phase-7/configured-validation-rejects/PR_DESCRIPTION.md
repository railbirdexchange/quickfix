Configured validation failures still boxed a fresh 64-byte reject for every
message, even when the tag, reason and text were fixed by the dictionary. Prepare
only applicable immutable tag/reason results during session setup and reuse them
after the existing validation traversal selects the failure. Required group
children now use the existing prepared required-field errors. The three
parameter-free invalid-type, unsupported-type and incorrect-value constructors
also reuse immutable values with the same concrete error type.

Reject precedence, text, reference tags, business/session classification, public
APIs and retained-error lifetime are preserved. Unknown tags and public
tag-specific constructors retain their owned fallback; no cache grows while
handling messages.

```text
session setup -> immutable dictionary -> finite tag/reason index + result block
session owner -> validation -> prepared immutable error -> reject control
  -> bounded journal/output slot -> writer channel -> completed write -> slot return
retained error -> immutable result block, independent of message and session reuse
parameter-free error -> package-owned immutable value
```

Matched Go 1.27.2, CPU1, five one-second samples; setup and 66 warm-ups are outside
the measured loops. Median times:

| Flow | Before | After |
|---|---:|---:|
| Eight configured static rejection categories | 64 B / 1 alloc each | 0 B / 0 alloc each |
| Completed enum reject | 3,683 ns, 64 B / 1 alloc | 3,598 ns, 0 B / 0 alloc |
| Completed group-count reject | 4,049 ns, 64 B / 1 alloc | 4,010 ns, 0 B / 0 alloc |
| Invalid-MsgType validation | 49.79 ns, 64 B / 1 alloc | 22.73 ns, 0 B / 0 alloc |
| Three parameter-free constructors | 64 B / 1 alloc each | 0 B / 0 alloc each |
| Invalid-MsgType validation + retained long `GetString(35)` | 80 B / 2 alloc | 16 B / 1 alloc |
| Malformed timestamp validation | 112 B / 3 alloc | 48 B / 2 alloc |
| Unknown-tag fallback | 64 B / 1 alloc | 64 B / 1 alloc |

Completed rejects include borrowed callback, persistence, sequence advancement,
encoding, completed writer copy, slot return, independent peer parse and journal
retirement. These are component measurements, not TCP RTT or FIX-server
throughput; the small completed-write timing differences are not a transport
latency claim. Exact whole-interval checks also measure zero allocations across
100 completed rejects and 1,000 validations/constructor calls.

The tradeoff is session setup. FIX44 validator construction rises from 45,016 to
564,448 total allocated bytes (281→343 allocations), and 27.220→499.638 µs. Its
4,629-result immutable block contains 74,064 bytes; retaining a prepared error
can retain that block. Lookup and existing required-field metadata are additional.
Total setup allocation includes transient map growth and is not retained-heap
size. No startup-heap reduction or arbitrary-error zero-allocation claim is made.
Malformed timestamps and custom `STRING`-declared group integer parser errors
retain their existing inner allocations and parsing semantics.

Focused before/after parity and retained-error fixtures pass, including paired
faults, FIXT application/admin header/trailer groups, overflow behavior and
completed wire fields. The required allocation baseline preserves all allocation
counts. Full package tests pass (core 38.833 s), the separate core/internal race
run passes (46.275 s/1.577 s), and vet passes. Only the unrelated
`TestMongoLogTestSuite` teardown failure is excluded from the all-package run;
its [original before-change evidence](benchmarks/zero-allocation/phase-1/tests-before.txt)
and [diagnosis](benchmarks/zero-allocation/phase-1/README.md#existing-optional-mongo-logger-test-failure)
are retained separately. No deployment is part of this revision.

Full measurements, commands, source hashes and raw output:
[phase evidence](benchmarks/zero-allocation/phase-7/configured-validation-rejects/README.md).
