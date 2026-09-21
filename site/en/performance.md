# Performance

This page answers two questions: **what does a request path cost**, and **where does the observability money go**. Every table comes with a command to reproduce it.

**Version**: pulse-web's `main` (no new tag since v0.1.0; depends on pulse v0.2.4). **This section is re-measured whenever the default sink or the request path changes, and at tag time** (the next round lands with v0.2.0) — and only paired ratios from the same round are compared.

**This round**: 2026-09-20 · i9-14900HX / 32 logical cores / Go 1.27 / Windows amd64 · **on AC power**.

## How to read this page

The numbers fall into two classes, and they are read differently:

- **The deterministic class**: `allocs/op` and `B/op`. They are frozen as CI assertions (`bench/budget_test.go`), so they hold **across runs and across machines**.
- **The floating class**: `ns/op`. The same cell drifts 2–4× between sessions, so it is **only comparable within one round** — never to another round or another machine.

So that "one round" is identifiable, every table below carries a **control row** that has nothing to do with the framework (stdlib ServeMux, pure standard library). When the control moves, the machine state moved, so **an ns figure without a same-round control row should not be quoted**.

For absolute values, run the commands below on a quiet machine and compare against the control row from **the same round**.

> This machine has one harder limit on top: `time.Now()` advances in quanta of about **0.3 ms** (2 million consecutive samples contained only 34 distinct non-zero deltas), so any interval shorter than that is read as zero or as a quantised value. ns here is not even precise to the microsecond. Method and raw data are in the measured-data section of the [design document](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md).

## Cost breakdown of a request path

Protocol: `-benchtime=20000x -count=5`, median, one round, one protocol. **Any Δ you subtract must use two rows of this table.**

> Every `Engine` row's B/op includes **16 bytes** for the `Ctx` field holding the span identity (2026-09-17 · [#76](https://github.com/Luo-root/pulse-web/issues/76)): `Ctx` is fixed-size, so a host that never installs a span hook still pays it. **Alloc counts are unchanged in every row.** The ns column is still the round from before that field existed (ns is not gated and drifts across rounds; alloc counts and B/op are the deterministic criteria).

| Scenario | ns/op | allocs | B/op |
|---|---|---|---|
| stdlib ServeMux route match (**control**, framework-independent) | 198 | 5 | 224 |
| scope derive + dispose (`Derive + Dispose` baseline) | 81 | **2** | 192 |
| + bare scope-local binding | 327 | 14 | 649 |
| + `observability.AttachCollector` | 342 | 14 | 681 |
| same · 10 / 50 / 100 plugin tree | 345 / 361 / 370 | 14 | 681 |
| same · 50 plugins, parallel | 566 | 14 | 681 |
| `Engine` request path (`New()`, 0 / 10 / 50 plugins) | 1774 / 1819 / 1937 | 22 | 6314 |
| `Engine` request path + `WithCollector()` | 2123 | **34** | 6803 |
| `Engine` request path (`Minimal()`) | 1443 | 17 | 5858 |
| `Engine` request path + `BodyLimit` route | 1877 | **23** | 6379 |
| `Engine` request path + `c.JSON` | see below | **25** | 6453 |

Δ derived from two rows of this table (same round, so subtracting is valid):

- **Per-request cost of `WithCollector()`** = 2123 − 1774 = **+349 ns / +12 allocs** (end to end)
- **What `Minimal()` saves** = 1774 − 1443 = **−331 ns / −5 allocs** (Trace / AccessLog / Sink switched off)
- **Per-request cost of `BodyLimit`** = 1877 − 1774 = **+103 ns / +1 alloc** — the extra allocation is the wrapper `http.MaxBytesReader` returns (bound to the request, not reusable); that 103 ns still sits inside the noise (the five runs of that cell landed between 1839 and 2177).
- **Assembly size does not reach the request path**: the `Engine` request path at 0 / 10 / 50 plugins is 1774 / 1819 / 1937 ns with **allocs constant at 22 and B/op constant at 6314**; at the kernel level the 10 / 50 / 100 plugin trees hold **allocs constant at 14**. Plugin-tree size does not change per-request cost — that is the premise the assembly story rests on.
- **Containment self-check (against inverted conclusions)**: `AttachCollector` = bare binding + one Collector struct, so the criterion is **B/op** (681 > 649). Both report 14 allocs; their ns happen to agree in direction this round (342 > 327) but are only 4% apart, inside the noise band — **do not draw this conclusion from ns**.
- **Encoding to a buffer in `c.JSON`**: **+2 allocs** per response (22 → 25, a deterministic criterion covered by the allocation gate), bought "an encoding failure no longer sends an empty 200". The B/op figure of 6453 comes from the **same-round probe** in the design document (6341 → 6453, i.e. +112 B) — it is *not* from the same round as this table's `Engine` row of 6314, so **do not subtract them**. For the time, run the control in the same round.

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

The sink is where the observability cost sits (the mechanism is in [observability](/en/guide/observability)) — **a slow sink is a slow request path**.

Same `Record`, every sink writing to `io.Discard` (keeps formatting and locking, excludes terminal/disk I/O):

| Sink | ns/op | allocs | B/op |
|---|---|---|---|
| `web.ConsoleSink` (**the default**) | 235 | **0** | **0** |
| `observability.LineSink` (upstream line sink) | 274 | **0** | **0** |
| `observability.SlogSink` (structured) | 1398 | **18** | 1127 |

End to end (one request path, only the sink changes): default sink **2471 ns** vs nop sink **2240 ns** = **+0.23 µs and +0 allocs per request**. That cell is noisy (the nop side landed between 2131 and 2920 over five runs), so it was run a second time independently: 2628 vs 2433 = **+0.20 µs**, same direction.

Two things to read out of it: the default sink allocates **nothing** (rendering happens inside the line sink's critical section, on a single buffer); and `SlogSink` costs 18 allocations and 5× the time per record — it is the **machine-readable** path, and that is both its cost and its purpose.

Reproduce:

```bash
go test -run '^$' -bench 'BenchmarkSinkWrite_ConsoleVsUpstream|BenchmarkRequestPath_DefaultSink' -benchmem -count=5 ./bench/
```

For choosing a sink see [observability](/en/guide/observability); when throughput matters, wrap the slow sink in `observability.NewAsyncSink`.

## Real load against gin (2026-09-20, on AC power)

This section answers "**is it on par with gin**". The measurement code is `loadtest/` **in this repository** — a separate module carrying the gin dependency (the core module keeps zero third-party dependencies and makes no exception just to be compared against gin). Only the protocol and conclusions are kept here.

Protocol: two separate processes sharing one `http.ListenAndServe` bootstrap with **only the handler varying**; within a cell the two sides run adjacent and swap order on odd/even rounds, with **only even rounds counting**; each cell takes the whole set of percentiles from the round whose RPS is the median, and the ratio takes the **median of the per-round paired ratios**. The load generator is hand-written (fixed concurrency, full connection reuse, every request inside the timing window entering the percentiles, no sampling). Machine: i9-14900HX / 32 logical cores / `GOMAXPROCS=32` / Go 1.27 / Windows amd64, **on AC power**. Both sides point their log sink at a null device — the per-request formatting cost is kept, terminal and pipe I/O are excluded.

| Pairing | Concurrency | pulse-web RPS | gin RPS | pulse/gin | pulse p99 | gin p99 |
|---|---|---|---|---|---|---|
| `bare` | 64 | 27494 | 31043 | **0.89x** | 11.32ms | 11.60ms |
| `bare` | 256 | 36036 | 37951 | **0.95x** | 31.18ms | 37.99ms |
| `obs` | 64 | 25332 | 29616 | **0.85x** | 11.49ms | 11.63ms |
| `obs` | 256 | 33437 | 36882 | **0.91x** | 32.16ms | 39.94ms |

- **The bare pairing is on par**: 0.89–0.95×; the p99 is lower on both concurrency levels (11.32ms vs 11.60ms, 31.18ms vs 37.99ms). The pairing is `gin.New()` ↔ `web.New(web.Minimal())` — neither side carries default middleware.
- **The observability pairing** (`gin.Default()` ↔ `web.New()`, both sides with their default middleware): 0.85× / 0.91×. On this pairing pulse-web additionally does three things gin does not — a TraceID, the route template (`/users/{id}` rather than the concrete path), and error classification; for reference, gin's own default middleware costs 4.6% / 2.8% between these two pairings.
- **What carries the protocol is the pairing, not the absolute values**: the same cell moved from 28k to 78k between sessions (this round's absolute RPS is about half of the previous round's — machine state, nothing else), so only the paired ratio within one round counts (whole-machine drift such as the power state cancels out). **The two earlier independent sessions agree item by item** (0.90/0.91, 0.95/0.95, 0.81/0.82, 0.92/0.93) — that is what makes this more trustworthy than the absolute values.
- The round before that (2026-09-14) had the observability pairing at **0.65× / 0.88×** — that round used the **then-current** default sink `SlogSink`. With the default now `ConsoleSink` and the same protocol re-run, you get 0.82× / 0.93×. Absolute values are not comparable across rounds; only the paired ratio from the same round is.
- This round (2026-09-20) was re-run twice, and the later run is the one recorded: first because the response writer **opened up `Hijack`** (a change on the request path; 0.90× / 0.94×, 0.87× / 0.92×), then a re-check after the review fix to the post-hijack write protection (0.89× / 0.95×, 0.85× / 0.91×) — both land in the same noise band, and **the conclusion is unchanged**. On 2026-09-21 the `Adapt` refill fix (also on the request path) triggered one more re-run (0.90× / 0.93×, 0.86× / 0.93×), again inside that band.
- The measurement code used to live on a side branch, so when the default sink changed it **kept silently measuring the old default** — no error, no warning. It now follows the main branch, and CI covers its build / vet / test separately so it cannot rot unnoticed.

Reproduce:

```bash
cd loadtest
go run ./cmd/compare -probe -passes 4 -d 8s -warmup 2s           # the run recorded in the docs
```

`loadtest`'s `replace` points at the repository root, so it measures the **current working copy**; CI only runs its build / vet / test and **never the load run or the micro-benchmarks** — timings on CI are noise.

## How this page and the design document divide the work

This page gives **conclusions, protocols and reproduction commands**; the derivations, the historical rounds (including the pulse v0.2.0 → v0.2.1 upgrade comparison) and the full notes live in the "measured data" section of the [design document](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md). Numbers change in both places at once.
