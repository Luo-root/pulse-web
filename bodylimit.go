package web

import "net/http"

// BodyLimit 返回一个限制请求体上限的中间件：可挂分组，也可挂单条路由。
//
//	app.Group("/upload", web.BodyLimit(100<<20)).POST("/avatar", h) // 只有这块放宽
//	app.POST("/api/export", h, web.BodyLimit(1<<20))                // 单条路由收紧
//
// n 是允许的**最大字节数**：body 恰好 n 字节通过，n+1 字节起 413——与
// WithMaxBodyBytes 同口径。n <= 0 表示不限，此时中间件直通（也不包 body）。
//
// # 闸门只能收紧，不能放宽
//
// 与引擎级 WithMaxBodyBytes 叠加时**取最严**：引擎级先套一层 MaxBytesReader，
// 本中间件再套一层，实际生效的是两者中更小的那个。`BodyLimit(1<<20)` 挂在
// `WithMaxBodyBytes(1<<10)` 的路由里仍会被 1 KiB 掐住——去掉内层包装意味着换掉
// 整个 body reader，代价与风险都不成比例，所以「就近覆盖」不做。
//
// # 与自己在中间件里包 MaxBytesReader 的区别
//
// 自己写成 `c.Request().Body = http.MaxBytesReader(c.Writer(), ...)` 拦得住超限，
// 但会静默丢一个语义：MaxBytesReader 的 w 参数**只在超限时用**，stdlib 会调
// `w.(requestTooLarger).requestTooLarge()` 给响应加 `Connection: close` 并在回复后
// 关连接（防继续灌数据、防 keep-alive 把残留 body 当成下一个请求）。这个未导出
// 接口只有 server 内部的 *response 实现，而 c.Writer() 是框架的包装器，传它就拿不到
// ——**原始 writer 只有包内实现拿得到**，所以这件事该由框架做（引擎级那道闸同理）。
//
// # 错误口径
//
// 超限统一 413 + "body_too_large"，cause 是 *http.MaxBytesError——与
// WithMaxBodyBytes 的预检 / 读取两条路径同型。自定义 ErrorHandler 用
// errors.As(err, &maxErr) 只认这一个类型，即可覆盖全部超限路径（含本中间件）。
func BodyLimit(n int64) Middleware {
	return func(c *Ctx, next Handler) error {
		if n <= 0 {
			return next(c)
		}
		// 预检 + 读取闸门，与引擎级 WithMaxBodyBytes 共用同一条实现（limitBody）。
		// w 传**原始 writer**（c.w.ResponseWriter）而不是 c.Writer()——理由见 godoc。
		if err := limitBody(c.w.ResponseWriter, c.r, n); err != nil {
			return err
		}
		return next(c)
	}
}

// limitBody 是请求体上限的两道闸，引擎级 WithMaxBodyBytes 与路由级 BodyLimit 共用：
// 声明即超限 → 返回 413（**不读 body**）；否则给 body 包一层 MaxBytesReader，
// 把「声明未知（chunked）/ 声明偏小」的超限留给读取路径兜底。
//
// w 必须是**原始 writer**：MaxBytesReader 的 w 参数只在超限时用，stdlib 会调
// `w.(requestTooLarger).requestTooLarge()` 给响应加 `Connection: close` 并在回复后
// 关连接。框架的 responseWriter 包装器不实现这个未导出接口，传包装器会静默丢掉该语义。
//
// 两道闸共用它，是因为这条不变式必须永远一致——边界是 `> n` 而不是 `>= n`（恰好 n
// 通过）、cause 用 *http.MaxBytesError（四条超限路径同型）、读闸门前不碰 body。
// 拆成两份写，迟早漂开。
func limitBody(w http.ResponseWriter, r *http.Request, n int64) error {
	if r.ContentLength > n {
		return TooLarge(codeBodyTooLarge, &http.MaxBytesError{Limit: n})
	}
	r.Body = http.MaxBytesReader(w, r.Body, n)
	return nil
}
