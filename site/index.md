---
layout: page
sidebar: false
---

<div class="pw-landing">

<div class="pw-hero">
  <p class="pw-lockup"><img class="pw-mark" src="/logo.svg" alt="" width="44" height="44"><span class="pw-wordmark">Pulse-Web</span></p>
  <h1 class="pw-headline">通用 Go web 框架</h1>
  <p class="pw-tagline">装配内核 + 一等观测。默认出口每请求一行、每请求一个 32hex TraceID，核心模块零第三方依赖。</p>
  <p class="pw-actions"><a class="pw-btn pw-btn-primary" href="/guide/getting-started">快速开始</a><a class="pw-btn" href="/guide/observability">观测怎么用</a></p>
</div>

<div class="pw-spec">
  <span><b>依赖</b>标准库 + 2 个 pulse 包</span>
  <span><b>Go</b>1.27+</span>
  <span><b>许可</b>MIT</span>
  <span><b>形态</b>库，无前端</span>
</div>

<div class="pw-grid">
  <div class="pw-cell">
    <span class="pw-num">01</span>
    <h3>装配内核</h3>
    <p>直接站在 pulse 的 kernel 上——IoC、可逆生命周期、请求作用域、事件总线。装配可组合，关掉的是真的关掉；每请求开销与插件树规模无关（100 插件实测 394 ns）。</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">02</span>
    <h3>一等观测</h3>
    <p>默认出口把每条记录渲染成一行给人读的列式输出，列宽固定、写完即落。同一个 TraceID 贯穿 handler、访问日志与下游调用。</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">03</span>
    <h3>零第三方依赖</h3>
    <p>只用标准库与两个 pulse 包（kernel、observability）。判据是真正编译进去的东西，不是 <code>go.mod</code> 里列了什么。</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">04</span>
    <h3>标准库互操作</h3>
    <p>入用 <code>web.Wrap</code>，出用 <code>Engine.Handler()</code>——与 <code>net/http</code> 双向兼容，不逼你换一套生态。</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">05</span>
    <h3>错误语义收口</h3>
    <p>handler 返回 error，状态码由统一的错误映射器决定；同一个决定进访问日志，测试里断言的也是它。</p>
  </div>
  <div class="pw-cell">
    <span class="pw-num">06</span>
    <h3>流式响应可用</h3>
    <p>响应写出器只承诺 <code>http.Flusher</code>——SSE 能写能刷，接口有意收窄，不放 Hijacker 那类后门。</p>
  </div>
</div>

<div class="pw-terminal">
  <p class="pw-terminal-head"><span class="pw-num">默认输出</span>什么都没配，<code>New()</code> 起服务之后 stdout 就已经每请求一行</p>
  <pre class="pw-pre">PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378</pre>
</div>

<div class="pw-entries">
  <a href="/guide/getting-started"><span class="pw-num">A</span>快速开始<span class="pw-arrow">→</span></a>
  <a href="/guide/observability"><span class="pw-num">B</span>观测怎么用<span class="pw-arrow">→</span></a>
  <a href="/performance"><span class="pw-num">C</span>性能数据<span class="pw-arrow">→</span></a>
  <a href="https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md"><span class="pw-num">D</span>设计文档<span class="pw-arrow">→</span></a>
</div>

</div>
