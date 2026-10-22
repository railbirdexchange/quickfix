# Rejected scalar preparation experiment

Disposition: **REJECTED AND REVERTED**. The byte-setter microbenchmarks improved,
but the completed-send benchmark showed no useful latency benefit. No production
change from this experiment remains. The two added benchmark/test files are
preserved as `.go.txt` diagnostic source, outside the compiled package.

The baseline is `4d74075f8fbfcf4a0e8decd35999825cb54672da`. The candidate changed
only `message_arena.go`: each setter carried its checked record index and encoded
lengths directly into mutation, avoiding repeated preparation. It added no
persistent cache, fields, allocation, or capacity bypass. The exact rejected
change is [candidate-runtime.diff](candidate-runtime.diff); the candidate source
and harness hashes are recorded in [candidate-source.sha256](candidate-source.sha256).
Those hashes use the original package-root file names at capture time.

The scalar benchmark was added and frozen before either capture. The existing
completed-send harness was unchanged. [Before](harness-before.sha256) and
[after](harness-after.sha256) harness hashes match. Setup and warm-up are outside
the timed loops; completed-send iterations wait for the write and returned slot.
The scalar `typed` operation performs three setters; the other scalar cases each
perform one byte setter.

Both captures used this exact command, with separate output files:

```sh
GOTOOLCHAIN=go1.25.3 rtk proxy go test . -run '^$' -bench '^(BenchmarkScopedOutboundSession|BenchmarkMessageArenaScalarMutation)$' -benchmem -benchtime=1s -count=5 -cpu=1
```

| Benchmark | Before median ns/op | Candidate median ns/op | Before / candidate B/op | Before / candidate allocs/op |
| --- | ---: | ---: | ---: | ---: |
| Scalar typed | 109.7 | 107.7 | 0 / 0 | 0 / 0 |
| Scalar external bytes | 45.05 | 36.95 | 0 / 0 | 0 / 0 |
| Scalar overlapping bytes | 54.43 | 45.89 | 0 / 0 | 0 / 0 |
| Completed NewOrderSingle send | 1966 | 1966 | 0 / 0 | 0 / 0 |
| Completed repeating-group send | 2813 | 2877 | 0 / 0 | 0 / 0 |
| Completed custom-type send | 1927 | 1935 | 0 / 0 | 0 / 0 |

All 30 rows in each capture reported zero allocated bytes and zero allocations.
The results do not establish a statistically significant session improvement;
the group median moved in the wrong direction. Raw evidence:

- [Before](before-4d74075f-go1.25.3.txt): PASS, 38.930 s.
- [Candidate](after-go1.25.3.txt): PASS, 38.592 s.

Focused correctness passed on Go 1.23.12 (0.790 s), including rejection before
staging, sticky capacity errors, overlapping bytes, group replacement, and
remove/reinsert accounting. Independent source review found no actionable issue.

```sh
GOTOOLCHAIN=go1.23.12 rtk proxy go test . -run '^(TestMessageArena|TestScopedControl|TestScopedOutboundFirstSend|TestScopedOutboundColdScalar|TestScopedOutboundRetainedFramesRespectReplayLimits)' -count=1 -v -timeout=60s
```

[Focused test output](focused-go1.23.12.txt) is candidate evidence. Race and the
seven-scenario full-session exact allocation check were **NOT RUN** for this
candidate because the measured change was rejected. Existing qualification of
the restored production baseline is unaffected. No new performance claim or PR
is based on this experiment.
