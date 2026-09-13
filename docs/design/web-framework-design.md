# feat: pulse-web 框架设计——kernel 装配内核 + HTTP 语义层（设计票）

> 完整设计文档（v1）。设计票与评审记录见 [Issue #1](https://github.com/Luo-root/pulse-web/issues/1)。
> 包名 `web`，模块 `github.com/Luo-root/pulse-web`——不与 pulse 根包（`package pulse`）冲突。

## 一句话

用 pulse 的 **kernel** 做装配内核（IoC / 可逆生命周期 / 请求作用域 / 事件），用 **observability** 做一等观测，构建一个与 gin / chi / echo 同级形态的**通用 Go web 服务框架**。卖点是装配能力 + 一等观测，不是极致性能。

## 做什么（v1）

1. **Engine**——路由注册、分组、中间件（闭包组合，洋葱模型）
2. **Ctx**——路径参数（读 `Request.PathValue`，不另存）、查询、请求级 KV、响应写出（JSON / Text / Status / Header / Flush）
3. **错误模型**——`HTTPError` + `StatusCoder` 接口 + 默认 mapper（脱敏）
4. **JSON 绑定**——stdlib `encoding/json`（Go 1.27 起由 v2 实现支撑）
5. **请求作用域**——每请求派生 kernel scope，handler 返回即 `Dispose`（在写响应之前）
6. **一等观测**——装配期 `Bootstrap` + 请求期 `Trace` + `AccessLog`；32hex TraceID；`X-Trace-Id` 回写
7. **进程级装配面**——`app.Root()` / `web.WithRoot(k)`；其余用 kernel 原生 API（`Provide` / `Use` / `Loader`）
8. **生命周期**——`Run`（信号 → `Server.Shutdown` → `OnShutdown` → `root.Dispose` → Sink flush）、`Serve`、`Handler`
9. **stdlib 互操作**——`Wrap(http.Handler) Handler`，`Handler()` 反向导出
10. **静态文件**——`Static(prefix, dir)`，**静态资源同样经过全局与分组中间件**（与普通路由共用注册路径）
11. **流式响应**——直接写 `ResponseWriter` + `Flush`（SSE / 大文件），一条 AccessLog
12. **HTML 模板**——`c.HTML()`，薄封装 stdlib `html/template`（生产缓存 / 开发热重载）

## 不做什么（v1 明确排除）

前端界面 / ORM / 策略中间件（认证 / 限流 / 熔断）/ 微服务治理 / 第三方路由库 / `HoldScope` / `RunTLS` / `Recover()` 中间件 / 内部 Router 抽象 / TTFB / 流式双记录 / 假 span-id。

**后续计划：WebSocket**——stdlib 没有 WebSocket 实现（`net/http` 只提供 Hijack / Upgrade 机制，帧协议需自实现），引入它必然带第三方依赖（社区域主流是 `coder/websocket`）。因此作为**可选子包**（`pulse-web/ws`）后续加入，核心保持零依赖。

另有一批"评审中被判定为非 v1"的项，列入文末「明确不做」清单（含各自去路）。

## 为什么

**生态位**：Go 缺一个"装配 + Web"一体框架——`wire` 编译期 DI 无运行时生命周期；`fx` 有 DI 无 HTTP；`kratos` / `go-zero` 是微服务全家桶、偏重。pulse-web 的位置是**带 IoC 生命周期与一等观测的服务基座**，目标场景是装配复杂度高的服务（多组件、需要热更新、需要统一观测），而不是极致轻量的 API 网关。

**为什么内核是 kernel**：kernel 的 scope 树 + Effect LIFO 天生就是"请求作用域"容器——Spring 的 request scope bean、Go 的 `context.Value` + `defer` 清理，kernel 用一个机制统一了，且更严格（销毁必回收）。observability 也早已按"每请求 TraceID"设计。

**借鉴 Spring Boot，不照抄**（理念可借鉴，Go 化必须换形态）：

| Java / Spring | Go / pulse-web | 原因 |
|---|---|---|
| 注解扫描 + 反射注入 | 泛型服务键 + 显式装配 | 依赖关系编译期可见 |
| 异常机制 | `error` 返回值 + 集中映射 | Go 错误是一等返回值 |
| ThreadLocal 请求上下文 | kernel scope 树 | Go 并发不是线程绑定 |
| 代码级热加载 | **不做**，Loader 是状态级重载 | Go 无法卸载已加载代码 |
| classpath 扫描 | `main` 显式装配 + 可选 YAML 条目 | 启动确定性优先 |

## 关键决策（已定）

| 决策 | 结论 | 依据 |
|---|---|---|
| 定位 | 通用 web 框架（库形态），非 agent 专用、不含前端 | 项目所有者拍板 |
| 包名 / 模块 | `package web` / `github.com/Luo-root/pulse-web` | 与 pulse 根包 `package pulse` 不冲突 |
| Go 版本 | `go 1.27.0`（pulse 自身保持 1.25，不冲突） | 实测 stdlib 路由 −22%；pulse 22 包测试在 1.27 全绿 |
| API 风格 | echo 式：`func(*Ctx) error` + 集中错误映射 + stdlib 双向兼容 | 与 pulse「不静默吞错」同构、DI 类型安全、可测 |
| 路由 | stdlib `ServeMux`（Go 1.22+ 模式） | 零依赖、190ns/匹配；**不抽 router 接口**（YAGNI，要换时再抽） |
| HTTP 中间件 | 闭包组合 | **不用** kernel `Waterfall`——那是事件 around 链，不是 HTTP 洋葱 |
| kernel 作用 | 每请求派生 scope（默认付 128ns）；**默认路径零 Provide** | 实测：Provide 触发全树广播 = O(插件树) |
| 依赖方向 | `pulse-web → pulse` 单向 | pulse 永不反向引用 |

### 路由选型的边界（实测，Go 1.27）

| 能力 | 结果 |
|---|---|
| 路径参数 `{id}` / 通配后缀 `{rest...}` | 支持（后者可匹配空段） |
| 方法限定（`GET /m` vs `/m`） | 支持，方法限定优先 |
| 冲突注册（`/a/{x}` 与 `/a/{y}`） | **注册期 panic**——与 pulse「装配期暴露错误」一致 |
| 尾斜杠 | `/dir` 命中 `/dir/{rest...}` 时 **307**；文档明示 |
| 正则约束 `{id:[0-9]+}` | **不支持**（解析 panic）——用应用层校验替代 |
| 分组 / 中间件继承 | ServeMux 没有，但那是**框架层语法糖**（注册期编译进 handler） |

**分组与顺序**（洋葱模型）：

```go
g1 := app.Group("/api", mw1)
g2 := g1.Group("/v1", mw2)
g2.GET("/users", h, mw3)
// 执行：mw1 → mw2 → mw3 → h → mw3 → mw2 → mw1
```

中间件在注册期闭包组合（运行期零额外开销）；冲突 panic 包装为带分组上下文的信息（`g1(/api) → g2(/v1)`）。

## 实测数据（2026-09-12，i9-14900HX，`-count=1`）

| 场景 | 耗时 | 分配 |
|---|---|---|
| stdlib ServeMux 路由匹配 | 190 ns | 5 allocs |
| kernel 服务读取 `Get` | 11.6 ns | 0 allocs |
| scope 派生 + 销毁 | 128 ns | 5 allocs |
| 请求级事件 `EmitLocal` | 32 ns | 2 allocs |
| 全树事件 `Emit` | 2123 ns | 60 allocs |
| 每请求 `AttachCollector`（空树） | 357 ns | 15 allocs |
| 同上（10 / 50 / 100 插件树） | 859 / 2666 / 5140 ns | — |

**结论**：请求作用域与请求级事件便宜；**每请求 Provide 服务 = O(插件树规模)，每插件 +47ns**（根因 `notifyServiceChange` 全树广播）。Go 1.25 → 1.27 同基准：stdlib 路由 244 → 190 ns（−22%），kernel 路径持平。

## 设计红线

1. 请求路径一律 `EmitLocal`，禁用全树 `Emit`（32ns vs 2123ns）
2. **默认路径零 `Provide`、零 `AttachCollector`**；`WithCollector()` 显式开启后才出现 O(插件树)（且仅限"插件经 `kernel.Get` 打点"场景）
3. 请求 scope 与"响应是否送达网络"解耦：**`Dispose` 在写响应之前**
4. `Ctx` 不可跨 goroutine；后台任务用 `Detach`（值 + 进程级 root）

## API 面

### 装配（进程级，与卖点对齐的一等公民）

```go
app := web.New()                  // 自建 root + 装 Bootstrap
app := web.New(web.WithRoot(k))   // 接入已有 kernel（与 host 共用同一棵树）
root := app.Root()                // 进程级 root：Provide / Use / 交给需要进程级 kernel 的组件

kernel.Provide(root, dbKey, db)   // 其余用 kernel 原生 API
kernel.Use(root, myPlugin)
```

**`Root()` 是装配叙事的关键**：任何需要"进程级 kernel"的组件（自建插件、第三方接入、跨请求存活的装配）都必须拿到它。**不要把 `c.Kernel()` 当进程级 kernel 用**——那是请求 scope，挂上去的插件与 Effect 会在请求结束时被级联销毁。

其余装配（`Provide` / `Use` / `Loader.Reconcile` / `FiberSnapshots`）直接用 kernel 原生 API，本框架**不包装**。

### Engine 与运行

```go
func (app *Engine) Run(addr string) error          // 阻塞至关闭完成
func (app *Engine) Serve(ln net.Listener) error    // 自定义 listener
func (app *Engine) Handler() http.Handler          // 导出视图（等价自身）
app.Static("/static", "./files")
app.OnShutdown(fn func(ctx context.Context) error) // 单回调
```

- `Run` **阻塞**；`nil` = 收到信号正常关闭，非 nil = 启动失败或关闭期错误。两条路径都完成 `root.Dispose()`——**调用方不能再 Dispose**
- `Run` 返回后 Engine **不可复用**（kernel 已销毁）
- 不提供 `RunTLS`（用 `tls.NewListener` + `Serve`，或反代终止 TLS）

`ServerConfig`（零值即默认）：

| 字段 | 默认 | 说明 |
|---|---|---|
| `ReadHeaderTimeout` | 5s | 防慢头攻击 |
| `ReadTimeout` | 30s | 读完整请求上限 |
| `WriteTimeout` | **0** | **刻意**——非 0 会切断 SSE；非流式服务建议显式设 30s |
| `IdleTimeout` | 120s | keep-alive 空闲上限 |
| `MaxHeaderBytes` | 1 MiB | 请求头上限 |
| `ShutdownTimeout` | 30s | 优雅关闭总预算 |

### Ctx

```go
func (c *Ctx) Path(name string) string   // == c.Request().PathValue(name)（同源，不另存）
func (c *Ctx) Query(name string) string
func (c *Ctx) Request() *http.Request
func (c *Ctx) TraceID() string

// 请求级 KV（框架自有，不进 kernel 仓库）
func (c *Ctx) Get[T any](k Key[T]) (T, bool)
func (c *Ctx) Set[T any](k Key[T], v T)

// 全局服务（kernel root 仓库）
func (c *Ctx) Service[T any](k kernel.ServiceKey[T]) (T, bool)
func (c *Ctx) MustService[T any](k kernel.ServiceKey[T]) T

func (c *Ctx) Kernel() *kernel.Context   // 请求 scope：挂 Effect / 事件监听 / 传给需要作用域的组件
func (c *Ctx) Observe(event string, set func(*observability.Attrs))
func (c *Ctx) Detach() Detached

// 响应
func (c *Ctx) JSON(code int, v any) error
func (c *Ctx) Text(code int, s string) error
func (c *Ctx) Status(code int) *Ctx      // 只设置，不立即写头
func (c *Ctx) SetHeader(k, v string)
func (c *Ctx) Flush() error              // 流式
```

类型约束：`Key[T]` 与 `kernel.ServiceKey[T]` 是不同类型，误用编译期报错——命名是第一道防线，类型是第二道。

**关系澄清**：kernel 里**不存在"scope 局部服务"**——所有服务绑定都在 root 仓库（`kernel/service.go:58`），`Get` 从任何活 scope 出发都读 root。所以 `c.Service(key)` ≡ `kernel.Get(c.Kernel(), key)`；`c.Kernel()` 的用途不是读服务，而是挂请求级资源。请求级数据必须由框架 KV 承担。

**`observability.Set` 的类型约束**：`Set[T AttrValue]` 只接受 `~string | ~int64 | ~float64 | ~bool`——`u.ID` 若是 `int` 或 UUID 类型需显式转换（`int64(u.ID)` 或 `u.ID.String()`）。

### 错误模型

```go
type HTTPError struct {
    Status  int    // HTTP 状态码
    Code    string // 机器可读业务码
    Message string // 人读消息（默认取状态码标准文案）
}
// cause 不导出：要携带原始错误请用构造器 —— 它只进观测记录，绝不进响应体。
func (e *HTTPError) Error() string
func (e *HTTPError) Unwrap() error

// 可扩展状态码：非 HTTPError 但实现本接口的 error，用其状态码，message 仍走默认脱敏
type StatusCoder interface {
    error
    StatusCode() int
}

func BadRequest(code string, cause error) *HTTPError   // 400
func Unauthorized(code string, cause error) *HTTPError // 401
func Forbidden(code string, cause error) *HTTPError    // 403
func NotFound(code string, cause error) *HTTPError     // 404
func Conflict(code string, cause error) *HTTPError     // 409
func Internal(code string, cause error) *HTTPError     // 500

type ErrorHandler func(c *Ctx, err error) error
app := web.New(web.WithErrorHandler(myMapper))
```

- 需要 429 / 422 / 503 等：实现 `StatusCoder`，或直接构造 `&web.HTTPError{Status: n, Code: "…"}`
- 默认响应体：`{"error": {"code": "user", "message": "not found"}}`
- **安全默认**：`cause` 只进观测记录；**非 `HTTPError` 的普通 error 一律 500** + 通用文案
- panic 有专用类型：`PanicError{Value, Stack}`——默认 mapper 一律 **500**（栈只进记录）。**陷阱要显眼写**：`panic(pulse.Unauthorized(…))` 不会返回 401，要 4xx 请 `return`

### 中间件与 stdlib 互操作

```go
type Handler func(*Ctx) error
func Wrap(h http.Handler) Handler   // stdlib → 框架
```

- `Wrap` 后的 handler **无法访问** `*Ctx`（`c.Path` / 请求级 KV / `c.Observe`），但可用 **`r.PathValue("id")`**（与 `c.Path` 同源，ServeMux 写入的同一份）；中间件链照常经过它
- panic 由 Engine 兜底接（见下）；响应已写出则只记录不重写

### HTML 模板

薄封装 stdlib `html/template`（不自己实现模板引擎）：

```go
app := web.New(web.WithTemplates(web.TemplateConfig{
    Root:      "./views",   // 模板根目录
    Pattern:   "*.html",    // glob 模式（默认值）
    DevReload: false,       // true = 每次请求重新解析（开发用）；生产保持缓存
}))

app.GET("/", func(c *web.Ctx) error {
    return c.HTML(200, "index.html", web.H{
        "Title": "首页",
        "User":  user,
    })
})
```

- **生产模式**：启动时解析一次并缓存（并发安全）
- **`DevReload: true`**：每请求重新 `ParseGlob`（每请求磁盘扫描 + 写锁）；仅供开发
- `web.H` = `map[string]any` 的类型别名（模板数据便利写法，也可传任意 struct）
- **`c.HTML` 的三条错误路径一律返回明确 error**（不静默 500）：未配置模板 / 模板名不存在 / 执行期报错。实现上**先渲染到内存缓冲、成功后才写响应头**——`WriteHeader` 一旦先生效，「已写响应不被覆盖」规则会把错误吞成 200 空页。代价是模板输出不流式（需要流式请直接写 Writer）
- 模板自动补 `Content-Type: text/html; charset=utf-8`

### 静态文件与中间件

`Static(prefix, dir)` **走与普通路由同一条注册路径**——静态资源同样经过全局与分组中间件（auth / CORS / 限流不会有例外），也照常进 Trace 与 AccessLog 收尾。内部注册前缀模式 `<prefix>/`，交给 `http.FileServer` 自行处理 404 / 405。

> ⚠️ 再注册 `GET <prefix>/{file...}` 会遮蔽静态服务（方法限定模式优先）；要放行个别路径请用字面量（字面量赢通配），如 `app.GET("/static/health", h)`。

## 运行时契约

### 请求作用域的生命周期（关键时序）

```
中间件链 → handler 返回
  → scope.Dispose()              ← 业务态结束：请求级 Effect 立即 LIFO 回收
  → 错误映射 + 序列化 + 写响应     ← 网络态
  → AccessLog 落盘               ← 响应侧字段此时才就绪
```

**`Dispose` 在写响应之前**：否则慢客户端会占着请求级资源（DB 事务、连接）直到 TCP 超时。（小响应通常只写 `bufio` 缓冲不阻塞，风险面是"大响应 + 慢客户端"；但边界要显式定义。）

**流式响应下同一条规则**：SSE 的 handler 不返回 → scope 活到流结束——这是**正确语义**（scope 生命周期 = 业务逻辑生命周期），不是冲突。使用指引：长连接不要把"用完该马上还"的资源（DB 事务、分布式锁）登记为请求级 Effect。

### 错误映射器契约

映射发生在 `scope.Dispose()` **之后**，所以"能碰什么"是显式契约：

| API | 可用 | 说明 |
|---|---|---|
| `c.Get` / `c.Set`（请求级 KV） | ✅ | KV 挂在 **Ctx** 上，生命周期覆盖到 Engine 收尾 |
| `c.Observe` | ✅ | 直写 Sink，不经 scope |
| `c.TraceID` / `c.Path` / `c.Query` | ✅ | 纯数据 |
| 响应写出 | ✅ | 映射器职责 |
| `c.Kernel()` / `c.Service` | ❌ | 请求 scope 已 Dispose：`Get` 返回 false、`Effect` 返回 `ErrDisposed`（**返回的是死 scope，不是 nil**，避免 nil panic） |

- `err` 参数是**原始 error（只读）**；`ErrorHandler` 返回值 = **映射器自身失败**（响应未写则兜底 500；已写则只进内部日志）
- **panic 兜底在 Engine 的 `defer`**（不是中间件）——所以 `Minimal()` 下 panic 依然有 500 响应与记录

### Ctx 生命周期与后台任务

`Ctx` 及其 `ResponseWriter` **不可跨 goroutine**（`ResponseWriter` 非线程安全；kernel scope Dispose 后静默失效——`Get` false、`Effect` 返回 `ErrDisposed`，比 panic 更隐蔽）。

**Ctx 不池化**（明确决策）：池化只省一次分配，代价是契约升级为"绝对不能保留引用"——用户存进全局 map 后对象复用会造成静默数据串扰。请求路径预算容得下这次分配。

**后台任务的正确姿势**：`Detach` 是**值袋子**，不拥有 scope：

```go
type Detached struct {
    TraceID string
    HostID  string
    Sink    observability.Sink
    Root    *kernel.Context   // 进程级
}
func (d Detached) Observe(event string, set func(*observability.Attrs))
func (d Detached) Service[T any](k kernel.ServiceKey[T]) (T, bool)
func (d Detached) MustService[T any](k kernel.ServiceKey[T]) T
```

```go
app.POST("/jobs", func(c *web.Ctx) error {
    bg := c.Detach()
    go func() {
        bg.Observe("job.start", nil)      // 直写 Sink，进同一 TraceID
        svc := bg.MustService(jobKey)     // 全局服务：读 root，不需要 scope
        scope, _ := bg.Root.Derive()      // 需要 scope 就自己派生
        defer scope.Dispose()             // 生命周期自己管
        svc.Do(context.Background(), input)
        bg.Observe("job.done", nil)
    }()
    return c.JSON(202, "accepted")
})
```

并发契约一句话：**`Detached` 是值，不拥有 scope**——scope 的派生与回收由调用方自理，不存在共享状态的竞态。

**`Detach` 不携带请求级 KV**：要传递的数据在 Detach 前显式取出（`user := c.MustService(userKey)` 式的显式取值），让"哪些数据进了后台"一眼可见。

### 优雅关闭时序

**kernel 的 `Context.Dispose()` 是级联截断，不是 drain**——递归销毁所有子 scope、静默 `forceUnload` 所有 fiber（`kernel/context.go:180-227`），**不等待在途工作**。drain 必须由 `net/http` 承担：

```
信号（SIGINT / SIGTERM）
  → ① http.Server.Shutdown(timeoutCtx)   ← drain 在这里
  → ② 超时 → srv.Close() 强制断开
  → ③ OnShutdown 回调                     ← 单回调，用户等待后台任务
  → ④ root.Dispose()                     ← 终局：级联截断
  → ⑤ Sink flush（若实现 Flusher 接口；**独立预算** `sinkFlushTimeout` = 3s，
       在 `root.Dispose()` 之后另起，**不计入 `ShutdownTimeout`**：
       总关闭时长上限 = ShutdownTimeout + 3s）
```

- ①②③ 共享**同一个 deadline**（这三段总时长 ≤ `ShutdownTimeout`，与 k8s `terminationGracePeriodSeconds` 对齐）；HTTP 用光预算时回调拿到已过期 ctx，记录"后台任务未等待"；⑤ 的 Sink flush 另起独立预算（见上，总上限 = ShutdownTimeout + 3s）
- **未注册等待的后台任务会被 ④ 截断**——明示行为
- `Detached.Root` 派生出的 scope 是 root 的子 scope，同样受 ④ 影响

## 观测设计

### 默认装配与组合

| 配置 | Bootstrap | Trace | AccessLog | panic→500 | Sink |
|---|---|---|---|---|---|
| `New()` | 开 | 开 | 开 | 开（不可关） | `SlogSink{slog.Default()}` |
| `Minimal()` | 关 | 关 | 关 | **开** | 无 |
| `Minimal(), WithSink(s)` | 关 | 关 | 关 | **开** | `s`（仅供 `c.Observe` 与用户自装中间件） |
| `New(WithCollector())` | 开 | 开 | 开 | 开 | 默认 Sink |

**`WithSink` 只换出口，不复活 Trace / AccessLog**——要观测就别用 `Minimal()`（或自己 `app.Use(web.Trace())`）。

`WithCollector()` 开启 `AttachCollector`（每请求 Provide，O(插件树)），供"按 `kernel.Get(scope, observability.CollectorKey)` 打点的组件"使用；无 Sink 时装配期 panic。

`WithoutAccessLog()` 关闭访问日志（用户已接自己的日志系统时用）——只关 AccessLog，`Trace` 与 panic 兜底不受影响。

v1 选项面：`New()` / `Minimal()` / `WithSink` / `WithoutAccessLog` / `WithRoot` / `WithHostID` / `WithServer` / `WithErrorHandler` / `OnShutdown`。第二批（AsyncSink、TrustedTraceHeader）见文末「明确不做」。

### 打点入口（复用上游，不新造协议）

| 场景 | 实现 |
|---|---|
| 请求内业务打点 | `Ctx` **构造**一个 `observability.Collector`（不 Provide），`c.Observe` 内部调 `Collector.WriteAttrs` |
| 插件经 `kernel.Get` 打点 | `WithCollector()` → `observability.AttachCollector`（这也是唯一 O(插件树) 的 Provide） |
| AccessLog / panic | 宿主**直写 `observability.Record`**——需要 `Duration` / `Err`，而 Collector 明确不带这两项（`collector.go:60` 注释：状态型事实） |

**不存在自定义的 Observe 协议**——`c.Observe` / `Detached.Observe` 都是 `Collector.write` 的薄包装。

### `http.request` 记录字段（对齐 OTel HTTP 语义约定）

| 位置 | 字段 | 说明 |
|---|---|---|
| `Record.HostID` | — | **必填**（D3 运行期记录约定）；来自 `WithHostID`，空则 `"pulse-web"` |
| `Record.TraceID` | — | 必填，32hex |
| `Record.Source` | `"http"` | 宿主自定义；**不用** `SourceAdapter`（历史名，字面值 `"bridge"`） |
| `Record.Event` | `http.request` | |
| `Record.Status` | — | 状态码字符串 |
| `Record.Duration` | — | 总耗时 |
| Attrs | `http.request.method` / `http.route` / `url.path` / `http.response.body.size` / `client.address` | |
| Attrs | `error.type` | 错误分类串；**与 `Record.Err` 并存**（`Err` 供 SlogSink 输出、`error.type` 供聚合查询，OTel 亦然） |

**隐私边界**：Attrs 只有标量，无 payload 逃生舱——能记 method / status / duration / bytes，**记不了**请求体、响应体、prompt。

### TraceID 兼容

**入站**：优先读 W3C `traceparent` 的 32hex trace-id；其次 B3 `X-B3-TraceId`；都没有则**框架自带生成器产出 32hex**（不用 `observability.NewTraceID()` 的异构格式——`traceid.go:19` 明确"返回值无契约语义、宿主可自带格式"）。

**出站**：只回 `X-Trace-Id`。**不写 `traceparent`、不编造假 span-id**——pulse 模型是平铺记录、没有 span，写一个假的 span-id 会让下游 APM 误认为存在真实 span 父子关系。

**APM 拓扑降级声明**：span 树型后端（Jaeger / Zipkin / SkyWalking）里，pulse-web 的记录是该 trace 下的**独立节点**；日志检索型后端（ELK / Loki）不受影响。要补父子边需 pulse 的 `Record` 增加 span 字段——上游决策，本票不做。

### 装配诊断

v1 只做当前视图：`app.Debug("/debug/pulse")` 输出 `kernel.FiberSnapshots()` 的 JSON（默认关）。历史 loader 动作**不另存**——`Bootstrap` 已把 `loader_action` 写进 Sink，去日志看。

## 明确不做（v1 之外，各有去路）

| 项 | 去路 |
|---|---|
| `AsyncSink`（队列 / Drop / flushTimeout） | **另开 observability 的票**——Sink 契约已写明"需要截止时间的导出器自己持队列"；web 只在关闭时 `if f, ok := sink.(Flusher)` |
| 流式双记录（Flush 启发式） | 不做——普通 handler / 中间件的 `Flush()` 会误判；SSE 的语义已由"handler 不返回 ⇒ AccessLog 晚写"覆盖。需要"流开始"再显式另开票 |
| 假 span-id / traceparent 回写 | 不做（见上） |
| TTFB / Content-Type 观测 | 不做（TTFB 依赖包装器状态，与"避免额外分配"冲突），另开票 |
| `Detached` 的迷你生命周期（锁 / 懒派生 scope / ErrDetachedDisposed） | 不做——值袋子 + 调用方自理 |
| 内部 `Router` 接口 | 不做（YAGNI）：要换底层时再抽 |
| `Debug` 的 loader 历史环形缓冲 | 不做（Bootstrap 已入 Sink） |
| 多钩 `OnShutdown` + 独立预算 | 不做——单回调 + 同一 deadline 已够 |
| `Recover()` 中间件 | 不做（Engine `defer` 兜底已覆盖） |
| `HoldScope()` / `Release()` | 不做（延长请求 scope 会破坏"请求结束即回收"） |
| `RunTLS` | 不做（`tls.NewListener` + `Serve` 或反代） |
| 请求级数据对 kernel 插件可见 | 不做（需给 kernel 加 scope 局部绑定，属上游改动） |
| 内置 agent / LLM 相关的观测与装配接线 | 不做——pulse-web **只依赖 kernel 与 observability**，与 pulse 其余组件（llm / loop / host / toolset…）无耦合；需要时由调用方在自己的装配代码里显式接入 |

## 仓库结构（初版）

```
pulse-web/
├── go.mod                     # module github.com/Luo-root/pulse-web（package web）
├── doc.go                     # 包文档（定位 / 快速开始 / 运行时契约）
├── engine.go                  # Engine、选项、Root()、路由注册与分组、Static、
│                              # ServeHTTP（时序）、Engine 层收尾、Run/Serve/Handler/OnShutdown
├── context.go                 # Ctx、请求级 KV、响应写出、responseWriter 包装器
├── errors.go                  # HTTPError / StatusCoder / PanicError / 默认 mapper
├── observe.go                 # TraceID 生成与上游头解析（32hex）
├── wrap.go                    # stdlib 互操作（Wrap）
├── detach.go                  # Detached 值袋子（跨 goroutine 的安全值）
├── templates.go               # html/template 薄封装 + web.H
├── debug.go                   # 装配诊断端点（FiberSnapshots 的 JSON 视图）
├── bench/                     # 性能回归基线（go test -bench . ./bench/）
└── .github/workflows/ci.yml   # build / vet / test -race
```

## 验收标准

- [ ] 垂直切片可跑：`app.Run()` 起服务，路由 / 中间件 / JSON / 优雅关闭全通
- [ ] **`WithRoot()` 可接入外部已有的 kernel 树**：双方 `Provide` 的服务彼此可见（同一 IoC 容器）
- [ ] **默认路径零 `Provide`**（benchmark 不随插件数线性涨）；仅 `WithCollector()` 后出现 O(插件树)
- [ ] **`c` 上的业务打点与 AccessLog 进同一个 Sink**：同一 `TraceID` / `HostID`；Source 分别为 `"http"`（AccessLog）与 `"bridge"`（Collector）
- [ ] 观测贯穿：单请求 TraceID 在 router → handler → Sink 一致；后台任务共享同一 TraceID
- [ ] 标准库兼容：挂载 stdlib 中间件无侵入；`Wrap` 双向适配
- [ ] 性能回归：请求路径开销进入仓库 bench，作为基线不劣化
- [ ] 真实负载下与 gin 同级（不以 micro-benchmark 胜负作承诺）
