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
//
//	c, done := app.NewTestContext(rec, req)
//	defer done()
//
//	if err := myHandler(c); err != nil {  // 想测哪一段就调哪一段
//		...
//	}
//	// 到这里 rec 与 Sink 里就是「走完整链路会长成什么样」的结果
//
// **同构是结构上的，不是近似**：装配与收尾都走真实请求那条路（`Engine.begin` +
// `finish`），所以状态码落定、错误映射、访问日志、`X-Trace-Id` 与真路径是同一份代码
// 产出的。
//
// 三条边界：
//
//   - **`done()` 必须调用**（defer 或显式）：它 dispose 请求作用域并把访问记录写进
//     Sink。重复调用安全（只生效一次）。
//   - **panic 不会被转成 `PanicError`**：真路径的 recover 在 `withCtx` 的 defer 里，
//     而这里 handler 由调用方在自己的栈上执行。要覆盖 panic 那条路径用 `ServeTest`。
//   - engine 已销毁（进程关闭中）时返回 `nil, func(){}`，此时 `w` 上已经写了 503。
func (e *Engine) NewTestContext(w http.ResponseWriter, r *http.Request) (*Ctx, func()) {
	c := e.begin(w, r)
	if c == nil {
		return nil, func() {}
	}
	var once sync.Once
	return c, func() { once.Do(func() { e.end(c) }) }
}

// ServeTest 用一组中间件（可选）跑一个 handler，走与真实请求**完全相同**的装配与收尾。
//
// 与 NewTestContext 的分工：那个给「只跑某一段」的自由；这个覆盖「handler + 一组
// 中间件」这条最常见路径，并且**连 panic 那条路径一起覆盖**（handler 在框架的 defer
// 里执行，panic 会照真路径那样变成 `PanicError` 走错误映射）。
//
//	rec := httptest.NewRecorder()
//	req := httptest.NewRequest("GET", "/users/42", nil)
//	req.SetPathValue("id", "42")
//
//	app.ServeTest(rec, req, myHandler, requireAuth, withTx)
//	// rec.Code / rec.Body / Sink 里就是完整链路的结果
func (e *Engine) ServeTest(w http.ResponseWriter, r *http.Request, h Handler, mw ...Middleware) {
	e.withCtx(w, r, func(c *Ctx) {
		if err := compose(mw, h)(c); err != nil {
			c.setErr(err)
		}
	})
}
