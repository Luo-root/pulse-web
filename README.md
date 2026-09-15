# pulse-web

基于 [pulse](https://github.com/Luo-root/pulse) 的 **kernel** 与 **observability** 构建的通用 Go web 服务框架。

- **装配内核**——kernel 的 IoC、可逆生命周期、请求作用域、事件总线
- **一等观测**——每请求 TraceID、结构化记录、装配诊断，默认装配；默认出口是**给人读**的列式单行（`ConsoleSink`，渲染 0 分配）
- **零第三方依赖**——只使用 stdlib 与 pulse 的两个基座包（kernel / observability）

> 状态：**v1 功能面已实现**（12/12）——验收标准逐条附可复跑的测试证据，见设计文档的「验收标准」节。API 在 1.0 之前，仍可能随 minor 调整。
> 设计与决策记录在 [Issue #1](https://github.com/Luo-root/pulse-web/issues/1)，完整设计见 [`docs/design/web-framework-design.md`](docs/design/web-framework-design.md)；逐项实现与实测记录见各 Issue（[#2](https://github.com/Luo-root/pulse-web/issues/2) 垂直切片、[#4](https://github.com/Luo-root/pulse-web/issues/4) 周边能力、[#6](https://github.com/Luo-root/pulse-web/issues/6) / [#27](https://github.com/Luo-root/pulse-web/issues/27) 上游采纳、[#20](https://github.com/Luo-root/pulse-web/issues/20) 默认出口、[#18](https://github.com/Luo-root/pulse-web/issues/18) 真实负载对比、[#30](https://github.com/Luo-root/pulse-web/issues/30) 验收证据收口）。

## 预览（API 草案）

```go
app := web.New()

app.GET("/users/{id}", func(c *web.Ctx) error {
    db := c.MustService(dbKey)          // 全局服务（kernel root 仓库）
    u, err := db.Find(c.Path("id"))     // 路径参数读 Request.PathValue，不另存
    if err != nil {
        return web.NotFound("user", err) // 显式 error → 状态码映射 + 观测记录
    }
    return c.JSON(200, u)
})

app.Run(":8080")                         // 内置优雅关闭：drain → OnShutdown → root.Dispose → Sink flush
```

默认出口写 stdout，一行一条（列宽固定；颜色只在终端生效）：

```
PULSE | 2026/09/14 - 08:30:00 | 200 |   585.1µs | 192.0.2.1:1234  | GET     /users/42 | route=/users/{id} | size=29 | host=pulse-web | trace=8f2e1a3b4c5d6e7f8a9b0c1d2e3f4a5b
PULSE | 2026/09/14 - 08:30:00 | 500 |    7.62ms | 192.0.2.1:1234  | GET     /boom | http_5xx "boom: Internal Server Error" | trace=3a71…
PULSE | 2026/09/14 - 08:30:00 | pulse.kernel.fiber_state host=svc fiber=db state=Starting→Running
```

（三行都由实现产出，只有第 2 行的 `trace=` 截断显示；路径列不是定宽列，所以 `/boom` 后面只有分隔符前那一个空格。）

行首 `PULSE` 是上游缺省标识——pulse 与 pulse-web 同根同源，同一进程树里两个出口的行首一致。
尾段的 `route=` / `size=` / `host=` / 错误 / `trace=` 是各自独立的 ` | ` 字段，有才出现。

要机器可读 / 接既有日志管道：`web.New(web.WithSink(observability.SlogSink{...}))`（或 `NewAsyncSink`、`NewLineSink`）——**只换出口，装配不变**。

> `WithRoot` 注意：接入既有 kernel 树时，`Run` / `Serve` 返回后该 root 会被级联销毁（连同挂在它上面的插件）——不要在 Run 返回后继续使用。

### 流式响应（SSE）

`c.Writer()` 拿响应写出器直接写字节，`c.Flush()` 逐段推给客户端：

```go
app.GET("/events", func(c *web.Ctx) error {
    c.SetHeader("Content-Type", "text/event-stream")
    c.SetHeader("Cache-Control", "no-cache")

    w := c.Writer()
    for ev := range events {
        if _, err := fmt.Fprintf(w, "data: %s\n\n", ev); err != nil {
            return nil // 客户端断开：响应已开始，静默收尾
        }
        if err := c.Flush(); err != nil { // 只在底层 writer 不支持 Flusher 时发生
            return err
        }
    }
    return nil // handler 返回 ⇒ 请求 scope 回收 + 一条 AccessLog
})
```

写出器是框架的包装器：状态码与响应体积照常进 AccessLog（写多少字节就记多少）。能力面是**有意的窄口**——只保证 `http.Flusher`（`http.NewResponseController(w).Flush()` 也可用）；`Hijacker` / `Pusher` / `FlushError` / `SetWriteDeadline` **不透出**，要升级协议拿原始 writer 请用 `web.Wrap` 包 stdlib handler。
（`Flush` 的首刷落 `Status()` 设置的状态码（缺省 200）；首刷之后响应头已发出，流式接口要在首刷**之前**设好状态码。）

## 请求体绑定

`c.Bind()` 按 Content-Type 分派——JSON / XML / form-urlencoded / multipart 一套覆盖，失败按语义映射错误码：

```go
app.POST("/users", func(c *web.Ctx) error {
    var in CreateUser
    if err := c.Bind(&in); err != nil {
        return err // 400 invalid_body / 413 body_too_large / 415 unsupported_media_type
    }
    return c.JSON(201, in)
})

app.GET("/users", func(c *web.Ctx) error {
    var q ListQuery
    if err := c.Bind(&q); err != nil { // 无 body 的请求自动落到 query
        return err // 400 invalid_query
    }
    return c.JSON(200, filter(q))
})
```

表单字段用 `form:"..."` / `query:"..."` tag（无 tag 用字段名，大小写不敏感）；支持 string / bool / 数值全系、多值 slice（`?ids=1&ids=2` → `[]int`）与指针字段；multipart 额外支持 `*multipart.FileHeader`。

请求体默认**不设上限**（对齐 gin / echo 的默认形态——上限值是业务策略）；生产建议显式设置，或依赖前置反代（nginx 默认 `client_max_body_size 1m`）：

```go
app := web.New(web.WithMaxBodyBytes(2 << 20)) // 2 MiB：超限读取立即失败 → 413
```

## 日志落地（持久化归谁）

**默认档只写 stdout，它本身不是持久化**：进程只把行写进 fd 1，落盘、轮转、保留都由平台负责。
三条常见路径，各配各的：

**① 容器 / k8s**——什么都不用改，运行时把 stdout 收成节点上的 JSON 文件。
**要显式设轮转**，否则默认值要么很小要么无限增长：

```yaml
# docker-compose
logging: { driver: json-file, options: { max-size: "50m", max-file: "5" } }
```
```yaml
# kubelet（节点级）
containerLogMaxSize: 50Mi
containerLogMaxFiles: 5
```

**② systemd（单机 VPS）**——服务的 stdout/stderr **默认就进 journald**，零额外组件；
保留策略在 `/etc/systemd/journald.conf`：

```ini
SystemMaxUse=2G
MaxRetentionSec=2week
```

查日志：`journalctl -u <服务名> --since "10 min ago"`，`-f` 跟流。

**③ 直接落文件**——自己开文件交给出口；轮转/保留自备（框架不内置，零依赖）：

```go
f, err := os.OpenFile("/var/log/myapp.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
if err != nil { log.Fatal(err) }
app := web.New(web.WithSink(web.NewConsoleSink(f)))
```

既要人能看又要送采集器，用上游的扇出：

```go
web.WithSink(observability.MultiSink{
    web.NewConsoleSink(os.Stdout),
    observability.NewLineSink(f),   // 32 KiB 缓冲，进程退出时引擎会 Flush
})
```

**丢多少由缓冲层级决定**：`ConsoleSink` 无缓冲（只丢内核 page cache 里没回写的那一段）→
`LineSink` 32 KiB → `AsyncSink` 队列（满时按策略丢）。要更强的保证得自己 `fsync`
（每请求毫秒级代价，access log 通常不值）；要「一条不丢」的语义，那不是日志通道的事。

**写失败要看一眼**：`Sink` 接口不返回错误，所以失败（stdout 管道被关、journald socket 满、
磁盘满）从 `ConsoleSink.Err()` 读——它返回**首次**写失败：

```go
if err := sink.Err(); err != nil { /* 告警：访问日志已经写不进去了 */ }
```

## 文档

- [框架设计（v1）](docs/design/web-framework-design.md)——定位、决策、API 面、运行时契约、观测设计、明确不做清单

## 开发

CI 门禁（`.github/workflows/ci.yml`）：`go build` / `go vet` / **`gofmt -l` 判空** / `go test -race` / bench 编译检查。

本地复现格式化门禁时，有**三条会造成假阳性的坑**，判据不要直接看输出：

- **CRLF**：仓库已用 `.gitattributes` 把行尾钉成 LF，**新克隆不会有这个问题**；但属性生效**之前**签出的工作副本仍是 CRLF（git 不会回头重写已签出的文件），这一条只对那种旧工作副本适用。判据是**转成 LF 副本后零差异**。
- **未跟踪目录**：`gofmt -l .` 会连未跟踪目录一起扫（例如 `_scratch/`）。只查已跟踪文件，用 `git ls-files '*.go'`。
- **gofmt 版本**：PATH 上可能是旧版，解析不了泛型方法一类的新语法，表现同样是**全量**报错。用工具链自带的那个。

与 CI 一致的命令（CI 用的就是 Linux 那条）：

```bash
# Linux（= CI）
"$(go env GOROOT)/bin/gofmt" -l $(git ls-files '*.go')
```

```powershell
# Windows PowerShell —— 这里 `go env GOROOT` 返回 `C:\...`，bash 起不来，所以给 PowerShell 形态
& (Join-Path (go env GOROOT) 'bin\gofmt.exe') -l (git ls-files '*.go')
```
