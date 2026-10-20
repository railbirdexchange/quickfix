# Owned message string encoding

Constructed `Message.String()` previously built an owned byte slice and then
copied the entire wire into an owned string. It now writes directly into a local
`strings.Builder`, reserving the exact serialized length before writing. The
existing field comparator is shared with append encoding, and each section keeps
its existing lock. `cook` still updates stored BodyLength and CheckSum fields.

Valid checksum text is prepared once: 256 string entries share one immutable
768-byte backing string. This adds a fixed 4 KiB string table on this 64-bit
target and removes the temporary three-byte string from each `cook` call.
`SetString` still copies into the field's existing owned storage; the formatting
fallback for values outside 0..255 is unchanged.

```text
caller-owned Message
  -> cook: update stored tags 9 and 10
  -> existing section locks and field ordering
  -> fresh local builder reserved for the complete wire
  -> owned immutable string retained by caller

package initialization -> fixed immutable checksum strings
  -> existing SetString ownership boundary -> message-owned field value

parsed raw message -> existing rawMessage.String() owned copy
```

This is a synchronous compatibility boundary. The returned string remains valid
after field mutation, message reuse and input-buffer reuse. Mutable messages
remain single-owner objects. No API signatures, byte encoding, resend body
handling, dictionary validation, logging, persistence, pools or queue behavior
change. This does not claim zero allocation for `String()` or the whole library:
an independently retained output string still owns its storage.

## Controlled measurements

Before runtime: `4f4c99e3a629f3227fa51b451417d280d0a12ab4`. Go 1.27.2,
darwin/arm64, Apple M4 Pro, one CPU, five one-second samples. Benchmark setup,
fixed timestamp assignment and warm-up are outside timing. Every result escapes
to a string sink. The arena rows use 4,096-byte / 64-field construction storage;
these limits are benchmark fixture limits, not a new public configuration.
No asynchronous operation is part of this measurement.

The original benchmark and String correctness files are unchanged between
captures; [before](harness-before.sha256) and [after](harness-after.sha256)
digests match. Checksum correctness was added in a separate file before its
runtime change and passed against the old formatter. Production hashes are
[before](runtime-before.sha256) and [after](runtime-after.sha256).

| Warmed operation | Before median ns/op | After median ns/op | Before B/op / allocs/op | After B/op / allocs/op |
|---|---:|---:|---:|---:|
| Ordinary constructed order String | 848.5 | 809.2 | 264 / 7 | 144 / 5 |
| Ordinary constructed reject String | 812.9 | 785.5 | 296 / 7 | 160 / 5 |
| Arena constructed order String | 694.6 | 687.0 | 227 / 3 | 112 / 1 |
| Arena constructed reject String | 689.1 | 673.5 | 259 / 3 | 128 / 1 |
| Raw order String control | 20.52 | 20.80 | 112 / 1 | 112 / 1 |
| Raw reject String control | 21.08 | 21.86 | 128 / 1 | 128 / 1 |
| Mandatory baseline: build order bytes | 827.1 | 806.1 | 152 / 6 | 144 / 5 |

The constructed String rows eliminate one complete wire allocation and the
temporary checksum string. Arena String reaches its one necessary owned output
allocation. All other mandatory baseline byte/object counts are unchanged,
including zero-allocation warmed parsing and validation. Ordinary field setters
still allocate; this phase does not change that ownership boundary.

Raw captures: [String before](string-before.txt), [String after](string-after.txt),
[mandatory before](baseline-before.txt), [mandatory after](baseline-after.txt).
The intermediate builder-only captures are retained as
[String](string-builder-only.txt) and [mandatory](baseline-builder-only.txt).
Their ordinary order/reject rows were 152 B / 6 allocations and 168 B / 6
allocations; arena rows were 115 B / 2 and 131 B / 2. Final median timings were
a few percent slower than that intermediate capture, while unchanged raw controls
also moved. The allocation reduction is exact in these samples; this evidence
does not isolate a checksum latency improvement or establish deployed latency.

```sh
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkAllocationBaseline$' -benchmem -benchtime=1s -count=5 -cpu=1
GOTOOLCHAIN=auto rtk proxy go test . -run '^$' -bench '^BenchmarkMessageString$' -benchmem -benchtime=1s -count=5 -cpu=1
```

## Correctness and qualification boundary

Independent golden wires cover logon, NewOrderSingle with resend headers,
reordered repeating groups and nested groups in ordinary and arena storage.
The group fixtures explicitly clear the parsed raw shortcut to exercise the
constructed serializer without depending on the separate `CopyInto` operation.
Existing append and resend fixtures remain enabled. Additional checks cover
stored tags 9/10 insertion and replacement, retained strings across mutation and
reuse, cached raw wire behavior, capacity panics, all 256 checksums, negative and
out-of-range formatting, and mutable checksum getter isolation.

The new String fixtures pass against the original runtime and final runtime;
the checksum fixtures pass against the original formatter and final formatter.
Receipts: [before correctness](correctness-before.txt),
[checksum before](checksum-before.txt), [final correctness](correctness-after.txt).
The intermediate all-package suite also passed in
[tests-builder-only.txt](tests-builder-only.txt).

Final all-package tests passed (core 39.342s), vet passed, and the separate
core/internal race checks passed (44.511s / 1.303s). Receipts:
[tests](tests-after.txt), [vet](vet-after.txt), [race](race-after.txt).
Frozen runtime, benchmark and correctness digests were verified after all checks.
Commands:

```sh
GOTOOLCHAIN=auto rtk proxy go test ./... -skip '^TestMongoLogTestSuite$' -count=1
GOTOOLCHAIN=auto rtk proxy go vet ./...
GOTOOLCHAIN=auto rtk proxy go test -race . ./internal/... -count=1
```

The previously reproduced Mongo logging teardown failure is excluded explicitly;
Mongo store tests remain enabled. No live FIX-server deployment or load-test
result is claimed by these local component measurements.
