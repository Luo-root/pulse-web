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
| 需要归一化的路径（`//users`、`/users//42`） | — | ✅ 中间件看得见 | ❌ ServeMux 在洋葱之前就 307 到干净路径 |

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
2. **中间件短路（不调 next）、或者调了 next 但在它外面写响应时，它写出的状态码与字节数记回框架的采集层**——访问日志与 Trace 看到的是真实响应，而不是「没写响应」。后一种就是 `Recoverer` / 兜底 404 那一格；那种记录里没有 `error.type`，因为框架没接住错误、不知道这份响应怎么来的。
3. **中间件换掉的 request 传得下去**——`r = r.WithContext(...)` 之后，handler 用 `c.Request()` 读到的就是换过的那一个。

外加：交给中间件的 writer 支持 `http.Flusher` 与 `http.Hijacker`——SSE 逐条 flush 照常，挂在 `Use(Adapt(…))` 之后的升级路由也照常拿得到连接。且**不虚报**：底层不能 Flush 时 `c.Flush()` 照旧返回明确 error，底层不能 Hijack 时 `Hijack()` 返回 `http.ErrNotSupported`。

## 它不做什么

1. **不搬动路由**。路由匹配仍在中间件之前，预检 `OPTIONS` 到不了中间件——只注册了 `GET /api` 时 ServeMux 直接 405（`Allow: GET, HEAD`）。**要拦预检的 CORS 要么外包**，要么给路由显式注册 `OPTIONS`（逐条，或一条兜底 `OPTIONS /{path...}`——两条都实测过，见下面「CORS 与 CSRF」那节）。没匹配到路由的请求（真 404、尾斜杠）与**需要归一化的路径**（`//users`、`/users//42`）同理：后者由 ServeMux 在**调用 handler 之前**就 307 到干净路径，洋葱内的中间件看不到它。注意这条请求的访问记录里 `http.route` **是非空的**（ServeMux 在重定向前就把 pattern 写上了），别把它当成「这条请求被那个 handler 处理过」的证据。
2. **不解决「收尾型中间件 × error/panic」**。框架的错误映射发生在中间件返回**之后**，同一条时序有两个方向上的后果：
   - **响应方向**：在 next 返回后无条件写响应的中间件（例如自己 `defer zw.Close()` 的 gzip）会把状态锁成 200。**压缩类默认推外包**。
   - **观测方向**：中间件自己的收尾逻辑读到的是 **0**——`chi Logger` 在返回 error / panic 的路由上记的是 `0 / 0B`，`promhttp` 计数器把 404 记成 `code="200"`（`sanitizeCode(0)`）、panic 那条一条不记。要按**真实响应**记日志打点，用框架自己的访问日志与 Trace。
   - 反过来还有一格：中间件在 next **外面**写响应时（`Recoverer` 接住 panic 写 500 就是这样），框架记的是**它写出的那份**状态码与体积（客户端真正收到的那份），不是收尾时的缺省 200——这条由 [#96](https://github.com/Luo-root/pulse-web/issues/96) 修掉。记录里**没有** `error.type` / 错误对象：响应不是框架接住的，它不知道原因，编一个出来就是假信息。唯一例外是**已经挂着待映射的 error** 时（即上面「响应方向」那条边界）：收下中间件的状态码会让框架跳过错误映射、错误体整个丢掉，所以那种情况照旧交给映射器。
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
| 要在路由之前拦请求（CORS 预检） | **外包 `Handler()`**；要预检同时进观测，就用洋葱内 + 一条兜底 `OPTIONS /{path...}` 路由（见「CORS 与 CSRF」那节） |
| 断言 `http.Hijacker`（WebSocket 升级） | 都行——`Adapt` 的代理会转发 `Hijack`；中间件自己再包一层 writer 且不转发时才会断 |

promhttp 是个拆开看的好例子：**采集**用 `Adapt` 接 `promhttp.InstrumentHandlerCounter`（这样 `r.Pattern` 路由模板读得到），**暴露**走 `Wrap`——因为 `promhttp.Handler()` 本来就是个 `http.Handler`：

```go
app.GET("/metrics", web.Wrap(promhttp.Handler()))
```

但采集这一半有个取舍要当面说清：洋葱内的计数器**在错误路由上 `code` 标签会失真**（把 404 记成 `code="200"`，panic 那条一条不记，见「它不做什么」第 2 条）。按真实状态码计数就把它外包 `Handler()`，代价是那时 `r.Pattern` 还没写、路由标签得自己补。

## CORS 与 CSRF：具体怎么接

这两件事是「用生态件」里最容易卡住的：前者卡在**预检根本进不了洋葱**，后者卡在**一串看着像 token 错的 403**。两条完整走法写在下面，每一条都有 `interop/` 里的用例守着。

### CORS

`rs/cors` 零依赖、够用，唯一的问题是**预检在路由之前就被 ServeMux 分派**（见「它不做什么」第 1 条）。三条接法：

| 接法 | 预检被拦 | 预检进框架观测 | 什么时候用 |
|---|---|---|---|
| 外包 `Handler()` | ✅ | ❌ 框架完全不知情（无 trace、无访问记录） | 只要 CORS 生效、不看预检流量 |
| 逐条注册 `OPTIONS` | ✅ | ✅（`http.route` 是真路由） | 路由少、路径固定 |
| **一条兜底 `OPTIONS /{path...}`** | ✅ | ✅（`http.route` 是兜底模式） | 默认推荐：一条覆盖全部路径，含未注册路径的预检 |

兜底那条长这样（`Use` 必须在注册路由**之前**）：

```go
corsMW := cors.New(cors.Options{
    AllowedOrigins:   []string{"https://app.example.com"},
    AllowedMethods:   []string{http.MethodGet, http.MethodPost},
    AllowedHeaders:   []string{"content-type", "x-csrf-token"},
    AllowCredentials: true,
}).Handler

app.Use(web.Adapt(corsMW)) // 实际请求的 CORS 头

// 预检那条路：一条兜底 OPTIONS 路由把请求送进洋葱，响应仍由中间件写。
app.OPTIONS("/{path...}", func(c *web.Ctx) error {
    // 走到这儿的都是**不是预检**的 OPTIONS（预检被中间件短路了）。
    return &web.HTTPError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed"}
})
```

实测要点（`TestMatrixRsCorsPreflightCatchAllRoute`、`TestMatrixRsCorsPreflightExplicitRoute`）：

- 预检拿到 `204` + `Access-Control-Allow-Origin/Methods/Headers`，**而且进了框架的采集层**：`http.route` 记的是兜底模式 `/{path...}` 而不是目标路由——这是兜底那条路的代价，逐条注册记的是真路由；
- 外包那条路上预检**没有** `X-Trace-Id`、没有访问记录，正是表格里「⚠️ 框架完全不知情」那一格；
- 裸 `OPTIONS`（不带 `Access-Control-Request-Method`）不是预检，落到上面那个 handler，由它答 405——要别的语义就改它；
- 显式注册过 `OPTIONS /x` 的路径仍走那条（ServeMux 更具体的模式优先）。

### CSRF

`gorilla/csrf` 的 `csrf.Protect` 本来就是 `func(http.Handler) http.Handler`，经 `Adapt` 挂进洋葱即可：

```go
app.Use(web.Adapt(csrf.Protect([]byte(key),
    csrf.Secure(true),                                   // 线上开着
    csrf.TrustedOrigins([]string{"app.example.com"}))))  // 认 host[:port]，不带 scheme
```

服务端渲染的表单把 token 交给模板（`csrf.TemplateField` 产出隐藏 input）：

```go
app.GET("/form", func(c *web.Ctx) error {
    return c.HTML(http.StatusOK, "form", map[string]any{"csrf": csrf.TemplateField(c.Request())})
})
```

前后端分离则走「先 GET 拿 cookie 与 token（`csrf.Token(r)` 发给前端），之后非安全请求带 `X-CSRF-Token`」——请求头名默认就是这个。

**四个坑**（全部有用例钉住）：

| 现象 | 原因 | 怎么办 |
|---|---|---|
| 403 `referer not supplied` / `referer invalid` | 中间件对非安全请求按 **https** 比 Referer/Origin，明文 HTTP 部署（含「TLS 在上游终止」）两条都过不了 | 在 CSRF **之前**插一层把 request 换掉：`next.ServeHTTP(w, csrf.PlaintextHTTPRequest(r))`（同样用 `Adapt` 挂——承诺 ③ 保证换过的 request 传得下去） |
| 跨源 POST 一律 403 `origin invalid` | `Origin` 与请求不同源，又没写进 `TrustedOrigins` | `csrf.TrustedOrigins([]string{"app.example.com"})`——**认的是 host[:port]、不认 scheme**；非默认端口要连端口一起写（`app.example.com:8443`） |
| 403 `CSRF token not found in request`（表单字段明明发了） | 默认表单字段名是 `gorilla.csrf.Token`，不是 `csrf_token` | 用 `csrf.TemplateField` 生成，或显式 `csrf.FieldName(...)` |
| 表单字段发了却报 `token invalid` | token 是 base64，含 `+` 时裸放进 `application/x-www-form-urlencoded` body 会被读成空格 | 按标准编码（浏览器会自动；手写客户端用 `url.Values{}.Encode()`） |

预检不会被它拦下：`OPTIONS` 在安全方法之列，中间件直接放行（这一条也有用例）。

**观测**：短路写出的 403 会被框架记回采集层（状态码 / 路由模板 / 体积），且**不带** `error.type`——那条响应是中间件写的，不是框架接住的错误。外包挂载时框架照样看不到这条请求（无记录、无 trace）。

### CORS 与 CSRF 同挂

两件事各管一半，缺一个预检就是 405：

1. **CSRF 放行预检**靠安全方法豁免；
2. **预检进得了洋葱**靠 CORS 那条兜底 `OPTIONS /{path...}` 路由。

完整走法见 `TestMatrixGorillaCSRFWithCorsPreflight`：预检 `204` 且进记录，随后的跨源 POST 带 token 正常通过。

### 手写客户端最容易写错的两处

浏览器按标准发，手写客户端（含测试）常在这两处栽：

- `Access-Control-Request-Headers` 要**小写**（Fetch 标准要求已排序、小写）：写成 `X-CSRF-Token` 时 `rs/cors` 直接判「header 不允许」，预检被动中止——响应仍是 `204`，但**一个 CORS 头都没有**；
- 表单字段值要按 `application/x-www-form-urlencoded` 编码：token 里的 `+` 不编码会被服务端读成空格。

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
| `rs/cors` | 普通请求一致；**预检到不了洋葱内**（405）→ 外包，或给该路由显式注册 `OPTIONS`、或一条兜底 `OPTIONS /{path...}`（三条矩阵里都有，后两条实测成立；兜底那条的记录里 `http.route` 是兜底模式） | 端到端 |
| `golang-jwt/jwt/v5` | 短路（401）与换 request 两条缝都对得上 | 端到端 |
| `httprate` | **有状态**限流器两个挂载点各一份实例：前两次 200、第三次 429 带 `Retry-After`，额度耗尽后连别的路由也 429 | 端到端 |
| `gorilla/csrf` | 端到端：先写头（`Set-Cookie`）、换 request、短路（403 记回采集层、不带 `error.type`）、按需读表单四条接缝全对；`TrustedOrigins` 认 host、明文 HTTP 要标记、字段名与编码两个坑各有用例 | 端到端 |

表里每一行都跑到了**端到端**：`gorilla/csrf` 早先只做过源码核对，这一轮补上了真依赖端到端——它那四个坑连同配方一起写在上面的「CORS 与 CSRF」那节。

chi 的 `RealIP` **不在表里**：它在 chi v5.3.2 已标 Deprecated（改写 `r.RemoteAddr`、可被伪造，见 GHSA-3fxj-6jh8-hvhx 等三条），表里跑的是替代写法 `ClientIPFromXFF`——它把结果放进 request context，形状上更考适配面。

## 自己验证一个中间件

判据不是「跑通了」，而是**同一个中间件、同一条路由，经 `Adapt` 与包在 `app.Handler()` 外面逐项比对**：状态码与你关心的响应头、body（**包括 handler 返回 error 和 panic 的路由**——那里才是差异集中的地方），再加上中间件自己的旁观测（日志行 / 指标标签）。只测「都能返回 200」会漏掉全部静默失真。

三个容易写空的判据，写的时候留意：**一是只比响应**——「中间件短路」这一行的差别不在响应上（两边都 401），在「框架知不知道有过这条请求」；**二是「两边都加了同一个头」与「两边都没加」长得一样**，所以每一条断言之外还要有一条**正面**断言钉住「这个中间件真的发挥了作用」；**三是「申报过差异的行就不比了」**——已知边界是整行跳过比对，于是那一行客户端收到的状态码反而没有判据：把中间件写的状态码吞掉（响应只剩 body 带来的缺省 200）也能全绿。而短路、错误、panic 那几行恰恰全是申报过的，所以那几行的响应状态码要单独正面断言。
