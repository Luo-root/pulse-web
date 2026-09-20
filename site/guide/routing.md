# 路由、分组与中间件

注册面是标准库 `http.ServeMux` 的薄封装：**路径模式就是 `net/http` 的模式**（`/users/{id}`、`/files/{path...}`），没有第二套语法要学。

## 注册路由

```go
app := web.New()

app.GET("/users/{id}", showUser)      // 也有 POST / PUT / PATCH / DELETE / HEAD / OPTIONS
app.Handle("GET /users", listUsers)   // 把方法写进模式（stdlib 1.22+ 的写法）
app.Handle("/debug/{path...}", debug) // 不限方法
```

- handler 签名固定 **`func(*web.Ctx) error`**；要不要写响应由你决定，**错误靠返回**，状态码由错误映射器决定（见[错误模型](/guide/errors)）。
- `Engine` 满足 `http.Handler`；`ServeHTTP` / `Handler()` 让它能塞进任何吃 `http.Handler` 的位置（见文末）。

## 路径参数

```go
app.GET("/users/{id}/posts/{slug}", func(c *web.Ctx) error {
    return c.Text(200, c.Path("id")+"/"+c.Path("slug"))
})
```

`c.Path(name)` 就是 `c.Request().PathValue(name)`（同一份，不另存）。**`Wrap` 进来的 stdlib handler 拿不到 `*Ctx`，但照样能用 `r.PathValue("id")`**——同源。

## 分组

```go
api := app.Group("/api/v1", requireAuth)   // 前缀 + 分组中间件
api.GET("/orders", listOrders)             // → GET /api/v1/orders
api.GET("/orders/{id}", showOrder)

admin := api.Group("/admin")               // 可以嵌套
admin.Use(requireAdmin)                    // 只影响 /api/v1/admin
```

分组创建的是**独立视图**：分组上 `Use` 的中间件只作用于该分组及其子分组，不会漏到别的路由。前缀拼接与中间件栈都在注册期算好，请求期不额外开销。

## 中间件

```go
type Handler func(*Ctx) error
type Middleware func(c *Ctx, next Handler) error
```

三种挂法，从外到内依次是：

```go
app.Use(globalMW)                                     // 1. 全局：所有路由
app.Group("/admin", groupMW).GET("/x", h)             // 2. 分组：该分组
app.GET("/y", h, routeMW)                             // 3. 路由：这一条
```

**顺序**：全局 → 分组 → 路由，`Use` 多次则按调用顺序。中间件里 `return err` 即短路，后续不再执行；`next(c)` 之前写响应也等于短路（后面再写会被「已写响应不被覆盖」规则挡掉）。

`next(c)` 就是「继续往下走」。生态里 `func(http.Handler) http.Handler` 形状的中间件（chi/middleware、rs/cors、promhttp……）**不是**用 `Wrap` 接——用 **`web.Adapt`** 把它接进洋葱内，承诺与边界单独成页：[stdlib 中间件接入](/guide/middleware)。

## 请求体上限（`BodyLimit`）

`BodyLimit(n)` 是一个**中间件**，所以它既能挂分组也能挂单条路由：

```go
app := web.New(web.WithMaxBodyBytes(2 << 20))                       // 全局闸：2 MiB
app.Group("/upload", web.BodyLimit(512<<10)).POST("/avatar", h)     // 分组再收紧到 512 KiB
app.POST("/api/export", h, web.BodyLimit(1<<20))                    // 路由再收紧到 1 MiB
```

- **只能收紧，不能放宽**：引擎级先套一层，路由级再套一层，生效的是**两者中更小的**。挂在 `WithMaxBodyBytes(1<<10)` 的路由上时，`BodyLimit(1<<20)` 仍被 1 KiB 掐住。
- **`n <= 0` 表示不限**（直通，不包 body）；`WithMaxBodyBytes(0)` 同口径，即默认不限。
- 超限产出 **413 + `body_too_large`**，`cause` 是 `*http.MaxBytesError`——两条路径（声明值快速失败 / 读取闸门兜底）同型，自定义 mapper 用 `errors.As` 认这一个类型即可。边界：body **恰好 n 字节通过**，n+1 起 413。
- **读取闸门是惰性的**：超限只在 handler 真的去读 body 时才触发。
- 细节与实测见[设计文档](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md)的「请求体上限」一节。

## 静态文件

```go
app.Static("/static", "./files")
```

`Static` 走**与普通路由同一条注册路径**：静态资源照样经过全局与分组中间件（auth / CORS / 限流不会有例外），也照常进 Trace 与访问日志。前缀模式按 `<prefix>/` 注册，404 / 405 交给 `http.FileServer` 自己判。

::: warning 别用通配模式遮蔽它
再注册 `GET /static/{file...}` 会**盖掉**静态服务（方法限定模式优先）。要放行个别路径请用字面量——字面量赢通配，例如 `app.GET("/static/health", h)`。
:::

## 与 stdlib 的边界

| 方向 | 做法 |
|---|---|
| stdlib → 框架 | `web.Wrap(h http.Handler) web.Handler` |
| stdlib 中间件 → 框架中间件 | `web.Adapt(mw func(http.Handler) http.Handler) web.Middleware`（洋葱内）；或把它整个包在 `app.Handler()` 外面 |
| 框架 → stdlib | `app.Handler()` 返回 `http.Handler`（也可以直接把 `app` 当 `http.Handler` 用） |

`Wrap` 之后**拿不到 `*Ctx`**（没有 `c.Path`、请求级 KV、`c.Observe`），但 `PathValue`、中间件链、Trace 与访问日志照常。所以：需要 `*Ctx` 的能力就写成 `func(*web.Ctx) error`，纯粹是「一段 http.Handler」才用 `Wrap`。

中间件走 `Adapt`（洋葱内）还是外包 `Handler()`，判据见 [stdlib 中间件接入](/guide/middleware)。
