# 测试：不启 server 也能测 handler

框架提供两个入口，让 handler 单测走的**装配与收尾与真实请求同源**——同一份代码（`Engine.begin` + `finish`）。所以状态码落定、错误映射、访问日志、`X-Trace-Id`，都是线上那一套产出的。

与真路径相差的只有 `ServeMux`：路由匹配、分组前缀、路由级 `BodyLimit` 不参与，路径参数与路由模板要自己补在 request 上。

## 两个入口

```go
app := web.New(web.WithSink(sink))       // sink 是测试里能读的东西

rec := httptest.NewRecorder()
req := httptest.NewRequest("GET", "/users/42?q=1", nil)
req.Pattern = "GET /users/{id}"          // 路由模板：不走 ServeMux 就自己补
req.SetPathValue("id", "42")             // 路径参数同理

c, done := app.NewTestContext(rec, req)
done(myHandler(c))                       // 跑 + 回灌 error + 收尾

if rec.Code != http.StatusNotFound {     // 错误映射照常生效
    t.Fatalf("status = %d", rec.Code)
}
```

```go
app.ServeTest(rec2, req2, myHandler, requireAuth, withTx)
// handler + 中间件链：传入的 mw **追加**在 Use 与分组中间件之后
```

**什么时候用哪个**：要「只跑某一段」用 `NewTestContext`——代价是它不接管 panic、不走请求体闸门，`Engine.Use` 的中间件也不参与。要覆盖这几样，用 `ServeTest`。

## 路径参数与路由模板要自己补

`c.Path("id")` 读的是 `Request.PathValue`，`http.route` 属性读的是 `Request.Pattern`——平时这两样由 `ServeMux` 填。两个测试入口都绕过了 mux，所以得自己写上；两个都是标准库的导出面，不需要框架特供的 API。

## 两张入口各自管什么

| | `NewTestContext` | `ServeTest` | 真路径 |
|---|---|---|---|
| 错误映射（`done(err)` / `return err`） | ✅ | ✅ | ✅ |
| `Engine.Use` 与分组中间件 | ❌ | ✅ | ✅ |
| 请求体闸门（`WithMaxBodyBytes`） | ❌ | ✅ | ✅ |
| panic → `PanicError`（500） | ❌ 传给你 | ✅ | ✅ |
| `ServeMux` 路由匹配 | ❌ | ❌ | ✅ |

`done` 幂等：**第一次调用生效**。handler 可能 panic 的场合，写 `defer done(nil)` 再显式 `done(err)`——正常路径下显式那次先到，panic 时才轮到 defer 兜底。

## 什么时候仍然该起真 server

::: warning 这三类问题只有真链路能答
- **协议层**：Content-Type 嗅探、chunked 编码、自动 `Content-Length`——这些发生在 net/http **服务端**，而 `httptest.ResponseRecorder` 根本不走那段逻辑（拿它测响应头会得出假结论，实测过：`Header().Set("Content-Type", "")` 与"完全不设这个头"在 Recorder 上都是"空"，在真链路上一是空头、一是 `text/html`）。
- **真实网络行为**：超时、`Flush` 的实时性、客户端断开、连接复用。
- **完整路由**：`ServeMux` 的模式匹配、优先级、分组前缀——两个测试入口都绕过了 mux。
:::

要真链路就用 `httptest.NewServer(app)`（`Engine` 本身就是 `http.Handler`），或者把 `app.Handler()` 交给上层框架。
