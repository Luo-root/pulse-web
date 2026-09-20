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
- `c.Blob(code, contentType, b)` writes raw bytes with the `Content-Type` **written as given** (no charset added, no sniffing) — for images, protobuf, exported files, anything where the type is the caller's call.

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

An empty response (204 / 304) is `c.NoContent(code)` — the same source as `c.Status(code)`: it only sets the status code and leaves the settling to the first flush, so a later `return`ed error is still taken over by the error mapper. The only difference is readability.

Cookies go through `c.SetCookie(&http.Cookie{Name: "sid", Value: v})`, backed by the standard library's `http.SetCookie`: **the same name appends** (one response can carry several `Set-Cookie` headers), and illegal bytes in the value are dropped by the standard library with a log line. To read a request-side cookie use `c.Cookie(name)` — see [requests](/en/guide/requests).

## Redirects

```go
app.GET("/old", func(c *web.Ctx) error {
    return c.Redirect(301, "/new")        // backed by http.Redirect
})

app.GET("/tenant", func(c *web.Ctx) error {
    return c.Redirect(302, "dashboard")   // relative: resolved against the request path
})
```

The semantics are the standard library's, word for word: a relative path is resolved against the **request path**, non-ASCII is escaped to `%XX`, and a GET request gets a short HTML hint body (HEAD and POST do not); `code` is not range-checked, and a non-3xx value is written as given.

**The difference from `Status` is when the code settles** — `Redirect` sends the headers right away, so a later `return`ed error is *not* taken over by the error mapper. Validate before you redirect.

## Files and downloads

```go
app.GET("/report", func(c *web.Ctx) error {
    return c.File("./data/report.csv")      // backed by http.ServeFile
})

app.GET("/report/download", func(c *web.Ctx) error {
    return c.Attachment("./data/report.csv", "2026-09-report.csv")
})
```

- `c.File(path)` takes the whole `http.ServeFile` semantics: Range, `If-Modified-Since` / `If-None-Match`, `Content-Type` sniffed from the extension and the content, and the 301 when a directory hits `index.html`.
- `c.Attachment(path, name)` adds `Content-Disposition: attachment` on top. `name` is encoded by `mime.FormatMediaType` — **non-ASCII names go through RFC 2231** (`filename*=utf-8''…`), the only way a Chinese filename survives the trip; hand-quoting it (`filename="中文.csv"`) is mojibake on some clients.

::: warning The file helpers **do not go through the error mapper**
When the file is missing, the standard library writes its own 404 (a plain-text page), and 403 when it is unreadable — **not** through the framework's error mapping, and with no error attribute in the observability record. If you want a uniform error body, `os.Stat` first and return `web.NotFound`.
:::

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

::: warning `Writer()`'s capability surface is an **explicit short list**
**Four are implemented and forwarded to the underlying writer**: `Flush` / `Hijack` / `SetWriteDeadline` / `EnableFullDuplex`. **Not surfaced**: `Pusher` / `FlushError` — and there is **no `Unwrap()`** either (that would hand the whole underlying writer over, those two included). The embedded `http.ResponseWriter` only promotes `Header` / `Write` / `WriteHeader`; every other capability has to be implemented deliberately — so the list is the whole surface, visible at a glance instead of depending on a lucky assertion.

**Protocol upgrades (WebSocket) go through `Hijack`**: `w := c.Writer().(http.Hijacker)` is that connection, and ecosystem libraries work unchanged (`gorilla/websocket` asserts `w.(http.Hijacker)` directly; `coder/websocket` asserts first and then walks the `Unwrap()` chain — both are satisfied). Once the connection is handed over the framework no longer writes this response (`Write` / `Flush` return `http.ErrHijacked`), and the connection can be **handed over only once** (a second `Hijack` returns the same error); it settles no status and writes no fallback error body; the access log records `101` with a `connection.hijacked` marker (an extension key this framework declares) and **does not** record `http.response.body.size` — the connection is gone, and a 0 would read as "the response was empty".

**Two boundaries**: ① **unavailable over HTTP/2** (the h2 writer is not a `Hijacker`); ② **available inside the onion, but at the mercy of middleware** — the proxy `Adapt` hands out forwards `Hijack` (upgrade routes behind `Use(Adapt(…))` keep working), while middleware that wraps the writer again without forwarding it breaks the same way it would in plain stdlib (see [stdlib middleware](/en/guide/middleware)).
:::

### WebSocket: a minimal `gorilla/websocket` example

```go
import (
    "net/http"

    "github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool { return true },   // deny-by-default: set your own policy
}

app.GET("/ws", func(c *web.Ctx) error {
    conn, err := upgrader.Upgrade(c.Writer(), c.Request(), nil)
    if err != nil {
        // gorilla already wrote the error response; just hand the error out
        return err
    }
    defer func() { _ = conn.Close() }()

    for {
        mt, data, err := conn.ReadMessage()
        if err != nil {
            return nil // client gone: ordinary ending
        }
        if err := conn.WriteMessage(mt, data); err != nil {
            return err
        }
    }
})
```

What `c.Writer()` hands to `Upgrade` is that connection itself — gorilla takes it with `w.(http.Hijacker)`, with no adapter in between. This example mirrors the handshake the repo actually runs (`interop/websocket_test.go`, `TestGorillaWebSocketUpgrade`).

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
