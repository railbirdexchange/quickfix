# Corrected ordinary session qualification

Production runtime: `a2fb11b923d1e0143a60427cf38ba2854cc142f6`.
Commit `01c466c0` adds only the direct-latency fixture and evidence; it does not
change runtime code. Before runtime:
`c41682e932a1429906c5786c6929a3f41918e485`.
The [before](before-harness.sha256) and [after](harness.sha256) manifests match
for all seven qualification fixture files. Captures ran serially without other
Go build/test work on the Apple M4 Pro, darwin/arm64.

## Completed TCP cycles

Both [before](full-session-before-go1.25.3.txt) and
[after](full-session-after-go1.25.3.txt) use Go 1.25.3, `-benchtime=1s -count=5
-cpu=1`. Setup, warm-up and shutdown are untimed. Every cycle waits for completed
writes, returned output slots and explicit retirement of acknowledged journal
history. The [operation definitions](../README.md) specify batching and recovery
work; these timings are not individual-request latency.

| Cycle | Before B/op | Before allocs/op | After B/op | After allocs/op |
|---|---:|---:|---:|---:|
| Ordinary, 32 changing requests | 3,495–3,498 | 346 | 0 | 0 |
| Nested groups, 32 changing requests | 3,482–3,488 | 345–346 | 0 | 0 |
| Resend, 40 original and 40 replayed responses | 5,728–5,752 | 537–538 | 0 | 0 |
| Inbound sequence gap | 3,876 | 70 | 0 | 0 |
| Slow consumer, 34 responses with bounded backpressure | Fails capacity admission | No comparable completed cycle | 0 | 0 |
| Warmed traffic after reconnect, 32 requests | 3,489–3,494 | 346 | 0 | 0 |
| Heartbeat/TestRequest | 3,432 | 42 | 0 | 0 |

All 35 AFTER samples report zero bytes and allocations. BEFORE six-flow capture
passes in 141.061 seconds; AFTER seven-flow capture passes in 156.671 seconds.
The revised slow-consumer operation deliberately holds an actual TCP write and
observes the 34th send blocked for one millisecond before releasing capacity.
It does not reject and retry a business request. The old source
[fails that behavior](../backpressure/before-regression-c41682e9-go1.23.12.txt),
so its historical rejection/retry timing is not compared here.

The separate [exact Go 1.23 check](../cold-scalars/full-session-exact-go1.23.12.txt)
measures whole-process `Mallocs` and `TotalAlloc` around 100 completed warmed
cycles of each scenario. All seven deltas are exactly zero; PASS, 9.709 seconds.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkBoundedFullSession$/(ordinary|groups|outbound_resend|inbound_gap|warmed_post_reconnect|heartbeat_test_request)$' -benchmem -benchtime=1s -count=5 -cpu=1 -timeout=10m
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkBoundedFullSession$' -benchmem -benchtime=1s -count=5 -cpu=1 -timeout=10m
```

Run the first command in the before worktree and the second in the candidate.
The required [allocation-baseline AFTER capture](allocation-baseline-after-go1.25.3.txt)
passes in 78.820 seconds. All allocation rows are preserved: reused parsing and
validation 0/0; legacy standalone build 152 B/6 allocations, framed read 160 B/2,
and arbitrary-precision decimal overflow 168 B/5.
Its [BEFORE capture](../scoped-allocation-baseline-before-go1.25.3.txt) uses the
same phase-6 source and unchanged `allocation_baseline_test.go`; that fixture's
hash is included in both manifests above.

```sh
rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
```

## Correctness, race and direct latency

- Go 1.23 all-package [correctness](tests-go1.23.12.txt): PASS, core 26.441 s.
  Only the independently reproduced optional Mongo logger teardown failure is
  excluded with `-skip '^TestMongoLogTestSuite$'`; no Mongo implementation changes.
- Go 1.23 [vet](vet-go1.23.12.txt): PASS, empty successful output.
- Separate full core Go 1.23 [race run](race-go1.23.12.txt): PASS, 28.492 s.
- Focused first-send, cold-scalar, backpressure and alias/ownership fixtures pass
  in their linked evidence directories. The independent source reviews found
  no actionable issue.
- Five-run [direct latency](../read-to-write/README.md): ordinary p50
  **8.875–8.959 µs**, p99 **13.166–13.875 µs**; groups p50 **9.875–10.000 µs**,
  p99 **14.416–16.042 µs**. All ten benchmark rows report 0 B/op and 0 allocs/op.

The direct interval is final socket Read return through final successful socket
Write return, including remaining framing, queues, validation, handler and
encoding/persistence. Every sample is accepted only after peer validation and
the ACK/slot-return/journal-retirement barrier. Instrumentation overhead is
included. This is not network-arrival latency, and no overhead is subtracted.
**The under-one-microsecond target remains unmet; no latency improvement is
demonstrated.**

An additional [Linux VM capture](../linux-read-to-write/README.md) uses the
unchanged production runtime and Go 1.23.12: ordinary direct p50 6.333–6.500 µs,
p99 10.834–11.500 µs; groups p50 7.083–7.334 µs, p99 11.792–13.250 µs. All ten
benchmark rows report 0 B/op and 0 allocs/op. The shared QEMU ARM64 development VM
also hosts other workloads. This is neither a controlled comparison with macOS
nor a production latency qualification, and it also misses one microsecond.

```sh
GOTOOLCHAIN=go1.23.12 rtk proxy go test ./... -skip '^TestMongoLogTestSuite$' -count=1 -timeout=5m
GOTOOLCHAIN=go1.23.12 rtk proxy go vet ./...
GOTOOLCHAIN=go1.23.12 rtk proxy go test -race . -count=1 -timeout=5m
```

The fixed profile, supported logger/store/handler scope, callback lifetimes,
exhaustion behavior and cold/convenience API exclusions remain in
[ZERO_ALLOCATION.md](../../../../ZERO_ALLOCATION.md). Final unchanged fix-server
loads pass for this runtime: 5,000 steady and 10,000 burst requests with all
trading/drop-copy/audit/order/recovery assertions. Whole-process allocation is
+0.369% steady / −0.522% burst against the matched `afbd0127` baseline, one run
each; no statistical or zero-allocation integration claim follows. The final
burst delivers 9,712.523 requests/s and 48,562.613 expanded FIX messages/s.
[Integration PR 1362](https://github.com/railbirdexchange/draftkings-exchange/pull/1362)
records exact binaries, dependency pins, broker isolation and cleanup. Existing
Kafka `client closed` errors after burst audit assertions leave shutdown audit
delivery unqualified. This remains draft work because the latency target is
unmet and [legacy callback lifetime compatibility](../callback-lifetime-audit.md)
is unresolved. These measurements use synchronous callbacks that do not retain
incoming message/byte views; they do not qualify unchanged pointer-retaining
applications.

All 17 integration CI jobs passed at PR 1362 head
`b71e166249c847f7cf1fb3e11e53b82041b5495b`, which pins runtime `a2fb11b9`.
The last [Omaha workflow](https://github.com/railbirdexchange/draftkings-exchange/actions/runs/38862667720)
completed at 2026-10-16 07:03:59 UTC; [fix-server tests and image build](https://github.com/railbirdexchange/draftkings-exchange/actions/runs/38862668113)
also passed. The complete set includes service tests/image builds, both race
jobs and formatting. These checks qualify that dependency pin, not a different
lifetime contract or the unmet latency target.
