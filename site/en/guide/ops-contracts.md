# Deployment and runtime contracts

This page has two halves: **how logs land** (the operations half — the framework only writes to stdout), and **the runtime sequence the framework promises** (the invariants you will encounter while writing handlers).

## Logs: the default writes to stdout only

**The default sink `NewConsoleSink(os.Stdout)` is not persistence**: the process writes lines to fd 1, and persistence, rotation and retention belong to the platform. That split is deliberate (12-factor): the application does not manage the filesystem, the platform does, and stdout is the seam between them.

| Path | How | Who owns rotation / retention |
|---|---|---|
| Containers / k8s | the default sink is enough | the runtime: docker `log-opts max-size/max-file`, kubelet `containerLogMaxSize/containerLogMaxFiles` — **defaults are often tiny or unbounded, set them explicitly** |
| systemd (bare metal) | the default sink is enough (stdout goes to journald) | `journald.conf`'s `SystemMaxUse` / `MaxRetentionSec`; read it with `journalctl -u <svc>` |
| Writing to a file directly | `os.OpenFile(…, O_APPEND\|O_CREATE\|O_WRONLY, 0o644)` + `WithSink(NewConsoleSink(f))` | **the host** (zero third-party dependencies means no built-in rotation) |
| Human-readable **and** a collector | upstream `observability.MultiSink{console, slog}` | same as above |

**The loss window depends on the buffering layer** (higher layers lose less and cost more):

1. `ConsoleSink` — **unbuffered**, written straight to the fd (you only lose whatever the kernel page cache has not flushed)
2. upstream `LineSink` — a 32 KiB buffer (the engine flushes it during graceful shutdown, see below)
3. upstream `AsyncSink` — a queue (capacity 1024 by default; full means back-pressure, or counted drops with `DropOnFull()`)

Stronger guarantees mean calling `fsync` yourself (a millisecond-scale cost per request, rarely worth it for an access log). "Not one record lost" is not something a log channel can promise.

**Write failures are not silent**: the sink's `Err()` returns the **first** write error (later failures or successes never overwrite it). The framework flushes once during shutdown and logs the first error at `slog.Warn`; to alert while the process is alive, read it from your health check:

```go
if err := sink.Err(); err != nil {
    // alert: the access log is no longer being written (first error, e.g. broken pipe)
}
```

**Explicitly not done here**: rotation / retention / compression (that is the host's or a third party's job), fsync policy, sampling drops, or pushing write errors into the request path.

## Runtime contracts

### The life of a request

```
request arrives
  → middleware chain (global → group → route)
  → handler returns nil or an error
  → request scope Dispose (recursive teardown)
  → the error mapper decides the status and writes the response
```

**`Dispose` happens before the response is written** (a design red line): scope teardown is decoupled from "did the response make it to the network", so an Effect failure never holds up the response and a failed write never skips teardown.

One direct consequence: **by the time the error mapper runs, the scope is gone**. All it can use is `Get` / `Set` / `Observe` / `TraceID()` / `Path` / `Query()` (the KV hangs off `Ctx` and outlives the scope); **`c.Service` / `c.Kernel()` are a dead scope at that moment**.

### The boundaries of `Ctx`

- **Never crosses goroutines**: it is bound to the request and its response writer. Background work uses `c.Detach()`, which yields a bag of values (`TraceID` / `HostID` / `Sink` / process-level `Root`) rather than a context — see [requests](/en/guide/requests).
- **The response writer promises `http.Flusher` only**: `Hijacker` / `Pusher` / `FlushError` / `SetWriteDeadline` are not surfaced. That keeps "what streaming can do" visible in the type system instead of depending on a lucky type assertion.
- **The status code locks at the first flush**: `c.Writer().Write`, `c.Flush()`, or the engine's wrap-up after the handler returns — whichever happens first.

### Error mapper contract

- The returned error determines the status code; **the `cause` reaches the observability record only, never the response body**.
- **Any plain error that is not an `HTTPError` becomes a 500** with a generic message (a safe default).
- `StatusCoder` (any error carrying `StatusCode() int`) supplies the status, and the message still goes through the default scrubbing.
- A panic becomes `PanicError` → **always 500** (the stack only reaches the record); if the response is already written it is recorded, not rewritten.
- A custom `ErrorHandler` returning `nil` means "handled"; returning an error continues through the default mapper.

### Graceful shutdown sequence

```
signal (SIGINT / SIGTERM)
  → ① srv.Shutdown(timeoutCtx)      ← draining happens here (in-flight requests finish)
  → ② timeout → srv.Close()         ← force connections shut
  → ③ OnShutdown callback           ← a single callback, for your own background work
  → ④ root.Dispose()                ← the endgame: cascade teardown, no waiting for in-flight work
  → ⑤ sink flush                    ← its own 3s budget, outside ShutdownTimeout
```

- Steps ①②③ **share one deadline** (total ≤ `ShutdownTimeout`, 30s by default, aligned with k8s `terminationGracePeriodSeconds`); if HTTP uses up the budget the callback sees an expired context and the framework records "background work not awaited".
- Step ④ is a **cascade teardown, not a drain**: **background tasks that registered no wait are cut off here** (stated behaviour). Wait for them in step ③.
- Step ⑤ **flushes without closing**: the sink belongs to whoever assembled it — closing it would silently drop records that `Detach`ed background tasks are still writing. It recognises both sink shapes (`Flush(context.Context) error` and `Flush() error`), and logs errors at `slog.Warn`.
- Total shutdown ceiling = `ShutdownTimeout` + 3s.

::: tip The full contract lives in the design document
This page is the **summary**: complete derivations, edges and measurements are in the [design document](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md), sections "runtime contracts" and "observability design".
:::
