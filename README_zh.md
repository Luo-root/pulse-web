<div align="center">
  <a href="https://luo-root.github.io/pulse-web/"><img alt="Pulse-Web" src="assets/banner.svg" width="353"></a>
</div>

<div align="center">
  <a href="https://go.dev/"><img alt="Go 1.27.0" src="https://img.shields.io/badge/Go-1.27.0-blue.svg"></a>
  <a href="LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-green.svg"></a>
  <a href="https://github.com/Luo-root/pulse-web/releases/latest"><img alt="Release: v0.1.0" src="https://img.shields.io/github/v/release/Luo-root/pulse-web?label=release&amp;color=2563eb&amp;sort=semver"></a>
  <a href="https://github.com/Luo-root/pulse-web/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/Luo-root/pulse-web/actions/workflows/ci.yml/badge.svg"></a>
  <a href="#安装"><img alt="依赖：标准库 + 两个 pulse 包" src="https://img.shields.io/badge/deps-stdlib%20%2B%202%20packages-2563eb.svg"></a>
  <a href="https://luo-root.github.io/pulse-web/"><img alt="文档：English | 中文" src="https://img.shields.io/badge/docs-English%20%7C%20%E4%B8%AD%E6%96%87-2563eb.svg"></a>
</div>

<br />

[English](README.md) | **中文**

基于 [pulse](https://github.com/Luo-root/pulse) 的 **kernel** 与 **observability** 构建的通用 Go web 服务框架——**带 IoC 生命周期与一等观测的服务基座**，面向装配本身就是难点的那类服务：组件多、需要热更新、需要一份统一的运行观测。

- **装配内核**——kernel 的 IoC、可逆生命周期、请求作用域与事件总线
- **一等观测**——每请求一个 32hex TraceID、结构化记录与装配诊断，默认就已接好；默认出口把每条记录渲染成一行给人读的列式输出
- **零第三方依赖**——只用标准库与两个上游包（kernel、observability）

## 安装

```bash
go get github.com/Luo-root/pulse-web
```

需要 **Go 1.27+**（`go.mod` 写死 `go 1.27.0`；工具链缺失时会自动下载）。

核心模块只依赖标准库与两个 `pulse` 包。判据是**真正编译进去的东西**，不是 `go.mod` 里列了什么：

```bash
go list -deps . | grep -E '^[^/]+\.[^/]+/'
# github.com/Luo-root/pulse/kernel
# github.com/Luo-root/pulse/observability
# github.com/Luo-root/pulse-web
```

## 快速开始

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

观测不需要任何配置：stdout 已经每请求一行，同一个 TraceID 贯穿 handler、这行日志，以及你之后挂上的任何出口。行首的 `PULSE` 是上游的默认前缀——pulse 与 pulse-web 同根同源、共用一套版式，两者同处一个进程树时输出保持一致。

```text
PULSE | 2026/09/15 - 20:27:01 | 200 |   502.0µs | 127.0.0.1:54161 | GET     /users/42 | route=/users/{id} | size=12 | host=pulse-web | trace=a9c8e7496977e0ee6e8971a2b2cfe378
```

## 用法

### 路由与分组

路径参数用 `c.Path(name)` 读，取的就是 `net/http` 写进请求里的那一份——框架不另存第二份。

```go
app.GET("/users/{id}", h)                       // {id} 是 stdlib ServeMux 的模式
app.POST("/users", h)
api := app.Group("/api/v1")                     // 前缀 + 中间件，下面注册的都继承
api.GET("/users/{id}", h)
app.Static("/assets", "./public")               // 静态资源同样经过全局与分组中间件
```

### 中间件

中间件拿到请求上下文与链上剩下的部分。执行就是一个普通的洋葱模型：每层在 `next` 之前的代码在进入时执行，之后的代码在回程执行。

```go
func auth(c *web.Ctx, next web.Handler) error {
	if c.Query("token") == "" {
		return web.Unauthorized("missing_token", nil)
	}
	return next(c)
}

app.Use(auth)                                   // 全局
app.POST("/admin/purge", purge, BodyLimit(1<<10)) // 单条路由
```

### 请求绑定与上限

`c.Bind` 按 `Content-Type` 分派（JSON、XML、form-urlencoded、multipart），请求没有 body 时自动落到 query。失败按语义返回错误：400 `invalid_body`、413 `body_too_large`、415 `unsupported_media_type`。

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

请求体默认**不设上限**——上限值是业务策略。可以全局设一个，也可以按分组 / 路由收紧：

```go
app := web.New(web.WithMaxBodyBytes(2 << 20))                    // 全局兜底：2 MiB
app.Group("/admin", web.BodyLimit(1<<20)).POST("/settings", h)   // 这条分组收紧到 1 MiB
app.POST("/api/export", h, web.BodyLimit(512<<10))               // 这条路由收紧到 512 KiB
```

路由级闸门只能**收紧**全局那道，不能放宽——全局那道闸门在 `ServeHTTP` 里、先于路由与中间件，所以**想让某条路径接受更大的 body，唯一办法是抬高全局天花板**。边界是精确的：body 恰好 `n` 字节通过，`n+1` 被拒。超限一律 413 + `body_too_large`，cause 是 `*http.MaxBytesError`，所以自定义 `WithErrorHandler` 里一次 `errors.As` 就能覆盖全部上限路径。

### 响应与错误

handler 返回 `error`，不手写状态码。状态码由引擎的错误映射器决定，同一个决定会记进访问日志。

```go
return c.JSON(http.StatusOK, user)              // 先编码成功才写头
return c.Text(http.StatusOK, "pong")
return c.HTML(http.StatusOK, "user.html", web.H{"user": user})
return web.NotFound("user", err)                // 404 + code "user"，cause 不进响应体
```

内置构造器：`BadRequest`、`Unauthorized`、`Forbidden`、`NotFound`、`Conflict`、`TooLarge`、`Internal`。handler 里的 panic 变成 500；panic 的值与栈留在进程内（挂在观测记录上，绝不发给客户端）。

### 流式响应（SSE）

直接写 `c.Writer()`，用 `c.Flush()` 把每段推出去。响应写出器是**有意收窄**的接口：只承诺 `http.Flusher`，别的都不透出。

```go
app.GET("/events", func(c *web.Ctx) error {
	c.SetHeader("Content-Type", "text/event-stream")
	c.SetHeader("Cache-Control", "no-cache")
	for ev := range events {
		if _, err := fmt.Fprintf(c.Writer(), "data: %s\n\n", ev); err != nil {
			return nil                          // 客户端断开：响应已开始
		}
		if err := c.Flush(); err != nil {
			return err
		}
	}
	return nil
})
```

状态码与响应体积照常记录——你写了多少字节就记多少。

### 静态文件与模板

```go
app.Static("/assets", "./public")

app := web.New(web.WithTemplates(web.TemplateConfig{
	Root:      "templates",
	Pattern:   "*.html",                        // 默认
	DevReload: false,                           // true = 每请求重新解析（仅供开发）
}))
// handler 里：c.HTML(200, "user.html", web.H{"user": user})
```

模板是对 `html/template` 的薄封装；没有需要额外学习的自研模板引擎。

### 测试

handler 单测不必起 server。测试入口与真路径**共用同一份请求装配**——同一份代码，所以状态码落定、错误映射、访问日志的表现与线上一致。

```go
rec := httptest.NewRecorder()
req := httptest.NewRequest("GET", "/users/42", nil)
req.Pattern = "GET /users/{id}"     // 路由模板与路径参数由你补上
req.SetPathValue("id", "42")

c, done := app.NewTestContext(rec, req)
done(myHandler(c))                  // 跑一遍、把 error 交回去、收尾
```

`app.ServeTest(rec, req, handler, mw...)` 覆盖「handler + 中间件链」，并且连 panic 与请求体闸门一起接管。两个入口都不经 `ServeMux`；确切边界——以及什么时候仍该起真 server——见[测试指南](https://luo-root.github.io/pulse-web/guide/testing)。

## 观测

每个请求都有一个 32hex 的 TraceID、一条 `http.request` 记录，以及一个**在 handler 返回时立即回收**的请求作用域——早于引擎做错误映射与任何后续写出。

`New()` 已经装好了出口：`ConsoleSink`——每请求一行给人读的列式输出，写 stdout。

```go
app := web.New(web.WithHostID("orders-api"))   // ConsoleSink → stdout 已经是默认
```

`WithSink` 用来换掉它——**换出口不改变装配里的任何其它东西**。按「谁来读这份输出」选：

```go
web.WithSink(web.NewConsoleSink(os.Stdout))                     // 给人读——默认：不缓冲，写完即落
web.WithSink(observability.SlogSink{Logger: slog.Default()})    // 给机器读——接宿主 logger / JSON / 采集器（默认写 stderr）
web.WithSink(observability.NewAsyncSink(inner))                 // 要吞吐——把写出口移出请求路径
web.WithSink(observability.MultiSink{a, b})                     // 同时送多个目的地
```

**如果瓶颈就在出口上，用 `NewAsyncSink` 把它包起来。** 观测的成本几乎全花在「把记录送出门」这一步，而 kernel 的事件派发是全同步的——出口有多慢，请求路径就有多慢。`AsyncSink` 把写入放到后台协程、前置一个预分配的环形队列，请求 goroutine 立刻返回。代价是**所有权归你**：队列是有界的（默认回压，`DropOnFull()` 改为丢弃并计数），生命周期也归你——关闭前 `Flush` 或 `Close`，否则队列里的记录随进程一起消失。

`WithoutAccessLog()` 给已经另有日志系统的服务关掉访问记录；Trace 与 panic 兜底不受影响。

- `c.TraceID()`——handler、访问日志与下游调用拿到的是同一个 id
- `c.Observe("order.paid", func(a *observability.Attrs) { a.Set("order_id", id) })`——你自己的事件与框架的进同一个出口
- `web.WithCollector()`——需要请求级 fiber 可见性时，把 kernel collector 挂到请求作用域
- `web.WithSpanHook(hook)`——让你的追踪栈拥有请求的 span；官方 OpenTelemetry 适配是嵌套 module [`otel/`](otel/)
- `web.WithRoot(root)`——接入既有 kernel 树；注意 `Run` / `Serve` 返回时会级联销毁它，之后不要再使用
- `app.Debug("/debug/assembly")`——装配状态的 JSON 视图（快照、fiber、插件）

span 出口让请求在你的追踪后端里成为**一条真实 span**。框架**不编造 span-id**：id 由你的 tracer 分配、框架采用它，于是 `Server-Timing`、访问记录里的 `span.id`、下游客户端发出去的 `traceparent` 三处指向同一条真实 span。注意 `Server-Timing` 与 **`X-Trace-Id` 是两个不同的头**：后者是本框架自己的既有契约、只有 trace-id；前者是 W3C 定义的**响应侧**绑定、带**本请求的 span-id**（有 span 才写）。

记录的值域只有标量（`~string | ~int64 | ~float64 | ~bool`），没有 `map[string]any` 逃生舱，所以请求体、请求头、query 在类型上就进不了日志。默认出口写 stdout，它**不是持久化层**——轮转与保留归你的平台（容器、journald，或你自己给的 writer）。

## 生态与兼容

`net/http` 不是这里的竞争对象，而是地基：

```go
app.Handle("/legacy", web.Wrap(http.HandlerFunc(oldHandler)))    // stdlib handler 进来
http.Handle("/app/", http.StripPrefix("/app", app.Handler()))    // 引擎作为 http.Handler 出去
```

它**不是**什么：不是自带一整套社区中间件目录的微框架。如果你要的是生态、是团队最高的熟悉度，gin、chi、echo 是更明显的选择。Pulse-Web 面向的是那些宁可把依赖面控制在「标准库加两个包」、并且想要开箱即得的观测能力的服务。

## 文档

- [文档站点](https://luo-root.github.io/pulse-web/)——指南、观测专题与公开的性能数据
- [设计文档](docs/design/web-framework-design.md)——定位、决策、API 面、运行时契约、观测设计，以及一份明确的「不做什么」清单，含成本分解与和 gin 的真实负载对比
- [Issue #1](https://github.com/Luo-root/pulse-web/issues/1)——上面那些决策被定下来的设计讨论

## 参与

非小改动先开 Issue；规则见 [CONTRIBUTING.md](CONTRIBUTING.md)（中英双语）。

- 安全问题：**不要开公开 Issue**——按 [SECURITY.md](SECURITY.md) 私密上报
- 社区交往：[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)
- 用 AI coding agent 在本仓库干活：[AGENTS.md](AGENTS.md)

## 许可证

[MIT](LICENSE)
