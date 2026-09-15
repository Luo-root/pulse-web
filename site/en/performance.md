# Performance

**Read the ground rules first, then the numbers.** Performance comparisons turn into "whoever's number looks best wins", so the three rules come first.

## Three ground rules

1. **Nanoseconds do not compare across runs.** The same cell can move from 49k to 78k RPS (±20%) in a different session, and micro-benchmarks drift 2–4× between runs. **Any Δ you subtract must come from two rows of the same table** — we got this wrong once: subtracting 325 ns (one run) from 227 ns (another run) produced "the second one is cheaper", while the second one *is* the first plus a Collector struct. Logically impossible.
2. **The deterministic criterion is allocation count, not elapsed time.** The `allocs/op` and `B/op` columns of table B are frozen as CI assertions (`bench/budget_test.go`, run by the CI allocation-budget step); **ns gets no threshold** — gating on ns would import machine state into CI.
3. **Absolute values are magnitude references only; what counts is the paired ratio within one round.** The three tables use different protocols, so **never subtract across tables**.

## Table A: pulse v0.2.0 → v0.2.1 (historical)

This table answers "**why the cost of a request path does not grow with assembly size**" — it is the original evidence behind the project's claim that kernel cost is decoupled from plugin-tree size.

Protocol: `-benchtime=3000x -count=3`, median of the run, **compared across two runs**. The control row (stdlib routing, untouched by any of the changes) itself moved +9%, which means the machine was slower in the second run — so the ns improvements are conservative estimates; the **run-stable hard evidence is the allocation count**.

| Scenario | v0.2.0 | v0.2.1 | Δ |
|---|---|---|---|
| stdlib ServeMux route match (**control**) | 173.6 ns / 5 allocs | 188.8 ns / 5 allocs | +9% |
| scope derive + dispose | 119.3 ns / 5 allocs | 74.4 ns / **2** allocs | allocs −60% |
| request-scoped event `EmitLocal` | 27.5 ns / 2 allocs | 23.2 ns / **1** alloc | allocs −50% |
| full-tree event `Emit` (50 plugins) | 2079 ns / 60 allocs | 1778 ns / **9** allocs | allocs −85% |
| kernel service read `Get` | 11.83 ns | 13.13 ns | **+11%** (`Get` now walks the local binding chain) |
| per-request `AttachCollector` (empty tree) | 294.8 ns / 15 allocs | 301.8 ns / 14 allocs | flat |
| same (10 / 50 / 100 plugin tree) | 778 / 2647 / 4946 ns | **314 / 350 / 394 ns** | **−60% / −87% / −92%** |
| same (50 plugins, parallel) | 1371 ns / 15 allocs | 531 ns / 14 allocs | −61% |
| Engine request path `New()` | 2008 ns / 26 allocs | 1961 ns / 22 allocs | allocs −15% |
| Engine request path `Minimal()` | 1382 ns / 20 allocs | 1476 ns / 17 allocs | allocs −15% |

**Conclusion**: v0.2.1 replaced full-tree service-change broadcast with delivery indexed by dependency name, so the cost of a per-request Provide is **fully decoupled from plugin-tree size** (100 plugins: 4946 → 394 ns). Request-scoped data moved to scope-local bindings (no global repository write, no change delivery, removed together with the scope) instead of using a global Provide as a per-request container.

::: warning These absolute values do not compare with table B
Different protocols: the same benchmark differs by 10–20%. This table answers one question only — what the upgrade bought.
:::

Reproduce by running both versions:

```bash
git worktree add --detach ../pulse-web-v020 aca2ea2   # go.mod pins pulse v0.2.0
git worktree add --detach ../pulse-web-v021 9b5e1bb   # go.mod pins pulse v0.2.1
cd ../pulse-web-v021
go test -run '^$' -bench 'BenchmarkStdlibServeMux_Route|BenchmarkScopeCycle_EmptyHost$|BenchmarkEmitLocal|BenchmarkEmitFullTree|BenchmarkServiceGet|BenchmarkRequestCycle_Collector' -benchtime=3000x -count=3 ./bench/
```

At `v0.2.0` the bench package had no Engine request-path benchmarks yet (they arrived with v0.2.1), so the last two rows were measured with **the same probe, changing only the pulse version in `go.mod`**.

## Table B: cost breakdown of the current version (same round, same protocol)

Protocol: `-benchtime=20000x -count=5`. **Any Δ you subtract must use two rows of this table.**

| Scenario | ns/op | allocs | B/op |
|---|---|---|---|
| `Derive + Dispose` (baseline) | 84.3 | **2** | 192 |
| + bare scope-local binding | 337.2 | 14 | 649 |
| + `observability.AttachCollector` | 353.9 | 14 | 681 |
| same · 10 / 50 / 100 plugin tree | 338 / 366 / 407 | 14 | 681 |
| same · 50 plugins, parallel | 565 | 14 | 681 |
| Engine request path (`New()`) | 1790 | 22 | 6314 |
| Engine request path + `WithCollector()` | 2175 | **34** | 6803 |
| Engine request path + `c.JSON` (JSON response) | 2137 | **25** | 6438 |
| Engine request path + `BodyLimit` route (allocation column only, see below) | — | **23** | 6362 |

Δ derived from two rows of this table:

- **Per-request cost of `WithCollector()`** = 2175 − 1790 = **+385 ns / +12 allocs** (end to end)
- **kernel-level `AttachCollector` over baseline** = 353.9 − 84.3 = **+270 ns / +12 allocs**
- **Containment self-check (against inverted conclusions)**: `AttachCollector` = bare binding + one Collector struct, so both ns and B/op must be strictly larger (353.9 > 337.2, 681 B > 649 B). Both rows report 14 allocs — **a single alloc value cannot tell these two apart**; containment needs B/op and ns.
- **Decoupled from plugin-tree size**: 10 / 50 / 100 plugins at 338 / 366 / 407 ns, allocs constant at 14.
- **What encoding to a buffer in `c.JSON` costs**: **+2 allocs / +97 B per response**, bought "an encoding failure no longer sends an empty 200".
- **A route with `BodyLimit` costs +1 alloc**: the wrapper `http.MaxBytesReader` returns (bound to the request, not reusable). **This row lists the allocation column only** — its ns was not measured in the same round as this table, and leaving it empty beats pasting a cross-run number.

Reproduce:

```bash
go test -run '^$' -bench 'BenchmarkScopeCycle|BenchmarkRequestCycle' -benchtime=20000x -count=5 ./bench/
go test -run '^$' -bench 'BenchmarkEngineRequestPath' -benchtime=20000x -count=5 ./bench/
go test -run TestRequestPathAllocBudget ./bench/     # allocation gate — must NOT run with -race
```

The allocation gate covers every row of table B, including `default+json` and `default+body-limit`. **A gate that stays green only proves that the paths it covers are unchanged**, so a new path has to land in both the table and the gate.

## Table C: real load against gin (a one-off verification)

This table answers "**is it on par with gin, and where does the observability money go**". The measurement code **never entered the repository** — `loadtest/` is a separate module carrying the gin dependency, kept on branch `bench/gin-compare`; this page keeps only its protocol and conclusions.

Protocol first:

- Two **separate processes** sharing one `http.ListenAndServe` bootstrap, **only the handler varies** (no `gin.Run()` / `Engine.Run()`, which would drag each framework's default server configuration into the comparison)
- Within a cell the two sides run **adjacent**, swapping order on odd/even rounds, and **only even rounds count** (running first carries a systematic advantage); each cell takes the **whole set of percentiles from the round whose RPS is the median**; the ratio takes the **median of the per-round paired ratios**
- Path `GET /users/42` → the same JSON on both sides; the load generator is hand-written (fixed concurrency, full connection reuse, **every** request inside the timing window enters the percentiles, no sampling)
- Machine: i9-14900HX / 32 logical cores / `GOMAXPROCS=32` / Go 1.27 / Windows amd64

Two pairings: `bare` = `gin.New()` ↔ `web.New(web.Minimal())`; `obs` = `gin.Default()` ↔ `web.New()`.

| Pairing | Concurrency | pulse-web RPS | gin RPS | pulse/gin | pulse p99 | gin p99 |
|---|---|---|---|---|---|---|
| bare | 64 | 58562 | 61790 | **0.94x** | 4.63ms | 4.71ms |
| bare | 256 | 64986 | 67915 | **0.97x** | 16.80ms | 20.07ms |
| obs | 64 | 38020 | 59127 | **0.65x** | 7.10ms | 4.98ms |
| obs | 256 | 57152 | 65874 | 0.88x | 20.57ms | 21.14ms |

- **The bare pairing is on par**: 0.94–0.97×, a 3–6% gap; at concurrency 256 the p99 is actually lower (16.80ms vs 20.07ms).
- **Absolute values are magnitude references only** — the same cell moved from 49k to 78k between sessions, so only the paired ratio within one round counts.

::: warning The `obs` pairing reflects the old default
It used the **old default sink `SlogSink`** at the time. That finding — the default observability pairing lagging behind — is exactly what led to switching the default sink to `ConsoleSink`, **so these numbers describe the old default, not the current one**; the pairing was not re-run after the switch, and the current magnitude lives in the micro-benchmarks below.
:::

**Where the observability money goes** (diagnostic pairings, pulse-web side only — they answer "where does the money go", not "who is faster"):

| Concurrency | bare | access log off | default `SlogSink` | `LineSink` | `AsyncSink(LineSink)` | prototype `fastsink` |
|---|---|---|---|---|---|---|
| 64 | 58562 | 54930 (−6%) | **38020 (−35%)** | 48583 (−17%) | 54727 (−7%) | 50621 (−14%) |
| 256 | 64986 | 63739 (−2%) | **57152 (−12%)** | 60711 (−7%) | 62588 (−4%) | 62309 (−4%) |

- **Generating the TraceID and assembling the record is nearly free**: turning the access log off costs 6% (c=64) / 2% (c=256).
- **Almost all of the money goes into getting the record out of the door**, and the sink changes everything: the default `SlogSink` costs 35%/12%, `LineSink` 17%/7%, `AsyncSink(LineSink)` only 7%/4%. On the gin side the same pairing costs 3% (its `Logger` is one line of text; there is no second option).
- **Rendering itself is an order of magnitude cheaper**: the prototype `fastsink` costs 14%/4% — the same fields, taking the default pairing from −35% down to −14%. The default sink `ConsoleSink` has since cashed that in (rendering at roughly 190 ns / 0 allocs).

Reproduce:

```bash
git worktree add --detach ../pulse-web-gin-compare origin/bench/gin-compare
cd ../pulse-web-gin-compare/loadtest
go run ./cmd/compare -probe -passes 4 -d 8s -warmup 2s   # the run recorded in the docs
go test -bench . -benchmem ./bench/                      # cost breakdown (not a verdict)
```

The comparison project is its own module: the core module has zero third-party dependencies, and **it does not make an exception to compare itself against gin** — gin appears only inside that module.

## How this page and the design document divide the work

This page is the public version of the three tables; the source of truth for **protocols, derivations and full notes** is the "measured data" section of the [design document](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md). Numbers change in both places at once.
