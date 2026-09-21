# stdlib middleware

Ecosystem middleware is almost always shaped `func(http.Handler) http.Handler`: chi/middleware, rs/cors, promhttp, httprate, gorilla/csrf, and so on. This page covers **how to wire them into this framework**, and where the limits of each route are.

For writing your own middleware (`func(*Ctx, Handler) error`) and attaching it, see [Routing and middleware](/en/guide/routing).

## Two routes, and they are not equivalent

`web.Adapt` turns stdlib middleware into framework middleware. Wrapping the whole thing around `app.Handler()` is the other route. **This is not a style choice** — the capabilities differ:

| | Hand-rolled adapter (a dozen lines of public API) | Wrap outside `Handler()` | `Adapt` (inside the onion) |
|---|---|---|---|
| Middleware wraps the writer to read the status | ❌ always 0 | ✅ | ✅ sees what the handler wrote; sees 0 on the framework-mapped error/panic path (below) |
| Middleware rewrites the body (gzip) | ❌ corrupt response | ✅ | ✅ on the normal path; degrades on error/panic (below) |
| Middleware short-circuits | ❌ access log says "no response written" | ⚠️ framework never learns (no log, no span) | ✅ recorded back |
| Middleware replaces the request | ❌ handler never sees it | ✅ | ✅ |
| Read the route template `r.Pattern` | ❌ | ❌ routing hasn't run yet | ✅ |
| Intercept preflight / run before routing | ❌ | ✅ | ❌ |
| Requests that match no route (real 404, trailing slash) | — | ✅ middleware sees them | ❌ never reaches the onion |
| Paths that need cleaning (`//users`, `/users//42`) | — | ✅ middleware sees them | ❌ ServeMux issues a 307 to the clean path before the onion |

The parenthetical notes after the first two ✅s are the price of the same ordering on the **error path**, spelled out in "What it does not do" item 2. On the normal path (the handler writes its own response) those three columns hold without qualification.

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
2. **When middleware short-circuits (never calls next), or calls next but writes the response around it, the status and byte count it wrote are recorded back into the framework's collection layer** — the access log and traces show the real response, not "nothing was written". The second shape is the `Recoverer` / fallback-404 square; such a record carries no `error.type`, because the framework never caught an error and does not know where that response came from.
3. **A request replaced by middleware is passed along** — after `r = r.WithContext(...)`, what the handler reads via `c.Request()` is the replaced one.

Plus: the writer handed to middleware supports `http.Flusher` and `http.Hijacker` — SSE still flushes per event, and upgrade routes behind `Use(Adapt(…))` still get their connection. And it **never overclaims**: if the underlying writer cannot flush, `c.Flush()` still returns an explicit error; if it cannot hijack, `Hijack()` returns `http.ErrNotSupported`.

## What it does not do

1. **It does not move routing.** Route matching still happens before middleware, so a preflight `OPTIONS` never reaches it — register only `GET /api` and ServeMux answers 405 (`Allow: GET, HEAD`) directly. **CORS that must intercept preflight either wraps outside**, or you register `OPTIONS` routes: per route, or one catch-all `OPTIONS /{path...}` — both are verified, see "CORS and CSRF" below. Requests that match no route (real 404, trailing slash) behave the same way, as do **paths that need cleaning** (`//users`, `/users//42`): ServeMux answers those with a 307 to the clean path **before calling any handler**, so middleware inside the onion never sees them. Note that the access record for such a request has a **non-empty `http.route`** (ServeMux sets the pattern before redirecting), so do not read it as "that handler processed this request".
2. **It does not solve "finalizing middleware × error/panic".** The framework's error mapping happens **after** middleware returns, and that single ordering has consequences in two directions:
   - **On the response**: middleware that unconditionally writes once next returns (e.g. gzip with its own `defer zw.Close()`) locks the status at 200. **Compression is pushed outside by default.**
   - **On observability**: the middleware's own finalizer reads **0** — `chi Logger` records `0 / 0B` on routes that return an error or panic, and a `promhttp` counter files a 404 under `code="200"` (`sanitizeCode(0)`), or records nothing at all for a panic. To log or measure the **real** response, use the framework's own access log and traces.
   - And one more square, in the other direction: when middleware writes the response **around** next (a `Recoverer` answering a panic with 500), the framework records **the status and byte count that middleware wrote** — the ones the client actually received — not its own default 200. That was fixed by [#96](https://github.com/Luo-root/pulse-web/issues/96). Such a record carries **no** `error.type` and no error object: the response was not written by the framework, it does not know why, and inventing a category would be false information. The one exception is when an error is already pending mapping (the "on the response" square above): taking the middleware's status there would make the framework skip error mapping and drop the error body entirely, so that case still goes to the mapper.
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
| Only touches headers / short-circuits / wraps the writer (logger, auth, rate limit, promhttp counters) | Either works; needs the route template or needs short-circuits in the access log → **`Adapt` (inside)**; must log or measure the **real status** → **wrap outside** (inside the onion it cannot see the framework-mapped error/panic response, see "What it does not do" item 2) |
| Rewrites the body and finalizes unconditionally after next (gzip) | **Outside `Handler()`** |
| Must intercept before routing (CORS preflight) | **Outside `Handler()`**; if the preflight must also be visible to the framework, stay inside the onion and add one catch-all `OPTIONS /{path...}` route (see "CORS and CSRF") |
| Asserts `http.Hijacker` (WebSocket upgrades) | Either — `Adapt`'s proxy forwards `Hijack`; only re-wrapping the writer without forwarding breaks it |

promhttp is a good example split in two: **collection** uses `Adapt` around `promhttp.InstrumentHandlerCounter` (so the route template `r.Pattern` is readable), while **exposure** goes through `Wrap`, because `promhttp.Handler()` is already an `http.Handler`:

```go
app.GET("/metrics", web.Wrap(promhttp.Handler()))
```

One trade-off in the collection half needs to be said out loud: **the counter's `code` label is wrong on error routes** when it runs inside the onion (a 404 is filed under `code="200"`, a panic is not recorded at all — see "What it does not do" item 2). To count by the real status code, wrap it outside `Handler()` instead — at the cost of `r.Pattern` not being populated yet, so you supply the route label yourself.

## CORS and CSRF: how to wire them up

These two are where "just use an ecosystem piece" usually gets stuck: CORS on **preflights never reaching the onion**, CSRF on **a string of 403s that look like token failures**. Both complete routes are below, and every claim is held by a case in `interop/`.

### CORS

`rs/cors` is zero-dependency and sufficient; the one problem is that **a preflight is dispatched by ServeMux before routing runs** (see "What it does not do" item 1). Three routes:

| Route | Preflight intercepted | Preflight visible to the framework | When to use |
|---|---|---|---|
| Wrap `Handler()` | ✅ | ❌ the framework never learns (no trace, no access record) | CORS just has to work; you do not look at preflight traffic |
| Register `OPTIONS` per route | ✅ | ✅ (`http.route` is the real route) | Few routes, fixed paths |
| **One catch-all `OPTIONS /{path...}`** | ✅ | ✅ (`http.route` is the catch-all pattern) | The default: one line covers every path, including preflights to unregistered ones |

The catch-all looks like this (`Use` must come **before** routes are registered):

```go
corsMW := cors.New(cors.Options{
    AllowedOrigins:   []string{"https://app.example.com"},
    AllowedMethods:   []string{http.MethodGet, http.MethodPost},
    AllowedHeaders:   []string{"content-type", "x-csrf-token"},
    AllowCredentials: true,
}).Handler

app.Use(web.Adapt(corsMW)) // CORS headers for real requests

// The preflight route: one catch-all OPTIONS route carries the request into the
// onion; the middleware still writes the response.
app.OPTIONS("/{path...}", func(c *web.Ctx) error {
    // Only OPTIONS requests that are NOT preflights reach this — the middleware
    // short-circuits preflights.
    return &web.HTTPError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed"}
})
```

What was measured (`TestMatrixRsCorsPreflightCatchAllRoute`, `TestMatrixRsCorsPreflightExplicitRoute`):

- the preflight gets `204` plus `Access-Control-Allow-Origin/Methods/Headers`, **and enters the framework's collection layer**: `http.route` records the catch-all pattern `/{path...}` rather than the target route — the price of the catch-all; per-route registration records the real route;
- on the wrapped-outside route the preflight carries **no** `X-Trace-Id` and leaves no access record — the "⚠️ framework never learns" square in the table above;
- a bare `OPTIONS` (no `Access-Control-Request-Method`) is not a preflight and lands on that handler, which answers 405 — change it if you want other semantics;
- a path with an explicitly registered `OPTIONS /x` still goes to that route (ServeMux prefers the more specific pattern).

### CSRF

`gorilla/csrf`'s `csrf.Protect` is already `func(http.Handler) http.Handler`, so `Adapt` puts it inside the onion:

```go
app.Use(web.Adapt(csrf.Protect([]byte(key),
    csrf.Secure(true),                                   // on in production
    csrf.TrustedOrigins([]string{"app.example.com"}))))  // host[:port], no scheme
```

Server-rendered forms hand the token to the template (`csrf.TemplateField` produces the hidden input):

```go
app.GET("/form", func(c *web.Ctx) error {
    return c.HTML(http.StatusOK, "form", map[string]any{"csrf": csrf.TemplateField(c.Request())})
})
```

With a separate front end: `GET` once for the `_gorilla_csrf` cookie and a token (`csrf.Token(r)`, handed to the client), then send `X-CSRF-Token` on every unsafe request — that is the default header name.

**Four traps** (each pinned by a case):

| Symptom | Cause | What to do |
|---|---|---|
| 403 `referer not supplied` / `referer invalid` | For unsafe requests the middleware compares Referer/Origin against **https**; a plain-HTTP deployment (including "TLS terminated upstream") fails both ways | Insert a layer **before** CSRF that replaces the request: `next.ServeHTTP(w, csrf.PlaintextHTTPRequest(r))` (mounted through `Adapt` too — promise ③ guarantees the replaced request travels on) |
| Every cross-origin POST gets 403 `origin invalid` | The `Origin` differs from the request and is not listed in `TrustedOrigins` | `csrf.TrustedOrigins([]string{"app.example.com"})` — it matches the **host[:port], not the scheme**; include the port when it is not the default (`app.example.com:8443`) |
| 403 `CSRF token not found in request` although the form field was sent | The default form field name is `gorilla.csrf.Token`, not `csrf_token` | Produce it with `csrf.TemplateField`, or set `csrf.FieldName(...)` explicitly |
| The form field was sent but yields `token invalid` | The token is base64; a bare `+` inside an `application/x-www-form-urlencoded` body is read as a space | Encode per the standard (browsers do; hand-written clients use `url.Values{}.Encode()`) |

A preflight is never blocked by it: `OPTIONS` is a safe method and passes straight through (also covered by a case).

**Observability**: a 403 written by a short-circuit is recorded back into the framework's collection layer (status / route template / size) and carries **no** `error.type` — that response was written by the middleware, not caught by the framework. Wrapped outside, the framework still knows nothing about the request (no record, no trace).

### CORS and CSRF together

Each half owns one thing; drop either and the preflight is a 405:

1. **CSRF letting the preflight through** relies on the safe-method exemption;
2. **the preflight reaching the onion** relies on CORS's catch-all `OPTIONS /{path...}` route.

The full walkthrough is `TestMatrixGorillaCSRFWithCorsPreflight`: the preflight gets its `204` and enters the record, and the cross-origin POST that follows goes through with its token.

### Two things hand-written clients get wrong

Browsers send these per the standard; hand-written clients (tests included) tend to trip:

- `Access-Control-Request-Headers` must be **lowercase** (the Fetch standard requires it sorted and lowercase): send `X-CSRF-Token` and `rs/cors` rejects it outright, aborting the preflight — the response is still `204`, but with **no CORS headers at all**;
- form field values must be encoded as `application/x-www-form-urlencoded`: an unencoded `+` in the token is read as a space by the server.

## Ecosystem pieces already exercised

These conclusions are held up by a comparison project that **runs in CI**: [`interop/`](https://github.com/Luo-root/pulse-web/tree/main/interop) (a nested module; `go build` / `go vet` / `go test` are all gates, same pattern as `otel/` and `loadtest/`). The criterion is not "it ran" but **the same middleware on the same route, compared item by item through `Adapt` against wrapped outside `app.Handler()`**, across three faces:

1. **The response** — status code plus the response headers that matter, plus the body;
2. **The middleware's own side channel** — the log line / metric labels it records;
3. **The framework's access record** — status, route template, response size, error category.

Face 3 gets its own place because for the short-circuit row the difference is not in the response at all (both sides return 401) but in whether the **framework knows the request happened**. If an upstream release changes behaviour, this goes red. The matrix itself lives in [`interop/middleware_test.go`](https://github.com/Luo-root/pulse-web/blob/main/interop/middleware_test.go).

| Middleware | Result | How far it was verified |
|---|---|---|
| chi `RequestID` / `ClientIPFromXFF` | **Identical** through `Adapt` vs wrapped outside, including whether the value they put in the request context reaches the handler | End to end |
| chi `Logger` / `Compress` | Identical on the **normal path**; the two sides differ on error routes (see "What it does not do" item 2) | End to end |
| chi `Timeout` | Identical (the 504 it writes after a timeout never takes effect on either side — the response is already committed) | End to end |
| chi `Recoverer` | Both 500, but the **body differs** (Recoverer writes its own empty body) → use the framework's panic handling | End to end |
| chi `CleanPath` | **Panics either way**, unusable | End to end |
| `promhttp.InstrumentHandlerCounter` | `code=200` matches on the normal path; on error routes it files a 404 under `code=200` | End to end |
| `rs/cors` | Real requests match; **preflight never arrives** (405) → wrap outside, or register an explicit `OPTIONS` route, or one catch-all `OPTIONS /{path...}` (the matrix covers all three; the latter two are verified to work, and the catch-all records `http.route` as the catch-all pattern) | End to end |
| `golang-jwt/jwt/v5` | Both seams hold: short-circuit (401) and request replacement | End to end |
| `httprate` | **Stateful** limiter, one instance per mount point: two 200s, a third request 429 with `Retry-After`, and once exhausted even other routes 429 | End to end |
| `gorilla/csrf` | End to end: all four seams hold — writes headers first (`Set-Cookie`), replaced request, short-circuit (403 recorded back, no `error.type`), reads the form on demand; `TrustedOrigins` takes a host, plaintext HTTP needs marking, and the field-name and encoding traps each have a case | End to end |

Every row in that table is now **end to end**: `gorilla/csrf` had only a source review before, and this round added a real-dependency end-to-end run — its four traps and the recipe are in the "CORS and CSRF" section above.

chi's `RealIP` is **not in the table**: it is marked Deprecated in chi v5.3.2 (it mutates `r.RemoteAddr` and can be spoofed — see GHSA-3fxj-6jh8-hvhx and two related advisories). The table runs its replacement `ClientIPFromXFF`, which stores the result in the request context — a shape that tests the adapter surface harder.

## Verifying a middleware yourself

The criterion is not "it ran" but **the same middleware on the same route, compared item by item through `Adapt` against wrapped outside `app.Handler()`**: status code, the response headers you care about, the body (**including routes where the handler returns an error or panics** — that is where the differences concentrate), plus the middleware's own side channel (its log line / metric labels). Testing only that "both return 200" misses every silent corruption.

Three criteria are easy to write vacuously. **First, comparing responses only**: for the short-circuit row the difference is not in the response (both return 401) but in whether the framework knows the request happened. **Second, "both sides set the same header" and "neither side set it" look identical** — so every comparison needs a companion **positive** assertion pinning down that the middleware actually did something. **Third, a row with a declared difference is no longer compared at all** — known boundaries skip the item-by-item pass for that whole row, which leaves the status code the client received with no criterion at all: swallowing the status the middleware wrote (leaving the response with only the implicit 200 that the body brings) still goes green. The short-circuit, error and panic rows are exactly the declared ones, so assert their response status explicitly.
