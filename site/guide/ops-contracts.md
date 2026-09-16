# 落地与运行时契约

这一页分两半：**日志怎么落**（运维那一半，框架只负责写到 stdout），以及**框架承诺的运行时时序**（哪些事在什么时候发生——写 handler 时会遇到的那些）。

## 日志落地：默认档只写 stdout

**默认出口 `NewConsoleSink(os.Stdout)` 本身不是持久化**：进程把行写进 fd 1，落盘 / 轮转 / 保留归平台。这条划分是有意的（12-factor）：应用不管文件系统，平台管，两者用 stdout 解耦。

| 路径 | 做法 | 轮转 / 保留归谁 |
|---|---|---|
| 容器 / k8s | 默认出口即可 | 运行时：docker `log-opts max-size/max-file`、kubelet `containerLogMaxSize/containerLogMaxFiles`——**默认值往往很小或无限增长，要显式设** |
| systemd（单机） | 默认出口即可（stdout 默认进 journald） | `journald.conf` 的 `SystemMaxUse` / `MaxRetentionSec`；查用 `journalctl -u <svc>` |
| 直接落文件 | `os.OpenFile(…, O_APPEND\|O_CREATE\|O_WRONLY, 0o644)` + `WithSink(NewConsoleSink(f))` | **宿主自备**（框架零依赖，不内置轮转） |
| 人看 + 送采集器 | 上游 `observability.MultiSink{console, slog}` | 同上 |

**丢失窗口按缓冲层级**（越往上越不容易丢，代价是越慢）：

1. `ConsoleSink` —— **无缓冲**，写完即落 fd（只丢内核 page cache 未回写的那一段）
2. 上游 `LineSink` —— 32 KiB 缓冲（引擎在优雅关闭时会 flush，见下）
3. 上游 `AsyncSink` —— 队列（默认容量 1024；满时默认回压，`DropOnFull()` 则计丢）

要更强保证得自己 `fsync`（每请求毫秒级代价，访问日志通常不值）；要「一条不丢」的语义，那不是日志通道的事。

**写失败不静默**：出口的 `Err()` 返回**首次**写错误（后续失败或成功都不覆盖它）。框架在关闭时会 `Flush()` 一次并把首错记成 `slog.Warn`；进程活着的时候要告警，就在自己的健康检查里定期读一次：

```go
if err := sink.Err(); err != nil {
    // 告警：访问日志已经写不进去了（首次错误，含 broken pipe 这类）
}
```

**明确不做**：轮转 / 保留 / 压缩（属宿主或第三方）、fsync 策略、采样丢弃、把写错误塞进请求路径。

## 运行时契约

### 请求的生命周期

```
请求到达
  → 中间件链（全局 → 分组 → 路由）
  → handler 返回 error / nil
  → 错误映射器决定状态码并写响应
  → 请求 scope Dispose（LIFO 回收）
```

**`Dispose` 在写响应之前**（设计红线）：请求作用域的回收与「响应是否送达网络」解耦——这样 Effect 的失败不会拖住响应，也不会因为响应写出失败而漏掉回收。

一个直接推论：**错误映射器运行时 scope 已经没了**。映射器里能用的只有 `Get` / `Set` / `Observe` / `TraceID()` / `Path` / `Query()`（KV 挂在 `Ctx` 上，活得比 scope 长）；**`c.Service` / `c.Kernel()` 在那一刻是死 scope**。

### `Ctx` 的边界

- **不可跨 goroutine**：它与请求及其响应写出器绑定。后台任务用 `c.Detach()`，拿到的是值袋子（`TraceID` / `HostID` / `Sink` / 进程级 `Root`），不是上下文——见[请求](/guide/requests)。
- **响应写出器只承诺 `http.Flusher`**：`Hijacker` / `Pusher` / `FlushError` / `SetWriteDeadline` 不透出。要让「流式能干什么」在类型层面一眼可见，而不是靠断言碰运气。
- **首刷之后状态码锁死**：`c.Writer().Write`、`c.Flush()`、handler 返回后的引擎收尾——三处里最先发生的那个落定状态码。

### 错误映射器契约

- 返回的 error 决定状态码；**`cause` 只进观测记录，绝不进响应体**。
- **非 `HTTPError` 的普通 error 一律 500** + 通用文案（安全默认）。
- `StatusCoder`（任何带 `StatusCode() int` 的 error）用它的状态码，消息仍走默认脱敏。
- panic → `PanicError` → **一律 500**（栈只进记录）；响应已写出则只记录不重写。
- 自定义 `ErrorHandler` 返回 `nil` = 已处理，返回 error = 继续走默认映射。

### 优雅关闭时序

```
信号（SIGINT / SIGTERM）
  → ① srv.Shutdown(timeoutCtx)      ← drain 在这里（等完在飞的请求）
  → ② 超时 → srv.Close()            ← 强制断开
  → ③ OnShutdown 回调               ← 单回调，用户等自己的后台任务
  → ④ root.Dispose()                ← 终局：级联截断，不等待在途工作
  → ⑤ 出口 flush                    ← 独立 3s 预算，不计入 ShutdownTimeout
```

- ①②③ **共享同一个 deadline**（总时长 ≤ `ShutdownTimeout`，默认 30s，与 k8s `terminationGracePeriodSeconds` 对齐）；HTTP 用光预算时回调拿到的是已过期的 ctx，框架会记录「后台任务未等待」。
- ④ 是**级联截断**，不是 drain：**未注册等待的后台任务会在这里被截断**（明示行为）。要等就在 ③ 里等。
- ⑤ **只 flush、不 `Close`**：出口的所有权归装配方——`Close` 会静默丢掉 `Detach` 出来的后台任务还在写的记录。它同时认两种出口形态（`Flush(context.Context) error` 与 `Flush() error`），返回的错误记 `slog.Warn`。
- 关闭总时长上限 = `ShutdownTimeout` + 3s。

::: tip 契约的完整版在设计文档
这一页给的是**摘要**：完整推导、边界与实测见[设计文档](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md)的「运行时契约」与「观测设计」两节。
:::
