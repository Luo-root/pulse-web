---
layout: page
sidebar: false
---

<div class="pw-landing">

<section class="pw-hero">
  <img class="pw-hero-motif" src="/logo.svg" alt="" aria-hidden="true" />
  <p class="pw-badge">Go 1.27+ · 开源 MIT</p>
  <h1 class="pw-hero-title"><img class="pw-mark" src="/logo.svg" alt="" width="44" height="44" /><span class="pw-wordmark">Pulse-Web</span></h1>
  <p class="pw-claim">通用 Go web 框架，<span class="pw-accent">默认就带观测</span></p>
  <p class="pw-tagline">站在 pulse 的 kernel 与 observability 上：装配可组合，关掉的是真的关掉；stdout 上每请求一行给人读的访问日志，每请求一个贯穿全链路与下游调用的 32hex TraceID。</p>
  <p class="pw-actions">
    <a class="pw-btn pw-btn-primary" href="/guide/getting-started">快速开始</a>
    <a class="pw-btn" href="/guide/observability">观测怎么用</a>
    <a class="pw-btn" href="https://github.com/Luo-root/pulse-web">GitHub</a>
  </p>
</section>

<div class="pw-spec">
  <span><b>依赖</b>标准库 + 2 个 pulse 包</span>
  <span><b>Go</b>1.27+</span>
  <span><b>许可</b>MIT</span>
  <span><b>形态</b>库，无前端</span>
</div>

<section class="pw-section">
  <p class="pw-eyebrow">核心能力</p>
  <h2 class="pw-h2">把「装配」与「观测」做成一等公民</h2>
  <div class="pw-grid">
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="3" width="7.5" height="7.5" rx="1.4"/><rect x="13.5" y="3" width="7.5" height="7.5" rx="1.4"/><rect x="8.25" y="13.5" width="7.5" height="7.5" rx="1.4"/></svg></span>
      <h3>装配内核</h3>
      <p>直接站在 pulse 的 kernel 上——IoC、可逆生命周期、请求作用域、事件总线。装配可组合，关掉的是真的关掉；插件树从 10 涨到 100，每请求的分配计数一动不动。</p>
      <a class="pw-link" href="/guide/assembly">看装配 →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2.5 12S6 6.5 12 6.5 21.5 12 21.5 12 18 17.5 12 17.5 2.5 12 2.5 12Z"/><circle cx="12" cy="12" r="2.6"/></svg></span>
      <h3>一等观测</h3>
      <p>默认出口把每条记录渲染成一行给人读的列式输出，列宽固定、写完即落；同一个 32hex TraceID 贯穿 handler、访问日志与下游调用。</p>
      <a class="pw-link" href="/guide/observability">看观测 →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 3.2 19 6v5.6c0 4.3-2.9 7.7-7 9.2-4.1-1.5-7-4.9-7-9.2V6z"/></svg></span>
      <h3>零第三方依赖</h3>
      <p>只编译进标准库与两个 pulse 包（kernel、observability）。判据是编译产物，不是 <code>go.mod</code> 里列了什么。</p>
      <a class="pw-link" href="/guide/ops-contracts">看边界 →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3.5 8.5h13"/><path d="M13.5 5.5 16.5 8.5 13.5 11.5"/><path d="M20.5 15.5h-13"/><path d="M10.5 12.5 7.5 15.5 10.5 18.5"/></svg></span>
      <h3>标准库互操作</h3>
      <p>入用 <code>web.Wrap</code>，出用 <code>Engine.Handler()</code>——与 <code>net/http</code> 双向兼容，不逼你换一套生态。</p>
      <a class="pw-link" href="/guide/requests">看请求 →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3.5 5h17l-6.5 7.5V18l-4 2v-7.5z"/></svg></span>
      <h3>错误语义收口</h3>
      <p>handler 返回 error，状态码由统一的错误映射器决定；同一个决定进访问日志，测试里断言的也是它。</p>
      <a class="pw-link" href="/guide/errors">看错误 →</a>
    </div>
    <div class="pw-cell">
      <span class="pw-icon"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2 15c2.5 0 2.5-6 5-6s2.5 6 5 6 2.5-6 5-6 2.5 6 5 6"/></svg></span>
      <h3>流式响应可用</h3>
      <p>响应写出器只承诺 <code>http.Flusher</code>——SSE 能写能刷，接口有意收窄，不放 Hijacker 那类后门。</p>
      <a class="pw-link" href="/guide/responses">看响应 →</a>
    </div>
  </div>
</section>

<section class="pw-section">
  <p class="pw-eyebrow">开始使用</p>
  <h2 class="pw-h2">两条路进来</h2>
  <div class="pw-paths">
    <div class="pw-path">
      <h3>安装</h3>
      <pre class="pw-pre">go get github.com/Luo-root/pulse-web</pre>
      <p>需要 Go 1.27+（<code>go.mod</code> 写 <code>go 1.27.0</code>，工具链缺失会自动下载）。核心模块只编译进标准库与两个 pulse 包。</p>
    </div>
    <div class="pw-path">
      <h3>最小可跑</h3>
      <pre class="pw-pre">app := web.New()
app.GET("/users/{id}", func(c *web.Ctx) error {
	return c.JSON(http.StatusOK, web.H{"id": c.Path("id")})
})
app.Run(":8080")</pre>
      <p>没有配任何观测项——起服务之后，日志就已经每请求一行。<a class="pw-link" href="/guide/getting-started">完整示例 →</a></p>
    </div>
  </div>
</section>

<section class="pw-section">
  <p class="pw-eyebrow">默认输出</p>
  <h2 class="pw-h2">什么都没配，日志已经在 stdout 上</h2>
  <div class="pw-terminal">
    <p class="pw-terminal-head"><span class="pw-tag">stdout</span>一次 <code>GET /users/42</code> 的访问日志——列宽固定，写完即落</p>
    <pre class="pw-pre pw-pre-log">PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378</pre>
  </div>
</section>

<section class="pw-section pw-cta">
  <h2 class="pw-h2">数字、口径与决策都在仓库里</h2>
  <p class="pw-tagline">性能页带同轮对照行与复现命令；设计文档是契约、决策与验收清单的事实源。</p>
  <p class="pw-actions">
    <a class="pw-btn" href="/performance">性能数据</a>
    <a class="pw-btn" href="https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md">设计文档</a>
    <a class="pw-btn" href="https://github.com/Luo-root/pulse-web/blob/main/CONTRIBUTING.md">贡献指南</a>
  </p>
</section>

</div>
