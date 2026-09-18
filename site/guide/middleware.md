# stdlib 中间件接入

生态里的中间件几乎都是 `func(http.Handler) http.Handler` 形状：chi/middleware、rs/cors、promhttp、httprate、gorilla/csrf……这一页讲**怎么把它们接进本框架**，以及每一条路的边界在哪。

框架自己的中间件（`func(*Ctx, Handler) error`）怎么写、怎么挂，见[路由与中间件](/guide/routing)。

## 先分清两条路

`web.Adapt` 把 stdlib 中间件接成框架中间件；把它整个包在 `app.Handler()` 外面也是一条路。**两者不是风格选择**——能力面不一样：

| | 手写适配器（十几行公开 API） | 外包 `Handler()` | `Adapt`（洋葱内） |
|---|---|---|---|
| 中间件包 writer 抓状态码 | ❌ 恒为 0 | ✅ | ✅ |
| 中间件改写 body（gzip） | ❌ 响应损坏 | ✅ | ✅ |
| 中间件短路写响应 | ❌ 访问日志记成「没写响应」 | ⚠️ 框架完全不知情（无日志、无 span） | ✅ 记回采集层 |
| 中间件换 request | ❌ handler 拿不到 | ✅ | ✅ |
| 读路由模板 `r.Pattern` | ❌ | ❌ 这时还没路由 | ✅ |
| 拦预检 / 路由之前 | ❌ | ✅ | ❌ |

第一列是**静默失真**——最麻烦的一种：看起来能跑，但指标全标成 `code="0"`、gzip 响应客户端直接报 `gzip: invalid header`。

## 用法

```go
app.Use(web.Adapt(func(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("X-Request-Id", rid)
        next.ServeHTTP(w, r)
    })
}))
```

`Adapt` 返回的是普通 `web.Middleware`，所以三种挂法都吃：全局 `Use`、`Group`、单条路由。

::: warning `Use` 只对之后注册的路由生效
先 `Use` 再注册路由。反过来写，先注册的那些路由不在链上——不报错，只是静默不生效。
:::

## 它承诺什么

1. **中间件包 ResponseWriter 时，抓到的是真实的状态码与字节数**（不是 0）——promhttp 计数器、chi Logger 这类直接成立。
2. **中间件短路（不调 next）时，它写出的状态码与字节数记回框架的采集层**——访问日志与 Trace 看到的是真实响应，而不是「没写响应」。
3. **中间件换掉的 request 传得下去**——`r = r.WithContext(...)` 之后，handler 用 `c.Request()` 读到的就是换过的那一个。

外加：交给中间件的 writer 支持 `http.Flusher`，SSE 逐条 flush 照常；且**不虚报**——底层不能 Flush 时 `c.Flush()` 照旧返回明确 error。

## 它不做什么

1. **不搬动路由**。路由匹配仍在中间件之前，预检 `OPTIONS` 到不了中间件——只注册了 `GET /api` 时 ServeMux 直接 405（`Allow: GET, HEAD`）。**要拦预检的 CORS 必须外包**，或为每条路由显式注册 `OPTIONS`。
2. **不解决「收尾型中间件 × error/panic」**。框架的错误映射发生在中间件返回**之后**，在 next 返回后无条件写响应的中间件（例如自己 `defer zw.Close()` 的 gzip）会把状态锁成 200。**压缩类默认推外包**。
3. **不透出 `http.Hijacker`**。框架对响应写出器的能力承诺只到 `http.Flusher`，断言 `Hijacker` 的中间件（WebSocket 升级）不可用。
4. **不保证「同形状就能接」**。依赖特定 router 上下文的照旧不可用——chi 的 `CleanPath` 读 `chi.RouteContext`，经 `Adapt` 与外包**都 panic**。
5. **不改中间件语义**。panic 谁接、错误响应体长什么样，仍由中间件自己决定。

## 中间件怎么跟 handler 传值

走 **request context**——`stdlib` 自己的通道，框架不额外开取值入口：

```go
type userKey struct{}

app.Use(web.Adapt(func(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx := context.WithValue(r.Context(), userKey{}, principal)
        next.ServeHTTP(w, r.WithContext(ctx))
    })
}))

app.GET("/me", func(c *web.Ctx) error {
    p, _ := c.Request().Context().Value(userKey{}).(Principal)
    return c.JSON(200, p)
})
```

不为这件事导出 `Ctx` 取用口是有意的：那会把「`Ctx` 放在 request context 里」这个实现细节升格成契约。洋葱内的中间件需要的另一样东西——路由模板——本来就在 request 上（`r.Pattern`），不用开新口子。

## 挂载点怎么选

| 你的中间件…… | 挂哪儿 |
|---|---|
| 只改 header / 短路 / 包 writer（logger、鉴权、限流、promhttp 计数） | 都行；要路由模板、要短路进访问日志 → **`Adapt`（洋葱内）** |
| 动 body 且会在 next 返回后无条件收尾（gzip） | **外包 `Handler()`** |
| 要在路由之前拦请求（CORS 预检） | **外包 `Handler()`** |
| 断言 `http.Hijacker` | 都不行（在能力面之外） |

promhttp 是个拆开看的好例子：**采集**用 `Adapt` 接 `promhttp.InstrumentHandlerCounter`（要状态码正确、要路由模板），**暴露**走 `Wrap`——因为 `promhttp.Handler()` 本来就是个 `http.Handler`：

```go
app.GET("/metrics", web.Wrap(promhttp.Handler()))
```

## 已经实测过的生态件

「同形状」不等于「能吸纳」，下表是逐项跑过的结论：

| 中间件 | 结论 | 验证程度 |
|---|---|---|
| chi `Logger` / `RequestID` / `RealIP` / `Timeout` / `Compress` | 经 `Adapt` 与外包**逐项一致** | 端到端（状态码 + 9 个响应头 + body） |
| `promhttp.InstrumentHandlerCounter` | 同上，含状态码 | 端到端 |
| rs/cors | 真实请求一致；**预检到不了**（405）→ 推荐外包 | 端到端 |
| chi `Recoverer` | 两边都 500，但**响应体不同**（Recoverer 自己写）→ panic 兜底用框架自己的 | 端到端 |
| chi `CleanPath` | 经 `Adapt` 与外包**都 panic**，不可用 | 端到端 |
| `httprate` | 全族是 `func(next http.Handler) http.Handler`，预期直接吃 | 只核过签名 |
| `gorilla/csrf` | 四条接缝（换 request / 先写头 / 短路 / 按需读表单）都对得上 | 源码核对，未端到端 |

## 自己验证一个中间件

判据不是「跑通了」，而是**同一个中间件、同一条路由，经 `Adapt` 与包在 `app.Handler()` 外面，逐项比对状态码与你关心的响应头**——包括 handler 返回 error 和 panic 的路由。只测「都能返回 200」会漏掉全部静默失真。
