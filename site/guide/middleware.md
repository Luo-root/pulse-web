# stdlib 中间件接入

生态里的中间件几乎都是 `func(http.Handler) http.Handler` 形状：chi/middleware、rs/cors、promhttp、httprate、gorilla/csrf……这一页讲**怎么把它们接进本框架**，以及每一条路的边界在哪。

框架自己的中间件（`func(*Ctx, Handler) error`）怎么写、怎么挂，见[路由与中间件](/guide/routing)。

## 先分清两条路

`web.Adapt` 把 stdlib 中间件接成框架中间件；把它整个包在 `app.Handler()` 外面也是一条路。**两者不是风格选择**——能力面不一样：

| | 手写适配器（十几行公开 API） | 外包 `Handler()` | `Adapt`（洋葱内） |
|---|---|---|---|
| 中间件包 writer 抓状态码 | ❌ 恒为 0 | ✅ | ✅ handler 写出的看得见；框架映射的 error/panic 那条路看到 0（见下） |
| 中间件改写 body（gzip） | ❌ 响应损坏 | ✅ | ✅ 正常路径；error/panic 路径退化（见下） |
| 中间件短路写响应 | ❌ 访问日志记成「没写响应」 | ⚠️ 框架完全不知情（无日志、无 span） | ✅ 记回采集层 |
| 中间件换 request | ❌ handler 拿不到 | ✅ | ✅ |
| 读路由模板 `r.Pattern` | ❌ | ❌ 这时还没路由 | ✅ |
| 拦预检 / 路由之前 | ❌ | ✅ | ❌ |
| 没匹配到路由的请求（真 404、尾斜杠） | — | ✅ 中间件看得见 | ❌ 不经洋葱 |

前两行「✅」后面的括注是同一条时序在**错误路径**上的两处代价，下面「它不做什么」第 2 条讲清楚了；正常路径（handler 自己写响应）那三列没有保留条件。

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

外加：交给中间件的 writer 支持 `http.Flusher` 与 `http.Hijacker`——SSE 逐条 flush 照常，挂在 `Use(Adapt(…))` 之后的升级路由也照常拿得到连接。且**不虚报**：底层不能 Flush 时 `c.Flush()` 照旧返回明确 error，底层不能 Hijack 时 `Hijack()` 返回 `http.ErrNotSupported`。

## 它不做什么

1. **不搬动路由**。路由匹配仍在中间件之前，预检 `OPTIONS` 到不了中间件——只注册了 `GET /api` 时 ServeMux 直接 405（`Allow: GET, HEAD`）。**要拦预检的 CORS 必须外包**，或为每条路由显式注册 `OPTIONS`。
2. **不解决「收尾型中间件 × error/panic」**。框架的错误映射发生在中间件返回**之后**，同一条时序有两个方向上的后果：
   - **响应方向**：在 next 返回后无条件写响应的中间件（例如自己 `defer zw.Close()` 的 gzip）会把状态锁成 200。**压缩类默认推外包**。
   - **观测方向**：中间件自己的收尾逻辑读到的是 **0**——`chi Logger` 在返回 error / panic 的路由上记的是 `0 / 0B`，`promhttp` 计数器把 404 记成 `code="200"`（`sanitizeCode(0)`）、panic 那条一条不记。要按**真实响应**记日志打点，用框架自己的访问日志与 Trace。
   - 反过来还有一格：中间件在 next **外面**写响应时（`Recoverer` 接住 panic 写 500 就是这样），框架记下的状态码是它自己的缺省 200，而客户端拿到的是中间件写的 500——已知缺陷（[#96](https://github.com/Luo-root/pulse-web/issues/96)），矩阵里按现状钉着。
3. **不透出 `http.Pusher` / `FlushError`**。响应写出器的能力面是一份显式的窄清单（`Flush` / `Hijack` / `SetWriteDeadline` / `EnableFullDuplex`），HTTP/2 的 Server Push 与 flush 错误上报不在其中。
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
| 只改 header / 短路 / 包 writer（logger、鉴权、限流、promhttp 计数） | 都行；要路由模板、要短路进访问日志 → **`Adapt`（洋葱内）**；要按**真实状态码**记日志 / 打指标 → **外包**（洋葱内看不到框架映射的 error/panic，见「它不做什么」第 2 条） |
| 动 body 且会在 next 返回后无条件收尾（gzip） | **外包 `Handler()`** |
| 要在路由之前拦请求（CORS 预检） | **外包 `Handler()`** |
| 断言 `http.Hijacker`（WebSocket 升级） | 都行——`Adapt` 的代理会转发 `Hijack`；中间件自己再包一层 writer 且不转发时才会断 |

promhttp 是个拆开看的好例子：**采集**用 `Adapt` 接 `promhttp.InstrumentHandlerCounter`（这样 `r.Pattern` 路由模板读得到），**暴露**走 `Wrap`——因为 `promhttp.Handler()` 本来就是个 `http.Handler`：

```go
app.GET("/metrics", web.Wrap(promhttp.Handler()))
```

但采集这一半有个取舍要当面说清：洋葱内的计数器**在错误路由上 `code` 标签会失真**（把 404 记成 `code="200"`，panic 那条一条不记，见「它不做什么」第 2 条）。按真实状态码计数就把它外包 `Handler()`，代价是那时 `r.Pattern` 还没写、路由标签得自己补。

## 已经跑过的生态件

下面这些结论由一个**进 CI 的对照工程**守着：[`interop/`](https://github.com/Luo-root/pulse-web/tree/main/interop)（嵌套 module，`go build` / `go vet` / `go test` 都是门禁，与 `otel/` / `loadtest/` 同款）。判据不是「跑通了」，而是**同一个中间件、同一条路由，经 `Adapt` 与包在 `app.Handler()` 外面逐项比对**，比三个面：

1. **响应**——状态码 + 关心的响应头 + body；
2. **中间件自己的旁观测**——它记下的日志行 / 指标标签；
3. **框架侧的访问记录**——状态码、路由模板、响应体积、错误类别。

第 3 面单独比，是因为「中间件短路」那一行的差别恰好不在响应上（两边都返回 401），而在「框架知不知道」。上游发版改了行为，这里会红。矩阵本体在 [`interop/middleware_test.go`](https://github.com/Luo-root/pulse-web/blob/main/interop/middleware_test.go)。

| 中间件 | 结论 | 验证到哪一步 |
|---|---|---|
| chi `RequestID` / `ClientIPFromXFF` | 经 `Adapt` 与外包**逐项一致**，含它放进 request context 的值能不能被 handler 读到 | 端到端 |
| chi `Logger` / `Compress` | **正常路径**逐项一致；错误路径两边不同（见「它不做什么」第 2 条） | 端到端 |
| chi `Timeout` | 逐项一致（超时后补写的 504 两边都不生效——响应已落定） | 端到端 |
| chi `Recoverer` | 两边都 500，但**响应体不同**（Recoverer 写自己的空体）→ panic 兜底用框架自己的 | 端到端 |
| chi `CleanPath` | 经 `Adapt` 与外包**都 panic**，不可用 | 端到端 |
| `promhttp.InstrumentHandlerCounter` | 正常路径 `code=200` 一致；错误路径会把 404 记成 `code=200` | 端到端 |
| `rs/cors` | 普通请求一致；**预检到不了洋葱内**（405）→ 外包，或给该路由显式注册 `OPTIONS`（两条矩阵里都有，后者的绕法实测成立） | 端到端 |
| `golang-jwt/jwt/v5` | 短路（401）与换 request 两条缝都对得上 | 端到端 |
| `httprate` | **有状态**限流器两个挂载点各一份实例：前两次 200、第三次 429 带 `Retry-After`，额度耗尽后连别的路由也 429 | 端到端 |
| `gorilla/csrf` | 四条接缝（换 request / 先写头 / 短路 / 按需读表单）在源码上对得上 | **源码核对**，没跑过 |

最后一行只是「形状上符合」，**不是「已经能跑」**——它是候选，不是结论。

chi 的 `RealIP` **不在表里**：它在 chi v5.3.2 已标 Deprecated（改写 `r.RemoteAddr`、可被伪造，见 GHSA-3fxj-6jh8-hvhx 等三条），表里跑的是替代写法 `ClientIPFromXFF`——它把结果放进 request context，形状上更考适配面。

## 自己验证一个中间件

判据不是「跑通了」，而是**同一个中间件、同一条路由，经 `Adapt` 与包在 `app.Handler()` 外面逐项比对**：状态码与你关心的响应头、body（**包括 handler 返回 error 和 panic 的路由**——那里才是差异集中的地方），再加上中间件自己的旁观测（日志行 / 指标标签）。只测「都能返回 200」会漏掉全部静默失真。

两个容易写空的判据，写的时候留意：**一是只比响应**——「中间件短路」这一行的差别不在响应上（两边都 401），在「框架知不知道有过这条请求」；**二是「两边都加了同一个头」与「两边都没加」长得一样**，所以每一条断言之外还要有一条**正面**断言钉住「这个中间件真的发挥了作用」。
