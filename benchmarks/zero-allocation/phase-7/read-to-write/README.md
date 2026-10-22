# Final socket read to completed write

This fixture measures a direct server interval alongside the existing full
client TCP/ACK round trip. It does not derive server latency by subtracting
parts of a round trip. The original `BenchmarkOrdinaryFullSessionRoundTrip`
remains unchanged.

Before: `c41682e932a1429906c5786c6929a3f41918e485`
([source](before-source.txt)). After:
`a2fb11b923d1e0143a60427cf38ba2854cc142f6`
([source](after-source.txt), [working-tree status](after-status.txt)).
Both runs use Go 1.23.12, darwin/arm64 ([toolchain](toolchain.txt)).
The benchmark host is Apple M4 Pro. The [start](started-at.txt) and
[finish](finished-at.txt) records are UTC; the
[source recheck](after-source-finished.txt) still identifies the same candidate.
The [before fixture manifest](before-harness.sha256) and
[after fixture manifest](after-harness.sha256) match for the two full-session
fixture files, unchanged RTT fixture and new direct-measurement fixture.
Before production source was not changed.

`Message.ReceiveTime` comes from `parser.lastRead`, stamped immediately after
the final underlying socket `Read` returns. It precedes completion of
`ReadMessageInto`; it is not an exact post-framing timestamp. With one request
outstanding and the previous ACK barrier complete, the interval includes
remaining framing/check/copy work, input queueing, dictionary/group parsing,
validation, the ordinary handler, encoding, persistence and writer queue waits.
It ends immediately after the final successful underlying TCP `Write` returns.
Network arrival, read blocking and framing work before that final read are
excluded.

The ordinary `FromApp` fixture copies the receive time and request identity into
a fixed channel, then executes its unchanged handler. A test-only connection
wrapper records the matching response's completed write, tracking partial
writes and excluding admin/unrelated responses. There is no production observer
hook. Failed or no-progress writes do not qualify a successful sample.

The direct interval ends before peer validation and the extra ACK. Every sample
is accepted only after the existing RTT helper validates the real response and
completes ACK processing, all 33 output-slot returns and journal retirement.
All validation, persistence, groups and handler behavior remain enabled.
Receive-time copying, identity inspection, clock calls and channel transfers
add measurement overhead; none is subtracted. Default `ns/op` and `requests/s`
also include sample recording and the full ACK barrier.

Both latency windows have fixed capacity 65,536, allocated before timing.
Percentiles use the final `min(N, 65536)` requests; maxima cover every measured
request. Setup, all-slot/journal warm-up, sorting and shutdown are untimed.
There is no goroutine or growing sample buffer per measured operation.

## Measured results

The [before log](before-benchmark.txt) and [after log](after-benchmark.txt)
contain five 1-second samples per scenario, on one CPU. Each range below is
across those five samples; the maximum column shows the range of each sample's
maximum, not a percentile. All times are microseconds.
Allocation columns cover the entire instrumented request/ACK operation, not
only the direct server interval.

| Direct final-read-to-write interval | p50 µs | p99 µs | Maximum µs | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| Before ordinary | 8.625–8.792 | 13.084–13.958 | 33.542–118.541 | 103–104 | 10 |
| After ordinary | 8.875–8.959 | 13.166–13.875 | 50.208–89.583 | 0 | 0 |
| Before groups | 9.709–9.834 | 14.500–14.916 | 38.292–83.459 | 103–104 | 10 |
| After groups | 9.875–10.000 | 14.416–16.042 | 48.792–106.708 | 0 | 0 |

| Full client TCP/ACK RTT from the same requests | p50 µs | p99 µs | Maximum µs |
|---|---:|---:|---:|
| Before ordinary | 43.500–44.291 | 69.084–72.292 | 143.708–220.250 |
| After ordinary | 44.459–44.792 | 71.250–76.000 | 168.583–193.291 |
| Before groups | 46.000–46.584 | 71.000–76.333 | 166.584–183.333 |
| After groups | 46.416–46.791 | 71.709–79.542 | 138.709–198.541 |

Every candidate row reports 0 B/op and 0 allocs/op. These are rounded benchmark
measurements, not a new exact whole-process `Mallocs`/`TotalAlloc` assertion.
The sub-microsecond target remains unmet at the directly observed server
boundary, and these samples do not demonstrate a latency improvement.

## Checks and commands

- Before Go 1.23 correctness: PASS, 1.617 s ([log](before-correctness.txt)).
- After Go 1.23 correctness: PASS, 1.981 s ([log](after-correctness.txt)).
- After separate Go 1.23 race check: PASS, 2.785 s ([log](after-race.txt)).
- Before five-sample benchmark: PASS, 49.964 s ([log](before-benchmark.txt)).
- After five-sample benchmark: PASS, 50.017 s ([log](after-benchmark.txt)).

The focused tests verify partial writes, admin and unrelated-response exclusion,
write errors/no progress, a gate before the real write, and a gate after actual
write completion but before output-slot return. The latter proves that the
direct timestamp can precede slot return while sample qualification and RTT
still wait for it.

Run correctness and race separately from performance, from the corresponding
worktree root:

```sh
GOTOOLCHAIN=go1.23.12 rtk proxy go test . -run '^TestOrdinaryReadToWrite' -count=1 -v -timeout=3m
GOTOOLCHAIN=go1.23.12 rtk proxy go test -race . -run '^TestOrdinaryReadToWrite' -count=1 -v -timeout=3m
GOTOOLCHAIN=go1.23.12 rtk proxy go test . -run '^$' -bench '^BenchmarkOrdinaryFullSessionReadToWrite$' -benchmem -benchtime=1s -count=5 -cpu=1 -timeout=10m
```

The before and after benchmarks run serially with the same fixed fixture and
no competing build/test work. The previous RTT benchmark was not repeated;
the new benchmark reports both intervals directly from the same requests.
