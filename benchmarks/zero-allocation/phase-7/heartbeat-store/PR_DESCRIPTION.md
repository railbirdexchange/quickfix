QuickFIX cloned every outgoing frame before passing it to an external MessageStore, even when that store consumed the bytes synchronously. Add an explicit `BorrowedMessageStore` capability so opted-in stores can read the sender-owned output slot during their save call and copy only the bytes they retain. Legacy stores still receive owned bytes; promoted decorator methods cannot bypass the decorator's ordinary save override. No existing MessageStore signature changes.

```text
control callback → sender-owned encoded slot
  → synchronous borrowed save → store-owned retained copy
  → writer channel → completed write → vacant slot channel
```

Borrowed input is read-only and expires when the save call returns, including errors. Capability selection occurs once during session setup; the prepared bounded journal retains priority. No new mutex, pool, dependency or hot-path growth is added.

Matched warmed benchmarks include callbacks, sequence updates, persistence, peer parsing, completed writes and output-slot return. They use a fixed independent receive buffer and are component measurements, not TCP/full-session latency. All figures below are for the explicit borrowed external-store fixture, five one-second samples on Go 1.25.3, Apple M4 Pro, `-cpu=1`.

| Flow | Before B/op; allocs/op | After B/op; allocs/op | Median ns/op before → after |
|---|---:|---:|---:|
| Idle heartbeat | 80; 1 | 0; 0 | 1687 → 1641 |
| Reused timer → session event → heartbeat | 80; 1 | 0; 0 | 2217 → 2166 |
| TestRequest | 80; 1 | 0; 0 | 1792 → 1777 |

Legacy external-store controls remain 80 B/1 allocation; the internal bounded-store controls remain 0/0. The allocation profile attributes the removed object to `bytes.Clone` in `prepareOutbound`; the matched after profile has no allocation samples there. Mandatory `BenchmarkAllocationBaseline` allocation counts are unchanged. Small timing changes are reported without claiming a broad latency gain.

Focused tests cover dispatch, explicit owner identity, legacy and re-opted-in decorators, noncomparable store/owner values, persistence errors preserving sequence and journal state, slot return, retry, disabled persistence, and retained bytes surviving two output-ring rotations. Go 1.23.12 core/local-package tests pass (core 31.319 s), the separate race run passes (core 36.384 s), and vet passes. Core qualification includes existing actual-TCP session/allocation/lifecycle tests. The historical Mongo logger teardown failure is documented separately and was not rerun.

Raw measurements, profiles, exact commands, source hashes and scope are in `benchmarks/zero-allocation/phase-7/heartbeat-store/README.md`.
