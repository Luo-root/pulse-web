package web

import (
	"net/http"
	"sync"
)

// NewTestContext 构造一个与真实请求**同构**的 *Ctx，供 handler 单测使用。
//
// **给谁用**：写 handler 单测的人——不必起真 server，也不必自己拼 Ctx（`Ctx` 的字段
// 全部未导出，包外本来也拼不出来）。
//
//	rec := httptest.NewRecorder()
//	req := httptest.NewRequest("GET", "/users/42", nil)
//	req.SetPathValue("id", "42")          // 不走 ServeMux 就得自己补路径参数
//	req.Pattern = "GET /users/{id}"       // 路由模板同理
//
//	c, done := app.NewTestContext(rec, req)
//	done(myHandler(c))                    // 跑 + 回灌 error + 收尾
//
//	if rec.Code != http.StatusNotFound {  // 错误映射照常生效
//		t.Fatalf("status = %d", rec.Code)
//	}
//
// **同构的范围**：装配与收尾走真实请求那条路（`Engine.begin` + `finish`），所以状态码
// 落定、错误映射、访问日志、`X-Trace-Id` 都是同一份代码产出的。`done` 收的那个 error
// 就是 handler 的返回值——非 nil 时走统一错误映射，与真路径上 `return err` 一致。
//
// **不在范围内的三件**（真路径有、这里没有）：
//
//   - **`ServeMux` 与 `Engine.Use`**：路由匹配、分组前缀、全局中间件都不参与——路径参数
//     与路由模板要自己补在 request 上（见上例）。要连中间件一起跑，用 `ServeTest`。
//   - **`WithMaxBodyBytes`**：请求体闸门在 `withCtx` 里，这条路上没有——测超限请用
//     `ServeTest`。
//   - **panic**：handler 在调用方自己的栈上执行，框架的 recover 不在那条栈上——panic 直接
//     传给你，由 testing 包正常报错。同样，要覆盖 panic 用 `ServeTest`。
//
// `done` 幂等：**第一次调用生效**，之后忽略。handler 会 panic 的场合，写
// `defer done(nil)` 再显式 `done(err)`——正常路径下显式那次先到，panic 时才轮到 defer 兜底。
func (e *Engine) NewTestContext(w http.ResponseWriter, r *http.Request) (*Ctx, func(error)) {
	c := e.begin(w, r)
	if c == nil {
		return nil, func(error) {}
	}
	var once sync.Once
	return c, func(err error) {
		once.Do(func() {
			if err != nil {
				c.setErr(err)
			}
			e.end(c)
		})
	}
}

// ServeTest 用一组中间件（可选）跑一个 handler，走与真实请求**完全相同**的装配与收尾。
//
// 与 NewTestContext 的分工：那个给「只跑某一段」的自由（代价是不走 mux / 体闸 / panic 接管）；
// 这个覆盖「handler + 中间件链」这条最常见路径——**传入的 mw 是追加**，`Use` 与分组中间件
// 照旧在链上（与真路径同一个 `chainMW`），并且连请求体闸门与 panic 一起覆盖。
//
//	rec := httptest.NewRecorder()
//	req := httptest.NewRequest("GET", "/users/42", nil)
//	req.SetPathValue("id", "42")
//
//	app.ServeTest(rec, req, myHandler, requireAuth, withTx)
//	// rec.Code / rec.Body / Sink 里就是完整链路的结果
//
// 与真路径唯一的不同是 **`ServeMux`**：它不做路由匹配，所以路径参数与路由模板同样要自己
// 补在 request 上；路由级的 `BodyLimit` 与 `Use` 之外的按路由注册的中间件也不在链上。
func (e *Engine) ServeTest(w http.ResponseWriter, r *http.Request, h Handler, mw ...Middleware) {
	e.withCtx(w, r, func(c *Ctx) {
		if err := compose(e.chainMW(mw), h)(c); err != nil {
			c.setErr(err)
		}
	})
}
