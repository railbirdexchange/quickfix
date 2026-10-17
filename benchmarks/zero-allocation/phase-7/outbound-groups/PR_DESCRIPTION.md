Dynamic outgoing repeating groups currently reconstruct `RepeatingGroup`, `Group`, templates, maps and copied fields on every message. Add `FieldMap.SetGroupValues` to copy ordered group fields into the caller message's fixed outbound construction storage, including flattened nested groups. The existing public `SetGroup` and owned getter behavior remain compatible.

The attached message arena allocates one `BoundedMaxFields` metadata slab during setup. Group construction, replacement, remove and clear reuse that slab; scalar or legacy group replacement reclaims its capacity. Input slices/strings are copied synchronously, with aliases staged before arena movement. Capacity failures preserve prior fields and latch an error until `Clear`; append/send observes that error before sequence/store changes. No hot-path growth, pool, refcount or new mutex is introduced.

```mermaid
flowchart LR
    Caller["Caller owns ordered descriptors and values"] -->|"synchronous copy"| Message["Caller owns reusable message arena"]
    Message -->|"SendToTarget encodes"| Slot["Reserved outbound wire slot"]
    Slot -->|"channel transfers ownership"| Writer["Writer owns slot until socket write completes"]
    Writer -->|"returns vacant slot"| Ring["Session output ring"]
```

Measured on Apple M4 Pro, Go 1.25.3, `-benchtime=1s -count=5 -cpu=1`, with setup/warm-up outside timing:

| Two-party construction plus caller-buffer append | Median ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| Existing `RepeatingGroup` / `SetGroup` | 1,688 | 3,552 | 68 |
| New ordered `SetGroupValues` | 204.7 | 0 | 0 |

The mandatory `BenchmarkAllocationBaseline` ran before and after this change. Its allocation rows are unchanged:

| Baseline fixture | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|
| Reused parse, no dictionary | 0 / 0 | 0 / 0 |
| Reused parse with dictionary | 0 / 0 | 0 / 0 |
| Reused parse with repeating group | 0 / 0 | 0 / 0 |
| Validate NewOrderSingle | 0 / 0 | 0 / 0 |
| Convenience build NewOrderSingle | 152 / 6 | 152 / 6 |
| Legacy framed read | 160 / 2 | 160 / 2 |

Validation:

- Full wire parity, nested groups, binary/SOH values, ownership, alias staging, metadata compaction, scalar/legacy replacement, clear, shallow-copy detachment and atomic capacity recovery pass. Focused suite: 2.067 seconds.
- A real TCP fixture rebuilds the nested outgoing group on every callback and completes 3,200 orders with **zero additional heap objects and bytes**, retaining dictionary validation, bounded persistence, completed writes and returned slots. Resend, inbound-gap and slow-consumer correctness also pass.
- Entire core package passes on Go 1.23.12: 30.650 seconds, including the existing seven exact full-session allocation scenarios.
- Separate full core race run passes: 34.412 seconds. Allocation measurements are separate from race instrumentation.

The construction benchmark measures construction and append, not socket latency. The completed-session allocation proof uses the existing bounded store/null logger/borrowed callback configuration; it does not claim that all FIX-server business, decimal, logger or persistence code is allocation-free. The unrelated pre-existing Mongo logger teardown issue is tracked separately; its integration package was not rerun here.

Raw before/after output, scope and reproduction commands: [outbound group evidence](benchmarks/zero-allocation/phase-7/outbound-groups/README.md).
