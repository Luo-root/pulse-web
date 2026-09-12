# pulse-web

基于 [pulse](https://github.com/Luo-root/pulse) 的 **kernel** 与 **observability** 构建的通用 Go web 服务框架。

- **装配内核**——kernel 的 IoC、可逆生命周期、请求作用域、事件总线
- **一等观测**——每请求 TraceID、结构化记录、装配诊断，默认装配
- **零第三方依赖**——只使用 stdlib 与 pulse 的两个基座包（kernel / observability）

> 状态：**实现中**。设计与评审在 [Issue #1](https://github.com/Luo-root/pulse-web/issues/1) 进行，完整设计见 [`docs/design/web-framework-design.md`](docs/design/web-framework-design.md)。
> 已落地（垂直切片）：Engine / 路由与分组 / Ctx / 错误模型 / 观测接线 / 优雅关闭；见 [Issue #2](https://github.com/Luo-root/pulse-web/issues/2)。

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

## 文档

- [框架设计（v1）](docs/design/web-framework-design.md)——定位、决策、API 面、运行时契约、观测设计、明确不做清单
