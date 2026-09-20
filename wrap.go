package web

import "net/http"

// Wrap 把 stdlib http.Handler 适配为本框架的 Handler。
//
// 语义：
//   - 中间件链照常经过它（它是链上的普通 handler）
//   - 它自行写出响应，框架收尾只记录、不重写
//   - panic 由 Engine 的 defer 兜底接（响应已写出则只记录）
//   - 路径参数用标准 API 读：r.PathValue(name) 与 c.Path(name) 同源
//
// 反向也成立：Engine 实现 http.Handler，可经 Handler() 挂到原生 ServeMux。
func Wrap(h http.Handler) Handler {
	return func(c *Ctx) error {
		h.ServeHTTP(c.w, c.r)
		return nil
	}
}

// Adapt 把 stdlib 形状的中间件（func(http.Handler) http.Handler）接成
// Middleware，让它在**洋葱内**忠实工作。
//
// # 为什么需要它
//
// 生态里的中间件几乎都是这个形状：chi/middleware、rs/cors、promhttp、
// httprate、gorilla/csrf……。把它们接进本框架有两条路，各有各的坑：
//
//   - **手写适配器**（只用公开 API 拼十几行）：看起来能跑，实际静默失真。
//     包 ResponseWriter 抓状态码的（promhttp、logger）恒抓到 0，指标全标成
//     code="0"；改写 body 的（gzip）写出「Content-Encoding: gzip + 明文 body」
//     这种损坏响应，客户端直接报 gzip: invalid header；短路写响应的在访问
//     日志里记成「没写响应」；换过 request 的，handler 拿不到中间件注入的值。
//   - **外包 Handler()**：这条是对的（Engine 本身就是 http.Handler），但中间件
//     跑在框架的请求作用域**外面**——读不到路由模板（Request.Pattern），短路
//     的请求框架完全不知情（没有访问日志、没有 span）。
//
// Adapt 是第三条路：在洋葱内，同时保住下面三条承诺。
//
// # 承诺什么
//
//  1. 中间件包 ResponseWriter 时，抓到的是**真实**的状态码与字节数（不是 0）。
//  2. 中间件短路（不调 next）时，它写出的状态码与字节数**记回框架的采集层**，
//     访问日志与观测看到的是真实响应。
//  3. 中间件换掉的 request（r = r.WithContext(...)）**传得下去**——handler 用
//     c.Request() 读到的就是换过的那一个。
//
// http.Flusher 照常透出，SSE 的逐条 flush 不受影响。
//
// 中间件与 handler 之间传值走 request context——这是 stdlib 自己的通道，框架
// 不额外开取值入口：
//
//	type userKey struct{}
//
//	app.Use(web.Adapt(func(next http.Handler) http.Handler {
//		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
//			ctx := context.WithValue(r.Context(), userKey{}, "u1")
//			next.ServeHTTP(w, r.WithContext(ctx))
//		})
//	}))
//
//	app.GET("/me", func(c *web.Ctx) error {
//		u, _ := c.Request().Context().Value(userKey{}).(string) // "u1"
//		return c.Text(http.StatusOK, u)
//	})
//
// # 明确不做什么
//
//   - **不搬动路由**：路由匹配仍在中间件之前，预检 OPTIONS 到不了中间件——只
//     注册了 GET /api 时 ServeMux 直接 405（Allow: GET, HEAD）。要拦预检的
//     CORS 类中间件请外包 Handler()，或为每条路由显式注册 OPTIONS。
//   - **不解决「收尾型中间件 × error/panic」**：框架的错误映射发生在中间件
//     返回**之后**，在 next 返回后无条件写响应的中间件（例如自己 defer
//     zw.Close() 的 gzip）会把状态锁成 200。压缩类默认推外包。
//   - **不透出 http.Hijacker**：框架对响应写出器的能力承诺只到 http.Flusher，
//     断言 Hijacker 的中间件（WebSocket 升级）不可用。
//   - **不保证「同形状就能接」**：依赖特定 router 上下文的中间件照旧不可用——
//     chi 的 CleanPath 读 chi.RouteContext，经 Adapt 与外包都会 panic。
//   - **不改中间件的语义**：panic 谁接、错误响应体长什么样，仍由中间件自己
//     决定（chi Recoverer 写出的 500 与框架兜底就不是同一个响应体）。
//
// 中间件该挂洋葱内还是外包 `Handler()`、以及各生态件的实测结论，见站点指南
// 「stdlib 中间件接入」（`site/guide/middleware.md`）。
func Adapt(m func(http.Handler) http.Handler) Middleware {
	if m == nil {
		panic("web: Adapt requires a non-nil middleware")
	}
	return func(c *Ctx, next Handler) error {
		rawW, rawR := c.w.ResponseWriter, c.r
		proxy := &adaptProxy{ResponseWriter: rawW}

		// 只在底层真的能 Flush 时才把 Flusher 交给中间件：代理一旦无条件带上
		// Flush 方法，就会替底层虚报能力，Ctx.Flush() 的「不支持」判定被吃掉。
		var handed http.ResponseWriter = proxy
		if _, ok := rawW.(http.Flusher); ok {
			handed = &adaptFlusher{proxy}
		}

		called := false
		var err error
		inner := m(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			c.w.ResponseWriter = w
			c.r = r
			// 还原必须走 defer：下游 panic 时顺序语句会被跳过，框架收尾
			// 就会写进中间件已经关掉的 writer（实测 500 整个丢掉、客户端
			// 拿到 200 空响应）。
			defer func() {
				c.w.ResponseWriter = rawW
				c.r = rawR
			}()
			err = next(c)
		}))
		inner.ServeHTTP(handed, rawR)

		if called {
			return err
		}
		// 短路：中间件写的是 proxy，没经过框架的采集层，把观测补回去。
		// 只累加不覆盖——外层中间件可能已经写过（那时状态码已落定）。
		if proxy.wrote {
			if !c.w.wrote {
				c.w.status = proxy.status
			}
			c.w.bytes += proxy.bytes
			c.w.wrote = true
		}
		return nil
	}
}

// adaptProxy 站在中间件外面，只记录、不改写：它兜住「中间件自己写响应」这条
// 不经过框架采集层的路径。
//
// 刻意**不**实现 http.Flusher——理由见 Adapt 里 handed 的注释。
type adaptProxy struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (p *adaptProxy) WriteHeader(code int) {
	if p.wrote {
		return
	}
	p.wrote, p.status = true, code
	p.ResponseWriter.WriteHeader(code)
}

func (p *adaptProxy) Write(b []byte) (int, error) {
	if !p.wrote {
		p.WriteHeader(http.StatusOK)
	}
	n, err := p.ResponseWriter.Write(b)
	p.bytes += n
	return n, err
}

// adaptFlusher 给 adaptProxy 补上 http.Flusher。只在底层确实实现 Flusher 时
// 才构造它，所以这里的断言不会失败，也不会替底层虚报能力。
type adaptFlusher struct{ *adaptProxy }

func (f *adaptFlusher) Flush() { f.ResponseWriter.(http.Flusher).Flush() }
