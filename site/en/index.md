---
layout: page
sidebar: false
---

<div class="pw-landing">

<div class="pw-hero">
  <p class="pw-lockup"><img class="pw-mark" src="/logo.svg" alt="" width="44" height="44"><span class="pw-wordmark">Pulse-Web</span></p>
  <h1 class="pw-headline">A general-purpose Go web framework</h1>
  <p class="pw-tagline">Assembly kernel plus first-class observability. One line per request on stdout, one 32-hex TraceID per request, zero third-party dependencies in the core module.</p>
  <p class="pw-actions"><a class="pw-btn pw-btn-primary" href="/en/guide/getting-started">Get started</a><a class="pw-btn" href="/en/guide/observability">Using observability</a></p>
</div>

<div class="pw-spec">
  <span><b>Dependencies</b>standard library + 2 pulse packages</span>
  <span><b>Go</b>1.27+</span>
  <span><b>License</b>MIT</span>
  <span><b>Shape</b>a library, no frontend</span>
</div>

<div class="pw-grid">
  <div class="pw-cell">
    <span class="pw-num">01</span>
    <h3>Assembly kernel</h3>
    <p>Sits directly on pulse's kernel — IoC, reversible lifecycle, request scope, event bus. Assembly composes, and what you switch off is really off; per-request cost does not grow with plugin-tree size (394 ns at 100 plugins).</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">02</span>
    <h3>Observability as a first-class citizen</h3>
    <p>The default sink renders each record as one human-readable, column-aligned line, written straight through. The same TraceID reaches the handler, the access log and downstream calls.</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">03</span>
    <h3>Zero third-party dependencies</h3>
    <p>The standard library plus two pulse packages (kernel, observability). The test is what actually gets compiled in, not what <code>go.mod</code> lists.</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">04</span>
    <h3>stdlib interop</h3>
    <p><code>web.Wrap</code> in, <code>Engine.Handler()</code> out — two-way compatible with <code>net/http</code>, no second ecosystem to adopt.</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">05</span>
    <h3>One error path</h3>
    <p>Handlers return an error; the engine's error mapper decides the status. The same decision lands in the access log and in your tests.</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">06</span>
    <h3>Streaming works</h3>
    <p>The response writer promises <code>http.Flusher</code> and nothing else — SSE writes and flushes behind a deliberately narrow interface, with no Hijacker-style back doors.</p>
  </div>
</div>

<div class="pw-terminal">
  <p class="pw-terminal-head"><span class="pw-num">default output</span>Nothing configured — once <code>New()</code> is serving, stdout already carries one line per request</p>
  <pre class="pw-pre">PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378</pre>
</div>

<div class="pw-entries">
  <a href="/en/guide/getting-started"><span class="pw-num">A</span>Get started<span class="pw-arrow">→</span></a>
  <a href="/en/guide/observability"><span class="pw-num">B</span>Using observability<span class="pw-arrow">→</span></a>
  <a href="/en/performance"><span class="pw-num">C</span>Performance<span class="pw-arrow">→</span></a>
  <a href="https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md"><span class="pw-num">D</span>Design document<span class="pw-arrow">→</span></a>
</div>

</div>
