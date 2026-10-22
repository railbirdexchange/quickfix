# Allocation and lifecycle references

Reviewed 2026-10-16 against the ordinary QuickFIX implementation and its Go 1.23
compatibility requirement. These are design inputs, not benchmark evidence for
this library. Local results remain in the phase evidence files.

| Reference | Useful idea and application |
| --- | --- |
| [Sandeep: Zero Allocation in Go](https://medium.com/@ksandeeptech07/zero-allocation-in-go-ce29e6a9ffdc) | Preallocate and reuse buffers, then profile the actual hot path. Here, frame storage belongs to fixed session rings and construction storage to the reused caller Message. The article's pools and fresh builders do not establish our bounded zero-allocation contract. |
| [Reddit: Zero alloc libraries](https://www.reddit.com/r/golang/comments/1pmq3ph/zero_alloc_libraries/) | The useful discussion is about creating data in its owning outer scope and measuring escapes across interfaces. Treat comments as leads; verify mechanisms with compiler diagnostics and runtime profiles. Do not replace lifetime analysis with blanket rules about pointers, interfaces or slices. |
| [Saleh Rahimzadeh: Zero-Allocation in Go](https://dev.to/saleh_rahimzadeh/zero-allocation-in-go-golang-237k) | Fixed data layouts and bounded buffers apply to dictionary metadata and field lookup. `copy` versus `append` is not the guarantee: append into sufficient checked capacity can also avoid allocation. A preceding allocating `make` still counts. |
| [Changkun Ou: Scheduling Function Calls with Zero Allocation](https://golang.design/research/zero-alloc-call-sched/) | Replace per-operation escaping closures and completion-channel creation with persistent owner state and work records. QuickFIX uses fixed output slots, existing channels and one session-owned resend cursor/callback. The paper's pool implementation is not adopted. Its 2026 correction distinguishes a roughly 366 ns unlocked-goroutine benchmark from roughly 9 us actual locked-main-thread dispatch on an M2; neither is a FIX or socket latency result. |
| [Kawin: Zero-Allocation Programming in Go](https://kawin.dev/en/posts/go/zero_allocation/) | Known-size arrays and outer-scope reuse are useful. A newly created `strings.Builder` or returning a slice does not by itself prove zero allocations; inspect the actual compiler and warmed operation. |
| [Go team: Allocating on the Stack](https://go.dev/blog/allocation-optimizations) | Escape analysis and inlining determine placement. The described speculative variable-size slice backing starts in Go 1.25, and append-site improvements in Go 1.26. This implementation must work on Go 1.23, so fixed backing and explicit capacity admission remain required; newer compiler behavior cannot substitute for that qualification. |
| [Alexander Obregon: Go Memory Zeroing Rules](https://alexanderobregon.substack.com/p/go-memory-zeroing-rules) | Reset/zeroing work and pointer retention remain CPU and GC concerns after allocation count reaches zero. Profile them separately. Preserve required pointer cleanup and initialize every visible field; do not bypass Go's zero-value semantics or introduce unsafe uninitialized storage. |
| [Lince Mathew: Zero Allocation in Go](https://medium.com/@lince-mathew/zero-allocation-in-go-writing-faster-code-7b9b91f4f899) | Reuse and profile; its general builder/pool examples are allocation-reduction suggestions, not proof of a bounded allocation-free network path. |
| [colega/zeropool](https://github.com/colega/zeropool) | The interface-boxing analysis explains why wrapping a slice value can allocate. Its implementation maintains two `sync.Pool`s and is not adopted: the required design has fixed capacity and explicit ownership. No dependency was added. |

The [Go 1.23 pool contract](https://github.com/golang/go/blob/go1.23.12/src/sync/pool.go)
allows pooled entries to disappear, so a pool is not a fixed-capacity resource
reservation. The [Go 1.23 string builder implementation](https://github.com/golang/go/blob/go1.23.12/src/strings/builder.go)
allocates backing when it grows and drops its buffer on Reset. It is useful for
constructing an owned string, not a substitute for the fixed FIX output slots.

The resulting lifecycle is concrete: a caller owns a construction Message;
the session owns parsed input and control scratch; the writer exclusively owns
an encoded output slot until Write returns; the journal owns retained bytes
until an explicit retention acknowledgement. Callback capture, interface
dispatch, custom field writers and section replacement are tested at those
ownership boundaries.

The [callback lifetime audit](callback-lifetime-audit.md) identifies an unresolved
compatibility gap in the current implementation. Internal single ownership does
not establish that an existing application stopped using a pointer when its
callback returned. These references do not remove the need for an ownership
signal or an owned boundary copy.

Allocation and latency are separate acceptance checks. Exact whole-process
allocation deltas cover warmed completed cycles; the individual-request latency
fixture includes TCP, peer validation and the acknowledgement barrier proving
write completion and slot return. No microbenchmark from these references is
used to claim the one-microsecond target.
