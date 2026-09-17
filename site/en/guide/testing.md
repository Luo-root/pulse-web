# Testing: unit-test handlers without a server

Two entry points let a handler unit test run through **exactly the same assembly and wrap-up as a real request** — not "close enough", the same code (`Engine.begin` + `finish`). Whatever you observe — how the status settles, error mapping, the access log, `X-Trace-Id` — is what production produces.

## The two entry points

```go
app := web.New(web.WithSink(sink))       // sink is something the test can read

rec := httptest.NewRecorder()
req := httptest.NewRequest("GET", "/users/42?q=1", nil)
req.Pattern = "GET /users/{id}"          // route template: fill it in yourself without ServeMux
req.SetPathValue("id", "42")             // path parameters likewise

c, done := app.NewTestContext(rec, req)  // ① get the Ctx and call whatever slice you want
defer done()                             //    done does the wrap-up (idempotent)
if err := myHandler(c); err != nil {
    t.Fatal(err)
}
// rec and sink now hold what a full trip through the stack would have produced
```

```go
app.ServeTest(rec, req, myHandler, requireAuth, withTx)
// ② runs the middleware chain too, and takes over panics the way the real path does
```

## Path parameters and the route template

`c.Path("id")` reads `Request.PathValue`; the `http.route` attribute reads `Request.Pattern` — normally `ServeMux` fills both in. Without the mux you write them yourself; both are standard-library surface, so no framework-specific API is needed.

## Who owns a panic

- **`ServeTest`**: the handler runs inside the framework's `defer`, so a panic becomes a `PanicError` and goes through error mapping (500), with an access record written as usual.
- **`NewTestContext`**: the handler runs on **your own stack**, and the framework's recover is not on it — the panic reaches you and the `testing` package reports it normally. To cover the panic path, use `ServeTest`.

## When you still want a real server

::: warning Three kinds of question only the real link can answer
- **Protocol level**: Content-Type sniffing, chunked encoding, automatic `Content-Length` — these happen in the net/http **server**, and `httptest.ResponseRecorder` does not go through any of it (measuring headers with a Recorder yields false conclusions; measured: `Header().Set("Content-Type", "")` and "not setting the header at all" both look "empty" on a Recorder, while on the real link one is an empty header and the other is `text/html`).
- **Real network behaviour**: timeouts, the timing of `Flush`, client disconnects, connection reuse.
- **Full routing**: `ServeMux` pattern matching, precedence, group prefixes — both entry points bypass the mux.
:::

For the real link use `httptest.NewServer(app)` (`Engine` is an `http.Handler` itself), or hand `app.Handler()` to the surrounding framework.
