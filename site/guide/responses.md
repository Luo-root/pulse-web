# 响应：写出、流式与模板

一个 handler 想写响应，有三条路：结构化写出（`JSON` / `Text` / `HTML`）、直接写字节（`Writer`）、流式推送（`Flush`）。状态码与响应头在**首刷**之前都可以改。

## 结构化写出

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
    return c.JSON(200, web.H{             // web.H = map[string]any 的类型别名
        "id":   c.Path("id"),
        "name": "jiangnan",
    })
})

app.GET("/healthz", func(c *web.Ctx) error {
    return c.Text(200, "ok")
})
```

- `c.JSON(code, v)` **先编码到内存缓冲、成功后才写响应头**——编码失败不会留下一个 200 空体，代价是每响应 **+2 allocs / +97 B**（数字见[性能](/performance)）。它的输出不流式。
- `c.Text(code, s)` 自动补 `text/plain; charset=utf-8`；`c.JSON` 补 `application/json`。

## 状态码与响应头

```go
app.POST("/users", func(c *web.Ctx) error {
    c.Status(202)                       // 只设置，不立即写头
    c.SetHeader("Location", "/users/42")
    c.SetHeader("X-Trace", c.TraceID())
    return c.JSON(202, created)         // 这里的 code 会覆盖上面的 Status
})
```

**首刷 = 第一次真正写出**，它可能来自三处里的最先一个：`c.Writer().Write`、`c.Flush()`、handler 返回后的引擎收尾。首刷之后响应头已发出，再调 `Status` 无效。所以「先 `Status(201)` 再写第一段、最后 `Flush`」这条自然顺序是成立的（三条路径共用同一实现，不会出现「`Status` 被第一次 `Write` 的隐式 200 吃掉」）。

## 直接写字节：`Writer`

```go
app.GET("/raw", func(c *web.Ctx) error {
    w := c.Writer()                     // http.ResponseWriter 的包装器
    w.Header().Set("X-Custom", "1")     // 等价于 c.SetHeader
    _, err := w.Write([]byte("chunk"))
    return err
})
```

包装器照常采集状态码与响应体积（所以访问日志里的 `size=` 对这条路径一样准）。

::: warning `Writer()` 只承诺 `http.Flusher`
底层能力（`Hijacker` / `Pusher` / `FlushError` / `SetWriteDeadline`）**不透出**——内嵌只提升 `Header` / `Write` / `WriteHeader`。所以 `http.NewResponseController(c.Writer())` 上只有 `Flush()` 可用，`Hijack()` / `SetWriteDeadline()` 返回 `http.ErrNotSupported`。**要升级协议（WebSocket）请用 `web.Wrap` 包一个 stdlib handler**，代价是拿不到 `*Ctx`。
:::

## 流式：SSE

```go
app.GET("/events", func(c *web.Ctx) error {
    c.SetHeader("Content-Type", "text/event-stream")
    c.SetHeader("Cache-Control", "no-cache")
    c.Status(200)
    if err := c.Flush(); err != nil {     // 首刷落定状态码，客户端开始收
        return err
    }
    for i := 0; i < 5; i++ {
        if _, err := c.Writer().Write([]byte("data: tick\n\n")); err != nil {
            return err
        }
        if err := c.Flush(); err != nil {
            return err
        }
        time.Sleep(time.Second)
    }
    return nil
})
```

`c.Flush()` 底层不支持 `http.Flusher` 时返回**明确 error**（不静默；`ServeHTTP` 的默认 writer 支持它）。

::: tip 想让 SSE 不被超时切断，看 `ServerConfig.WriteTimeout`
默认是 **0（不限）**，这是刻意的——非 0 会切断长连接流。反过来说，**非流式服务建议显式设 30s**，见[装配与运行](/guide/assembly)。
:::

## 模板

薄封装标准库 `html/template`（不自己实现模板引擎）：

```go
app := web.New(web.WithTemplates(web.TemplateConfig{
    Root:      "./views",   // 模板根目录
    Pattern:   "*.html",    // glob 模式（默认值）
    DevReload: false,       // true = 每请求重新解析（开发用）
}))

app.GET("/", func(c *web.Ctx) error {
    return c.HTML(200, "index.html", web.H{
        "Title": "首页",
        "User":  user,
    })
})
```

- **生产模式**启动时解析一次并缓存（并发安全）；`DevReload: true` 每请求重新 `ParseGlob`（每请求磁盘扫描 + 写锁），**仅供开发**。
- `c.HTML` 的三条错误路径（未配置模板 / 模板名不存在 / 执行期报错）**一律返回明确 error**，不静默 500——实现上先渲染到内存缓冲、成功后才写响应头，代价是模板输出不流式（要流式用 `c.Writer()` + `c.Flush()`）。
- 自动补 `Content-Type: text/html; charset=utf-8`。

需要自定义模板函数时，用标准库 `html/template` 的 `Funcs` 在 `Root` 指向的模板上注册——框架不额外包装这一层。
