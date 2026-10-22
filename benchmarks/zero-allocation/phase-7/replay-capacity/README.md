# Replay-capacity candidate qualification

Disposition: **REJECTED AND REVERTED.** The
completed outbound component became modestly faster, but the direct A-B-A
control does not establish a stable full-path latency benefit. The baseline
binary itself improved by more than the candidate's apparent group benefit.
No sub-microsecond or production speedup claim follows from this experiment.
The benchmark fixtures were unchanged throughout.

The BEFORE executable was preserved from clean checkout
`338c3f03efd663365d050baffb6af4f24e91448d`, production implementation
`a2fb11b923d1e0143a60427cf38ba2854cc142f6`. It was not rebuilt after candidate
source work began. Its SHA-256 is
`0515b641faac565783b2f3b0eefcac9504c3422939d4b2c1bef990e94188df03`.
[Source identities](before-source-git.txt), [compiler settings](before-compiler.txt)
and [fixture hashes](before-fixture-source.sha256) were copied from the frozen
[current Linux profile capture](../linux-latency-profile/README.md).

The native Go 1.23.12 linux/arm64 binary and unchanged FIX40/FIX44 XML files were
uploaded into `/tmp/quickfix-replay-capacity-338c3f03.SZp1jV` on
`ubuntu@192.168.252.5`. [Local](before-upload-local.sha256) and
[remote](before-upload-remote.sha256) hashes match. The machine remains the same
shared four-vCPU ARM64 QEMU VM; no infrastructure, affinity or scheduling change
was made. See [load before](vm-load-before.txt) and the profile capture's complete
machine metadata. VM results are not production or bare-metal evidence.

Completed outbound BEFORE capture:

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-replay-capacity-338c3f03.SZp1jV && timeout 150s ./before.test -test.run "^$" -test.bench "^BenchmarkScopedOutboundSession$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=120s'
```

[Raw component output](component-before-go1.23.12.txt) contains all three existing
cases. The command exited 0 and ended in PASS; all 15 rows reported zero allocated
bytes and zero allocations. Each row below summarizes its five repetitions.

| Component | BEFORE median ns/op | BEFORE range ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| NewOrderSingle | 2119 | 2099–2203 | 0 | 0 |
| Repeating group | 3127 | 3110–3189 | 0 | 0 |
| Custom message type | 2083 | 2066–2086 | 0 | 0 |

The candidate component capture exited 0 and ended in PASS, again with zero
bytes and allocations in all 15 rows. It used the same command above with
`after.test` in place of `before.test`; [raw candidate output](component-after-go1.23.12.txt).

| Component | BEFORE median ns/op | Candidate median ns/op | Candidate range ns/op | Median change |
| --- | ---: | ---: | ---: | ---: |
| NewOrderSingle | 2119 | 2021 | 1994–2047 | −4.6% |
| Repeating group | 3127 | 3000 | 2965–3040 | −4.1% |
| Custom message type | 2083 | 2012 | 1993–2031 | −3.4% |

These are descriptive results from five repetitions on a shared VM, not a
statistical significance claim or a production improvement guarantee.

Setup and warm-up are outside the timed loop; each operation qualifies
completed writes, returned output slots, peer parse/sequence checking and explicit
journal retirement. The component keeps 1023 prior messages retained and uses an
independent in-memory receive buffer. It is not the full TCP/direct latency path.

The direct BEFORE capture is the existing fresh five-row-per-case
[unprofiled Linux baseline](../linux-latency-profile/baseline-go1.23.12.txt),
using the exact same executable. Profiled diagnostic rows are not the baseline.
The [post-capture load observation](vm-load-after-before-capture.txt) is retained.

## Direct A-B-A comparison

The initial frozen BEFORE (A1), candidate (B) and final frozen BEFORE (A2) each
ran ordinary and group scenarios five times with one-second benchtime and CPU 1.
All three commands exited 0 and ended in PASS; all 30 rows report **0 B/op,
0 allocs/op**. A2 re-used the original binary, with its
[hash checked again](control-before-binary.sha256).

| Scenario | A1 median p50 ns (range) | B median p50 ns (range) | A2 median p50 ns (range) |
| --- | ---: | ---: | ---: |
| Ordinary | 6875 (6791–7000) | 6875 (6792–7000) | 6833 (6791–6917) |
| Groups | 7708 (7583–7791) | 7584 (7333–7875) | 7459 (7333–7792) |

Each median is the median of the five reported per-run p50 values, not a pooled
request percentile. The initial group difference of 124 ns is smaller than the
249 ns change between the two unchanged-binary captures. Ordinary p50 is
unchanged from A1 to B. This rejects a stable direct-path gain on this evidence;
it does not establish the cause of VM timing drift.

Timed `ns/op` medians (including ACK/sample qualification) are ordinary
21562 → 21924 → 21533 and groups 23073 → 22894 → 22887. Direct latency ends at the
successful socket write; samples are accepted only after peer validation, ACK,
slot returns and journal retirement. Instrumentation overhead is not subtracted.
The exact fixture boundary and configured limits are documented in the
[Linux baseline](../linux-latency-profile/README.md).

Raw outputs retain every tail/outlier:

- [A1](../linux-latency-profile/baseline-go1.23.12.txt).
- [B](direct-after-go1.23.12.txt), including a 1.242499 ms ordinary direct maximum
  and 3.645956 ms ordinary RTT maximum.
- [A2](direct-before-control-go1.23.12.txt), including a 4.529582 ms group RTT
  maximum.

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-replay-capacity-338c3f03.SZp1jV && timeout 180s ./after.test -test.run "^$" -test.bench "^BenchmarkOrdinaryFullSessionReadToWrite$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=150s'
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-replay-capacity-338c3f03.SZp1jV && timeout 180s ./before.test -test.run "^$" -test.bench "^BenchmarkOrdinaryFullSessionReadToWrite$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=150s'
```

Capture order was A1 direct, diagnostic profiles, component BEFORE, mandatory
BEFORE, candidate component, candidate direct, candidate mandatory, then A2
direct. The component did not receive an A2 control, so its descriptive 3–5%
gain also remains subject to shared-VM drift.

## Mandatory allocation comparison

The mandatory allocation BEFORE baseline uses the same preserved executable,
whose [remote hash was rechecked](mandatory-before-binary.sha256). The
[harness hash](mandatory-before-harness.sha256) is recorded;
[mandatory-before-harness.diff](mandatory-before-harness.diff) is empty against
`338c3f03`. This capture was started while candidate source/tests were being
edited and did not compile those changes.

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-replay-capacity-338c3f03.SZp1jV && timeout 240s ./before.test -test.run "^$" -test.bench "^BenchmarkAllocationBaseline$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=210s'
```

[Raw mandatory BEFORE output](baseline-before-go1.23.12.txt) is retained separately
from the completed outbound component and full TCP direct measurements. The
command exited 0 and ended in PASS. All five repetitions have the allocation
values shown below; the repeating-group row's nonzero bytes are preserved
exactly as reported and are not rounded down to zero.

| Mandatory baseline case | B/op | allocs/op |
| --- | ---: | ---: |
| Decimal short / signed fraction / max uint128 | 0 | 0 |
| Decimal overflow | 168 | 5 |
| Reused parse without / with dictionary | 0 | 0 |
| Reused repeating-group parse | 3 | 0 |
| NewOrderSingle validation | 0 | 0 |
| Legacy NewOrderSingle build | 152 | 6 |
| Legacy steady framed read | 160 | 2 |

The [candidate mandatory capture](baseline-after-go1.23.12.txt) also exited 0 and
ended in PASS. Every allocation row above is identical BEFORE and candidate,
including the repeating-group convenience parser's **3 B/op, 0 allocs/op**.
Its dictionary setup and first warm-up parse precede `ResetTimer`; the byte
allocation source has not been established. This row is not the session-slot
path, and integer `allocs/op` does not prove no allocation occurred anywhere.

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-replay-capacity-338c3f03.SZp1jV && timeout 240s ./after.test -test.run "^$" -test.bench "^BenchmarkAllocationBaseline$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=210s'
```

## Rejected candidate identity and retained artifacts

The candidate was built from `338c3f03` plus the exact
[runtime diff](candidate-runtime.diff) and new
[differential fixtures](replay_capacity_test.go.txt).
[Candidate source hashes](candidate-source.sha256) pin all three changed runtime
files and the added test file. All original benchmark, peer-fixture, XML and
module hashes, including the mandatory harness, matched BEFORE prior to build.
The candidate uses the same Go 1.23.12 cross-compiler settings
([settings](after-compiler.txt), [build info](after-binary-buildinfo.txt));
compilation exited 0 with [empty output](build-after-go1.23.12.txt).

```sh
GOTOOLCHAIN=go1.23.12 GOOS=linux GOARCH=arm64 CGO_ENABLED=0 rtk proxy go test -c -o benchmarks/zero-allocation/phase-7/replay-capacity/after-linux-arm64.test .
```

Candidate binary SHA-256:
`5fc63c09da7f3a5e26797c7fb15a5e87892dd0c579883e0100faf43f6de1c7eb`.
[Local](after-upload-local.sha256) and [remote](after-upload-remote.sha256) candidate
binary/XML hashes matched before execution.

All requested paired captures are complete. The task-owned remote directory was
retained for the independent cached-length experiment, then cleaned at
2026-10-16 15:41:35 UTC. Its original binaries, that experiment's additional
binary and the two XML fixtures were removed by exact path, followed by the
empty `spec/` and task directories. The
[pre-cleanup inventory](../cached-length/remote-cleanup-before.txt) and
[successful removal/absence proof](../cached-length/remote-cleanup.txt) are
preserved. No shared infrastructure was changed. The historical commands above
require uploading their hash-matched files again before reuse.
`after.test` belonged to this rejected candidate, not the retained cached-length
candidate. The local BEFORE executable remains at
`../linux-latency-profile/quickfix-linux-arm64.test`, explicitly git-ignored there.
The distinct rejected executable is `after-linux-arm64.test`, explicitly ignored
by this directory's `.gitignore`. Neither executable is staged. The owner applied
the exact runtime patch in reverse and verified no diff in `message.go`,
`message_slot.go`, or `scoped_outbound_session.go`. The active added test file was
removed; its exact diagnostic `.go.txt` copy remains here. No runtime or active
test change from this experiment remains. This worker changed only evidence.
No new race or
full-session exact-allocation suite was run by this worker for the rejected
candidate. CPU ownership was released after A2. No commit or push was performed.
