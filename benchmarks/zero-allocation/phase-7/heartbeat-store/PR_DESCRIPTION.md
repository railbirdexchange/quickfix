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

The preferred development and benchmark toolchain is now Go 1.27.2. The module retains `go 1.23`; CI explicitly checks Go 1.23.12 and Go 1.27.2 with automatic toolchain switching disabled. Acceptance jobs use Go 1.27.2, the devcontainer is updated, and CodeQL selects the preferred toolchain with an action version that supports Go 1.27. The existing linter remains on Go 1.23.12.

The compiler comparison uses identical optimized runtime source and five one-second CPU1 samples. Borrowed-store allocation counts remain 0 B/op and 0 allocs/op:

| Borrowed-store component | Go 1.25.3 median ns/op | Go 1.27.2 median ns/op |
|---|---:|---:|
| Idle heartbeat | 1641 | 1576 |
| Reused timer → session event → heartbeat | 2166 | 2073 |
| TestRequest | 1777 | 1700 |

These sample medians are about 4% lower; they do not establish a network latency improvement. The mandatory allocation baseline remains unchanged, including the documented allocating legacy and overflow paths. The separate [Go 1.27.2 evidence](https://github.com/railbirdexchange/quickfix/blob/perf/heartbeat-lifecycle/benchmarks/zero-allocation/phase-7/go-1.27.2/README.md) records compiler qualification and its execution boundaries.

On the upgraded configuration, Go 1.27.2 core/local-package tests pass (core 30.529 s), the separate race run passes (core 33.223 s), and vet passes. Explicit Go 1.23.12 core/local-package tests also pass (core 30.579 s). Automatic local toolchain selection resolves to Go 1.27.2. GitHub Actions, CodeQL execution, the devcontainer build and Ruby acceptance suites are not represented by these local checks.
