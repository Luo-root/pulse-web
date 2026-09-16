# Observability

The point of this stack is not that you *can* attach a sink — it is that **a sink is already attached**. Once `web.New()` is serving, every request gets an access line, a 32-hex TraceID, and a request scope that is released on time. No configuration.

## The default sink: a console line for humans

`web.New()` installs `web.NewConsoleSink(os.Stdout)`: it renders each `Record` as one column-aligned line, fixed widths, unbuffered (written straight through).

```text
PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378
```

The trailing `route=` / `size=` / `host=` / error / `trace=` segments are independent fields that appear only when present. The status column is coloured by range when the destination is a terminal, and turns itself off when output is redirected to a file or pipe (`NewConsoleSink(w, WithColor(true))` forces it back on).

## Replacing the sink: pick by who reads the output

`WithSink` swaps the sink and **changes nothing else in the assembly**.

```go
web.WithSink(web.NewConsoleSink(os.Stdout))                     // humans — the default: unbuffered, writes straight through
web.WithSink(observability.SlogSink{Logger: slog.Default()})    // machines — host logger, JSON, log collectors (stderr by default)
web.WithSink(observability.NewAsyncSink(inner))                 // throughput — the write leaves the request path
web.WithSink(observability.MultiSink{a, b})                     // several destinations at once
```

| Sink | Reach for it when | What to know |
|---|---|---|
| `web.ConsoleSink` (default) | You want to read it in a terminal or `kubectl logs` | Unbuffered; write failures never panic, but `Err()` reports the first one |
| `observability.SlogSink` | You already have a logging pipeline, want JSON, feed a collector | Writes to **stderr** by default; takes on your logger's format and levels |
| `observability.LineSink` | You want the upstream default layout with a 32 KiB buffer | The engine flushes it for you during graceful shutdown |
| `observability.NewAsyncSink(inner)` | The sink is your bottleneck (file or network exporter) | Bounded queue, lifecycle is yours — see below |
| `observability.MultiSink` | Human and machine destinations at once | Fan-out; nil members are skipped |

::: warning `SlogSink` is not the default
It is the **machine-readable** sink. The default is `ConsoleSink` — `0.585` and `585.1µs` are the same duration, and the first thing you want at boot is the second one.
:::

## The sink is where your throughput goes: when to wrap `AsyncSink`

Almost all of the observation cost sits in getting the record out of the door, and the kernel dispatches events **synchronously** (`Emit` / `EmitLocal` / `Waterfall`; even `Parallel` waits for completion). **A slow sink is a slow request path.**

`NewAsyncSink(inner)` takes a slow sink off the caller's goroutine: `Write` only deep-copies `Attrs` and enqueues, while a single background worker calls `inner.Write` in FIFO order.

```go
app := web.New(
	web.WithHostID("orders-api"),
	web.WithSink(observability.NewAsyncSink(
		observability.SlogSink{Logger: slog.Default()},
	)),
)
```

What you pay is **ownership**, and both halves need handling:

1. **The queue is bounded** (1024 by default). When full it **back-pressures** by default — no records are dropped, the producer absorbs the cost. `observability.DropOnFull()` switches to dropping the *new* record and counting it in `Dropped()`.
2. **The lifecycle is yours.** An async sink changes how "no residues after the tree is disposed" is achieved: you must `Flush(ctx)` or `Close(ctx)` before shutdown, or the queued records leave with the process. The engine's shutdown sequence flushes what has already been produced, but it does not `Close` for you — if you hand ownership to the framework, background tasks you detached get silently truncated.

::: tip Where the numbers are
The measurements behind "how much does the sink cost" (real-load comparison, micro-benchmarks, allocation counts) are on the [performance page](/en/performance) — the public version of the three tables, with reproduction commands and protocols; the full protocols and derivations live in the design document's measured-data section.
:::

## What every request gets

Whatever sink you use, the `New()` assembly gives every request:

- A 32-hex **TraceID** (`c.TraceID()`; inbound W3C `traceparent` / B3 headers can be adopted, see `WithTrustedTraceHeader`)
- One **`http.request` record**: status, duration, route pattern, response size, host, trace
- A **request scope** released the moment the handler returns — before the engine maps the outcome and writes anything further
- Panic recovery: a panic in a handler becomes a 500, and the panic value and stack stay in the process (attached to the record, never sent to the client)

## Your own events: `c.Observe`

```go
app.POST("/orders", func(c *web.Ctx) error {
	var in Order
	if err := c.Bind(&in); err != nil {
		return err
	}
	c.Observe("order.paid", func(a *observability.Attrs) {
		a.Set("order_id", in.ID)
		a.Set("amount_cents", in.AmountCents)
	})
	return c.JSON(http.StatusCreated, in)
})
```

Your events and the framework's land in the **same sink**, so swapping the sink moves both.

## Switches and diagnostics

```go
app := web.New(
	web.WithHostID("orders-api"),   // the host= field in the access line
	web.WithoutAccessLog(),         // when you already log elsewhere: turns off the access record only
	web.WithCollector(),            // attach the kernel collector to the request scope for per-request fiber visibility
)

app.Debug("/debug/assembly")        // a JSON view of the assembly: snapshots, fibers, plugins
```

`WithoutAccessLog()` leaves trace and panic recovery alone. Note that **panic records stop too**: the access record is their only path out.

::: warning The lifecycle of `WithRoot`
`web.WithRoot(root)` attaches an existing kernel tree; `Run` / `Serve` dispose it **cascading** on return, so do not keep using it afterwards.
:::

## The edges of a record

A `Record` holds scalars only (`~string | ~int64 | ~float64 | ~bool`) and has **no `map[string]any` escape hatch** — so request bodies, headers and query strings cannot reach your logs by construction, rather than by remembering not to.

No field is missing from the `Record`; only the rendering layer is. Switch to `SlogSink` when you need machine output — you do not need a second set of instrumentation.

## Persistence is not the framework's job

The default sink writes to stdout and is **not a persistence layer**. Rotation and retention belong to your platform — the container runtime, systemd / journald, or a writer you pass in via `WithSink`. The deployment side of that, including which layer decides what gets dropped, is covered in [operations & runtime contracts](/en/guide/ops-contracts).
