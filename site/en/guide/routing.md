# Routing, groups and middleware

The registration surface is a thin wrapper around the standard library's `http.ServeMux`: **patterns are `net/http` patterns** (`/users/{id}`, `/files/{path...}`). There is no second syntax to learn.

## Registering routes

```go
app := web.New()

app.GET("/users/{id}", showUser)      // POST / PUT / PATCH / DELETE / HEAD / OPTIONS too
app.Handle("GET /users", listUsers)   // method inside the pattern (stdlib 1.22+ style)
app.Handle("/debug/{path...}", debug) // any method
```

- A handler is always **`func(*web.Ctx) error`**. Whether you write a response is up to you; **errors travel by return value** and the error mapper decides the status code (see [the error model](/en/guide/errors)).
- `Engine` satisfies `http.Handler`; `ServeHTTP` / `Handler()` let you drop it anywhere an `http.Handler` is expected (see the end of this page).

## Path parameters

```go
app.GET("/users/{id}/posts/{slug}", func(c *web.Ctx) error {
    return c.Text(200, c.Path("id")+"/"+c.Path("slug"))
})
```

`c.Path(name)` is `c.Request().PathValue(name)` (the same value, not a copy). A stdlib handler brought in through `Wrap` cannot see `*Ctx`, but it can still use `r.PathValue("id")` — same source.

## Groups

```go
api := app.Group("/api/v1", requireAuth)   // prefix + group middleware
api.GET("/orders", listOrders)             // → GET /api/v1/orders
api.GET("/orders/{id}", showOrder)

admin := api.Group("/admin")               // nesting works
admin.Use(requireAdmin)                    // affects /api/v1/admin only
```

A group is an **independent view**: middleware registered with `Use` on a group applies to that group and its children, and never leaks to other routes. Prefix joining and the middleware stack are resolved at registration time — nothing extra happens per request.

## Middleware

```go
type Handler func(*Ctx) error
type Middleware func(Handler) Handler
```

Three places to attach one, outermost first:

```go
app.Use(globalMW)                                     // 1. global: every route
app.Group("/admin", groupMW).GET("/x", h)             // 2. group
app.GET("/y", h, routeMW)                             // 3. a single route
```

**Order** is global → group → route, and repeated `Use` calls keep their call order. Returning an error short-circuits the chain; writing a response before `next(c)` does too (later writes hit the "a written response is never overwritten" rule).

stdlib middleware plugs in through `web.Wrap`:

```go
app.Use(func(next web.Handler) web.Handler {
    return web.Wrap(myStdlibMiddleware(stdlibHandler(next)))   // or simply: Wrap the whole handler
})
```

## Request body limits (`BodyLimit`)

`BodyLimit(n)` is a **middleware**, so it attaches to a group or to a single route:

```go
app := web.New(web.WithMaxBodyBytes(2 << 20))                       // global gate
app.Group("/upload", web.BodyLimit(100<<20)).POST("/avatar", h)     // wider on this branch
app.POST("/api/export", h, web.BodyLimit(1<<20))                    // tighter on this route
```

- **It can only tighten, never widen**: the engine wraps the body first, the route wraps it again, and the **smaller** limit wins. `BodyLimit(1<<20)` on a route under `WithMaxBodyBytes(1<<10)` is still capped at 1 KiB.
- **`n <= 0` means no limit** (pass-through, no wrapper); `WithMaxBodyBytes(0)` is the same, i.e. unlimited by default.
- Exceeding a limit produces **413 + `body_too_large`** with a `cause` of `*http.MaxBytesError` — both paths (fast failure on a declared length, or the reading gate) produce the same shape, so a custom mapper can match that one type with `errors.As`. Boundary: a body of **exactly n bytes passes**, n+1 is 413.
- **The reading gate is lazy**: it only fires when the handler actually reads the body.
- Details and measurements live in the [design document](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md), section "request body limits".

## Static files

```go
app.Static("/static", "./files")
```

`Static` goes through **the same registration path as ordinary routes**: static assets pass through global and group middleware (auth, CORS, rate limiting get no exemption) and still appear in traces and access logs. The prefix is registered as `<prefix>/`, and 404 / 405 are left to `http.FileServer`.

::: warning Do not shadow it with a wildcard pattern
Registering `GET /static/{file...}` **covers** the static service (method-qualified patterns win). To expose an individual path, use a literal — literals beat wildcards — e.g. `app.GET("/static/health", h)`.
:::

## The boundary with the standard library

| Direction | How |
|---|---|
| stdlib → framework | `web.Wrap(h http.Handler) web.Handler` |
| framework → stdlib | `app.Handler()` returns an `http.Handler` (or use `app` itself as one) |

After `Wrap` you **cannot reach `*Ctx`** (no `c.Path`, no request-scoped KV, no `c.Observe`), but `PathValue`, the middleware chain, tracing and the access log all still apply. So: if you need `*Ctx` capabilities, write `func(*web.Ctx) error`; reach for `Wrap` only when what you have is "just an http.Handler".
