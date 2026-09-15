# pulse-web

基于 [pulse](https://github.com/Luo-root/pulse) 的 **kernel** 与 **observability** 构建的通用 Go web 服务框架。

- **装配内核**——kernel 的 IoC、可逆生命周期、请求作用域、事件总线
- **一等观测**——每请求 TraceID、结构化记录、装配诊断，默认装配；默认出口是**给人读**的列式单行（`ConsoleSink`，渲染 0 分配）
- **零第三方依赖**——只使用 stdlib 与 pulse 的两个基座包（kernel / observability）

> 状态：**实现中**。设计与决策记录在 [Issue #1](https://github.com/Luo-root/pulse-web/issues/1)，完整设计见 [`docs/design/web-framework-design.md`](docs/design/web-framework-design.md)。
> 已落地：垂直切片（Engine / 路由与分组 / Ctx / 错误模型 / 观测接线 / 优雅关闭，[#2](https://github.com/Luo-root/pulse-web/issues/2)）；周边能力（Detach / HTML 模板 / Debug 端点 / TraceID 信任开关，[#4](https://github.com/Luo-root/pulse-web/issues/4)）；pulse v0.2.2 采纳（请求级绑定语义 + 观测出口 flush 覆盖 + `WithCollector()`，[#6](https://github.com/Luo-root/pulse-web/issues/6)）。

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
