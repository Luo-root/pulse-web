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
