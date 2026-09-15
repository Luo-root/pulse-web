---
layout: home

hero:
  name: Pulse-Web
  text: A general-purpose Go web framework
  tagline: Assembly kernel plus first-class observability. One 32-hex TraceID per request, zero third-party dependencies.
  actions:
    - theme: brand
      text: Quick start
      link: /en/guide/getting-started
    - theme: alt
      text: Using observability
      link: /en/guide/observability

features:
  - title: Assembly kernel
    details: Sits directly on pulse's kernel — IoC, reversible lifecycle, request scope, event bus. Assembly composes, and what you switch off is really off.
  - title: Observability as a first-class citizen
    details: The default sink renders each record as one human-readable, column-aligned line; the same TraceID reaches the handler, the access log and downstream calls.
  - title: Zero third-party dependencies
    details: The standard library plus two pulse packages (kernel, observability). The test is what actually gets compiled in, not what go.mod lists.
  - title: stdlib interop
    details: web.Wrap(...) in, Engine.Handler() out — two-way compatible with net/http, no second ecosystem to adopt.
  - title: One error path
    details: Handlers return an error; the engine's error mapper decides the status, and the same decision lands in the access log.
  - title: Streaming works
    details: The response writer promises http.Flusher and nothing else — SSE writes and flushes behind a deliberately narrow interface.
---

## The default output

Nothing to configure: once `New()` is serving, stdout already carries one line per request.

```text
PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378
```

- [Quick start](/en/guide/getting-started) — five lines to a running service, and the line above on your second run
- [Using observability](/en/guide/observability) — the default sink, how to pick between sinks, when to wrap `AsyncSink`
- [Design document](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md) — the source of truth for positioning, decisions, runtime contracts and every measured number
