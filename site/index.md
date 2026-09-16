---
layout: home

hero:
  name: Pulse-Web
  text: 通用 Go web 框架
  tagline: 装配内核 + 一等观测。每个请求一条 32hex TraceID，核心模块零第三方依赖。
  actions:
    - theme: brand
      text: 快速开始
      link: /guide/getting-started
    - theme: alt
      text: 观测怎么用
      link: /guide/observability

features:
  - title: 装配内核
    details: 直接站在 pulse 的 kernel 上——IoC、可逆生命周期、请求作用域、事件总线。装配可组合，关掉的是真的关掉。
  - title: 一等观测
    details: 默认出口把每条记录渲染成一行给人读的列式输出；同一个 TraceID 贯穿 handler、访问日志与下游调用。
  - title: 零第三方依赖
    details: 只用标准库与两个 pulse 包（kernel、observability）。判据是真正编译进去的东西，不是 go.mod 里列了什么。
  - title: 标准库互操作
    details: 入用 web.Wrap(...)，出用 Engine.Handler()——与 net/http 双向兼容，不逼你换一套生态。
  - title: 错误语义收口
    details: handler 返回 error，状态码由统一的错误映射器决定；同一个决定进访问日志，测试里断言的也是它。
  - title: 流式响应可用
    details: 响应写出器只承诺 http.Flusher——SSE 能写能刷，接口有意收窄，不放 Hijacker 那类后门。
---

## 默认出口长这样

不需要配任何东西：`New()` 起服务之后，stdout 就已经每请求一行。

```text
PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378
```

- [快速开始](/guide/getting-started)——五行代码起一个服务，第二次运行就能看到上面这行
- [观测怎么用](/guide/observability)——默认出口、四种出口怎么选、什么时候该包 `AsyncSink`
- [设计文档](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md)——定位、决策、运行时契约与全部实测数据的事实源
