# 测试：不启 server 也能测 handler

框架提供两个入口，让 handler 单测走的**装配与收尾与真实请求完全一致**——不是"近似一致"，是同一份代码（`Engine.begin` + `finish`）。所以测出来的状态码落定、错误映射、访问日志、`X-Trace-Id`，就是线上那一套。

## 两个入口

```go
app := web.New(web.WithSink(sink))       // sink 是测试里能读的东西

rec := httptest.NewRecorder()
req := httptest.NewRequest("GET", "/users/42?q=1", nil)
req.Pattern = "GET /users/{id}"          // 路由模板：不走 ServeMux 就自己补
req.SetPathValue("id", "42")             // 路径参数同理

c, done := app.NewTestContext(rec, req)  // ① 拿到 Ctx，想测哪一段就调哪一段
defer done()                             //    done 负责收尾（幂等）
if err := myHandler(c); err != nil {
    t.Fatal(err)
}
// 到这里 rec 与 sink 里就是走完整链路的结果
```

```go
app.ServeTest(rec, req, myHandler, requireAuth, withTx)
// ② 连中间件一起跑，并且 panic 也照真路径那样被接管
```

## 路径参数与路由模板要自己补

`c.Path("id")` 读的是 `Request.PathValue`，`http.route` 属性读的是 `Request.Pattern`——平时这两样由 `ServeMux` 填。不走 mux 就得自己写上；两个都是标准库的导出面，不需要框架特供的 API。

## panic 归谁管

- **`ServeTest`**：handler 在框架的 `defer` 里执行，panic 会照真路径那样变成 `PanicError` 走错误映射（500），并照常落访问记录。
- **`NewTestContext`**：handler 在**你自己的栈上**执行，框架的 recover 不在那条栈上——panic 直接传给你，由 `testing` 包正常报错。要覆盖 panic 那条路径请用 `ServeTest`。

## 什么时候仍然该起真 server

::: warning 这三类问题只有真链路能答
- **协议层**：Content-Type 嗅探、chunked 编码、自动 `Content-Length`——这些发生在 net/http **服务端**，而 `httptest.ResponseRecorder` 根本不走那段逻辑（拿它测响应头会得出假结论，实测过：`Header().Set("Content-Type", "")` 与"完全不设这个头"在 Recorder 上都是"空"，在真链路上一是空头、一是 `text/html`）。
- **真实网络行为**：超时、`Flush` 的实时性、客户端断开、连接复用。
- **完整路由**：`ServeMux` 的模式匹配、优先级、分组前缀——两个测试入口都绕过了 mux。
:::

要真链路就用 `httptest.NewServer(app)`（`Engine` 本身就是 `http.Handler`），或者把 `app.Handler()` 交给上层框架。
