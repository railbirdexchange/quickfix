# Go 1.27.2 toolchain qualification

The module now prefers `toolchain go1.27.2` while retaining `go 1.23` as its
language and minimum-version declaration. No runtime Go source, dependency
version or generated source changes are included in this upgrade. The
application that imports QuickFIX selects its own compiler; this module's
toolchain preference applies when working in QuickFIX itself.

CI builds and unit tests select Go 1.23.12 and Go 1.27.2 explicitly. The six
protocol acceptance suites select Go 1.27.2. `GOTOOLCHAIN=local` prevents the
minimum-version job from automatically switching to the preferred compiler.
The existing golangci-lint v1.64.6 job stays on Go 1.23.12. The devcontainer
uses `mcr.microsoft.com/devcontainers/go:1.27` pinned to manifest digest
`sha256:8f1c83128e4bce9d745b1dc0f28f8c027e303d3886e2434966ff135296c41770`.
CodeQL now selects Go from `go.mod` with setup-go v7 and uses CodeQL actions
v4. The old v1 actions predate Go 1.27 support, which was added in
[CodeQL CLI 2.26.4](https://codeql.github.com/docs/codeql-overview/codeql-changelog/codeql-cli-2.26.4/).
These workflow changes are recorded in the configuration hashes; local
qualification does not assert a completed hosted CI or CodeQL run.

## Matched comparison

The BEFORE records are the immediately preceding Go 1.25.3 captures on the
same runtime implementation, reused rather than rerun:

- [Mandatory allocation baseline](../heartbeat-store/baseline-after.txt).
- [Heartbeat/store benchmark](../heartbeat-store/heartbeat-after.txt).
- [Actual previous compiler](../heartbeat-store/toolchain.txt).
- [Previous runtime/harness hashes](../heartbeat-store/after-source.sha256).

The [runtime hash check](runtime-source-check.txt) confirms the matched files
are unchanged. [runtime-source.txt](runtime-source.txt) records the common
committed runtime head. [upgrade-source.sha256](upgrade-source.sha256) records
the upgrade configuration, module files and unchanged benchmark harnesses.
New results use the compiler recorded in [toolchain.txt](toolchain.txt):
Go 1.27.2, darwin/arm64. Both captures use Apple M4 Pro, `-cpu=1`, five
one-second samples, setup/warm-up outside timing, and serialized execution.

```sh
GOTOOLCHAIN=go1.27.2 rtk proxy go version
GOTOOLCHAIN=auto rtk proxy go version
GOTOOLCHAIN=go1.27.2 rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=go1.27.2 rtk proxy go test . -run '^$' -bench '^BenchmarkHeartbeatStore$' -benchmem -benchtime=1s -count=5 -cpu=1
```

The heartbeat fixture includes callbacks, sequence updates, persistence,
completed writer copies, returned output slots and independent peer parsing.
The timer row additionally expires the reused EventTimer and dispatches the
real session timeout event. It is a completed-write component measurement,
not network or full-service latency. Its ownership contract and warm-up are
documented in the [unchanged fixture description](../heartbeat-store/README.md).

The [new mandatory baseline](baseline-after.txt) passes with unchanged
allocation counts: normal unsigned-decimal parsing, all reused parse rows and
validation are 0 B/0 allocations; decimal overflow is 168 B/5, convenience
building is 152 B/6, and legacy framing is 160 B/2.

All [heartbeat samples](heartbeat-after.txt) preserve the previous allocation
counts. Median times in nanoseconds per operation:

| Flow / store | B/op, both compilers | Allocs/op, both compilers | Go 1.25.3 | Go 1.27.2 |
|---|---:|---:|---:|---:|
| Idle heartbeat / bounded | 0 | 0 | 1880 | 1823 |
| Idle heartbeat / legacy external | 80 | 1 | 1674 | 1588 |
| Idle heartbeat / borrowed external | 0 | 0 | 1641 | 1576 |
| Timer heartbeat / bounded | 0 | 0 | 2425 | 2327 |
| Timer heartbeat / legacy external | 80 | 1 | 2216 | 2089 |
| Timer heartbeat / borrowed external | 0 | 0 | 2166 | 2073 |
| TestRequest / bounded | 0 | 0 | 2103 | 1956 |
| TestRequest / legacy external | 80 | 1 | 1806 | 1731 |
| TestRequest / borrowed external | 0 | 0 | 1777 | 1700 |

The borrowed external-store medians are approximately 4% lower in this run.
This is a component comparison on one host, not a claim about service latency
or a new allocation reduction from the compiler itself. The preceding runtime
change's removed frame allocation remains removed.

Mandatory baseline medians show the comparison's limits as well:

| Baseline operation | B/op, both | Allocs/op, both | Go 1.25.3 ns/op | Go 1.27.2 ns/op |
|---|---:|---:|---:|---:|
| Unsigned decimal, short | 0 | 0 | 11.52 | 11.30 |
| Unsigned decimal, signed fraction | 0 | 0 | 28.51 | 28.01 |
| Unsigned decimal, maximum uint128 | 0 | 0 | 55.79 | 55.35 |
| Unsigned decimal, overflow | 168 | 5 | 323.0 | 318.7 |
| Parse without dictionary | 0 | 0 | 395.9 | 373.7 |
| Parse with dictionary | 0 | 0 | 445.2 | 429.3 |
| Parse with repeating group | 0 | 0 | 842.6 | 821.9 |
| Validate NewOrderSingle | 0 | 0 | 694.8 | 687.2 |
| Convenience build | 152 | 6 | 787.6 | 776.1 |
| Legacy framed read | 160 | 2 | 110.0 | 112.9 |

The legacy framed-read median is slightly higher. These five-sample results
are reported without claiming every flow became faster.

## Verification commands

```sh
GOTOOLCHAIN=go1.27.2 rtk proxy go test -p=1 . ./internal ./datadictionary ./store/memory ./store/file ./log/composite ./log/file -count=1 -timeout=180s
GOTOOLCHAIN=go1.27.2 rtk proxy go test -race -p=1 . ./internal -count=1 -timeout=180s
GOTOOLCHAIN=go1.23.12 rtk proxy go version
GOTOOLCHAIN=go1.23.12 rtk proxy go test -p=1 . ./internal ./datadictionary ./store/memory ./store/file ./log/composite ./log/file -count=1 -timeout=180s
GOTOOLCHAIN=go1.27.2 rtk proxy go vet . ./internal ./datadictionary
```

The complete core suite includes exact allocation intervals through real TCP,
group construction, callback ownership, heartbeat persistence, reconnect,
backpressure, error recovery and decimal parity. Race instrumentation is run
separately from all performance measurements. CI version selections were
parsed with `yq` and recorded in [ci-version-selections.txt](ci-version-selections.txt).

Local qualification does not execute GitHub Actions, CodeQL, the devcontainer
build or the Ruby protocol acceptance matrices. External Mongo/SQL integration
tests are not included. The known historical Mongo logger teardown failure
remains documented separately in [phase 1](../../phase-1/README.md#existing-optional-mongo-logger-test-failure).

## Verification results

- [Go 1.27.2 core/local packages](core-go1.27.2.txt): PASS, core 30.529 s,
  plus internal helpers, dictionaries, memory/file stores and file/composite
  loggers. The core run includes the exact zero-allocation session intervals.
- [Separate Go 1.27.2 race run](core-race-go1.27.2.txt): PASS, core 33.223 s
  and internal helpers 1.479 s.
- [Minimum compiler](minimum-toolchain.txt): actual Go 1.23.12 confirmed after
  adding the preferred toolchain directive. The [compatibility suite](core-go1.23.12.txt)
  passes on all seven core/local packages, core 30.579 s.
- [Go 1.27.2 vet](vet-go1.27.2.txt): PASS, exit 0 with no diagnostics.
- [Automatic preferred-toolchain selection](preferred-toolchain-selection.txt):
  `GOTOOLCHAIN=auto` selects actual Go 1.27.2 in this module.
