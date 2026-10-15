# Phase 3: dictionary and bounded group parsing

Dictionary parsing resolves the message definition once and uses precomputed
immediate-child lookups. A reusable ancestor stack keeps each repeating group as
a contiguous range in the message's field slab. It replaces repeated MsgType
conversions, nested lookup maps and temporary tag paths.

`MessageSlot` provides fixed lookup tables, field storage and group-stack capacity.
Unlike warming an ordinary Go map with one fixture, this storage also stays fixed
when successive messages use different tag combinations. `BorrowedGroup` lets a
handler inspect entries, nested groups and byte values without cloning templates
or constructing mutable group objects. Dictionaries are immutable during use.

```mermaid
sequenceDiagram
    participant Owner as Exclusive frame and slot owner
    participant Slot as MessageSlot
    participant Handler as Borrowed message consumer
    Owner->>Slot: Parse borrowed frame with configured limits
    Slot->>Slot: Fill preallocated indexes and contiguous group ranges
    alt Capacity exceeded or invalid input
        Slot-->>Owner: Error; Message returns nil
    else Parsed
        Slot-->>Owner: Borrowed Message
        Owner->>Handler: Message and borrowed group views
        Handler-->>Owner: Processing completed
    end
    Owner->>Slot: Reset or parse next owned frame
    Note over Owner,Handler: All prior views expire before frame or slot reuse
```

The default parse configuration is 16 KiB per frame, 512 actual fields including
repeated fields, 128 total declared/observed group entries, and eight nesting
levels. All are explicit constructor parameters. Capacity errors are static;
the parser never grows fixed storage or changes session sequence/store state.
Header and trailer groups use transport-dictionary definitions and share the
same entry/depth budget. XMLData delimiter bytes do not consume field slots.

Existing constructors and APIs remain available. Legacy parsing can still grow
its maps/slabs for new inputs. String getters, setters, CopyInto and mutable
RepeatingGroup convenience APIs may allocate. These are outside the bounded
parse/read contract. Fixed-map setters cannot return errors through their old
signatures and panic with `ErrFieldCapacity` before mutation on exhaustion.
`MessageSlot.Parse` returns errors instead. Malformed-input diagnostic formatting
is not claimed allocation-free in this phase.

## Controlled benchmark

Before source: `607d2df9f1ef9d5df1dda1fa142dade81bcc5b56` (phase 2).
Measured October 15, 2026, Go 1.25.3, darwin/arm64, Apple M4 Pro.
The unchanged baseline harness has Git blob
`80bc8bf1fd51732a053235c53bdd76ab173cdabf`. Setup and warm-up are outside timing.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
```

| Warmed flow | Before B/op | Before allocs/op | After B/op | After allocs/op |
|---|---:|---:|---:|---:|
| Parse, reused message, no dictionary | 0 | 0 | 0 | 0 |
| Parse with dictionary | 112 | 7 | 0 | 0 |
| Parse with repeating group | 1,184 | 34 | 0 | 0 |
| Validate NewOrderSingle | 1,200 | 23 | 1,200 | 23 |
| Legacy build NewOrderSingle | 152 | 6 | 152 | 6 |
| Framed read | 160 | 2 | 160 | 2 |
| Decimal short/fraction/uint128 | 0 | 0 | 0 | 0 |
| Decimal overflow, excluded | 168 | 5 | 168 | 5 |

All allocation rows agree across five samples. See [before](before-go1.25.3.txt)
and [after](after-go1.25.3.txt) for all timings and samples.

The additive bounded API is measured separately using the same FIX44 group
fixture and default limits, with construction and warm-up outside the timer:

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkBoundedParsing$' -benchmem -benchtime=1s -count=5 -cpu=1
```

`MessageSlot.Parse` measures **0 B/op, 0 allocs/op** in all five samples:
[raw evidence](bounded-go1.25.3.txt). Allocation assertions also cover changing
tag sets and borrowed nested-group traversal, including malformed group counts.

## Verification

Fixtures cover exact wire rebuilding, multiple/nested/sibling groups, dictionary
validation of the parsed NewOrderSingle, original resend body boundaries,
header/trailer groups, embedded XML delimiters, capacity boundaries, empty and
overflowing group counts, missing checksum, truncation, failed-parse reuse,
borrowed view lifetimes, fixed-map value copies and tag churn.

```sh
GOTOOLCHAIN=go1.23.12+auto rtk proxy go test ./... -skip '^TestMongoLogTestSuite$' -count=1
GOTOOLCHAIN=go1.23.12+auto rtk proxy go vet ./...
GOTOOLCHAIN=auto rtk proxy go test -race . -count=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -fuzz '^FuzzMessageSlotParse$' -fuzztime=15s -parallel=1
```

Evidence: [package tests](tests-go1.23.12.txt), [race run](race.txt), and
[malformed-input fuzzing](fuzz.txt). Performance and race measurement run
separately. All passed; Go 1.23.12 vet exited zero with no diagnostics. Fuzzing
completed 287,976 executions without a failure. Independent reviews found count
overflow, header/trailer limit and accessor/capacity edge cases; each was fixed
and covered by regression fixtures before the final verification.

The known optional Mongo logging teardown failure is documented in
[phase 1](../phase-1/README.md#existing-optional-mongo-logger-test-failure);
Mongo integration coverage is not claimed here.

This phase does not yet bound validation, socket framing, output-slot lifetime,
resend retention or the complete session. Those remain phases 4–7.
