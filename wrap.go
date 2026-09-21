package web

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
)

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
//  1. 中间件包 ResponseWriter 时，抓到的是**handler 真正写出的**状态码与字节数
//     （不是 0）——promhttp 计数器、chi Logger 这类直接成立。边界：框架自己映射出
//     来的 error / panic 响应它**看不到**（映射发生在中间件返回之后），那两条路上
//     它读到的是 0，见「明确不做什么」第 2 条。
//  2. 中间件短路（不调 next）、或者**调了 next 但在它外面写响应**（`Recoverer` 接住
//     panic 写 500 就是这样）时，它写出的状态码与字节数**记回框架的采集层**，访问
//     日志与观测看到的就是客户端收到的那份响应。这条路上记录里**没有** `error.type`
//     与错误对象——响应不是框架接住的，它不知道原因，编一个出来就是假信息。
//     唯一的例外见「明确不做什么」第 2 条末尾（有待映射的 error 时不收）。
//  3. 中间件换掉的 request（r = r.WithContext(...)）**传得下去**——handler 用
//     c.Request() 读到的就是换过的那一个。
//
// http.Flusher 照常透出，SSE 的逐条 flush 不受影响；http.Hijacker 同样透出
// （底层支持时）——挂在 Adapt 之下的路由照样能升级协议，见 responseWriter.Hijack。
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
//     注册了 GET /api 时 ServeMux 直接 405（Allow: GET, HEAD）。CORS 用框架
//     自带的 Engine.CORS（它按已注册路径补 OPTIONS，把预检送进洋葱）；其余
//     要拦预检的中间件请外包 Handler()，或为每条路由显式注册 OPTIONS。
//
//     没匹配到路由的请求（真 404、尾斜杠）与**需要归一化的路径**（`//users`、
//     `/users//42`）同理：后者由 ServeMux 在**调用 handler 之前**自己 307 到
//     干净路径，洋葱内的中间件看不到它。注意这时访问记录里的 `http.route`
//     **是非空的**（ServeMux 在重定向前就把 pattern 写上了），别把它当成
//     「这条请求被那个 handler 处理过」的证据。
//
//   - **不解决「收尾型中间件 × error/panic」**：框架的错误映射发生在中间件
//     返回**之后**，在 next 返回后无条件写响应的中间件（例如自己 defer
//     zw.Close() 的 gzip）会把状态锁成 200。压缩类默认推外包。
//
//     同一条时序在观测面上还有两个后果（都是实测，矩阵里有对应行）：① **中间件
//     自己的收尾读到的是 0**——chi Logger 在返回 error / panic 的路由上记
//     `0 / 0B`，promhttp 计数器把 404 记成 `code="200"`（`sanitizeCode(0)`），
//     panic 那条干脆一条不记。要按**真实响应**记日志打点就用框架自己的访问日志
//     与 Trace；② 反过来，中间件在 next 外面写响应时（`Recoverer` 接住 panic 写
//     500 就是这样）记录**向代理对齐**：状态码与体积取中间件写出的那份（#96 之
//     前记的是框架自己的缺省 200，等于把一条 5xx 从面板上抹掉），但**不收**它的
//     响应——框架有待映射的 error 时（收尾型中间件 × error 那条路）收下状态码会
//     跳过错误映射、错误体整个丢掉，所以那种情况照旧交给映射器。
//
//   - **不透出 http.Pusher / FlushError**：响应写出器的能力面是一份显式的窄清单
//     （Flush / Hijack / SetWriteDeadline / EnableFullDuplex），HTTP/2 的 Server
//     Push 与 flush 错误上报不在其中。
//
//   - **不保证「同形状就能接」**：依赖特定 router 上下文的中间件照旧不可用——
//     chi 的 CleanPath 读 chi.RouteContext，经 Adapt 与外包都会 panic。
//
//   - **不改中间件的语义**：panic 谁接、错误响应体长什么样，仍由中间件自己
//     决定（chi Recoverer 写出的 500 与框架兜底就不是同一个响应体）。
//
// 中间件该挂洋葱内还是外包 `Handler()`、以及各生态件的实测结论，见站点指南
// 「stdlib 中间件接入」（`site/guide/middleware.md`）；**对照矩阵本体**在
// `interop/middleware_test.go`（嵌套 module，进 CI）——同一条路由两个挂载点逐项
// 比对三个面：响应、中间件自己的旁观测、框架侧记下的那条访问记录。
func Adapt(m func(http.Handler) http.Handler) Middleware {
	if m == nil {
		panic("web: Adapt requires a non-nil middleware")
	}
	return func(c *Ctx, next Handler) error {
		rawW, rawR := c.w.ResponseWriter, c.r
		proxy := &adaptProxy{ResponseWriter: rawW, target: c.w}

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
			// 「调了 next，但在 next **外面**写响应」——`Recoverer` 接住 panic 写 500 就是
			// 这一格（兜底 404、自研 recovery 同形）。框架自己的 writer 一个字节都没写，
			// 客户端却已经收到中间件写的那份响应：唯一看见它的是代理，记录向它对齐（#96）。
			//
			// 判据用局部 `err` 而不是 `c.err`：`c.err` 由路由包装器在整条链返回**之后**才写
			// （engine.go 的 register），在这一层恒为 nil，拿它当闸门等于没判——实测过。
			//
			// **有待映射的 error 就不收**：返回 error 那条路上，收尾型中间件也会在 next
			// 外面写（gzip 落的那段尾），收下它的状态码会让框架**跳过错误映射**、错误体
			// 整个丢掉——那是把响应弄得更糟（见 TestAdaptUnconditionalFinalizeLocksStatus）。
			if err == nil && proxy.wrote && !c.w.wrote && !c.w.hijacked {
				c.w.status, c.w.bytes, c.w.wrote = proxy.status, proxy.bytes, true
			}
			return err
		}
		// 短路：中间件写的是 proxy，没经过框架的采集层，把观测补回去。
		// 只累加不覆盖——外层中间件可能已经写过（那时状态码已落定）。
		//
		// 连接已交出的那条路上**不补状态码**：那条请求的状态码是 hijack 的约定值
		// （101），不是中间件在交出之后写的那个（实测：补了就会记成 200）。
		if proxy.wrote {
			if !c.w.wrote && !c.w.hijacked {
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
// 刻意**不**实现 `http.Flusher`——理由见 Adapt 里 handed 的注释。`Hijack` 的取舍
// 相反，**无条件实现**：它有返回值，底层不支持时能如实报错，不存在「替底层虚报
// 能力」的问题；而少了它，挂在 `Use(Adapt(...))` 之下的升级路由会集体失效——
// handler 手上的 writer 链要穿过这一层。
//
// 「连接已交出」这件事**只在 target（框架侧那条 responseWriter）上存一份**，代理
// 不另存副本，两条守卫读的都是它。洋葱内的 handler 是从 `c.Writer()` 拿的连接，
// 那一刻走的是 responseWriter.Hijack，代理这边什么都不知道——代理若只信自己的标记，
// 中间件在 next 返回后（例如 defer 里的兜底写）就会照常往已经交出去的连接上写：
// net/http 会打两行告警，而行号属于本文件、使用者要查的是自己的中间件。契约一份，
// 两个入口都读它。
type adaptProxy struct {
	http.ResponseWriter
	target *responseWriter // 框架侧那条 Ctx writer：契约（含 hijack 标记）的唯一存放处
	status int
	bytes  int
	wrote  bool
}

func (p *adaptProxy) WriteHeader(code int) {
	if p.wrote || p.target.hijacked {
		return
	}
	p.wrote, p.status = true, code
	p.ResponseWriter.WriteHeader(code)
}

func (p *adaptProxy) Write(b []byte) (int, error) {
	if p.target.hijacked {
		// 与 responseWriter.Write 同型。**这层守卫不能省**：底层是 net/http 时它确实
		// 也会返回 ErrHijacked，但那是「借来的保护」——而且代价是 net/http 会打两行
		// 归属到本文件行号的告警（`response.WriteHeader on hijacked connection`），
		// 并且代理会把自己记成「写过 200」，短路回填就把这条 hijack 的请求记成
		// status=200（实测）。自己挡住，这三件一起消失。
		return 0, http.ErrHijacked
	}
	if !p.wrote {
		p.WriteHeader(http.StatusOK)
	}
	n, err := p.ResponseWriter.Write(b)
	p.bytes += n
	return n, err
}

// Hijack 把连接交给调用方，并让框架知道自己已经管不着这条响应了。
//
// 与 responseWriter.Hijack 是同一个契约的两个入口：中间件直接在洋葱里升级、
// 或洋葱内的 handler 升级，两条路都记到同一处（`target`）；交出之后**两边**都拒写
// （读的是同一份标记）。
func (p *adaptProxy) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := p.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("web: ResponseWriter does not support Hijack: %w", http.ErrNotSupported)
	}
	conn, buf, err := h.Hijack()
	if err != nil {
		return nil, nil, err
	}
	p.target.hijacked = true
	return conn, buf, nil
}

// adaptFlusher 给 adaptProxy 补上 http.Flusher。只在底层确实实现 Flusher 时
// 才构造它，所以这里的断言不会失败，也不会替底层虚报能力。
type adaptFlusher struct{ *adaptProxy }

func (f *adaptFlusher) Flush() { f.ResponseWriter.(http.Flusher).Flush() }
