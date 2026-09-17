# Testing: unit-test handlers without a server

Two entry points let a handler unit test run through **the same assembly and wrap-up as a real request** — the same code (`Engine.begin` + `finish`). So how the status settles, error mapping, the access log and `X-Trace-Id` are all produced the way production produces them.

The only thing missing versus the real path is `ServeMux`: route matching, group prefixes and route-level `BodyLimit` do not take part, so path parameters and the route template are yours to fill in.

## The two entry points

```go
app := web.New(web.WithSink(sink))       // sink is something the test can read

rec := httptest.NewRecorder()
req := httptest.NewRequest("GET", "/users/42?q=1", nil)
req.Pattern = "GET /users/{id}"          // route template: fill it in yourself without ServeMux
req.SetPathValue("id", "42")             // path parameters likewise

c, done := app.NewTestContext(rec, req)
done(myHandler(c))                       // run + hand the error back + wrap up

if rec.Code != http.StatusNotFound {     // error mapping still applies
    t.Fatalf("status = %d", rec.Code)
}
```

```go
app.ServeTest(rec2, req2, myHandler, requireAuth, withTx)
// handler + middleware chain: the mw you pass is **appended** after Use and group middleware
```

**Which one to reach for**: use `NewTestContext` when you want to run just one slice — the price is that it does not take over panics, does not run the request-body gate, and `Engine.Use` middleware does not take part. Use `ServeTest` when you need any of those.

## Path parameters and the route template

`c.Path("id")` reads `Request.PathValue`; the `http.route` attribute reads `Request.Pattern` — normally `ServeMux` fills both in. Both entry points bypass the mux, so you write them yourself; both are standard-library surface, so no framework-specific API is needed.

## What each entry point covers

| | `NewTestContext` | `ServeTest` | real path |
|---|---|---|---|
| Error mapping (`done(err)` / `return err`) | ✅ | ✅ | ✅ |
| `Engine.Use` and group middleware | ❌ | ✅ | ✅ |
| Request-body gate (`WithMaxBodyBytes`) | ❌ | ✅ | ✅ |
| panic → `PanicError` (500) | ❌ reaches you | ✅ | ✅ |
| `ServeMux` route matching | ❌ | ❌ | ✅ |

`done` is idempotent: **the first call wins**. Where the handler may panic, write `defer done(nil)` and then call `done(err)` explicitly — on the normal path the explicit call arrives first, and only a panic leaves it to the deferred one.

## When you still want a real server

::: warning Three kinds of question only the real link can answer
- **Protocol level**: Content-Type sniffing, chunked encoding, automatic `Content-Length` — these happen in the net/http **server**, and `httptest.ResponseRecorder` does not go through any of it (measuring headers with a Recorder yields false conclusions; measured: `Header().Set("Content-Type", "")` and "not setting the header at all" both look "empty" on a Recorder, while on the real link one is an empty header and the other is `text/html`).
- **Real network behaviour**: timeouts, the timing of `Flush`, client disconnects, connection reuse.
- **Full routing**: `ServeMux` pattern matching, precedence, group prefixes — both entry points bypass the mux.
:::

For the real link use `httptest.NewServer(app)` (`Engine` is an `http.Handler` itself), or hand `app.Handler()` to the surrounding framework.
