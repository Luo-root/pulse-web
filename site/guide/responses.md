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
- `c.Blob(code, contentType, b)` 写原始字节，`Content-Type` **原样写出**（不补 charset、不嗅探）——图片、protobuf、导出文件这类「类型由调用方说了算」的场景用它。

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

空响应（204 / 304）用 `c.NoContent(code)`——它与 `c.Status(code)` 同源：只设置状态码，由首刷落定，所以之后 `return` 的 error 仍会被错误映射接管。两者的差别只有可读性。

Cookie 用 `c.SetCookie(&http.Cookie{Name: "sid", Value: v})`，走标准库 `http.SetCookie`：**同名是追加**（一次响应可以写多条 `Set-Cookie`），值里的非法字节被标准库丢掉并记一条日志。读请求侧的 cookie 用 `c.Cookie(name)`，见[请求](/guide/requests)。

## 重定向

```go
app.GET("/old", func(c *web.Ctx) error {
    return c.Redirect(301, "/new")        // 走标准库 http.Redirect
})

app.GET("/tenant", func(c *web.Ctx) error {
    return c.Redirect(302, "dashboard")   // 相对路径：按请求路径补成绝对
})
```

语义与标准库逐字一致：相对路径按**请求路径**补成绝对、非 ASCII 转义成 `%XX`、GET 请求带一段 HTML 提示体（HEAD 与 POST 不带）；`code` 不做范围校验，非 3xx 也照写。

**它与 `Status` 的差别在落码时机**——`Redirect` 当场把响应头发出去，之后再 `return` 一个 error 也不会被错误映射接管。要先校验再重定向，校验放在调用之前。

## 文件与下载

```go
app.GET("/report", func(c *web.Ctx) error {
    return c.File("./data/report.csv")      // 走标准库 http.ServeFile
})

app.GET("/report/download", func(c *web.Ctx) error {
    return c.Attachment("./data/report.csv", "2026 年 9 月报告.csv")
})
```

- `c.File(path)` 把标准库 `http.ServeFile` 的语义整个拿来：Range、`If-Modified-Since` / `If-None-Match`、按扩展名与内容嗅探 `Content-Type`、目录命中 `index.html` 时的 301。
- `c.Attachment(path, name)` 在上面基础上加 `Content-Disposition: attachment`。`name` 交给 `mime.FormatMediaType` 编码——**非 ASCII 名走 RFC 2231**（`filename*=utf-8''…`），中文名只有这样才带得出去；手拼引号（`filename="中文.csv"`）在部分客户端上是乱码。

::: warning 文件类方法的失败**不走错误映射**
文件不存在时标准库自己写 404（纯文本错误页），不可读写写 403——**不经**框架的错误映射，观测记录里也没有错误属性。要在缺失时给出统一的错误体，自己先 `os.Stat` 再返回 `web.NotFound`。
:::

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

::: warning `Writer()` 的能力面是一份**显式的窄清单**
**显式实现并转发底层的四个**：`Flush` / `Hijack` / `SetWriteDeadline` / `EnableFullDuplex`；**不透出** `Pusher` / `FlushError`，也**不提供 `Unwrap()`**（那等于把底层 writer 整个交出去，连上面两个一起）。内嵌 `http.ResponseWriter` 只提升 `Header` / `Write` / `WriteHeader`，其余能力必须显式实现才有——所以清单就是全部，一眼可见，不靠断言碰运气。

**协议升级（WebSocket）走 `Hijack`**：`w := c.Writer().(http.Hijacker)` 拿到的就是这条连接，生态库零改动直接可用（`gorilla/websocket` 直接断言 `w.(http.Hijacker)`；`coder/websocket` 先断言、再沿 `Unwrap()` 链找——两条路都满足）。连接交出去之后框架不再写这条响应（`Write` / `Flush` 返回 `http.ErrHijacked`），收尾不落码、也不写兜底错误体；访问日志按 `101` 记并带 `connection.hijacked` 标记（本框架声明的扩展键），**不记** `http.response.body.size`——连接已交出，记 0 会被读成「响应是空的」。

**两条边界**：① **HTTP/2 下不可用**（h2 的 writer 不是 `Hijacker`）；② **洋葱内可用，但受中间件影响**——`Adapt` 交出去的代理会转发 `Hijack`（`Use(Adapt(…))` 之后的升级路由照常工作），而中间件自己再包一层且不转发时同样会断，这与纯 stdlib 下完全同形（见 [stdlib 中间件接入](/guide/middleware)）。
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
