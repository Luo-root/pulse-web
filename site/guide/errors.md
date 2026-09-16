# 错误模型

handler 不写状态码，**只返回 error**；由**错误映射器**统一决定「回什么状态、记什么日志、暴露多少信息」。这一页讲这套约定怎么用、边界在哪。

## 基本形态

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
    u, err := loadUser(c.Path("id"))
    switch {
    case errors.Is(err, errNotFound):
        return web.NotFound("user", err)     // 404，业务码 user
    case err != nil:
        return err                           // 未知错误 → 500（原始 error 只进记录）
    }
    return c.JSON(200, u)
})
```

**返回什么就是什么**：handler 里不用碰状态码，测试里断言的也是同一个返回值。

## `HTTPError` 与构造器

```go
type HTTPError struct {
    Status  int    // HTTP 状态码
    Code    string // 机器可读业务码
    Message string // 人读消息（默认取状态码标准文案）
}
func (e *HTTPError) Error() string   // 消息
func (e *HTTPError) Unwrap() error   // 原始错误（cause）
func (e *HTTPError) StatusCode() int // 实现 StatusCoder
```

7 个构造器，全部 `(code string, cause error)`：

| 构造器 | 状态码 | 典型场景 |
|---|---|---|
| `web.BadRequest(code, cause)` | 400 | 参数不合法 |
| `web.Unauthorized(code, cause)` | 401 | 未认证 |
| `web.Forbidden(code, cause)` | 403 | 已认证但无权限 |
| `web.NotFound(code, cause)` | 404 | 资源不存在 |
| `web.Conflict(code, cause)` | 409 | 并发/唯一性冲突 |
| `web.TooLarge(code, cause)` | 413 | 请求体超限 |
| `web.Internal(code, cause)` | 500 | 服务端出错 |

需要 429 / 422 / 503 这类：**实现 `StatusCoder`**（任何 `error` 带 `StatusCode() int` 即可），或直接 `&web.HTTPError{Status: 429, Code: "rate_limited"}`。

```go
type rateLimited struct{ retry int }
func (e rateLimited) Error() string   { return "rate limited" }
func (e rateLimited) StatusCode() int { return 429 }

app.GET("/api", func(c *web.Ctx) error {
    return rateLimited{retry: 30}      // 429，message 走默认脱敏
})
```

## 安全默认

- **`cause` 只进观测记录，绝不进响应体**。响应体是 `{"error": {"code": "user", "message": "not found"}}` 这个形状。
- **非 `HTTPError` 的普通 error 一律 500** + 通用文案（不把内部错误文案漏给调用方）。
- 消息默认取状态码的标准文案；要自定义就用 `&web.HTTPError{...}` 显式给 `Message`。

## panic

panic 由 Engine 兜底接住，包成 `PanicError{Value, Stack}`——**默认一律 500**，栈只进记录。

::: warning 陷阱：`panic` 一个 HTTPError 不会变成那个状态码
`panic(web.Unauthorized("x", nil))` 得到的是 **500**，不是 401。要 4xx 请**返回**它。这条是显式设计：panic 是异常路径，不该被当成控制流。
:::

响应已经写出后再 panic：只记录，不重写（「已写响应不被覆盖」）。

## 自定义映射器

```go
app := web.New(web.WithErrorHandler(func(c *web.Ctx, err error) error {
    var maxErr *http.MaxBytesError
    if errors.As(err, &maxErr) {
        c.SetHeader("X-Limit", strconv.FormatInt(maxErr.Limit, 10))
    }
    var he *web.HTTPError
    if errors.As(err, &he) && he.Status == 400 {
        c.Observe("bad_request", func(a *observability.Attrs) {
            observability.Set(a, "path", c.Path("id"))
        })
    }
    // 返回 nil 表示已处理；返回 err 交给默认映射器继续处理
    return nil
}))
```

映射器拿到的 `*Ctx` **仍能** `Get` / `Set` / `Observe` / `TraceID()` / `Path` / `Query()`——KV 挂在 `Ctx` 上、活得比请求作用域长；但 `c.Kernel()` 对应的 scope 已经回收，**那时再 `c.Service` 是死 scope**（要查全局服务用 `c.Service` 之外的路径，或提前在 KV 里放好）。

`ErrorHandler` 是可组合的中间件式钩子：返回 `nil` = 「我已处理」，返回 error = 「继续走默认映射」。默认映射器的完整规则（含 415 / 413 / panic 的优先级）见[设计文档](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md)的「错误映射器契约」一节。

## 与访问日志的关系

同一个「决定」既进响应，也进访问日志——状态码与 `Code` 都记在[访问日志](/guide/observability)的那一行里。所以排障时不用猜：拿到 trace id，看那一行就够。
