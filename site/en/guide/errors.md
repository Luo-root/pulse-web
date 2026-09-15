# The error model

Handlers do not write status codes — they **return an error**, and an **error mapper** decides the status, what gets logged, and how much is exposed. This page covers how to use that contract and where its edges are.

## The basic shape

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
    u, err := loadUser(c.Path("id"))
    switch {
    case errors.Is(err, errNotFound):
        return web.NotFound("user", err)     // 404, application code "user"
    case err != nil:
        return err                           // unknown → 500 (the original error only reaches the log)
    }
    return c.JSON(200, u)
})
```

**What you return is what you get**: handlers never touch status codes, and tests assert on the same returned value.

## `HTTPError` and its constructors

```go
type HTTPError struct {
    Status  int    // HTTP status code
    Code    string // machine-readable application code
    Message string // human-readable message (defaults to the status text)
}
func (e *HTTPError) Error() string   // the message
func (e *HTTPError) Unwrap() error   // the original error (cause)
func (e *HTTPError) StatusCode() int // implements StatusCoder
```

Seven constructors, all `(code string, cause error)`:

| Constructor | Status | Typical use |
|---|---|---|
| `web.BadRequest(code, cause)` | 400 | malformed input |
| `web.Unauthorized(code, cause)` | 401 | not authenticated |
| `web.Forbidden(code, cause)` | 403 | authenticated but not allowed |
| `web.NotFound(code, cause)` | 404 | resource missing |
| `web.Conflict(code, cause)` | 409 | concurrency / uniqueness conflict |
| `web.TooLarge(code, cause)` | 413 | body over the limit |
| `web.Internal(code, cause)` | 500 | server-side failure |

For 429 / 422 / 503 and friends, either **implement `StatusCoder`** (any `error` carrying `StatusCode() int`) or construct one directly: `&web.HTTPError{Status: 429, Code: "rate_limited"}`.

```go
type rateLimited struct{ retry int }
func (e rateLimited) Error() string   { return "rate limited" }
func (e rateLimited) StatusCode() int { return 429 }

app.GET("/api", func(c *web.Ctx) error {
    return rateLimited{retry: 30}      // 429, message goes through the default scrubbing
})
```

## Safe defaults

- **The `cause` only reaches the observability record, never the response body.** The body is always shaped like `{"error": {"code": "user", "message": "not found"}}`.
- **Any plain error that is not an `HTTPError` is a 500** with a generic message — internal wording is never leaked to the caller.
- The message defaults to the standard text for the status code; pass a message explicitly via `&web.HTTPError{...}` when you need your own.

## Panics

The engine catches panics and wraps them as `PanicError{Value, Stack}` — **always a 500**, with the stack going only to the record.

::: warning Trap: panicking with an HTTPError does not produce that status
`panic(web.Unauthorized("x", nil))` yields **500**, not 401. To get a 4xx, **return** it. This is deliberate: a panic is an exceptional path, not control flow.
:::

Panicking after the response has been written only records the event; it does not rewrite the response (a written response is never overwritten).

## A custom mapper

```go
app := web.New(web.WithErrorHandler(func(c *web.Ctx, err error) error {
    var maxErr *http.MaxBytesError
    if errors.As(err, &maxErr) {
        c.SetHeader("X-Limit", strconv.FormatInt(maxErr.Limit, 10))
    }
    var he *web.HTTPError
    if errors.As(err, &he) && he.Status == 400 {
        c.Observe("bad_request", func(a *observability.Attrs) {
            observability.Set(a, "path", c.Path("id"))
        })
    }
    // returning nil means "handled"; returning err hands it back to the default mapper
    return nil
}))
```

The `*Ctx` a mapper receives **can still** call `Get` / `Set` / `Observe` / `TraceID()` / `Path` / `Query()` — the KV hangs off `Ctx` and outlives the request scope. But the scope behind `c.Kernel()` is already collected, so **`c.Service` is a dead scope at that point** (put whatever you need into the KV earlier, or reach the application repository another way).

`ErrorHandler` composes like middleware: return `nil` for "I handled it", return an error for "keep going through the default mapper". The full default rules (including the precedence between 415 / 413 / panics) live in the [design document](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md), section "error mapper contract".

## How this relates to the access log

One decision shows up in two places: the response **and** the access log — status code and `Code` are both on the [observability](/en/guide/observability) line. So when you debug, you do not have to guess: take the trace id and read that line.
