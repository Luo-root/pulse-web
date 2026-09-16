# Getting started

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

## A service

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

::: tip Path parameters have no second copy
`c.Path("id")` hands back exactly what `net/http` wrote into the request — `{id}` is a stdlib ServeMux pattern, and the framework stores no second copy of it.
:::

## You configured nothing, yet observability is already running

That `curl` leaves a line like this on stdout:

```text
PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378
```

The default sink is `web.ConsoleSink` (stdout): one line per request, fixed column widths, meant for humans. That same `trace=` is available to you — call `c.TraceID()` inside the handler:

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
	log.Printf("trace=%s", c.TraceID()) // the same value as trace= above
	return c.JSON(http.StatusOK, web.H{"id": c.Path("id")})
})
```

The leading `PULSE` is the upstream default (`observability.DefaultLinePrefix`): pulse and pulse-web share one process tree and one line format, so `grep PULSE` pulls every line from both.

## Next

- [Using observability](/en/guide/observability) — the default sink, choosing between sinks, the cost of `AsyncSink`
- [Design document](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md) — runtime contracts, shutdown ordering, measured performance
- [README](https://github.com/Luo-root/pulse-web#readme) — the one-page conclusions and entry points
