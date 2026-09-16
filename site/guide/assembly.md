# 装配与运行

`web.New()` 起一个 `Engine`；所有配置项都是**函数式选项**（`web.Option`），没有配置结构体、也没有字符串开关。这一页按「起服务 → 调装配 → 跑起来 → 关掉」的顺序讲。

## 起服务

```go
app := web.New()                      // 自建 kernel root + 装 Bootstrap
err := app.Run(":8080")               // 阻塞至关闭完成；nil = 收到信号正常关闭
```

三种组装方式：

| 写法 | 什么时候用 |
|---|---|
| `web.New()` | 默认：自建 root、装好观测、默认出口写 stdout |
| `web.New(web.WithRoot(k))` | 与宿主 / 别的组件**共用同一棵 kernel 树** |
| `web.New(web.Minimal())` | 只要路由与中间件，**不要** Bootstrap 与默认出口（loadtest 里的 `bare` 对照档就是它） |

```go
app := web.New(web.Minimal())          // 等价于「不装观测」的引擎
```

::: warning `Minimal()` 与 `WithCollector()` 不能单独配
`WithCollector()` 要把请求级 Collector 绑到作用域上，需要一个 Sink 承接记录——**`Minimal()` + `WithCollector()` 而不给 `WithSink` 会在装配期 panic**（这是装配错误，不该拖到第一个请求才 panic）。
:::

## 装配面：`Root()`

```go
root := app.Root()                     // 进程级 kernel root
kernel.Provide(root, dbKey, sqlDB)     // 其余用 kernel 原生 API：Provide / Use / Loader / FiberSnapshots
kernel.Use(root, myPlugin)
```

**`Root()` 是装配叙事的关键**：任何需要「进程级 kernel」的组件（自建插件、第三方接入、跨请求存活的装配）都必须拿它。**不要把 `c.Kernel()` 当进程级 kernel 用**——那是请求 scope，挂上去的插件与 Effect 会在请求结束时被级联销毁。

框架**不包装** kernel 的其余装配 API：`Provide` / `Use` / `Loader.Reconcile` / `FiberSnapshots` 直接用原生的。

## 选项一览

| 选项 | 作用 |
|---|---|
| `WithRoot(k)` | 接入已有 kernel root（默认自建） |
| `WithHostID(id)` | 写进每条观测记录的 `host` 字段 |
| `WithSink(sink)` | 换观测出口（默认 `NewConsoleSink(os.Stdout)`） |
| `WithCollector()` | 每请求挂一个 Collector（**+12 allocs**，见[性能](/performance)） |
| `WithTrustedTraceHeader(false)` | **默认 `true`**：采纳入站 `traceparent` / B3 链路头 |
| `WithoutAccessLog()` | 关掉访问日志（Trace 与 panic 兜底还在） |
| `WithServer(ServerConfig)` | 覆盖 HTTP server 参数（见下） |
| `WithErrorHandler(mapper)` | 换错误映射器（见[错误模型](/guide/errors)） |
| `WithTemplates(TemplateConfig)` | 装模板（见[响应](/guide/responses)） |
| `WithMaxBodyBytes(n)` | 全局请求体上限（`0` = 不限） |

默认出口想要配色或换 writer：

```go
app := web.New(web.WithSink(web.NewConsoleSink(os.Stdout, web.WithColor(false))))
```

`NewConsoleSink(w io.Writer, opts ...web.ConsoleOption) *web.ConsoleSink`——`WithColor(bool)` 就是其中一枚 `ConsoleOption`。出口的选法见[观测](/guide/observability)。

## `ServerConfig`

零值即默认；只想改一项时用 `web.DefaultServerConfig()` 起步：

```go
cfg := web.DefaultServerConfig()
cfg.WriteTimeout = 30 * time.Second    // 非流式服务建议显式设；默认 0 是为了不切断 SSE
app := web.New(web.WithServer(cfg))
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `ReadHeaderTimeout` | 5s | 防慢头攻击 |
| `ReadTimeout` | 30s | 读完整请求上限 |
| `WriteTimeout` | **0** | **刻意不限**——非 0 会切断 SSE |
| `IdleTimeout` | 120s | keep-alive 空闲上限 |
| `MaxHeaderBytes` | 1 MiB | 请求头上限 |
| `ShutdownTimeout` | 30s | 优雅关闭总预算（不含出口 flush，见下） |

## 运行：`Run` / `Serve` / `Handler`

```go
app.Run(":8080")                       // 内置 http.Server + 信号处理
app.Serve(ln)                          // 自定义 listener（TLS 走 tls.NewListener，或反代终止）
app.Handler()                          // 导出 http.Handler 视图；也可直接把 app 当 http.Handler
```

- `Run` **阻塞**；`nil` = 收到信号正常关闭，非 nil = 启动失败或关闭期错误。两条路径都完成 `root.Dispose()`——**调用方不能再 Dispose**。
- **`Run` 返回后 Engine 不可复用**（kernel 已销毁）。
- 没有 `RunTLS`：TLS 交给 `tls.NewListener` + `Serve`，或者由反向代理终止。
- `ServeHTTP` / `Handler()` 让它能塞进任何吃 `http.Handler` 的位置（`httptest` 测试、挂到别的 mux 上、serverless 适配器都行）。

## 关闭：`OnShutdown`

```go
app.OnShutdown(func(ctx context.Context) error {
    return backgroundJobs.Wait(ctx)     // 单回调，用来等在途后台任务
})
```

关闭时序（`Run` / `Serve` 都会走完）：

1. `srv.Shutdown(timeoutCtx)` —— **drain 在这里**（等完在飞的请求）
2. 超时 → `srv.Close()` 强制断开
3. `OnShutdown` 回调 —— 用户等自己的后台任务
4. `root.Dispose()` —— 终局：级联截断（**不等待在途工作**；未注册等待的后台任务会在这里被截断）
5. **出口 flush** —— 独立 3s 预算，**不计入 `ShutdownTimeout`**

关闭总时长上限 = `ShutdownTimeout` + 3s；deadline 如何分摊、flush 预算与出口所有权归谁，见[落地与运行时契约](/guide/ops-contracts)。

## 装配诊断：`Debug`

```go
app.Debug("/debug/kernel")             // 挂一个只读 JSON 端点
```

`func (e *Engine) Debug(path string)`：把 `kernel.FiberSnapshots()` 渲染成 JSON——排查「装配到底装上了什么、哪个 Effect 在哪一层」用。它只做**当前视图**，不另存 loader 历史（历史已在 Bootstrap 写入 Sink，去日志里看）。**默认不挂载；暴露前自行评估鉴权**（输出含插件名与失败原因）。
