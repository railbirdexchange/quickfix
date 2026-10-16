# Linux VM direct read-to-write qualification

The unchanged production implementation reports **0 B/op and 0 allocs/op in all
10 benchmark rows** on this VM. Direct final-read-to-completed-write p50 ranges
from 6.333–6.500 µs for ordinary messages and 7.083–7.334 µs with groups.
**The sub-microsecond target is not met.**

This is a shared Linux ARM64 QEMU VM measurement, not production or bare-metal
evidence. No Linux baseline comparison was run, and these results do not establish
an improvement over another operating system or machine. Existing k3s, EP3 and
dependency processes remained running. No infrastructure, service, CPU affinity,
or scheduling configuration was changed.

## Source and environment

- Checkout: `12c7cd1f34c70c93168bc6089e8bcd54d19a74ec`, initially clean.
- Production implementation: `a2fb11b923d1e0143a60427cf38ba2854cc142f6`.
  The comparison of Go source, module files and the required dictionary is empty
  after excluding the added direct-latency fixture. The checkout contains that
  existing fixture and documentation updates; this run changed no source.
- Source Git tree identities: [source-git.txt](source-git.txt), ordered as checkout
  commit, checkout tree, production commit, production tree. The empty
  [production-source-diff.txt](production-source-diff.txt) records the successful
  production-source comparison. Fixture/module/XML SHA-256 values are in
  [fixture-source.sha256](fixture-source.sha256).
- Cross-compiler: Go 1.23.12, `GOOS=linux`, `GOARCH=arm64`, `GOARM64=v8.0`,
  `CGO_ENABLED=0`, no `GOFLAGS`. See [compiler.txt](compiler.txt), whose lines are
  `GOVERSION GOOS GOARCH GOARM64 CGO_ENABLED GOTOOLCHAIN GOFLAGS`, and
  [binary-buildinfo.txt](binary-buildinfo.txt).
- VM: `ubuntu@192.168.252.5`, hostname `docker-vm`, Linux `7.0.0-38-generic`,
  aarch64, four online Apple virtual CPUs; [uname](vm-uname.txt),
  [CPU](vm-cpu.txt), [QEMU detection](vm-virtualization.txt).
- No Go compiler is installed on the VM. Existing native SHA-256 and timeout
  tools were used; [tool availability](vm-tools.txt).
- Shared-load observations: [before](vm-load-before.txt),
  [after](vm-load-after.txt), [process snapshot](vm-processes-before.txt).
  CPU percentages are the process snapshot's averages, not isolated benchmark
  utilization. [Capture completion time](completed-at-utc.txt) is UTC.

The test binary SHA-256 was
`0515b641faac565783b2f3b0eefcac9504c3422939d4b2c1bef990e94188df03`.
The required unchanged `spec/FIX44.xml` SHA-256 was
`c74393c72b0abd1dee8cf63f8a22946ef5010515602a4e0bd1b4f911a5c393ca`.
[Local](upload-local.sha256) and [remote](upload-remote.sha256) hashes matched
before execution.

## Measurement boundary and configuration

The existing `BenchmarkOrdinaryFullSessionReadToWrite` fixture timestamps
`parser.lastRead` immediately after the final socket read returns. The direct
interval includes remaining framing/check/copy work, inbound queueing, parsing,
dictionary/group validation, the ordinary handler, persistence, encoding, writer
queue waits and the final successful TCP write. It excludes network arrival,
read blocking, and framing before the final read. No instrumentation overhead is
subtracted.

Each request is independently correlated with its response. Direct latency ends
at successful write completion; the sample is accepted only after peer response
validation, the ACK, returned slots, and journal retirement. Consequently,
`ns/op`, requests/s and RTT include the ACK/sample qualification, whereas the
`read-write-*` metrics do not. There is one outstanding request. Setup and warm-up
are outside the timed loop. Percentiles cover `min(N, 65536)` final samples;
every row here has fewer than 65536 requests, so its percentiles cover all timed
requests. Maxima cover all timed requests.

The unchanged fixture uses 16 KiB frames, 512 fields, 128 group entries, group
depth 8, inbound channel capacity 32, outbound writer channel capacity 32 and
pending-output capacity 32 (33 output slots). It retains dictionary validation,
the ordinary application callbacks, `BoundedStore(1024, 16384)`, and `NullLog`.
The peer ACK permits explicit journal retirement. These are the fixture's limits
and components, not a guarantee for arbitrary handlers, stores or loggers.

## Results

Both commands exited 0 and ended in PASS. All three focused direct-latency tests
passed, including partial writes, failed writes, writer wait inclusion and the
slot-return qualification barrier; [focused output](focused-go1.23.12.txt).

Ranges below span the five rows per scenario. Raw rows and all samples/counts
are retained in [benchmark-go1.23.12.txt](benchmark-go1.23.12.txt).

| Scenario | Direct p50 ns | Direct p95 ns | Direct p99 ns | Direct maximum ns | Timed ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Ordinary | 6333–6500 | 8000–8708 | 10834–11500 | 157250–521501 | 20691–21350 | 0 | 0 |
| Groups | 7083–7334 | 9542–9958 | 11792–13250 | 86583–343334 | 22203–23074 | 0 | 0 |

Ordinary throughput was 46838–48331 requests/s; group throughput was
43339–45039 requests/s. These rates include the one-request-at-a-time ACK barrier.
No throughput saturation claim follows from them. Outliers are retained in the
raw output. No race run or seven-scenario exact-allocation run was part of this
bounded platform qualification; existing source qualification remains separate.

## Exact execution

From the repository root, the existing test package was cross-compiled:

```sh
GOTOOLCHAIN=go1.23.12 GOOS=linux GOARCH=arm64 CGO_ENABLED=0 rtk proxy go test -c -o benchmarks/zero-allocation/phase-7/linux-read-to-write/quickfix-read-to-write-linux-arm64.test .
```

[Build output](build-go1.23.12.txt) is empty and compilation exited 0. Native
`scp` copied that exact executable and the unchanged XML into the unique
task-owned directory `/tmp/quickfix-read-to-write-12c7cd1f.WAtBYS`, preserving the
fixture's relative `spec/FIX44.xml` path. Only the following tests were executed:

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-read-to-write-12c7cd1f.WAtBYS && timeout 90s ./quickfix.test -test.run "^TestOrdinaryReadToWrite" -test.count=1 -test.cpu=1 -test.v -test.timeout=60s'
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-read-to-write-12c7cd1f.WAtBYS && timeout 180s ./quickfix.test -test.run "^$" -test.bench "^BenchmarkOrdinaryFullSessionReadToWrite$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=150s'
```

The compiled-binary `-test.*` flags are the native equivalents of the repository
`go test` flags; `-test.cpu=1` sets GOMAXPROCS to one, without pinning a CPU. Raw
output includes the existing SSH host-key informational warning.

Cleanup succeeded: the uploaded executable and XML were removed, followed by
only the two now-empty task-created directories. The locally generated test
binary was also removed after hashing. No executable is retained in this
evidence directory, and no commit or push was performed by the qualification
worker.
