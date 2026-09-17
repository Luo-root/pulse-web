# 请求：读取、绑定与请求级状态

这一页讲「handler 怎么把请求里的东西拿出来」，以及哪些状态挂在请求上、活多久。

## 取值：路径、查询、头

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
    id := c.Path("id")                 // 路径参数（同 c.Request().PathValue）
    page := c.Query("page")            // 查询参数；不存在返回空串
    ua := c.Request().Header.Get("User-Agent")   // 需要原始请求就用 c.Request()
    c.SetHeader("X-Trace", c.TraceID())          // 响应头（写头之前有效）
    return c.Text(200, id+"@"+page)
})
```

`c.Query(name)` 只取第一个值；要多值 / 复杂结构就走下面的绑定。**同一个请求里要取多个参数，取一次存成变量**——每次调用都会重新解析一遍 query string，框架不替调用方做缓存（最简的做法是调用方自己取一次）。

## 绑定请求体：`Bind`

```go
type CreateUser struct {
    Name  string `json:"name"  form:"name"  query:"name"`
    Email string `json:"email" form:"email" query:"email"`
}

app.POST("/users", func(c *web.Ctx) error {
    var in CreateUser
    if err := c.Bind(&in); err != nil {
        return err          // 交给错误映射器（默认 400 / 413 / 415）
    }
    return c.JSON(201, in)
})
```

`Bind` 按 **`Content-Type` 分派**：

| Content-Type | 行为 |
|---|---|
| `application/json`（含 `+json` 后缀） | 解码 JSON |
| `application/xml` / `text/xml` | 解码 XML |
| `application/x-www-form-urlencoded` | 解析表单 |
| `multipart/form-data` | 解析 multipart（`c.Request().FormFile` 取文件） |
| 其它非空类型 | **415**（不支持的类型），不猜 |
| **没有 body** | 落到 **query**（GET 语义，标签用 `query`） |

取纯查询参数（过滤、分页这类）用 `BindQuery`，不看 `Content-Type`：

```go
app.GET("/users", func(c *web.Ctx) error {
    var f struct {
        Q    string `query:"q"`
        Page int    `query:"page"`
    }
    if err := c.BindQuery(&f); err != nil {
        return err          // 默认 400
    }
    return c.JSON(200, f)
})
```

**标签名**：JSON / XML 走各自标签，表单走 `form`，查询走 `query`。同一字段可以都写（如上例），从而对「有 body 走 body、无 body 走 query」两种情况都成立。

### 上限与错误

请求体两道闸：全局 `web.WithMaxBodyBytes(n)` 与路由级 `web.BodyLimit(n)`（**只能收紧**，见[路由与中间件](/guide/routing)）。超限一律 **413 + `body_too_large`**，`cause` 是 `*http.MaxBytesError`；类型不支持是 **415**；解析失败 / 校验失败是 **400**。

要区分具体原因，在自定义 `ErrorHandler` 里用 `errors.As` 看 `*web.HTTPError`（`Code` 字段）或 `*http.MaxBytesError`——不要靠文案匹配。

## 请求级 KV：`Set` / `Get`

中间件往请求上放东西、后面的 handler 取出来，用类型安全的键（**不是字符串**）：

```go
// 键在包级声明一次，类型写进 Key[T]，用错类型编译期就报错
var userKey = web.NewKey[*User]("user")

func requireAuth(next web.Handler) web.Handler {
    return func(c *web.Ctx) error {
        u, err := loadUser(c)          // 你自己的逻辑
        if err != nil {
            return err                 // 未登录：直接 401
        }
        c.Set(userKey, u)              // 放进请求级 KV
        return next(c)
    }
}

app.Use(requireAuth)
app.GET("/me", func(c *web.Ctx) error {
    u, ok := c.Get(userKey)            // (*User, bool)
    if !ok {
        return web.Internal("no_user", nil)
    }
    return c.JSON(200, u)
})
```

两个要点：

- **键是 `Key[T]` 类型，不是字符串**——写错类型编译不过，不会在运行期静默拿到 nil。
- **KV 挂在 `Ctx` 上，活得比请求作用域长**：handler 返回后，错误映射器仍能 `c.Get` / `c.Set` / `c.Observe` / `c.TraceID()` / `c.Path` / `c.Query()`。但 `c.Kernel()` 对应的 scope 已经回收，**那时候再 `c.Service` 是死 scope**。

## 全局服务：`Service` / `MustService`

装配期 `kernel.Provide(root, key, v)` 放进去的东西，在请求里这样取：

```go
var dbKey = kernel.NewServiceKey[*sql.DB]("db")

app := web.New()
kernel.Provide(app.Root(), dbKey, sqlDB)     // 进程级：挂在 root 上

app.GET("/users", func(c *web.Ctx) error {
    db, ok := c.Service(dbKey)               // (T, bool)
    if !ok {
        return web.Internal("db_unavailable", nil)
    }
    db2 := c.MustService(dbKey)              // 没有就 panic —— 只用在「装配期就该有」的依赖
    _ = db
    return c.Text(200, db2.Stats().OpenConnections)
})
```

- `c.Service` ≡ `kernel.Get(c.Kernel(), key)`：**先走请求 scope 的局部链，再回全局仓库**。
- `c.Kernel()` 是**请求作用域**：挂 Effect、事件监听、传给需要 scope 的组件都行；**它不是进程级 kernel**，挂上去的插件与 Effect 会在请求结束时被级联销毁。进程级用 `app.Root()`（见[装配与运行](/guide/assembly)）。

## 观测：`TraceID` 与 `Observe`

```go
app.GET("/users/{id}", func(c *web.Ctx) error {
    c.Observe("user.fetch", func(a *observability.Attrs) {
        observability.Set(a, "user.id", c.Path("id"))
        observability.Set(a, "cache", "miss")
    })
    return c.JSON(200, payload)
})
```

- `c.TraceID()` 返回本请求的 32 位十六进制串；它同时出现在访问日志与下游调用上（透传见[装配与运行](/guide/assembly)的 `WithTrustedTraceHeader`）。
- `c.Observe(event, set)` 往访问日志里补字段。**类型受限**：`Set` 只接受 `~string | ~int64 | ~float64 | ~bool`——`int` 或 UUID 类型要显式转换（`int64(n)` / `u.String()`）。

## 跨 goroutine：`Detach`

`*Ctx` **不可跨 goroutine**（它与请求及其响应写出器绑定）。后台任务用 `Detach()`——它给的是一个**值袋子**，不是上下文：

```go
app.POST("/jobs", func(c *web.Ctx) error {
    d := c.Detach()                 // TraceID / HostID / Sink / Root（进程级 root）
    id := c.Path("id")              // ⚠️ 要用的请求级数据在这里显式取出
    go func() {
        job := runJob(id)
        d.Observe("job.done", func(a *observability.Attrs) {   // 直写 Sink，与请求共享 TraceID
            observability.Set(a, "ok", job.OK)
        })
        db := d.MustService(dbKey)  // 读**全局**服务
        _ = db
        // 需要作用域就自己 d.Root.Derive()，用完自理 Dispose
    }()
    return c.Text(202, "accepted")
})
```

三条契约，都在类型上：

- **不带请求级 KV**（有意）：`c.Set` 进去的东西不会跟着走——要传给后台就在 `Detach` 之前显式取出，这样「哪些数据进了后台」在代码里一眼可见，也不会把 KV 的生命周期契约悄悄拉长。
- **`Service` / `MustService` 读的是全局仓库**（`d.Root`）——`Detached` 不拥有任何 scope，请求作用域在 handler 返回时就回收了。
- 打点走 `d.Observe`（直写 Sink，零广播）。响应写出、`Kernel()` 都不在 `Detached` 上——「后台任务不能碰响应」由此变成类型层面的事实。
