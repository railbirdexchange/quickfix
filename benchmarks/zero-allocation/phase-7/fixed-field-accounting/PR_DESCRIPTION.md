Fixed field maps scanned every reserved hash-table entry when computing wire
length and checksum. The saved service CPU profile attributed 1.81 seconds to
`length` and 0.68 seconds to `total`. Accounting now visits the existing bounded
touched-entry index and skips tombstones. Lookup, ordering, wire bytes and
retained-output ownership stay unchanged.

The tag and touched-entry indexes use disjoint, capacity-capped regions of one
allocation, removing one setup allocation per fixed map without increasing
measured setup bytes. No public API, dependency, pool or hot-path growth is
added.

```text
message slot / construction arena
  -> owns entry table and one fixed index backing
       -> independently capped tag region
       -> independently capped touched-entry region
  -> length/checksum visit touched occupied entries
  -> remove clears references; reset clears all touched entries

outgoing callback -> caller owns Message.String result
  -> encode/persist -> writer owns output slot
  -> completed write -> slot returns -> independent peer parses bytes
```

Matched Go 1.27.2, darwin/arm64, CPU=1 measurements; times are medians of five
one-second samples. Full commands and raw captures are in
[the evidence README](https://github.com/railbirdexchange/quickfix/blob/perf/fixed-field-accounting/benchmarks/zero-allocation/phase-7/fixed-field-accounting/README.md).

| Operation | Before ns/op | After ns/op | B/op before → after | allocs/op before → after |
|---|---:|---:|---:|---:|
| Setup, capacity 4 | 137.2 | 140.2 | 1,072 → 1,072 | 4 → 3 |
| Setup, capacity 64 | 832.7 | 739.7 | 15,184 → 15,184 | 4 → 3 |
| Setup, capacity 256 | 2,078 | 1,586 | 55,376 → 55,376 | 4 → 3 |
| Sparse length + checksum | 473.9 | 61.96 | 0 → 0 | 0 → 0 |
| Dense length + checksum | 2,377 | 1,746 | 0 → 0 | 0 → 0 |
| Grouped length + checksum | 554.0 | 126.4 | 0 → 0 | 0 → 0 |
| Tombstones, 256 touched / 8 occupied | 482.3 | 278.4 | 0 → 0 | 0 → 0 |
| Lookup, 256 populated | 2.077 | 2.073 | 0 → 0 | 0 → 0 |
| Remove/reinsert/reset churn | 116.5 | 116.3 | 0 → 0 | 0 → 0 |
| Owned String callback + completed write | 6,057 | 2,287 | 112 → 112 | 1 → 1 |

The completed-write row uses a fixed in-memory copying connection, bounded
store and null logger, and waits for write completion, slot return and peer
parsing. It is a component measurement, not TCP or deployed latency. The owned
String allocation remains necessary under the current public contract. The
smallest setup fixture became slightly slower; no universal setup-time gain is
claimed.

The mandatory allocation baseline passed before and after with unchanged
allocation rows. Focused tests passed for field/group accounting, tombstones,
remove/reinsert, disjoint index capacities, sorting, reference clearing, capacity
failure, message-slot reuse, String and append behavior.

Core and affected local packages passed, including the existing exact bounded
full-session real-TCP and observed-heartbeat allocation assertions. Separate
race tests passed for the core and internal packages; vet passed without
diagnostics. All checks used the configured Go 1.27.2 toolchain. The previously
reported external Mongo logging teardown failure was outside these checks and
was not rerun or claimed fixed.
