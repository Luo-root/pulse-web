// Package web 是基于 pulse kernel 与 observability 构建的通用 Go web 服务框架。
//
// 定位 = 装配能力 + 一等观测：
//
//   - 装配内核：kernel 的 IoC、可逆生命周期、请求作用域、事件总线
//   - 一等观测：每请求 32hex TraceID、结构化记录（observability.Record）、装配诊断
//   - 零第三方依赖：只用 stdlib 与 pulse 的 kernel / observability
//
// # 快速开始
//
//	app := web.New()                          // 自建 kernel root，并最先装配 Bootstrap
//	app.GET("/users/{id}", func(c *web.Ctx) error {
//		db := c.MustService(dbKey)            // 全局服务（kernel root 仓库）
//		u, err := db.Find(c.Path("id"))       // 路径参数与 r.PathValue 同源
//		if err != nil {
//			return web.NotFound("user", err)  // 显式 error → 状态码映射 + 观测记录
//		}
//		return c.JSON(http.StatusOK, u)
//	})
//	app.Run(":8080")                          // 阻塞：信号 → drain → OnShutdown → root.Dispose → Sink flush
//
// # 运行时契约
//
//   - **请求路径零 Provide**：框架不在请求热路径注册服务——每请求 Provide 会触发
//     kernel 的服务变更全树广播，成本随插件树规模线性增长（实测 ≈ +47ns/插件/请求）。
//   - **Dispose 先于写响应**：请求 scope 在 handler 返回后立即回收，慢客户端不会
//     钉住请求级资源（DB 事务、锁）。
//   - **Ctx 不可跨 goroutine**：需要异步时用 Ctx.Detach() 取独立的值与 kernel 句柄。
//   - **中间件无例外**：Static 注册的静态资源同样经过全局与分组中间件。
//   - **错误脱敏**：HTTPError.cause 与 panic 栈只进观测记录，绝不进响应体。
//
// 设计文档：docs/design/web-framework-design.md
package web
