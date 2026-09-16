# Assembly and running

`web.New()` creates an `Engine`; every knob is a **functional option** (`web.Option`) — no config struct, no string switches. This page follows the order you actually meet them: start a server, adjust the assembly, run it, shut it down.

## Starting a server

```go
app := web.New()                      // builds its own kernel root and installs Bootstrap
err := app.Run(":8080")               // blocks until shutdown completes; nil = clean signal shutdown
```

Three ways to assemble:

| Form | When to use it |
|---|---|
| `web.New()` | the default: own root, observability installed, default sink writing to stdout |
| `web.New(web.WithRoot(k))` | **share one kernel tree** with a host application or another component |
| `web.New(web.Minimal())` | routing and middleware only — **no** Bootstrap, no default sink (this is the bare pairing used in load comparisons) |

```go
app := web.New(web.Minimal())          // an engine with observability switched off
```

::: warning `Minimal()` and `WithCollector()` cannot be combined alone
`WithCollector()` binds a per-request Collector to the scope, and that needs a sink to write into. **`Minimal()` + `WithCollector()` without `WithSink` panics at assembly time** — an assembly error should not wait for the first request to panic.
:::

## The assembly surface: `Root()`

```go
root := app.Root()                     // the process-level kernel root
kernel.Provide(root, dbKey, sqlDB)     // everything else uses kernel's own API: Provide / Use / Loader / FiberSnapshots
kernel.Use(root, myPlugin)
```

**`Root()` is the pivot of the assembly story**: any component that needs a process-level kernel (your own plugins, third-party integrations, anything that outlives a request) has to take it from here. **Do not use `c.Kernel()` as a process-level kernel** — that is the request scope, and plugins or Effects attached there are destroyed with the request.

The framework **does not wrap** the rest of kernel's assembly API: `Provide` / `Use` / `Loader.Reconcile` / `FiberSnapshots` are used as they come.

## Options

| Option | Effect |
|---|---|
| `WithRoot(k)` | adopt an existing kernel root (default: build one) |
| `WithHostID(id)` | value written into the `host` field of every record |
| `WithSink(sink)` | replace the observability sink (default `NewConsoleSink(os.Stdout)`) |
| `WithCollector()` | attach a Collector per request (**+12 allocs**, see [performance](/en/performance)) |
| `WithTrustedTraceHeader(false)` | **defaults to `true`**: accept inbound `traceparent` / B3 headers |
| `WithoutAccessLog()` | turn the access log off (tracing and panic recovery stay) |
| `WithServer(ServerConfig)` | override HTTP server parameters (below) |
| `WithErrorHandler(mapper)` | replace the error mapper (see [the error model](/en/guide/errors)) |
| `WithTemplates(TemplateConfig)` | install templates (see [responses](/en/guide/responses)) |
| `WithMaxBodyBytes(n)` | global request body limit (`0` = unlimited) |

To colour the default output or point it at another writer:

```go
app := web.New(web.WithSink(web.NewConsoleSink(os.Stdout, web.WithColor(false))))
```

`NewConsoleSink(w io.Writer, opts ...web.ConsoleOption) *web.ConsoleSink` — `WithColor(bool)` is one such `ConsoleOption`. For choosing a sink see [observability](/en/guide/observability).

## `ServerConfig`

The zero value is the default; to change one field, start from `web.DefaultServerConfig()`:

```go
cfg := web.DefaultServerConfig()
cfg.WriteTimeout = 30 * time.Second    // set this explicitly for non-streaming services; the default 0 keeps SSE alive
app := web.New(web.WithServer(cfg))
```

| Field | Default | Notes |
|---|---|---|
| `ReadHeaderTimeout` | 5s | slow-header protection |
| `ReadTimeout` | 30s | whole-request read budget |
| `WriteTimeout` | **0** | **deliberately unlimited** — anything non-zero cuts SSE |
| `IdleTimeout` | 120s | keep-alive idle budget |
| `MaxHeaderBytes` | 1 MiB | request header limit |
| `ShutdownTimeout` | 30s | graceful shutdown budget (not counting the sink flush, below) |

## Running: `Run` / `Serve` / `Handler`

```go
app.Run(":8080")                       // built-in http.Server plus signal handling
app.Serve(ln)                          // bring your own listener (TLS via tls.NewListener, or terminate at a proxy)
app.Handler()                          // export an http.Handler view; you can also use app itself
```

- `Run` **blocks**; `nil` means a clean signal shutdown, anything non-nil means a startup failure or a shutdown-time error. Both paths complete `root.Dispose()` — **the caller must not dispose again**.
- **After `Run` returns the engine is not reusable** (its kernel is gone).
- There is no `RunTLS`: do TLS with `tls.NewListener` + `Serve`, or terminate it at a reverse proxy.
- `ServeHTTP` / `Handler()` let you drop the engine anywhere an `http.Handler` fits — `httptest`, another mux, a serverless adapter.

## Shutdown: `OnShutdown`

```go
app.OnShutdown(func(ctx context.Context) error {
    return backgroundJobs.Wait(ctx)     // a single callback, for waiting on your background work
})
```

The shutdown sequence (both `Run` and `Serve` walk it):

1. `srv.Shutdown(timeoutCtx)` — **draining happens here** (in-flight requests finish)
2. on timeout → `srv.Close()` forces connections shut
3. the `OnShutdown` callback — your turn to wait for background work
4. `root.Dispose()` — the endgame: cascade teardown (**in-flight work is not awaited**; background tasks that registered no wait are cut off here)
5. **sink flush** — its own 3s budget, **not counted in `ShutdownTimeout`**

The total shutdown ceiling is `ShutdownTimeout` + 3s; how the deadline is split, what the flush budget covers and who owns the sink are specified in [operations & runtime contracts](/en/guide/ops-contracts).

## Assembly diagnostics: `Debug`

```go
app.Debug("/debug/kernel")             // mount a read-only JSON endpoint
```

`func (e *Engine) Debug(path string)` renders `kernel.FiberSnapshots()` as JSON — the tool for "what actually got assembled, and which Effect sits at which layer". It is a **current view only**; loader history is already in the sink, so read the logs for that. **Not mounted by default; evaluate authentication before exposing it** (it contains plugin names and failure reasons).
