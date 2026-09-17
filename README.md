<div align="center">
  <a href="https://luo-root.github.io/pulse-web/"><img alt="Pulse-Web" src="assets/banner.svg" width="353"></a>
</div>

<div align="center">
  <a href="https://go.dev/"><img alt="Go 1.27.0" src="https://img.shields.io/badge/Go-1.27.0-blue.svg"></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-green.svg"></a>
  <a href="https://github.com/Luo-root/pulse-web/releases/latest"><img alt="Release: v0.1.0" src="https://img.shields.io/github/v/release/Luo-root/pulse-web?label=release&amp;color=2563eb&amp;sort=semver"></a>
  <a href="https://github.com/Luo-root/pulse-web/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/Luo-root/pulse-web/actions/workflows/ci.yml/badge.svg"></a>
  <a href="#install"><img alt="Deps: standard library + two pulse packages" src="https://img.shields.io/badge/deps-stdlib%20%2B%202%20packages-2563eb.svg"></a>
  <a href="https://luo-root.github.io/pulse-web/"><img alt="Docs: English | 中文" src="https://img.shields.io/badge/docs-English%20%7C%20%E4%B8%AD%E6%96%87-2563eb.svg"></a>
</div>

<br />

**English** | [中文](README_zh.md)

A general-purpose Go web framework built on the **kernel** and **observability** packages of [pulse](https://github.com/Luo-root/pulse) — a **service substrate with an IoC lifecycle and first-class observability**, for services where assembling the thing is the hard part: many components, parts of the tree reloaded without a restart, one uniform view of what is running.

- **Assembly kernel** — IoC, reversible lifecycle, request scope and an event bus, from pulse's kernel
- **Observability as a first-class citizen** — a per-request 32-hex TraceID, structured records and assembly diagnostics, wired in by default; the default sink renders each record as one human-readable, column-aligned line
- **Zero third-party dependencies** — the standard library plus two upstream packages (kernel, observability)

## Install

```bash
go get github.com/Luo-root/pulse-web
```

Requires **Go 1.27+** (`go.mod` pins `go 1.27.0`; the toolchain downloads itself when missing).

The core module depends on nothing but the standard library and two `pulse` packages. The check is what actually gets compiled in, not what `go.mod` lists:

```bash
go list -deps . | grep -E '^[^/]+\.[^/]+/'
# github.com/Luo-root/pulse/kernel
# github.com/Luo-root/pulse/observability
# github.com/Luo-root/pulse-web
```

## Quick start

```go
package main

import (
	"net/http"

	"github.com/Luo-root/pulse-web"
)

func main() {
	app := web.New()

	app.GET("/users/{id}", func(c *web.Ctx) error {
		return c.JSON(http.StatusOK, web.H{"id": c.Path("id")})
	})

	app.Run(":8080")
}
```

```bash
go run .
curl -s localhost:8080/users/42
# {"id":"42"}
```

No configuration was needed for observability: stdout already carries one line per request, and the same TraceID reaches the handler, the log and any sink you attach later. The `PULSE` prefix at the start of the line is the upstream default — pulse and pulse-web share one process tree and one line format, so their output stays consistent when both are present.

```text
PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378
```

## Usage

### Routing and groups

Path parameters are read with `c.Path(name)`, which is the same value `net/http` wrote into the request — the framework stores no second copy.

```go
app.GET("/users/{id}", h)                       // {id} is a stdlib ServeMux pattern
app.POST("/users", h)
api := app.Group("/api/v1")                     // prefix + middleware, inherited by everything below
api.GET("/users/{id}", h)
app.Static("/assets", "./public")               // static files go through global and group middleware too
```

### Middleware

A middleware takes the request context and the rest of the chain. Execution is a plain onion: each layer's code before `next` runs on the way in, and after it on the way out.

```go
func auth(c *web.Ctx, next web.Handler) error {
	if c.Query("token") == "" {
		return web.Unauthorized("missing_token", nil)
	}
	return next(c)
}

app.Use(auth)                                   // global
app.POST("/admin/purge", purge, BodyLimit(1<<10)) // per route
```

### Request binding and body limits

`c.Bind` dispatches on `Content-Type` (JSON, XML, form-urlencoded, multipart) and falls back to the query string when the request has no body. Failures come back as mapped errors: 400 `invalid_body`, 413 `body_too_large`, 415 `unsupported_media_type`.

```go
type CreateUser struct {
	Name  string   `json:"name" form:"name"`
	Email string   `json:"email" form:"email"`
	Tags  []string `form:"tags"`                 // ?tags=a&tags=b
}

app.POST("/users", func(c *web.Ctx) error {
	var in CreateUser
	if err := c.Bind(&in); err != nil {
		return err
	}
	return c.JSON(http.StatusCreated, in)
})
```

The request body has **no size limit by default** — a limit is a business policy. Set one globally, or tighten it per group/route:

```go
app := web.New(web.WithMaxBodyBytes(2 << 20))                    // global ceiling: 2 MiB
app.Group("/admin", web.BodyLimit(1<<20)).POST("/settings", h)   // this group tightens to 1 MiB
app.POST("/api/export", h, web.BodyLimit(512<<10))               // this route tightens to 512 KiB
```

A route limit can only **tighten** the global one, never loosen it — the global gate runs in `ServeHTTP`, ahead of routing and middleware, so **raising the ceiling is the only way to allow a bigger body**. The boundary is exact: a body of exactly `n` bytes passes, `n+1` is rejected. Over-limit requests are always 413 + `body_too_large` with a `*http.MaxBytesError` cause, so one `errors.As` in a custom `WithErrorHandler` covers every limit path.

### Responses and errors

Handlers return an `error`; they do not write status codes by hand. The engine's error mapper decides the status, and the same decision goes into the access log.

```go
return c.JSON(http.StatusOK, user)              // encodes first, writes the header only on success
return c.Text(http.StatusOK, "pong")
return c.HTML(http.StatusOK, "user.html", web.H{"user": user})
return web.NotFound("user", err)                // 404 + code "user", cause kept out of the response
```

Built-in constructors: `BadRequest`, `Unauthorized`, `Forbidden`, `NotFound`, `Conflict`, `TooLarge`, `Internal`. A panic in a handler becomes a 500; the panic value and stack stay in the process (they are attached to the observation record, never sent to the client).

### Streaming (SSE)

Write to `c.Writer()` and call `c.Flush()` to push each chunk out. The response writer is a deliberately narrow interface: it promises `http.Flusher` and nothing else.

```go
app.GET("/events", func(c *web.Ctx) error {
	c.SetHeader("Content-Type", "text/event-stream")
	c.SetHeader("Cache-Control", "no-cache")
	for ev := range events {
		if _, err := fmt.Fprintf(c.Writer(), "data: %s\n\n", ev); err != nil {
			return nil                          // client gone: response already started
		}
		if err := c.Flush(); err != nil {
			return err
		}
	}
	return nil
})
```

Status codes and response size are recorded as usual — whatever you write is what gets counted.

### Static files and templates

```go
app.Static("/assets", "./public")

app := web.New(web.WithTemplates(web.TemplateConfig{
	Root:      "templates",
	Pattern:   "*.html",                        // default
	DevReload: false,                           // true re-parses on every request (development only)
}))
// handlers: c.HTML(200, "user.html", web.H{"user": user})
```

Templates are a thin wrapper over `html/template`; there is no template engine of our own to learn.

### Testing

Handlers can be unit-tested without starting a server. The entry points share the request assembly with the real path — the same code — so how the status settles, error mapping and the access log behave exactly as they do in production.

```go
rec := httptest.NewRecorder()
req := httptest.NewRequest("GET", "/users/42", nil)
req.Pattern = "GET /users/{id}"     // route template and path parameters are yours to fill in
req.SetPathValue("id", "42")

c, done := app.NewTestContext(rec, req)
done(myHandler(c))                  // run, hand the error back, wrap up
```

`app.ServeTest(rec, req, handler, mw...)` covers the handler plus a middleware chain, taking over panics and the request-body gate as well. Neither entry point goes through `ServeMux`; the exact boundary — and when a real server is still the right answer — is in the [testing guide](https://luo-root.github.io/pulse-web/en/guide/testing).

## Observability

Every request gets a 32-hex TraceID, one `http.request` record, and a request scope that is disposed as soon as the handler returns — before the engine maps the outcome and writes anything further.

`New()` already installs a sink: `ConsoleSink` — one human-readable column-aligned line per request, written to stdout.

```go
app := web.New(web.WithHostID("orders-api"))   // ConsoleSink → stdout is already the default
```

`WithSink` replaces it — **changing the sink changes nothing else in the assembly**. Pick by who reads the output:

```go
web.WithSink(web.NewConsoleSink(os.Stdout))                     // humans — the default: unbuffered, writes straight through
web.WithSink(observability.SlogSink{Logger: slog.Default()})    // machines — host logger, JSON, log collectors (stderr by default)
web.WithSink(observability.NewAsyncSink(inner))                 // throughput — the write leaves the request path
web.WithSink(observability.MultiSink{a, b})                     // several destinations at once
```

**If the sink is what your throughput is waiting on, wrap it in `NewAsyncSink`.** The observation cost sits almost entirely in getting the record out of the door, and the kernel dispatches events synchronously — so a slow sink is a slow request path. `AsyncSink` puts the write on a background worker behind a preallocated ring queue and hands the request goroutine straight back. What you pay is ownership: the queue is bounded (back-pressure by default, `DropOnFull()` to drop-and-count instead), and its lifecycle is yours — `Flush` or `Close` it on shutdown, or the queued records leave with the process.

`WithoutAccessLog()` turns the access record off for services that already log elsewhere; trace and panic recovery stay on.

- `c.TraceID()` — the same id in the handler, the access log and downstream calls
- `c.Observe("order.paid", func(a *observability.Attrs) { a.Set("order_id", id) })` — your own events land in the same sink as the framework's
- `web.WithCollector()` — attach the kernel collector to the request scope when you need per-request fiber visibility
- `web.WithSpanHook(hook)` — let your tracing stack own the request's span; the official OpenTelemetry adapter is the nested [`otel/`](otel/) module
- `web.WithRoot(root)` — attach an existing kernel tree; note that `Run` / `Serve` dispose it on return, so do not keep using it afterwards
- `app.Debug("/debug/assembly")` — a JSON view of the assembly state (snapshots, fibers, plugins)

A span hook is what makes a request a real span in your tracing backend. The framework never invents a span-id: your tracer allocates it and the framework adopts it, so the same real span appears in `Server-Timing`, in the access record's `span.id`, and in the `traceparent` your downstream clients send. That `Server-Timing` header is **not** `X-Trace-Id` — the latter is this framework's own header carrying the trace id only, while `Server-Timing` is the W3C-defined response binding and carries this request's span id (written only once a span exists).

Records hold scalars only (`~string | ~int64 | ~float64 | ~bool`) with no `map[string]any` escape hatch, so request bodies, headers and query strings cannot end up in your logs by construction. The default sink writes to stdout and is not a persistence layer — rotation and retention belong to your platform (containers, journald, or your own writer).

## Ecosystem and compatibility

`net/http` is not a competitor here, it is the substrate:

```go
app.Handle("/legacy", web.Wrap(http.HandlerFunc(oldHandler)))    // stdlib handler in
http.Handle("/app/", http.StripPrefix("/app", app.Handler()))    // the engine out, as an http.Handler
```

What it is not: a batteries-included micro-framework with a large middleware catalogue. If you want an ecosystem of community middleware and maximum familiarity for a team, gin, chi or echo are the obvious picks. Pulse-Web aims at services that would rather keep the dependency surface at "standard library plus two packages" and get first-class observability out of the box.

## Documentation

- [Documentation site](https://luo-root.github.io/pulse-web/) — guides, the observability walkthrough and the public performance numbers
- [Design document](docs/design/web-framework-design.md) — positioning, decisions, API surface, runtime contracts, observability design and an explicit "what we do not do" list, including the measured cost breakdown and the load-test comparison with gin
- [Issue #1](https://github.com/Luo-root/pulse-web/issues/1) — the design discussion where those decisions were settled

## Contributing

Non-trivial changes start as an issue; the rules are in [CONTRIBUTING.md](CONTRIBUTING.md) (bilingual).

- Security problems: **do not open a public issue** — report privately per [SECURITY.md](SECURITY.md)
- Community behaviour: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)
- Working on this repository with an AI coding agent: [AGENTS.md](AGENTS.md)

## License

[MIT](LICENSE)
