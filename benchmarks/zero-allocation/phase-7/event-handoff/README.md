# Event-loop output handoff experiment

Disposition: **REJECTED AND REVERTED.** The direct A–B–A measurements do not
demonstrate an individual final-read-to-completed-write latency improvement beyond unchanged
baseline drift. The mandatory allocation capture passed with unchanged results.
The owner removed the runtime change and active test. All tracked Go source,
tests and modules again match retained commit `8033f27d`; remote cleanup is
complete.

The owner froze the candidate after focused Go 1.23.12 tests passed in 1.352 s
and source review found no issue. This worker received the exclusive CPU
handoff and ran measurements sequentially; no concurrent Go workload was
introduced. A component or batched gain does not substitute for this experiment's
individual-latency criterion.

## Frozen baseline

The published baseline implementation is
`8033f27d5f154fd0f0ec69dd1ed5facbfff38cae`;
[source identity](before-source-git.txt). Its preserved Linux ARM64 executable
is the retained cached-length candidate from the preceding experiment:

`c94f257ea8d9d24f523abd82dfde653ae64d56c304986374f59712a0eb755946`.

It was built with Go 1.23.12, `GOOS=linux`, `GOARCH=arm64`, `GOARM64=v8.0` and
`CGO_ENABLED=0`; [compiler settings](../cached-length/after-compiler.txt) and
[binary build information](../cached-length/after-binary-buildinfo.txt).
That binary was built before publication from `338c3f03` plus the exact retained
cached-length change. The published Go-source difference contains only
`message_append.go` and `message_append_cached_test.go`
([change summary](published-source-change.txt)); both files
[match the original candidate source hashes](before-source-verification.txt).
The binary has not been rebuilt for this baseline.

[Current local binary, XML and harness hashes](before-local.sha256) and
[remote binary/XML hashes](before-remote.sha256) are retained. The remote binary
and XML match their earlier frozen identities. Original benchmark fixtures
remain unchanged.

The unchanged mandatory allocation BEFORE is the prior
[cached-length candidate capture](../cached-length/baseline-after-go1.23.12.txt),
which used this exact binary and count 5, one-second benchtime, CPU 1. It ended
in PASS. Normal decimals, no-dictionary/dictionary parse and validation report
0 B/op and 0 allocs/op. Decimal overflow is 168 B/op and 5 allocs/op; legacy
build is 152 B/op and 6 allocs/op; legacy framed read is 160 B/op and 2 allocs/op.
Legacy repeating-group convenience parsing reports 3 B/op and 0 integer
allocs/op; the byte count is preserved and its source remains unproven.

## Candidate identity

The runtime change is confined to `session.go`: after an incoming message scope
completes, it handles one pending coalesced output hint before returning to the
main select. [Exact runtime diff](candidate-runtime.diff) and the new
[focused fixtures](session_incoming_handoff_test.go.txt) are preserved, with
[source hashes](candidate-source.sha256). The owner's focused correctness
[output](focused-go123.txt) is retained. The baseline binary, XML, original
benchmark fixtures and module files
[matched their frozen hashes before build](before-build-verification.txt).

```sh
GOTOOLCHAIN=go1.23.12 GOOS=linux GOARCH=arm64 CGO_ENABLED=0 rtk proxy go test -c -o benchmarks/zero-allocation/phase-7/event-handoff/event-handoff-linux-arm64.test .
```

The build exited 0 with [empty output](candidate-build-go1.23.12.txt).
[Compiler settings](candidate-compiler.txt) and [build information](candidate-buildinfo.txt)
are retained. Candidate SHA-256:
`5cadbef9be9a9e58eddd94e6a77fdfc79c167b7d9a85b4b0c9a599ffad533a55`.
[Local](candidate-local.sha256) and [remote](candidate-remote.sha256) binary/XML
hashes match. The original baseline binary's hash was also rechecked on the VM.

## Controlled captures

Fresh direct ordinary/group measurements used the frozen baseline (A1),
candidate (B), then unchanged baseline (A2), each five one-second repetitions on
CPU 1. All three commands exited 0 and ended in PASS. All 30 rows report 0 B/op
and 0 allocs/op. The baseline command was:

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-event-handoff-8033f27d.LCn8N7 && timeout 180s ./before.test -test.run "^$" -test.bench "^BenchmarkOrdinaryFullSessionReadToWrite$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=150s'
```

The direct interval begins at final socket read return and ends at successful
socket write completion. Samples are accepted after response validation, peer
ACK, returned slots and journal retirement. Overall benchmark `ns/op` includes
the full request cycle and is distinct from the instrumented latency interval.
Raw benchmark output retains every reported percentile and maximum value.
Clock and connection-wrapper instrumentation overhead is not subtracted.

The candidate used the same command with `candidate.test` in place of
`before.test`. A2 re-used the untouched baseline executable, whose
[hash was rechecked](control-before-binary.sha256). Raw captures:
[A1](direct-before-go1.23.12.txt), [B](direct-candidate-go1.23.12.txt),
[A2](direct-before-control-go1.23.12.txt).

Each median below is the median of five reported per-run p50 values; the range
spans those five p50s. These are not pooled request percentiles.

| Flow | A1 p50 median/range, ns | Candidate B p50 median/range, ns | A2 p50 median/range, ns |
| --- | ---: | ---: | ---: |
| Ordinary | 6792 / 6708–6917 | 6916 / 6833–6958 | 6959 / 6833–7041 |
| Groups | 7667 / 7625–8042 | 7625 / 7541–7875 | 7625 / 7625–7750 |

Ordinary's candidate median lies between the two unchanged-source medians.
The 42 ns group improvement relative to A1 disappears against A2. No stable
direct improvement is established. Overall request-cycle `ns/op` medians were
21515 → 21691 → 21828 ordinary and 23186 → 23129 → 22979 groups; these are not
the direct interval. No reported tail or maximum value was filtered from the
raw output.

Full TCP 32-request batches and additional performance tests are **NOT RUN**:
the direct result did not pass the prerequisite for expanding qualification.
The sub-microsecond target is unmet.

## Mandatory allocation comparison

The [candidate capture](baseline-candidate-go1.23.12.txt) exited 0 and ended in
PASS. All 50 rows match the corresponding same-binary BEFORE values in the
[retained cached-length capture](../cached-length/baseline-after-go1.23.12.txt).
Every case's allocation values were identical across its five repetitions:

| Case | BEFORE B/op | Candidate B/op | BEFORE allocs/op | Candidate allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Decimal short | 0 | 0 | 0 | 0 |
| Decimal signed fraction | 0 | 0 | 0 | 0 |
| Decimal max uint128 | 0 | 0 | 0 | 0 |
| Decimal overflow | 168 | 168 | 5 | 5 |
| Reused parse without dictionary | 0 | 0 | 0 | 0 |
| Reused parse with dictionary | 0 | 0 | 0 | 0 |
| Legacy reused repeating-group parse | 3 | 3 | 0 | 0 |
| NewOrderSingle validation | 0 | 0 | 0 | 0 |
| Legacy NewOrderSingle build | 152 | 152 | 6 | 6 |
| Legacy framed read | 160 | 160 | 2 | 2 |

The nonzero repeating-group byte count is not rounded down; an integer zero
allocs/op does not prove that no allocation occurred anywhere. No allocation
reduction is claimed for this rejected latency experiment.

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-event-handoff-8033f27d.LCn8N7 && timeout 240s ./candidate.test -test.run "^$" -test.bench "^BenchmarkAllocationBaseline$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=210s'
```

## Rollback and verification

The runtime/new-test [manifest matched after capture](source-verification-after.txt),
and the original binary, benchmark fixtures, XML and module files
[also matched](fixture-verification-after.txt). The owner then removed the helper
and call in `session.go` and deleted active `session_incoming_handoff_test.go`.
The exact rejected source/test snapshots remain in this evidence directory.

```sh
rtk proxy git diff --exit-code 8033f27d -- '*.go' go.mod go.sum
```

This command exited 0 with [empty output](runtime-rollback.diff), confirming all
tracked runtime, tests and modules match the retained baseline. The active new
test's absence was checked, and the retained cached-length source manifest
[still matches](retained-source-verification.txt). No code from this experiment
remains active. Additional race and exact-allocation suites were NOT RUN for
the rejected candidate. CPU ownership was released after the mandatory capture.

## Resources and environment

The new task-owned directory is
`/tmp/quickfix-event-handoff-8033f27d.LCn8N7` on `ubuntu@192.168.252.5`, recorded
in [remote-directory.txt](remote-directory.txt). It contained only
`before.test`, `candidate.test`, `spec/FIX40.xml` and `spec/FIX44.xml`.
No shared infrastructure was changed. The existing shared ARM64 QEMU VM remained
subject to other workloads;
[preparation metadata](vm-preparation.txt) records the kernel, CPUs and load;
[capture-start metadata](vm-before-capture.txt) records UTC time and current load.
This is not production or bare-metal evidence.

Remote cleanup completed at 2026-10-16 16:03:31 UTC. The
[pre-cleanup inventory/load](remote-cleanup-before.txt) and
[removal/absence proof](remote-cleanup.txt) record removal of exactly those four
files, followed by the two empty task-owned directories. The cleanup command
exited 0. Historical benchmark commands above require uploading the same
hash-matched executable and XML again before use.

The local baseline remains
explicitly ignored at `../cached-length/cached-length-linux-arm64.test`.
The local candidate is ignored at `event-handoff-linux-arm64.test` by this
directory's `.gitignore`. No generated executable will be staged or committed.
