# Performance

This page answers two questions: **what does a request path cost**, and **where does the observability money go**. Every table comes with a command to reproduce it.

**Version**: pulse-web v0.1.0 (current `main`; depends on pulse v0.2.4). This section is re-measured at tag time.

## How to read this page

The numbers fall into two classes, and they are read differently:

- **The deterministic class**: `allocs/op` and `B/op`. They are frozen as CI assertions (`bench/budget_test.go`), so they hold **across runs and across machines**.
- **The floating class**: `ns/op`. The same cell drifts 2–4× between sessions, so it is **only comparable within one round** — never to another round or another machine.

So that "one round" is identifiable, every table below carries a **control row** that has nothing to do with the framework (stdlib ServeMux, pure standard library). When the control moves, the machine state moved.

In the current round (2026-09-15) that control row tells the whole story:

| Round | Machine state | stdlib control | Same-round `Engine` request path | Allocations |
|---|---|---|---|---|
| 2026-09-13 (the round recorded in the design doc) | on AC power | 174 ns | 1790 ns | 22 allocs |
| 2026-09-15 (current round) | **on battery** (turbo limited) | **750 ns** | **6592 ns** | **22 allocs** |

Same machine, same code: **the ns differ by 4.3×, the allocation count is identical**. That is why this page treats allocation counts as the criterion and ns as structure only. For absolute values, run the commands below on a quiet machine and compare against the control row from **the same round**.

## Cost breakdown of a request path

Protocol: `-benchtime=20000x -count=5`, median, one round, one protocol. **Any Δ you subtract must use two rows of this table.**

| Scenario | ns/op | allocs | B/op |
|---|---|---|---|
| stdlib ServeMux route match (**control**, framework-independent) | 750 | 5 | 224 |
| scope derive + dispose (`Derive + Dispose` baseline) | 213 | **2** | 192 |
| + bare scope-local binding | 1271 | 14 | 649 |
| + `observability.AttachCollector` | 1197 | 14 | 681 |
| same · 10 / 50 / 100 plugin tree | 1301 / 1300 / 1428 | 14 | 681 |
| same · 50 plugins, parallel | 1888 | 14 | 681 |
| `Engine` request path (`New()`, 0 / 10 / 50 plugins) | 6592 / 6806 / 7132 | 22 | 6298 |
| `Engine` request path + `WithCollector()` | 7746 | **34** | 6787 |
| `Engine` request path (`Minimal()`) | 4689 | 17 | 5842 |
| `Engine` request path + `BodyLimit` route | 6584 | **23** | 6362 |
| `Engine` request path + `c.JSON` | see below | **25** | 6438 |

Δ derived from two rows of this table (same round, so subtracting is valid):

- **Per-request cost of `WithCollector()`** = 7746 − 6592 = **+1154 ns / +12 allocs** (end to end)
- **Per-request cost of `BodyLimit`** = 6584 − 6592 ≈ **0 ns / +1 alloc** — the extra allocation is the wrapper `http.MaxBytesReader` returns (bound to the request, not reusable); the ns sits inside the noise.
- **Assembly size does not reach the request path**: the `Engine` request path at 0 / 10 / 50 plugins is 6592 / 6806 / 7132 ns with **allocs constant at 22 and B/op constant at 6298**; at the kernel level the 10 / 50 / 100 plugin trees hold **allocs constant at 14**. Plugin-tree size does not change per-request cost, and that is the premise that makes "assembly" a selling point.
- **Containment self-check (against inverted conclusions)**: `AttachCollector` = bare binding + one Collector struct, so the criterion is **B/op** (681 > 649). Both report 14 allocs and their ns land in the same noise band this round (1197 vs 1271, order flipped) — **neither a single alloc value nor ns can tell these two apart**, which is precisely what "read structure, not absolute values" means.
- **Encoding to a buffer in `c.JSON`**: **+2 allocs / +97 B per response** (25 vs 22, 6438 vs 6298), bought "an encoding failure no longer sends an empty 200". Its ns is not listed separately — the allocation side is covered by the gate, and if you want the time, run the control in the same round.

Reproduce:

```bash
# control row + kernel layer
go test -run '^$' -bench 'BenchmarkStdlibServeMux_Route|BenchmarkScopeCycle|BenchmarkRequestCycle' -benchtime=20000x -count=5 ./bench/

# Engine request path (default / collector / minimal / body-limit)
go test -run '^$' -bench 'BenchmarkEngineRequestPath' -benchtime=20000x -count=5 ./bench/

# allocation gate: freezes the allocs / B/op columns above as assertions (must NOT run with -race)
go test -run TestRequestPathAllocBudget ./bench/
```

## What a sink costs

Almost all of the observability money goes into getting the record out of the door, and kernel event dispatch is fully synchronous (`Emit` / `EmitLocal` / `Waterfall`, `Parallel` included) — **the sink is as slow as your request path**.

Same `Record`, every sink writing to `io.Discard` (keeps formatting and locking, excludes terminal/disk I/O):

| Sink | ns/op | allocs | B/op |
|---|---|---|---|
| `web.ConsoleSink` (**the default**) | 865 | **0** | **0** |
| `observability.LineSink` (upstream line sink) | 945 | **0** | **0** |
| `observability.SlogSink` (structured) | 4730 | **18** | 1127 |

End to end (one request path, only the sink changes): default sink **8134 ns** vs nop sink **7255 ns** = **+0.9 µs and +0 allocs per request**.

Two things to read out of it: the default sink allocates **nothing** (rendering happens inside the line sink's critical section, on a single buffer); and `SlogSink` costs 18 allocations and 5× the time per record — it is the **machine-readable** path, and that is both its cost and its purpose.

Reproduce:

```bash
go test -run '^$' -bench 'BenchmarkSinkWrite_ConsoleVsUpstream|BenchmarkRequestPath_DefaultSink' -benchmem -count=5 ./bench/
```

For choosing a sink see [observability](/en/guide/observability); when throughput matters, wrap the slow sink in `observability.NewAsyncSink`.

## Real load against gin (a one-off verification, 2026-09-14)

This section answers "**is it on par with gin**". The measurement code (`loadtest/`, a separate module carrying the gin dependency) **never entered the repository**; it lives on branch `bench/gin-compare`. Only the protocol and conclusions are kept here.

Protocol: two separate processes sharing one `http.ListenAndServe` bootstrap with **only the handler varying**; within a cell the two sides run adjacent and swap order on odd/even rounds, with **only even rounds counting**; each cell takes the whole set of percentiles from the round whose RPS is the median, and the ratio takes the **median of the per-round paired ratios**. The load generator is hand-written (fixed concurrency, full connection reuse, every request inside the timing window entering the percentiles, no sampling). Machine: i9-14900HX / 32 logical cores / `GOMAXPROCS=32` / Go 1.27 / Windows amd64.

| Pairing | Concurrency | pulse-web RPS | gin RPS | pulse/gin | pulse p99 | gin p99 |
|---|---|---|---|---|---|---|
| `bare` | 64 | 58562 | 61790 | **0.94x** | 4.63ms | 4.71ms |
| `bare` | 256 | 64986 | 67915 | **0.97x** | 16.80ms | 20.07ms |

- **The bare pairing is on par**: 0.94–0.97×, a 3–6% gap; at concurrency 256 the p99 is actually lower (16.80ms vs 20.07ms). The pairing is `gin.New()` ↔ `web.New(web.Minimal())` — neither side carries default middleware.
- **Absolute values are magnitude references only**: the same cell moved from 49k to 78k between sessions, so only the paired ratio within one round counts.
- At the time an observability pairing (`gin.Default()` ↔ `web.New()`) was also run, but it used the **old default sink** (then `SlogSink`, since replaced by `ConsoleSink`), so that pairing does not describe the current state and was not re-run — it stays in the design document as the record of *why* the default sink changed.

Reproduce:

```bash
git worktree add --detach ../pulse-web-gin-compare origin/bench/gin-compare
cd ../pulse-web-gin-compare/loadtest
go run ./cmd/compare -probe -passes 4 -d 8s -warmup 2s   # the run recorded in the docs
```

The comparison project is its own module: the core module has zero third-party dependencies, and **it does not make an exception to compare itself against gin** — gin appears only inside that module.

## How this page and the design document divide the work

This page gives **conclusions, protocols and reproduction commands**; the derivations, the historical rounds (including the pulse v0.2.0 → v0.2.1 upgrade comparison) and the full notes live in the "measured data" section of the [design document](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md). Numbers change in both places at once.
