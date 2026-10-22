Boolean timestamp validation constructed and discarded parsing errors for bad
wire values. Reuse the canonical byte checks and normalize Go's accepted comma,
leading-plus, and negative-zero fractions in 27 stack bytes. Public timestamp
reader behavior, dictionary validation, reject contents, persistence, and
completed-write ownership remain intact.

Stacked on `perf/configured-validation-rejects` at
`0701bc4dac50b427ed05bdac8d6870c997af5bf8`.

```text
message owner -> borrowed timestamp bytes
  -> canonical checks / local [27]byte normalization
  -> boolean -> configured immutable reject
  -> control slot -> bounded journal -> writer channel
  -> completed writer copy -> returned output slot
```

Five matched single-CPU samples on Go 1.27.2 (Apple M4 Pro):

| Flow | Before | After |
|---|---:|---:|
| Invalid-calendar validation | 402.9 ns, 104 B, 2 allocs | 297.5 ns, 0 B, 0 allocs |
| Invalid calendar through completed reject | 3759 ns, 104 B, 2 allocs | 3670 ns, 0 B, 0 allocs |
| Negative nonzero fraction boolean check | 111.6 ns, 136 B, 3 allocs | 4.267 ns, 0 B, 0 allocs |
| Valid full validation | 634.7 ns, 0 B, 0 allocs | 635.8 ns, 0 B, 0 allocs |

Exact counters confirm zero objects and bytes across 100 completed timestamp
rejects. The required allocation-baseline rows remain unchanged. Canonical
boolean checks increased by 0.37–0.61 ns in the samples; full valid validation
remained effectively stable. The reject benchmark uses the existing bounded
writer fixture and completed slot return, not TCP latency.

Independent `time.Parse` parity covers all bytes, all accepted lengths, signed
and comma fractions, calendar boundaries, and year zero; 20s differential fuzz
passed 242,194 executions. Retained reject and public reader parity pass.
Full core, internal, dictionary, and config checks pass. All seven existing
real-TCP exact-counter scenarios pass before and after with zero objects and
bytes across 100 completed cycles each. Separate race checks and vet pass.
Raw before/after commands, hashes, and receipts are in this directory's README.
