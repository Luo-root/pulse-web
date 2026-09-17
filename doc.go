// Package web 是基于 pulse kernel 与 observability 构建的通用 Go web 服务框架。
//
// 定位 = 装配能力 + 一等观测：
//
//   - 装配内核：kernel 的 IoC、可逆生命周期、请求作用域、事件总线
//   - 一等观测：每请求 32hex TraceID、结构化记录（observability.Record）、装配诊断；
//     默认出口 `ConsoleSink` 把记录渲染成**给人读**的列式单行（零分配）
//   - 链路出口（可选）：`WithSpanHook` 把请求交给宿主的追踪体系（W3C Trace Context / OTel
//     HTTP semconv），span-id 由追踪体系分配、框架不编造；官方适配在 nested module
//     github.com/Luo-root/pulse-web/otel
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
//	app.POST("/users", func(c *web.Ctx) error {
//		var in CreateUser
//		if err := c.Bind(&in); err != nil {   // 按 Content-Type 分派：JSON / XML / form / multipart
//			return err                        // 400 / 413 / 415 已按语义映射
//		}
//		return c.JSON(http.StatusCreated, in)
//	})
//	app.Run(":8080")                          // 阻塞：信号 → drain → OnShutdown → root.Dispose → Sink flush
//
// # 运行时契约
//
//   - **请求路径零全局 Provide**：框架不在请求热路径写全局服务仓库——全局命名
//     空间是**装配面**，请求级值写进去会被并发请求互相覆盖。请求级数据走
//     `kernel.Local()` 作用域局部绑定（随作用域销毁撤除），且只在
//     `WithCollector()` 这类显式开关下才出现在请求路径上。
//   - **Dispose 先于写响应**：请求 scope 在 handler 返回后立即回收，慢客户端不会
//     钉住请求级资源（DB 事务、锁）。
//   - **Ctx 不可跨 goroutine**：它与其绑定的 ResponseWriter 都不得跨 goroutine
//     使用；异步场景用 `Ctx.Detach()` 取独立的值与进程级 kernel 句柄。
//   - **中间件无例外**：Static 注册的静态资源同样经过全局与分组中间件。
//   - **错误脱敏**：HTTPError.cause 与 panic 栈只进观测记录，绝不进响应体。
//   - **测试入口与真路径同源**：`NewTestContext` / `ServeTest` 与真实请求共用同一份
//     装配与收尾（`Engine.begin` + `finish`），差别只有 `ServeMux` 不参与——路径参数
//     与路由模板要调用方补在 request 上。
//
// 设计文档：docs/design/web-framework-design.md
package web
