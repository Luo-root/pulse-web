---
layout: page
sidebar: false
---

<script setup>
import { withBase } from 'vitepress'
</script>

<div class="pw-landing">

<section class="pw-hero">
  <img class="pw-hero-motif" src="/logo.svg" alt="" aria-hidden="true" />
  <p class="pw-badge">Go 1.27+ · MIT</p>
  <h1 class="pw-hero-title"><img class="pw-mark" src="/logo.svg" alt="" width="44" height="44" /><span class="pw-wordmark">Pulse-Web</span></h1>
  <p class="pw-claim">A general-purpose Go web framework that <span class="pw-accent">ships observability</span></p>
  <p class="pw-tagline">Built on pulse's kernel and observability: assembly composes, and observability is part of the default assembly rather than a middleware you bolt on.</p>
  <p class="pw-actions">
    <a class="pw-btn pw-btn-primary" :href="withBase('/en/guide/getting-started')">Get started</a>
    <a class="pw-btn" :href="withBase('/en/guide/observability')">Using observability</a>
    <a class="pw-btn" href="https://github.com/Luo-root/pulse-web">GitHub</a>
  </p>
</section>

<div class="pw-spec">
  <span><b>Dependencies</b>standard library + 2 pulse packages</span>
  <span><b>Go</b>1.27+</span>
  <span><b>License</b>MIT</span>
</div>

<section class="pw-section">
  <p class="pw-eyebrow">Core capabilities</p>
  <h2 class="pw-h2">Assembly and observability, both first-class</h2>
  <div class="pw-grid">
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="7.5" height="7.5" rx="1.4"/><rect x="13.5" y="3" width="7.5" height="7.5" rx="1.4"/><rect x="8.25" y="13.5" width="7.5" height="7.5" rx="1.4"/></svg></span>
      <h3>Assembly kernel</h3>
      <p>Sits directly on pulse's kernel — IoC, reversible lifecycle, request scope, event bus. Assembly composes and what you switch off is really off; grow the plugin tree from 10 to 100 and the per-request allocation count does not move.</p>
      <a class="pw-link" :href="withBase('/en/guide/assembly')">Assembly →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 12S6 6.5 12 6.5 21.5 12 21.5 12 18 17.5 12 17.5 2.5 12 2.5 12Z"/><circle cx="12" cy="12" r="2.6"/></svg></span>
      <h3>Observability as a first-class citizen</h3>
      <p>The default sink renders each record as one human-readable, column-aligned line; the same 32-hex TraceID reaches the handler, the access log and downstream calls.</p>
      <a class="pw-link" :href="withBase('/en/guide/observability')">Observability →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3.2 19 6v5.6c0 4.3-2.9 7.7-7 9.2-4.1-1.5-7-4.9-7-9.2V6z"/></svg></span>
      <h3>Zero third-party dependencies</h3>
      <p>The standard library plus two pulse packages (kernel, observability) and nothing else. The test is what actually gets compiled in, not what <code>go.mod</code> lists.</p>
      <a class="pw-link" :href="withBase('/en/guide/ops-contracts')">Contracts →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3.5 8.5h13"/><path d="M13.5 5.5 16.5 8.5 13.5 11.5"/><path d="M20.5 15.5h-13"/><path d="M10.5 12.5 7.5 15.5 10.5 18.5"/></svg></span>
      <h3>stdlib interop</h3>
      <p><code>web.Wrap</code> in, <code>Engine.Handler()</code> out — two-way compatible with <code>net/http</code>, no second ecosystem to adopt.</p>
      <a class="pw-link" :href="withBase('/en/guide/requests')">Requests →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3.5 5h17l-6.5 7.5V18l-4 2v-7.5z"/></svg></span>
      <h3>One error path</h3>
      <p>Handlers return an error; the engine's error mapper decides the status. The same decision lands in the access log and in your tests.</p>
      <a class="pw-link" :href="withBase('/en/guide/errors')">Errors →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2 15c2.5 0 2.5-6 5-6s2.5 6 5 6 2.5-6 5-6 2.5 6 5 6"/></svg></span>
      <h3>Streaming works</h3>
      <p>The response writer promises <code>http.Flusher</code> and nothing else — SSE writes and flushes behind a deliberately narrow interface, with no Hijacker-style back doors.</p>
      <a class="pw-link" :href="withBase('/en/guide/responses')">Responses →</a>
    </div>
  </div>
</section>

<section class="pw-section">
  <p class="pw-eyebrow">Get started</p>
  <h2 class="pw-h2">Two ways in</h2>
  <div class="pw-paths">
    <div class="pw-path">
      <h3>Install</h3>
      <pre class="pw-pre">go get github.com/Luo-root/pulse-web</pre>
      <p>Requires Go 1.27+ (<code>go.mod</code> pins <code>go 1.27.0</code>; the toolchain downloads itself when missing).</p>
    </div>
    <div class="pw-path">
      <h3>Smallest runnable app</h3>
      <pre class="pw-pre">app := web.New()
app.GET("/users/{id}", func(c *web.Ctx) error {
	return c.JSON(http.StatusOK, web.H{"id": c.Path("id")})
})
app.Run(":8080")</pre>
      <p>Nothing observability-related is configured — once it serves, stdout already carries one line per request. <a class="pw-link" :href="withBase('/en/guide/getting-started')">Full example →</a></p>
    </div>
  </div>
</section>

<section class="pw-section">
  <p class="pw-eyebrow">Default output</p>
  <h2 class="pw-h2">Nothing configured, and the log is already on stdout</h2>
  <div class="pw-terminal">
    <p class="pw-terminal-head"><span class="pw-tag">stdout</span>the access log of one <code>GET /users/42</code> — fixed columns, written straight through</p>
    <pre class="pw-pre pw-pre-log">PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378</pre>
  </div>
</section>

<section class="pw-section pw-cta">
  <h2 class="pw-h2">Numbers, method and decisions all live in the repo</h2>
  <p class="pw-tagline">The performance page carries same-round control rows and reproduction commands; the design document is the source of truth for contracts, decisions and acceptance criteria.</p>
  <p class="pw-actions">
    <a class="pw-btn" :href="withBase('/en/performance')">Performance</a>
    <a class="pw-btn" href="https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md">Design document</a>
    <a class="pw-btn" href="https://github.com/Luo-root/pulse-web/blob/main/CONTRIBUTING.md">Contributing</a>
  </p>
</section>

</div>
