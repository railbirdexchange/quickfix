# Existing arena-length candidate qualification

Disposition: **RETAINED**, runtime commit
`8033f27d5f154fd0f0ec69dd1ed5facbfff38cae`. The completed-outbound component and
full TCP batch improvements survived unchanged-binary A–B–A controls. Direct
individual latency did not improve. Final race, vet and exact allocation checks
passed. Isolated fix-server integration is qualified separately at its published
dependency pin.

This independent candidate has completed component, direct and full TCP batch
A–B–A measurements, plus the mandatory allocation comparison.
The prior capacity-only parser experiment was rejected and reverted before this
candidate began. Benchmark fixtures are unchanged. Small component and batched
throughput gains were observed; direct latency did not demonstrate improvement.

All reused BEFORE measurements and the fresh component capture use the same
preserved executable from clean checkout
`338c3f03efd663365d050baffb6af4f24e91448d`, production implementation
`a2fb11b923d1e0143a60427cf38ba2854cc142f6`, cross-compiled with Go 1.23.12 for
linux/arm64, `CGO_ENABLED=0`. The binary SHA-256 is
`0515b641faac565783b2f3b0eefcac9504c3422939d4b2c1bef990e94188df03`.
[Source identity](before-source-git.txt), [compiler settings](before-compiler.txt),
and [fixture hashes](before-fixture-source.sha256) retain the frozen identities.
The retained remote binary and unchanged FIX40/FIX44 XML were
[hash-checked again](before-remote.sha256) before the new component capture.

The VM remains `ubuntu@192.168.252.5`, shared four-vCPU ARM64 QEMU with existing
k3s/EP3 workloads unchanged. See the [current Linux profile metadata](../linux-latency-profile/README.md)
and load [before](vm-load-before.txt), [after the initial captures](vm-load-after.txt)
and [after the final controls](vm-load-final-control.txt).
No infrastructure, scheduling,
affinity, runtime or fixture edit was performed by this worker. This is not
production or bare-metal evidence.

## Fresh completed-outbound BEFORE

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-replay-capacity-338c3f03.SZp1jV && timeout 150s ./before.test -test.run "^$" -test.bench "^BenchmarkScopedOutboundSession$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=120s'
```

[Raw component BEFORE](component-before-go1.23.12.txt) is a fresh capture for this
experiment. It uses all three existing cases, with setup and warm-up excluded,
completed writes and returned slots, peer parse/sequence checking, and explicit
journal retirement while retaining 1023 prior messages. Its connection uses an
independent in-memory receive buffer, so this is a component measurement.

The command exited 0 and ended in PASS. All 15 rows reported zero bytes and zero
allocations. Summaries of the five repetitions per case:

| Component | BEFORE median ns/op | BEFORE range ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: |
| NewOrderSingle | 2110 | 2084–2144 | 0 | 0 |
| Repeating group | 3182 | 3118–3307 | 0 | 0 |
| Custom message type | 2091 | 2063–2112 | 0 | 0 |

The [candidate component capture](component-after-go1.23.12.txt) also exited 0 and
ended in PASS, with zero bytes/allocations in every row. It used the same command
with `cached-length.test` in place of `before.test`.

| Component | BEFORE median ns/op | Candidate median ns/op | Candidate range ns/op | Median change |
| --- | ---: | ---: | ---: | ---: |
| NewOrderSingle | 2110 | 2061 | 2039–2089 | −2.3% |
| Repeating group | 3182 | 3024 | 2993–3065 | −5.0% |
| Custom message type | 2091 | 2036 | 2027–2050 | −2.6% |

These are descriptive five-repetition summaries on the shared VM, without a
statistical significance or production speedup claim.

The later [unchanged-source component control](component-before-control-go1.23.12.txt)
also exited 0 and ended in PASS, using the original command and the
[hash-verified frozen binary](component-batched-control-binary.sha256). All 15
control rows reported 0 B/op and 0 allocs/op.

| Component | BEFORE A1 median ns/op | Candidate B median ns/op | BEFORE A2 median ns/op | A2 range ns/op |
| --- | ---: | ---: | ---: | ---: |
| NewOrderSingle | 2110 | 2061 | 2130 | 2122–2141 |
| Repeating group | 3182 | 3024 | 3124 | 3107–3207 |
| Custom message type | 2091 | 2036 | 2096 | 2087–2112 |

The component reduction survives this control: all candidate ranges are lower
than the corresponding A2 ranges. This still does not establish an individual
full-session latency reduction.

## Reused unchanged-source baselines

The immediately preceding
[frozen-BEFORE direct control](../replay-capacity/direct-before-control-go1.23.12.txt)
is the direct BEFORE for this experiment. It used the same untouched binary,
five one-second repetitions per ordinary/group case, CPU 1, and passed with zero
bytes/allocations in every row. The median of the five reported p50 values was
6833 ns ordinary (6791–6917 ns range) and 7459 ns groups (7333–7792 ns range).
It measures final read return through successful write completion, accepting
each sample only after response validation, ACK, slot returns and retirement.

The [mandatory BEFORE allocation capture](../replay-capacity/baseline-before-go1.23.12.txt)
also used this exact preserved binary and unchanged harness. Its full five-run
results are reused rather than rerunning an identical unaffected flow. Parse
without/with dictionary and validation report 0 B/op, 0 allocs/op; the legacy
repeating-group convenience parse reports 3 B/op, 0 allocs/op. Decimal overflow
is 168 B/op, 5 allocs/op; legacy build is 152 B/op, 6 allocs/op; legacy framed
read is 160 B/op, 2 allocs/op. Normal decimal cases are zero. The legacy group
byte count is preserved, not represented as zero; its source is unproven.

## Direct final-read-to-completed-write comparison

The candidate [direct capture](direct-after-go1.23.12.txt) and subsequent
[unchanged-source control](direct-before-control-go1.23.12.txt) both exited 0 and
ended in PASS. All 20 rows reported 0 B/op and 0 allocs/op. The original binary
was [hash-checked](control-before-binary.sha256) before the control. Both runs
used the same command below, substituting `before.test` for the final control:

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-replay-capacity-338c3f03.SZp1jV && timeout 180s ./cached-length.test -test.run "^$" -test.bench "^BenchmarkOrdinaryFullSessionReadToWrite$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=150s'
```

Each table entry is the median of five per-run reported p50 values; ranges are
the smallest and largest of those five p50s. They are not pooled percentiles.

| Flow | BEFORE A1 p50 median/range, ns | Candidate B p50 median/range, ns | BEFORE A2 p50 median/range, ns |
| --- | ---: | ---: | ---: |
| Ordinary | 6833 / 6791–6917 | 6833 / 6708–6917 | 6916 / 6833–7083 |
| Groups | 7459 / 7333–7792 | 7584 / 7459–7750 | 7667 / 7542–8083 |

The candidate does not demonstrate a direct latency improvement: ordinary's
median matches A1, and the group's candidate median lies between the two
unchanged-source captures. Raw p95, p99 and maximum values remain in the files.
No outliers were removed. The overall benchmark `ns/op` includes the complete
one-outstanding request cycle and is distinct from the instrumented read/write
interval. Its A1/B/A2 medians were 21533/21619/21665 ns for ordinary and
22887/22775/22988 ns for groups.

## Mandatory allocation comparison

The [candidate mandatory capture](baseline-after-go1.23.12.txt) exited 0 and
ended in PASS. All 50 rows match the allocation categories in the reused
BEFORE capture described above, including 3 B/op and 0 reported allocs/op for
legacy repeating-group convenience parsing. No allocation reduction is claimed
for this latency-only experiment. To reproduce its controlled benchmark arguments:

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-replay-capacity-338c3f03.SZp1jV && ./cached-length.test -test.run "^$" -test.bench "^BenchmarkAllocationBaseline$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1'
```

## Full TCP batched flow

The [BEFORE](batched-before-go1.23.12.txt) and
[candidate](batched-after-go1.23.12.txt) commands both exited 0 and ended in PASS.
All 20 rows reported 0 B/op and 0 allocs/op. The BEFORE command below was followed
by the same command using `cached-length.test`:

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-replay-capacity-338c3f03.SZp1jV && timeout 180s ./before.test -test.run "^$" -test.bench "^BenchmarkBoundedFullSession$/(ordinary|groups)$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=150s'
```

Each operation processes 32 requests over real TCP, including dictionary/group
validation, ordinary callbacks, variable-field construction, persistence, peer
validation, completed writes, slot return and acknowledgement. Setup and warm-up
are excluded. These are nanoseconds per **32-request cycle**, not individual
latency measurements.

| Flow | BEFORE median ns/cycle | BEFORE range | Candidate median ns/cycle | Candidate range | Median change |
| --- | ---: | ---: | ---: | ---: | ---: |
| Ordinary | 301813 | 292621–304049 | 297404 | 293635–298678 | −1.5% |
| Groups | 341255 | 336366–345244 | 332882 | 332161–337441 | −2.5% |

The later [unchanged-source batch control](batched-before-control-go1.23.12.txt)
used the original command and frozen binary, exited 0 and ended in PASS. All 10
rows reported 0 B/op and 0 allocs/op.

| Flow | BEFORE A1 median ns/cycle | Candidate B median ns/cycle | BEFORE A2 median ns/cycle | A2 range |
| --- | ---: | ---: | ---: | ---: |
| Ordinary | 301813 | 297404 | 304471 | 299165–305176 |
| Groups | 341255 | 332882 | 342133 | 337548–344839 |

The small descriptive batched improvement survives this control: the candidate
ranges remain below A2. The initial A1/candidate ranges overlap. These captures
do not establish statistical significance or production improvement. Direct
individual latency above remains flat within the observed unchanged-source
drift. The sub-microsecond target is unmet.

## Correctness checks

The integration owner ran the frozen candidate with Go 1.23.12:

- Existing append/arena/replay-limit tests: PASS, 0.588 s;
  [raw output](focused-existing-go1.23.12.txt).
- Five new cached-length test families: PASS, 0.546 s;
  [raw output](focused-cached-go1.23.12.txt).
- All-package tests: PASS with core 26.742 s;
  [raw output](tests-go1.23.12.txt). The sole exclusion remains the existing
  `TestMongoLogTestSuite` teardown failure, documented
  [separately before phase 1](../../phase-1/README.md#existing-optional-mongo-logger-test-failure).
  This does not establish live Mongo coverage.
- Seven exact full-session allocation scenarios: PASS, 10.427 s;
  [raw output](exact-go1.23.12.txt). Each scenario completed 100 cycles with
  exactly zero additional `Mallocs` and `TotalAlloc` bytes: ordinary, groups,
  resend, inbound gap, slow consumer, warmed post-reconnect and heartbeat/TestRequest.
- Separate complete core race run: PASS, 29.742 s;
  [raw output](race-go1.23.12.txt).
- All-package vet: PASS, exit 0; [empty successful output](vet-go1.23.12.txt).

```sh
GOTOOLCHAIN=go1.23.12 rtk proxy go test . -run '^TestBoundedFullSessionExactAllocations$' -count=1 -v -timeout=5m
GOTOOLCHAIN=go1.23.12 rtk proxy go test -race . -count=1 -timeout=5m
GOTOOLCHAIN=go1.23.12 rtk proxy go vet ./...
```

## Candidate identity

The only runtime change is pinned by [candidate-runtime.diff](candidate-runtime.diff)
for `message_append.go`. The new fixtures are retained as
[message_append_cached_test.go.txt](message_append_cached_test.go.txt), with
[source hashes](candidate-source.sha256). All original benchmark/peer fixture,
XML/module and mandatory allocation-harness hashes matched BEFORE prior to build.
The original benchmark/peer, XML and module hashes also
[matched after all measurements](fixture-verification-after.txt), and the runtime
and new test source still matched the candidate manifest.

```sh
GOTOOLCHAIN=go1.23.12 GOOS=linux GOARCH=arm64 CGO_ENABLED=0 rtk proxy go test -c -o benchmarks/zero-allocation/phase-7/cached-length/cached-length-linux-arm64.test .
```

The build exited 0 with [empty output](build-after-go1.23.12.txt).
[Compiler settings](after-compiler.txt) and [build information](after-binary-buildinfo.txt)
are retained. The candidate binary SHA-256 is
`c94f257ea8d9d24f523abd82dfde653ae64d56c304986374f59712a0eb755946`;
[local](after-upload-local.sha256) and [remote](after-upload-remote.sha256) binary/XML
hashes match. The distinct remote filename is `cached-length.test`.

Remote cleanup completed at 2026-10-16 15:41:35 UTC. The
[pre-cleanup inventory](remote-cleanup-before.txt) contained only the three
uploaded executables and two required XML files. The exact five files were
removed, followed by their now-empty `spec/` and task directories;
[cleanup output](remote-cleanup.txt) records every removal and the successful
absence check. No shared infrastructure was changed. The historical commands
above require uploading their hash-matched binary and XML again before reuse.

Local executables remain preserved and explicitly ignored: the BEFORE executable
at `../linux-latency-profile/quickfix-linux-arm64.test`, this candidate at
`cached-length-linux-arm64.test`, and the prior rejected parser candidate at
`../replay-capacity/after-linux-arm64.test`. Their identities remain distinct;
none is staged.
