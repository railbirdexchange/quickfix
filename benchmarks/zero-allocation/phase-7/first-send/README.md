# First-send scratch and convenience reparse

The fix-server allocation profile attributed approximately 34.54 MB to the
1,152-byte `outboundFields` allocation on fresh messages. The controlled pair
below measures that lifecycle directly and also captures the repeated arena
attachment caused by convenience reparsing into a previously sent Message.

BEFORE is runtime commit `88f1e22d5ca1ed8e392ee9612162aff91e828022` with the new
benchmark harness. AFTER is the runtime patch in `runtime-change.diff`, pinned by
`after-source.sha256`. `harness-before.sha256` and `harness-after.sha256` match.
Both captures use Go 1.25.3 on darwin/arm64, Apple M4 Pro, with exclusive benchmark
CPU scheduling. Go 1.23.12 compatibility and race checks are recorded separately.

| Completed ordinary send flow | Before ns/op range | After ns/op range | Before B/op | After B/op | Before allocs/op | After allocs/op |
|---|---:|---:|---:|---:|---:|---:|
| Fresh Message plus convenience parse each operation | 3,376–3,436 | 2,970–3,074 | 3,824 | 2,823 | 21 | 30 |
| Same Message, convenience reparse each operation | 21,049–21,843 | 2,571–2,600 | 442,366 | 151 | 96 | 10 |

Fresh messages allocate 1,001 fewer bytes per operation (26.2%) while creating
nine more small scalar allocations. This is a measured byte reduction with an
explicit cold object-count tradeoff. Neither row is a zero-allocation claim.
The second row no longer constructs a roughly 440 KB arena after every reparse.
These are completed-write component timings, not TCP latency or a submicrosecond
result; ranges from five runs are not a statistical significance claim.

The benchmark includes ordinary message construction/reparse, the original
ToApp callback, header preparation, persistence, encoding, completed peer copy,
returned output slot, peer parsing, and explicit journal retirement. Dictionary,
session, writer, fixed buffers, 1,023 retained records, and 66 warmup sends are
prepared before timing. The retained window remains populated throughout.

```text
caller owns Message and its scalar values
    -> synchronous callback and encoding
    -> fixed output slot transfers to writer
    -> completed Write
    -> vacant channel returns the slot
    -> peer consumes bytes and explicitly retires journal history
```

A fresh ordinary Message uses small owned engine scalars. Successful preparation
records the exact Message owner without allocating another object. Sending that
same representation again establishes the existing fixed construction arena and
engine scratch. Controls and parsed session/replay slots retain their existing
scope storage. The callback and socket writer never share mutable field buffers.

Convenience ParseMessage and CopyInto replacement reset the reuse marker.
Self-copy remains a no-op, and a shallow value copy cannot inherit the original
Message's ownership identity. Internal cold byte setters copy borrowed values,
including BodyLength and CheckSum from an output slot, before ordinary SetBytes;
getter bytes remain separate from encoded field bytes. Public SetBytes alias
semantics remain unchanged.

## Reproduction and checks

Run the same command before and after the runtime change:

```sh
GOTOOLCHAIN=go1.25.3 rtk proxy go test . -run '^$' -bench '^BenchmarkScopedOutboundFirstSend$' -benchmem -benchtime=1s -count=5 -cpu=1
```

- `before-88f1e22d-go1.25.3.txt`: PASS, 14.257 seconds.
- `after-go1.25.3.txt`: PASS, 14.237 seconds.
- `focused-go1.23.12.txt`: first-send, arena, and control fixtures PASS, 0.684 seconds.
- `focused-race-go1.23.12.txt`: the same fixtures plus backpressure regressions PASS, 3.024 seconds.
- `full-session-exact-go1.23.12.txt`: all seven TCP scenarios PASS, 100 cycles each,
  zero additional mallocs and allocated bytes in every scenario, 10.163 seconds.

Focused ownership fixtures overwrite every returned output slot, mutate callback
getter bytes, exercise internal borrowed timestamp input, reparse cold and warm
Messages, and cover self-copy, CopyInto, and shallow value copies. A warmed reused
ordinary Message separately asserts zero allocations over 100 completed sends.
The full TCP exact-allocation qualification is recorded in
`full-session-exact-go1.23.12.txt`.
