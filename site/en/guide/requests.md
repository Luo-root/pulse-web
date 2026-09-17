# Requests: reading, binding and per-request state

This page covers how a handler gets data out of a request, and which state hangs off a request — and for how long.

## Scalar access: path, query, headers

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
    id := c.Path("id")                 // path parameter (same as c.Request().PathValue)
    page := c.Query("page")            // query parameter; empty string when absent
    ua := c.Request().Header.Get("User-Agent")   // need the raw request? c.Request()
    c.SetHeader("X-Trace", c.TraceID())          // response header (valid before the first flush)
    return c.Text(200, id+"@"+page)
})
```

`c.Query(name)` returns the first value only; for multiple values or nested structures use binding below. **When one request needs several parameters, call it once and keep the values in local variables** — every call re-parses the query string, and the framework deliberately does not cache it for you (calling once is the simplest fix).

## Binding a body: `Bind`

```go
type CreateUser struct {
    Name  string `json:"name"  form:"name"  query:"name"`
    Email string `json:"email" form:"email" query:"email"`
}

app.POST("/users", func(c *web.Ctx) error {
    var in CreateUser
    if err := c.Bind(&in); err != nil {
        return err          // the default mapper turns it into 400 / 413 / 415
    }
    return c.JSON(201, in)
})
```

`Bind` dispatches on **`Content-Type`**:

| Content-Type | Behaviour |
|---|---|
| `application/json` (including `+json` suffixes) | decode JSON |
| `application/xml` / `text/xml` | decode XML |
| `application/x-www-form-urlencoded` | parse the form |
| `multipart/form-data` | parse multipart (`c.Request().FormFile` for files) |
| any other non-empty type | **415** (unsupported media type); nothing is guessed |
| **no body at all** | falls back to **query** (GET semantics, `query` tag) |

For pure query filtering (search, pagination) use `BindQuery`, which ignores `Content-Type`:

```go
app.GET("/users", func(c *web.Ctx) error {
    var f struct {
        Q    string `query:"q"`
        Page int    `query:"page"`
    }
    if err := c.BindQuery(&f); err != nil {
        return err          // 400 by default
    }
    return c.JSON(200, f)
})
```

**Tags**: JSON and XML use their own tags, forms use `form`, queries use `query`. A field can carry several (as above), which is what makes "body when there is one, query when there is not" work with a single struct.

### Limits and errors

Two gates protect the body: the global `web.WithMaxBodyBytes(n)` and the per-route `web.BodyLimit(n)` (**it only tightens**, see [routing and middleware](/en/guide/routing)). Over the limit is always **413 + `body_too_large`** with a `cause` of `*http.MaxBytesError`; an unsupported type is **415**; a parse or validation failure is **400**.

To react to a specific reason, inspect the error in a custom `ErrorHandler` with `errors.As` — either `*web.HTTPError` (its `Code` field) or `*http.MaxBytesError`. Do not match on message text.

## Per-request KV: `Set` / `Get`

Middleware puts something on the request, later handlers take it out — through a typed key (**not a string**):

```go
// Declare the key once at package level; the type lives in Key[T], so a mismatch fails to compile.
var userKey = web.NewKey[*User]("user")

func requireAuth(next web.Handler) web.Handler {
    return func(c *web.Ctx) error {
        u, err := loadUser(c)          // your own logic
        if err != nil {
            return err                 // not signed in: 401
        }
        c.Set(userKey, u)              // store it in the per-request KV
        return next(c)
    }
}

app.Use(requireAuth)
app.GET("/me", func(c *web.Ctx) error {
    u, ok := c.Get(userKey)            // (*User, bool)
    if !ok {
        return web.Internal("no_user", nil)
    }
    return c.JSON(200, u)
})
```

Two things worth internalising:

- **The key is a `Key[T]`, not a string** — a wrong type does not compile, so you never silently read nil at runtime.
- **The KV hangs off `Ctx` and outlives the request scope**: after the handler returns, the error mapper can still call `c.Get` / `c.Set` / `c.Observe` / `c.TraceID()` / `c.Path` / `c.Query()`. But the scope behind `c.Kernel()` has already been collected, so **`c.Service` at that point is a dead scope**.

## Application services: `Service` / `MustService`

What you provided at assembly time (`kernel.Provide(root, key, v)`) is read like this inside a request:

```go
var dbKey = kernel.NewServiceKey[*sql.DB]("db")

app := web.New()
kernel.Provide(app.Root(), dbKey, sqlDB)     // process-level: attached to root

app.GET("/users", func(c *web.Ctx) error {
    db, ok := c.Service(dbKey)               // (T, bool)
    if !ok {
        return web.Internal("db_unavailable", nil)
    }
    db2 := c.MustService(dbKey)              // panics when missing — only for assembly-guaranteed deps
    _ = db
    return c.Text(200, db2.Stats().OpenConnections)
})
```

- `c.Service` is `kernel.Get(c.Kernel(), key)`: it walks the **request scope's local chain first**, then the global repository.
- `c.Kernel()` is the **request scope** — fine for Effects, event listeners and components that need a scope, but **it is not the process-level kernel**: plugins and Effects attached there are destroyed with the request. For process-level work use `app.Root()` (see [assembly and running](/en/guide/assembly)).

## Observability: `TraceID` and `Observe`

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
    c.Observe("user.fetch", func(a *observability.Attrs) {
        observability.Set(a, "user.id", c.Path("id"))
        observability.Set(a, "cache", "miss")
    })
    return c.JSON(200, payload)
})
```

- `c.TraceID()` returns this request's 32-hex string; it shows up in the access log and on downstream calls (propagation is controlled by `WithTrustedTraceHeader`, see [assembly and running](/en/guide/assembly)).
- `c.Observe(event, set)` adds fields to the access log. **The types are constrained**: `Set` accepts `~string | ~int64 | ~float64 | ~bool` only, so `int` or UUID types need an explicit conversion (`int64(n)` / `u.String()`).

## Crossing goroutines: `Detach`

A `*Ctx` **must not cross goroutines** (it is bound to the request and its response writer). Background work uses `Detach()`, which hands you a **bag of values**, not a context:

```go
app.POST("/jobs", func(c *web.Ctx) error {
    d := c.Detach()                 // TraceID / HostID / Sink / Root (process-level root)
    id := c.Path("id")              // ⚠️ pull out the request data you need here
    go func() {
        job := runJob(id)
        d.Observe("job.done", func(a *observability.Attrs) {   // writes straight to the sink, shares the TraceID
            observability.Set(a, "ok", job.OK)
        })
        db := d.MustService(dbKey)  // reads **application** services
        _ = db
        // need a scope? derive your own with d.Root.Derive() and dispose it yourself
    }()
    return c.Text(202, "accepted")
})
```

Three contracts, all expressed in types:

- **No per-request KV** (deliberately): whatever you `c.Set` does not come along — pull it out explicitly before `Detach`, which makes "what entered the background" visible in the code and avoids quietly extending the KV's lifetime contract.
- **`Service` / `MustService` read the application repository** (`d.Root`) — a `Detached` owns no scope, and the request scope is collected when the handler returns.
- Reporting goes through `d.Observe` (straight to the sink, no broadcast). Response writing and `Kernel()` are absent from `Detached`, which turns "a background task cannot touch the response" into a type-level fact.
