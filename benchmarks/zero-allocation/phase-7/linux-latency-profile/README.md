# Current Linux latency baseline and CPU diagnostics

This capture establishes a fresh baseline and identifies sampled work; it does
not establish a minimum achievable latency or deliver an optimization. The
unchanged binary still reports **0 B/op and 0 allocs/op in all ten unprofiled
rows**. Ordinary direct p50 is 6.791–7.000 µs and groups p50 is 7.583–7.791 µs.
The sub-microsecond target remains unmet.

The machine is the existing shared ARM64 QEMU VM at `ubuntu@192.168.252.5`.
Existing k3s/EP3/dependency workloads were left running. These are neither
production nor bare-metal results. No services, affinity, scheduling, network
configuration, production source, or fixture code changed. No other Go workload
from this task ran concurrently with either benchmark or profile capture.

## Frozen inputs and execution identity

- Clean checkout at capture: `338c3f03efd663365d050baffb6af4f24e91448d`.
- Production implementation: `a2fb11b923d1e0143a60427cf38ba2854cc142f6`.
  [production-source-diff.txt](production-source-diff.txt) is empty for Go source,
  module files and dictionaries after excluding the existing direct-latency
  fixture. [source-git.txt](source-git.txt) records checkout commit/tree and
  production commit/tree, in that order.
- Compiler: Go 1.23.12, `GOOS=linux`, `GOARCH=arm64`, `GOARM64=v8.0`,
  `CGO_ENABLED=0`, empty `GOFLAGS`. [compiler.txt](compiler.txt) contains
  `GOVERSION GOOS GOARCH GOARM64 CGO_ENABLED GOTOOLCHAIN GOFLAGS`, in that order;
  [binary-buildinfo.txt](binary-buildinfo.txt) independently identifies Go 1.23.12.
- Executable SHA-256:
  `0515b641faac565783b2f3b0eefcac9504c3422939d4b2c1bef990e94188df03`.
  This is identical to the earlier [Linux read-to-write capture](../linux-read-to-write/README.md).
- FIX40 XML SHA-256:
  `8f116f5277af9d401db0d6296b15645dfd0afd00d097e70b20c2d9cfbd73ce24`.
  FIX44 XML SHA-256:
  `c74393c72b0abd1dee8cf63f8a22946ef5010515602a4e0bd1b4f911a5c393ca`.
  [Local](upload-local.sha256) and [remote](upload-remote.sha256) upload hashes
  match. [fixture-source.sha256](fixture-source.sha256) pins the relevant
  benchmark/peer fixtures, dictionaries and module files.
- Environment: [uname](vm-uname.txt), [CPU](vm-cpu.txt),
  [virtualization](vm-virtualization.txt), [load before](vm-load-before.txt),
  [load after](vm-load-after.txt), [process snapshot](vm-processes-before.txt).
  Linux is `7.0.0-38-generic`, with four online Apple virtual CPUs. Process CPU
  percentages are snapshot averages, not exclusive benchmark utilization.
- [completed-at-utc.txt](completed-at-utc.txt) records capture completion.
  Local pprof text renders the profile start time in EDT.

## Fresh unprofiled direct baseline

The unchanged `BenchmarkOrdinaryFullSessionReadToWrite` measures final socket
read return through the final successful response write. It includes remaining
framing, queueing, parse, dictionary/group validation, handler, persistence and
encoding. It excludes network arrival, blocked read time and framing before the
final read. Instrumentation overhead is retained.

Each sample is accepted only after the peer validates the response, sends the
ACK, and the slot/journal retirement barriers finish. Direct latency ends at
write completion; `ns/op` and requests/s also include that later ACK/barrier and
sample recording. There is one outstanding request. Setup and warm-up are outside
the timed loop. Every final row has fewer than 65536 requests, so its reported
percentiles cover all its timed requests; its maximum also covers all requests.

The fixture uses FIX44 validation, 16 KiB frames, 512 fields, 128 group entries,
group depth 8, configured inbound/writer/pending queue capacities of 32, 33 output
slots, `BoundedStore(1024, 16384)`, `NullLog`, and ordinary application callbacks.
The peer ACK explicitly permits journal retirement.

Five 1-second repetitions, CPU 1, completed with exit 0 and PASS. Ranges below
span the five rows; [raw output](baseline-go1.23.12.txt) retains every row.

| Scenario | Direct p50 ns | Direct p95 ns | Direct p99 ns | Direct maximum ns | Timed ns/op | B/op | allocs/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Ordinary | 6791–7000 | 7750–9250 | 11708–13292 | 96083–177500 | 21217–22030 | 0 | 0 |
| Groups | 7583–7791 | 8542–10125 | 14125–15416 | 97958–310791 | 22716–23443 | 0 | 0 |

The earlier ordinary p50 of 6.333–6.500 µs came from the identical executable.
The observed run-to-run difference reinforces the shared-VM measurement limit;
it is not attributed to a code regression or a specific external cause.

## CPU diagnostics and likely next targets

Two separate 10-second profile runs completed with exit 0 and PASS:

- [Direct ordinary run](direct-profile-run-go1.23.12.txt),
  [raw CPU profile](direct-ordinary.cpu.pprof): 15.91 s elapsed profile duration,
  12.77 s sampled CPU.
- [Completed outbound NewOrderSingle component](outbound-profile-run-go1.23.12.txt),
  [raw CPU profile](outbound-nos.cpu.pprof): 14.32 s elapsed profile duration,
  14.18 s sampled CPU. This existing component uses FIX40, an in-memory connection
  with an independent receive buffer, completed writer/returned-slot checks, peer
  parse, and explicit retirement with 1023 prior messages retained.

**These profiles cover the whole process, including setup, benchmark calibration,
warm-up, peer work, ACK handling and cleanup.** The benchmark timer excludes
setup, but the CPU profiler does not. CPU samples do not measure blocked duration,
and the profile includes reads/peer work outside the direct latency interval.
Profiled `ns/op`/percentiles are diagnostics, not the unprofiled baseline.
No sampled percentage below is a percentage of the 6–7 µs direct latency or a
prediction of removable time. Cumulative rows overlap and must not be summed.

| Direct-process sampled work | CPU seconds | Share of 12.77 s samples | Interpretation |
| --- | ---: | ---: | --- |
| `internal/runtime/syscall.Syscall6` flat | 5.58 | 43.70% | Server and peer socket/syscall work, not solely the measured response interval |
| `runtime.selectgo` cumulative | 1.07 | 8.38% | Includes fixture ACK/checkpoint coordination and session work |
| `doParsing` cumulative, all callers | 0.98 | 7.67% | 0.38 s inbound session parse, 0.32 s outbound admission parse, 0.28 s peer parse |
| Server `prepareOutbound` cumulative | 0.80 | 6.26% | Includes 0.34 s replay-capacity `MessageSlot.Parse`, 0.17 s header filling, 0.10 s append |
| Server `sessionValidator.Validate` cumulative | 0.49 | 3.84% | Includes incoming requests and ACKs; not all samples fall inside the direct interval |
| `SetWriteDeadline` cumulative | 0.06 | 0.47% | Small sampled share in this capture |

Both server and peer `validateFIXWithScratch` together account for 0.90 s
(7.05%). Restricting to the server validator gives `validateFields` 0.26 s,
`validateWalkWithTagSet` 0.12 s, `getFieldType` 0.06 s, and numeric dictionary-map
lookup 0.09 s. These are overlapping call paths. Dictionary lookup duplication is
a measurable candidate for a small change, but this capture does not support
calling it the dominant latency cost.

| Outbound-component sampled work | CPU seconds | Share of 14.18 s samples |
| --- | ---: | ---: |
| `prepareOutbound` cumulative | 7.43 | 52.40% |
| Replay-capacity `MessageSlot.Parse` under preparation | 2.48 | 17.49% |
| `AppendTo` call in preparation | 1.68 | 11.85% |
| Header filling | 1.44 | 10.16% |
| Replay reset immediately before parse | 0.16 | 1.13% |
| Replay reset immediately after parse | 0.26 | 1.83% |

The full component's `doParsing` is 5.10 s cumulative, but only 2.34 s of that is
under `prepareOutbound`; the peer parse accounts for the other major path.
The source listing identifies the exact admission parse and reset calls. This
makes redundant preparation/replay bookkeeping a concrete area to assess while
preserving every field/group/depth limit and pre-commit admission check. It does
not justify skipping replay admission or retaining stale parse state. The
pre-parse reset is a small measured part of the component, not a likely solution
to the entire target by itself.

The profile also supports examining encoded-field iteration and header work in
isolation. Any candidate still needs ownership/mutation proof and a paired
completed-write/direct benchmark. No such change was made during this task.

Reports:

- Direct: [flat](direct-top.txt), [cumulative](direct-cum.txt),
  [preparation](direct-prepare.txt), [parse](direct-parsing.txt),
  [append](direct-append.txt), [validation](direct-validation.txt),
  [server-only validation](direct-session-validation.txt),
  [validation source lines](direct-validation-lines.txt),
  [runtime/channels/deadlines](direct-runtime.txt),
  [deadline/header-time detail](direct-deadline-time.txt).
- Outbound: [flat](outbound-top.txt), [cumulative](outbound-cum.txt),
  [preparation](outbound-prepare.txt), [selected preparation nodes](outbound-prepare-selected.txt),
  [preparation source lines](outbound-prepare-lines.txt),
  [parse](outbound-parsing.txt), [append](outbound-append.txt),
  [validation filter](outbound-validation.txt), [runtime](outbound-runtime.txt).
  The component does not perform normal incoming dictionary validation, so its
  validation filter is not a substitute for the direct/server report.

Selected-node reports may attribute hidden child flat time to visible ancestors;
use the unfiltered flat reports for self-time. Focused cumulative values retain
the original whole-profile denominator. The source-level reports are diagnostic
sample attribution, not instrumentation of each line.

## Commands and retained artifacts

The test binary was cross-compiled from the repository root:

```sh
GOTOOLCHAIN=go1.23.12 GOOS=linux GOARCH=arm64 CGO_ENABLED=0 rtk proxy go test -c -o benchmarks/zero-allocation/phase-7/linux-latency-profile/quickfix-linux-arm64.test .
```

Compilation exited 0; [build output](build-go1.23.12.txt) is empty. Native `scp`
uploaded that executable and unchanged FIX40/FIX44 XML files to the unique
task-owned directory `/tmp/quickfix-latency-profile-338c3f03.NOVbHI`.

```sh
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-latency-profile-338c3f03.NOVbHI && timeout 180s ./quickfix.test -test.run "^$" -test.bench "^BenchmarkOrdinaryFullSessionReadToWrite$" -test.benchmem -test.benchtime=1s -test.count=5 -test.cpu=1 -test.timeout=150s'
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-latency-profile-338c3f03.NOVbHI && timeout 120s ./quickfix.test -test.run "^$" -test.bench "^BenchmarkOrdinaryFullSessionReadToWrite$/^ordinary$" -test.benchmem -test.benchtime=10s -test.count=1 -test.cpu=1 -test.cpuprofile=direct-ordinary.cpu.pprof -test.timeout=90s'
rtk proxy ssh -o BatchMode=yes -o ConnectTimeout=10 ubuntu@192.168.252.5 'cd /tmp/quickfix-latency-profile-338c3f03.NOVbHI && timeout 120s ./quickfix.test -test.run "^$" -test.bench "^BenchmarkScopedOutboundSession$/^new_order_single$" -test.benchmem -test.benchtime=10s -test.count=1 -test.cpu=1 -test.cpuprofile=outbound-nos.cpu.pprof -test.timeout=90s'
```

After native `scp` retrieval, [local](profiles-local.sha256) and
[remote](profiles-remote.sha256) profile hashes matched. Native Go 1.23.12 pprof
was used against the exact executable. Report options were `-top -nodecount=60`,
`-top -cum -nodecount=60`, and cumulative focuses for `prepareOutbound`,
`doParsing`, `appendTo`, `validateFIX|validateFields|validateWalk`, and
`runtime\.(chan|select|schedule|findRunnable|netpoll)|[Dd]eadline` (nodecount 80).
The specific active filters also appear in each report. Example:

```sh
GOTOOLCHAIN=go1.23.12 rtk proxy go tool pprof -top -cum -nodecount=60 -focus=prepareOutbound benchmarks/zero-allocation/phase-7/linux-latency-profile/quickfix-linux-arm64.test benchmarks/zero-allocation/phase-7/linux-latency-profile/outbound-nos.cpu.pprof
```

The uploaded binary, XML files, profiles and empty task-created remote
directories were removed after retrieval. The local exact executable remains
temporarily for profile inspection and paired captures; this directory's
`.gitignore` explicitly excludes it from staging. Raw profiles and text/hash
evidence remain. No production/fixture edits, new correctness/race qualification,
commit or push were performed by this worker. CPU ownership was released after
profile extraction.
