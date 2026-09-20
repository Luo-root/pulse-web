# stdlib middleware

Ecosystem middleware is almost always shaped `func(http.Handler) http.Handler`: chi/middleware, rs/cors, promhttp, httprate, gorilla/csrf, and so on. This page covers **how to wire them into this framework**, and where the limits of each route are.

For writing your own middleware (`func(*Ctx, Handler) error`) and attaching it, see [Routing and middleware](/en/guide/routing).

## Two routes, and they are not equivalent

`web.Adapt` turns stdlib middleware into framework middleware. Wrapping the whole thing around `app.Handler()` is the other route. **This is not a style choice** — the capabilities differ:

| | Hand-rolled adapter (a dozen lines of public API) | Wrap outside `Handler()` | `Adapt` (inside the onion) |
|---|---|---|---|
| Middleware wraps the writer to read the status | ❌ always 0 | ✅ | ✅ |
| Middleware rewrites the body (gzip) | ❌ corrupt response | ✅ | ✅ |
| Middleware short-circuits | ❌ access log says "no response written" | ⚠️ framework never learns (no log, no span) | ✅ recorded back |
| Middleware replaces the request | ❌ handler never sees it | ✅ | ✅ |
| Read the route template `r.Pattern` | ❌ | ❌ routing hasn't run yet | ✅ |
| Intercept preflight / run before routing | ❌ | ✅ | ❌ |

The first column is **silent corruption** — the worst kind: it looks like it works, while counters report `code="0"` and gzip responses make clients fail with `gzip: invalid header`.

## Usage

```go
app.Use(web.Adapt(func(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("X-Request-Id", rid)
        next.ServeHTTP(w, r)
    })
}))
```

`Adapt` returns an ordinary `web.Middleware`, so all three attachment points accept it: global `Use`, a `Group`, or a single route.

::: warning `Use` only affects routes registered after it
Call `Use` before registering routes. Do it the other way round and the already-registered routes are simply not on the chain — no error, just silently absent.
:::

## What it promises

1. **When middleware wraps the ResponseWriter, it sees the real status code and byte count** (not 0) — promhttp counters and chi's Logger work as-is.
2. **When middleware short-circuits (never calls next), the status and byte count it wrote are recorded back into the framework's collection layer** — the access log and traces show the real response, not "nothing was written".
3. **A request replaced by middleware is passed along** — after `r = r.WithContext(...)`, what the handler reads via `c.Request()` is the replaced one.

Plus: the writer handed to middleware supports `http.Flusher` and `http.Hijacker` — SSE still flushes per event, and upgrade routes behind `Use(Adapt(…))` still get their connection. And it **never overclaims**: if the underlying writer cannot flush, `c.Flush()` still returns an explicit error; if it cannot hijack, `Hijack()` returns `http.ErrNotSupported`.

## What it does not do

1. **It does not move routing.** Route matching still happens before middleware, so a preflight `OPTIONS` never reaches it — register only `GET /api` and ServeMux answers 405 (`Allow: GET, HEAD`) directly. **CORS that must intercept preflight has to be wrapped outside**, or you register an explicit `OPTIONS` route.
2. **It does not solve "finalizing middleware × error/panic".** The framework's error mapping happens **after** middleware returns, so middleware that unconditionally writes once next returns (e.g. gzip with its own `defer zw.Close()`) locks the status at 200. **Compression is pushed outside by default.**
3. **It does not expose `http.Pusher` or `FlushError`.** The response writer's capability surface is an explicit short list (`Flush` / `Hijack` / `SetWriteDeadline` / `EnableFullDuplex`); HTTP/2 server push and flush-error reporting are not on it.
4. **It does not guarantee "same shape means it works".** Middleware that depends on a particular router context stays unusable — chi's `CleanPath` reads `chi.RouteContext` and **panics either way**, adapted or wrapped outside.
5. **It does not change middleware semantics.** Who recovers a panic, and what an error body looks like, is still the middleware's call.

## How middleware hands values to a handler

Through the **request context** — the standard library's own channel. The framework adds no extra accessor:

```go
type userKey struct{}

app.Use(web.Adapt(func(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx := context.WithValue(r.Context(), userKey{}, principal)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}))

app.GET("/me", func(c *web.Ctx) error {
    p, _ := c.Request().Context().Value(userKey{}).(Principal)
    return c.JSON(200, p)
})
```

Not exporting a `Ctx` accessor here is deliberate: it would promote "`Ctx` lives in the request context" from an implementation detail into a contract. The other thing middleware inside the onion needs — the route template — is already on the request (`r.Pattern`), so no new opening is required.

## Choosing where to mount

| Your middleware… | Where |
|---|---|
| Only touches headers / short-circuits / wraps the writer (logger, auth, rate limit, promhttp counters) | Either works; needs the route template or needs short-circuits in the access log → **`Adapt` (inside)** |
| Rewrites the body and finalizes unconditionally after next (gzip) | **Outside `Handler()`** |
| Must intercept before routing (CORS preflight) | **Outside `Handler()`** |
| Asserts `http.Hijacker` (WebSocket upgrades) | Either — `Adapt`'s proxy forwards `Hijack`; only re-wrapping the writer without forwarding breaks it |

promhttp is a good example split in two: **collection** uses `Adapt` around `promhttp.InstrumentHandlerCounter` (needs correct status codes and the route template), while **exposure** goes through `Wrap`, because `promhttp.Handler()` is already an `http.Handler`:

```go
app.GET("/metrics", web.Wrap(promhttp.Handler()))
```

## Ecosystem pieces already exercised

::: warning This table is a local spike, **not in CI, not a maintenance guarantee**
The data comes from one end-to-end matrix on a dev machine (same middleware, same route, compared item by item through `Adapt` against wrapped outside `app.Handler()`: status, response headers, body). That evidence does not live in the repo and does not run in CI, so it may already have drifted as upstream releases. Treat it as "this name is worth trying", not as a contract. If you adopt one, run the check in the next section yourself.
:::

| Middleware | Result | How far it was verified |
|---|---|---|
| chi `Logger` / `RequestID` / `RealIP` / `Timeout` / `Compress` | **Identical** through `Adapt` vs wrapped outside | End to end (status + 9 response headers + body) |
| `promhttp.InstrumentHandlerCounter` | Same, including the status code | End to end |
| rs/cors | Real requests match; **preflight never arrives** (405) → wrap outside | End to end |
| chi `Recoverer` | Both 500, but the **body differs** (Recoverer writes its own) → use the framework's panic handling | End to end |
| chi `CleanPath` | **Panics either way**, unusable | End to end |
| `httprate` | Whole family is `func(next http.Handler) http.Handler` | **Signature only**, never ran |
| `gorilla/csrf` | All four seams line up in the source (replaced request / writes headers first / short-circuits / reads the form on demand) | **Source review only**, never ran |

The last two rows only mean "the shape fits" — **not** "it works". They are candidates, not conclusions.

## Verifying a middleware yourself

The criterion is not "it ran" but **the same middleware on the same route, compared item by item through `Adapt` against wrapped outside `app.Handler()`** — status code plus the response headers you care about, including routes where the handler returns an error or panics. Testing only that "both return 200" misses every silent corruption.
