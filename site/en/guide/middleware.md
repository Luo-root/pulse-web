# stdlib middleware

Ecosystem middleware is almost always shaped `func(http.Handler) http.Handler`: chi/middleware, promhttp, httprate, gorilla/csrf, and so on. This page covers **how to wire them into this framework**, and where the limits of each route are. **CORS is the exception** — the framework ships a zero-dependency implementation, so you do not pull a package in for it (see "CORS: it ships with the framework" below).

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

1. **It does not move routing.** Route matching still happens before middleware, so a preflight `OPTIONS` never reaches it — register only `GET /api` and ServeMux answers 405 (`Allow: GET, HEAD`) directly. **CORS uses the framework's own `app.CORS(...)`**: it adds `OPTIONS` routes for the paths you registered, which is how a preflight reaches the onion (see "CORS: it ships with the framework" below). Requests that match no route (real 404, trailing slash) behave the same way, as do **paths that need cleaning** (`//users`, `/users//42`): ServeMux answers those with a 307 to the clean path **before calling any handler**, so middleware inside the onion never sees them. Note that the access record for such a request has a **non-empty `http.route`** (ServeMux sets the pattern before redirecting), so do not read it as "that handler processed this request".
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
| Must intercept before routing (CORS preflight) | **Use the framework's own `app.CORS(...)`** — it adds `OPTIONS` routes for registered paths, so preflights stay visible to the framework (see "CORS: it ships with the framework"); wrapping outside `Handler()` also works, but then the framework never sees the preflight |
| Asserts `http.Hijacker` (WebSocket upgrades) | Either — `Adapt`'s proxy forwards `Hijack`; only re-wrapping the writer without forwarding breaks it |

promhttp is a good example split in two: **collection** uses `Adapt` around `promhttp.InstrumentHandlerCounter` (so the route template `r.Pattern` is readable), while **exposure** goes through `Wrap`, because `promhttp.Handler()` is already an `http.Handler`:

```go
app.GET("/metrics", web.Wrap(promhttp.Handler()))
```

One trade-off in the collection half needs to be said out loud: **the counter's `code` label is wrong on error routes** when it runs inside the onion (a 404 is filed under `code="200"`, a panic is not recorded at all — see "What it does not do" item 2). To count by the real status code, wrap it outside `Handler()` instead — at the cost of `r.Pattern` not being populated yet, so you supply the route label yourself.

## CORS: it ships with the framework

```go
app.CORS(
    web.CORSAllowOrigins("https://app.example.com"), // required; "*" = any origin
    web.CORSAllowMethods(http.MethodGet, http.MethodPost),
    web.CORSAllowHeaders("content-type", "x-csrf-token"),
    web.CORSAllowCredentials(),
    web.CORSExposeHeaders("X-Total-Count"),
    web.CORSMaxAge(10*time.Minute),
)
```

`CORS(opts ...web.CORSOption)` is implemented in this package with **zero third-party dependencies** — CORS is this framework's own business and you should not have to install a package for it. It does two things:

1. pushes the CORS middleware into the onion — real requests carry CORS headers;
2. **adds a matching `OPTIONS` route for every route you register afterwards** — that is how a preflight reaches the onion.

**Mounting rules (one allow-list per mux)**:

| What you want | How to write it |
|---|---|
| One policy for the whole site | `app.CORS(...)` first, then `Group` / register routes |
| Cover one prefix only | Mount on **that group alone**: after `api := app.Group("/api")`, call `api.CORS(...)`; nothing on the parent |
| A node that already has CORS | Calling `CORS` on a group derived from it is an **assembly-time panic** — parent and child share one mux, and two policies on it only fight |

Same rule as `Use`: **group first, then `CORS` does not take effect** (a group snapshots its middleware and observers at `Group` time). One ordering tip: put `CORS(...)` **before** your other `Use` calls — it is itself a `Use`, and whichever comes first sits on the outside, so mount it first if you want preflights to skip auth and friends.

### Why that extra `OPTIONS` is needed

Route matching runs before middleware (see "What it does not do", item 1): with only `GET /api` registered, `OPTIONS /api` is answered 405 by ServeMux and the middleware never sees the preflight. The generated route carries **the same chain** (global plus group middleware), so:

| Request | Without CORS | With `app.CORS` |
|---|---|---|
| `OPTIONS /api` (a real preflight) | 405, invisible inside the onion | **204 + CORS headers; enters the access record with `http.route` = `/api`** |
| `OPTIONS /api` (a bare OPTIONS) | 405 + `Allow: GET, HEAD` | the same 405 + `Allow: GET, HEAD`; the difference is that it enters the onion and the body is the framework's unified error body |
| `GET /nope` (matches no route) | 404 | **404 (unchanged)** |

That last row is where a measurement changed the design: the lazier version is to register one catch-all `OPTIONS /{path...}`, and it has two hard problems — (1) `{path...}` matches every path, so ServeMux reads "path matched, method did not" as 405, and **every unmatched request site-wide turns from 404 into 405**; (2) it **conflicts with the `<prefix>/` pattern that `Static` registers** (`<prefix>/` matches more methods, `/{path...}` has the more specific path, and neither is more specific overall), so registering both is an **assembly-time panic** — in either order. The framework therefore adds `OPTIONS` per registered path, and `TestCORSUnmatchedRouteHasNoCORSHeaders` pins that 404.

### Semantics that matter

- **A rejected preflight is a 403**, not a silently thinner response: if the origin, the method or a requested header is not on the list you get `403` plus the framework's unified error body (`cors_origin_not_allowed` / `cors_method_not_allowed` / `cors_header_not_allowed`). Both shapes are a CORS failure in the browser, but this one is **visible to people and to monitoring** — `error.type` lands in the access record.
- **`CORSAllowOrigins("*")` and `CORSAllowCredentials()` cannot be combined**: that pair means "any website may call this service with credentials", which is a vulnerability rather than a configuration, so assembly panics; list the sites you mean instead.
- Methods default to `GET` / `POST` / `HEAD` (the Fetch simple methods); **requested headers default to `Accept` / `Content-Type`** — `application/json` is not on the CORS safelist (only form-urlencoded / multipart / text/plain are), and browsers send `Access-Control-Request-Headers: content-type` for a JSON POST, so a JSON API that only configures origins just works without `CORSAllowHeaders`. `CORSAllowHeaders("*")` allows any request header.
- **The generated `OPTIONS` carries only the global and group middleware**, never the per-route middleware of the route that triggered it: an `OPTIONS` is not a stand-in for its `GET` / `POST`, so an auth guard on `GET /x` does not block `OPTIONS /x`.
- Origins are compared as strings, after normalising case and a trailing slash (`https://App.Example.com/` equals `https://app.example.com`).
- **Only what was asked for is echoed**: `Access-Control-Allow-Methods` returns the one method this request asked about, `Allow-Headers` returns the headers it asked about (normalised, de-duplicated). The response stays minimal and no unasked-for capability is advertised.
- A preflight **does not check whether the target route exists** — the usual CORS middleware semantics; whether the real request finds a handler is a separate question.
- **A prefix mounted by `Static` needs nothing special**: the `<prefix>/` pattern carries no method, so it already accepts `OPTIONS`; the preflight is answered by the middleware and enters the access record as usual (`TestCORSPreflightOnStaticPrefix`), and no matching `OPTIONS` is added for it (that would shadow FileServer's own 404/405).
- A rejected **real** request gets **no CORS headers and no error**, and is passed through: the browser blocks it, the server does not decide on its behalf.

### Taking over a path's `OPTIONS`

Register it **before** that path's method routes and the path is left alone:

```go
app.CORS(web.CORSAllowOrigins("https://app.example.com"))
app.OPTIONS("/api/users", myOptions) // registered first: this path's OPTIONS semantics are yours
app.GET("/api/users", listUsers)     // no OPTIONS /api/users will be added
```

The other order is an **assembly-time error** (that path's automatic `OPTIONS` is already on the mux, and ServeMux rejects a duplicate pattern); the panic message spells out the fix.

### Two boundaries

- **A request that matches no route never enters the onion**, so its response carries no CORS headers either: a cross-origin call to a path that does not exist shows up in the browser as a CORS failure rather than as that 404. This is the framework's existing "routing is not moved" boundary.
- **A preflight to a path with no registered methods at all** (`OPTIONS /api/nowhere`) has no generated `OPTIONS` → 404. Covering that case would mean moving the whole routing chain behind the middleware, a change of another order of magnitude.

### Existing projects: keep `rs/cors`

If you already run `rs/cors` there is no need to switch for its own sake, but on the preflight path it can only carry the request into the onion by "registering an `OPTIONS` route". Three ways:

| Route | Preflight intercepted | Preflight visible to the framework | When to use |
|---|---|---|---|
| Wrap `Handler()` | ✅ | ❌ the framework never learns (no trace, no access record) | CORS just has to work; you do not look at preflight traffic |
| Register `OPTIONS` per route | ✅ | ✅ (`http.route` is the real route) | Few routes, fixed paths |
| One catch-all `OPTIONS /{path...}` | ✅ | ✅ (`http.route` is the catch-all pattern) | One line covers every path, including preflights to unregistered ones. **Two prices**: unmatched requests site-wide turn from 404 into 405; and it **conflicts with `Static`'s prefix pattern** (registering both is an assembly-time panic) — so do not pick it if you mount static files |

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
- on the wrapped-outside route the preflight carries **no** `X-Trace-Id` and leaves no access record — the "framework never learns" square in the table above;
- a bare `OPTIONS` (no `Access-Control-Request-Method`) is not a preflight and lands on that handler, which answers 405 — change it if you want other semantics;
- a path with an explicitly registered `OPTIONS /x` still goes to that route (ServeMux prefers the more specific pattern);
- the catch-all **cannot coexist with `Static`**: `Static` registers `<prefix>/`, and neither pattern is more specific than `OPTIONS /{path...}`, so registering both is an **assembly-time panic** (in either order). If you mount static files, use "register `OPTIONS` per route" — or just switch to the built-in `app.CORS(...)`.

Both ways stay documented because plenty of projects are already on them; **new projects should use `app.CORS`** — the same semantics, one dependency fewer, and no extra route to maintain just so preflights show up in your telemetry.

## CSRF: yours to own

CSRF is a business concern — how sessions are stored, which requests count as state-changing, whether you want double submission; the framework knows none of those premises — so it **ships nothing for it** and documents one measured route instead.

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

## CORS and CSRF together

Each half owns one thing; drop either and the preflight is a 405:

1. **CSRF letting the preflight through** relies on the safe-method exemption (`OPTIONS` is a safe method);
2. **the preflight reaching the onion** relies on the `OPTIONS` route CORS adds for each route.

The full walkthrough is `TestMatrixGorillaCSRFWithCorsPreflight`: the preflight gets its `204` and enters the record, and the cross-origin POST that follows goes through with its token. That case runs `rs/cors` with the catch-all route; swapping in the framework's own `app.CORS` inside the onion is the same thing with one less route to maintain.

## Two things hand-written clients get wrong

Browsers send these per the standard; hand-written clients (tests included) tend to trip:

- `Access-Control-Request-Headers` must be **lowercase** (the Fetch standard requires it sorted and lowercase): send `X-CSRF-Token` and `rs/cors` rejects it outright, aborting the preflight — the response is still `204`, but with **no CORS headers at all**. The built-in `app.CORS` normalises both sides to lowercase before comparing, so it does not take that hit;
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
| `rs/cors` | Real requests match; **preflight never arrives** (405) → existing projects can keep it (wrap outside, or register an explicit `OPTIONS` route, or one catch-all `OPTIONS /{path...}` — the matrix covers all three, and the catch-all records `http.route` as the catch-all pattern); new projects use the built-in `app.CORS`, which is the only way a preflight shows up in your telemetry | End to end |
| `golang-jwt/jwt/v5` | Both seams hold: short-circuit (401) and request replacement | End to end |
| `httprate` | **Stateful** limiter, one instance per mount point: two 200s, a third request 429 with `Retry-After`, and once exhausted even other routes 429 | End to end |
| `gorilla/csrf` | End to end: all four seams hold — writes headers first (`Set-Cookie`), replaced request, short-circuit (403 recorded back, no `error.type`), reads the form on demand; `TrustedOrigins` takes a host, plaintext HTTP needs marking, and the field-name and encoding traps each have a case | End to end |

Every row in that table is now **end to end**: `gorilla/csrf` had only a source review before, and this round added a real-dependency end-to-end run — its four traps and the recipe are in the "CSRF: yours to own" section above.

chi's `RealIP` is **not in the table**: it is marked Deprecated in chi v5.3.2 (it mutates `r.RemoteAddr` and can be spoofed — see GHSA-3fxj-6jh8-hvhx and two related advisories). The table runs its replacement `ClientIPFromXFF`, which stores the result in the request context — a shape that tests the adapter surface harder.

## Verifying a middleware yourself

The criterion is not "it ran" but **the same middleware on the same route, compared item by item through `Adapt` against wrapped outside `app.Handler()`**: status code, the response headers you care about, the body (**including routes where the handler returns an error or panics** — that is where the differences concentrate), plus the middleware's own side channel (its log line / metric labels). Testing only that "both return 200" misses every silent corruption.

Three criteria are easy to write vacuously. **First, comparing responses only**: for the short-circuit row the difference is not in the response (both return 401) but in whether the framework knows the request happened. **Second, "both sides set the same header" and "neither side set it" look identical** — so every comparison needs a companion **positive** assertion pinning down that the middleware actually did something. **Third, a row with a declared difference is no longer compared at all** — known boundaries skip the item-by-item pass for that whole row, which leaves the status code the client received with no criterion at all: swallowing the status the middleware wrote (leaving the response with only the implicit 200 that the body brings) still goes green. The short-circuit, error and panic rows are exactly the declared ones, so assert their response status explicitly.
