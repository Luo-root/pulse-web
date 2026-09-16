# Responses: writing, streaming and templates

A handler can write a response in three ways: structured (`JSON` / `Text` / `HTML`), raw bytes (`Writer`), or streaming (`Flush`). The status code and headers stay editable until the **first flush**.

## Structured output

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
    return c.JSON(200, web.H{             // web.H is a type alias for map[string]any
        "id":   c.Path("id"),
        "name": "jiangnan",
    })
})

app.GET("/healthz", func(c *web.Ctx) error {
    return c.Text(200, "ok")
})
```

- `c.JSON(code, v)` **encodes into a memory buffer first and only then writes the headers** — an encoding failure never leaves a 200 with an empty body. The price is **+2 allocs / +97 B per response** (numbers on the [performance page](/en/performance)), and the output does not stream.
- `c.Text(code, s)` sets `text/plain; charset=utf-8`; `c.JSON` sets `application/json`.

## Status codes and headers

```go
app.POST("/users", func(c *web.Ctx) error {
    c.Status(202)                       // sets, does not write the header yet
    c.SetHeader("Location", "/users/42")
    c.SetHeader("X-Trace", c.TraceID())
    return c.JSON(202, created)         // this code overrides the Status above
})
```

**The first flush** is the first actual write, and it can come from any of three places: `c.Writer().Write`, `c.Flush()`, or the engine's own wrap-up after the handler returns. Once it happens the headers are out and `Status` no longer has an effect. That is why the natural streaming order — `Status(201)`, write the first chunk, `Flush` — works: all three paths share one implementation, so a `Status` never gets eaten by the implicit 200 of the first `Write`.

## Writing bytes: `Writer`

```go
app.GET("/raw", func(c *web.Ctx) error {
    w := c.Writer()                     // a wrapper around http.ResponseWriter
    w.Header().Set("X-Custom", "1")     // same as c.SetHeader
    _, err := w.Write([]byte("chunk"))
    return err
})
```

The wrapper still records the status code and the response size (so `size=` in the access log is just as accurate on this path).

::: warning `Writer()` promises `http.Flusher` and nothing else
Underlying capabilities (`Hijacker` / `Pusher` / `FlushError` / `SetWriteDeadline`) are **not surfaced** — the embedded interface only promotes `Header` / `Write` / `WriteHeader`. So `http.NewResponseController(c.Writer())` can only `Flush()`; `Hijack()` / `SetWriteDeadline()` return `http.ErrNotSupported`. **To upgrade the protocol (WebSocket), wrap a stdlib handler with `web.Wrap`** — the price is losing `*Ctx`.
:::

## Streaming: SSE

```go
app.GET("/events", func(c *web.Ctx) error {
    c.SetHeader("Content-Type", "text/event-stream")
    c.SetHeader("Cache-Control", "no-cache")
    c.Status(200)
    if err := c.Flush(); err != nil {     // first flush settles the status; the client starts reading
        return err
    }
    for i := 0; i < 5; i++ {
        if _, err := c.Writer().Write([]byte("data: tick\n\n")); err != nil {
            return err
        }
        if err := c.Flush(); err != nil {
            return err
        }
        time.Sleep(time.Second)
    }
    return nil
})
```

When the underlying writer does not support `http.Flusher`, `c.Flush()` returns an explicit error rather than failing silently (`ServeHTTP`'s default writer supports it).

::: tip Keep SSE from being cut off — look at `ServerConfig.WriteTimeout`
It defaults to **0 (unlimited)**, deliberately, because a non-zero timeout cuts long-lived streams. The flip side: **for non-streaming services, set it to 30s explicitly** — see [assembly and running](/en/guide/assembly).
:::

## Templates

A thin wrapper around the standard library's `html/template` (no template engine of our own):

```go
app := web.New(web.WithTemplates(web.TemplateConfig{
    Root:      "./views",   // template root
    Pattern:   "*.html",    // glob pattern (the default)
    DevReload: false,       // true = re-parse on every request (development only)
}))

app.GET("/", func(c *web.Ctx) error {
    return c.HTML(200, "index.html", web.H{
        "Title": "Home",
        "User":  user,
    })
})
```

- In **production mode** templates are parsed once at startup and cached (concurrency-safe). `DevReload: true` re-runs `ParseGlob` per request (a disk scan plus a write lock on every request) — **development only**.
- All three failure paths in `c.HTML` (no templates configured, unknown template name, execution error) **return an explicit error** instead of silently rendering a 500 page: it renders into a buffer and only writes headers on success. The price is that template output does not stream (for streaming use `c.Writer()` + `c.Flush()`).
- `Content-Type: text/html; charset=utf-8` is set automatically.

To register custom template functions, use the standard library's `Funcs` on the templates under `Root` — the framework does not wrap that layer.
