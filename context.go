package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// Handler 是框架的请求处理器：返回的 error 由集中错误映射处理。
type Handler func(*Ctx) error

// Middleware 是洋葱模型中间件：自行决定是否、何时调用 next。
type Middleware func(c *Ctx, next Handler) error

// Key 是请求级 KV 的类型化键（与 kernel.ServiceKey 同构，但独立命名空间）。
type Key[T any] struct{ name string }

// NewKey 定义请求级 KV 键。
func NewKey[T any](name string) Key[T] { return Key[T]{name: name} }

// Ctx 是一次请求的上下文。
//
// 生命周期与线程安全：Ctx 及其绑定的 ResponseWriter **不可跨 goroutine 使用**；
// 请求结束后 kernel scope 静默失效（Get 返回 false、Effect 返回 ErrDisposed）。
type Ctx struct {
	engine  *Engine
	w       *responseWriter
	r       *http.Request
	scope   *kernel.Context
	traceID string
	started time.Time

	kv     map[string]any
	status int
	err    error
}

// ---- 请求信息 ----

// Request 返回底层 *http.Request（只读）。
func (c *Ctx) Request() *http.Request { return c.r }

// Path 返回路径参数值——与 Request().PathValue 同源（ServeMux 写入的同一份），不另存。
func (c *Ctx) Path(name string) string { return c.r.PathValue(name) }

// Query 返回查询参数。
func (c *Ctx) Query(name string) string { return c.r.URL.Query().Get(name) }

// TraceID 返回本请求的 32hex trace 标识。
func (c *Ctx) TraceID() string { return c.traceID }

// Kernel 返回**请求 scope**：登记 Effect、注册事件监听、派生更深 scope、
// 或传给需要作用域的组件。它是随请求销毁的，不是进程级 kernel ——
// 进程级请用 Engine.Root()。
func (c *Ctx) Kernel() *kernel.Context { return c.scope }

// ---- 请求级 KV（框架自有；不进 kernel 仓库、零广播、O(1)）----

// Set 写入请求级值。
func (c *Ctx) Set[T any](k Key[T], v T) {
	if c.kv == nil {
		c.kv = make(map[string]any, 4)
	}
	c.kv[k.name] = v
}

// Get 读取请求级值。
func (c *Ctx) Get[T any](k Key[T]) (T, bool) {
	v, ok := c.kv[k.name]
	if !ok {
		var zero T
		return zero, false
	}
	tv, ok := v.(T)
	return tv, ok
}

// ---- 全局服务（kernel root 仓库；任何活 scope 都能读）----

// Service 读取全局服务。
func (c *Ctx) Service[T any](k kernel.ServiceKey[T]) (T, bool) {
	return kernel.Get(c.scope, k)
}

// MustService 读取全局服务，缺失即 panic —— 缺失属装配错误，应在启动期暴露。
func (c *Ctx) MustService[T any](k kernel.ServiceKey[T]) T {
	return mustService(c.scope, k)
}

// mustService 是 Ctx.MustService 与 Detached.MustService 的共用实现：
// 两者只在「从哪个 scope 读」上不同，panic 文案是一条语义，不该有两份。
func mustService[T any](scope *kernel.Context, k kernel.ServiceKey[T]) T {
	v, ok := kernel.Get(scope, k)
	if !ok {
		panic("web: service not provided: " + k.Name())
	}
	return v
}

// ---- 观测打点 ----

// Observe 直写一条业务记录到 Sink —— 不经 scope、零广播，与请求共享 TraceID。
// 实现见 writeObservation（与 Detached.Observe 共用）。
func (c *Ctx) Observe(event string, set func(*observability.Attrs)) {
	writeObservation(c.engine.sink, c.engine.hostID, c.traceID, event, set)
}

// ---- 响应写出 ----

// SetHeader 设置响应头（必须在首次写出之前调用）。
func (c *Ctx) SetHeader(key, value string) { c.w.Header().Set(key, value) }

// Writer 返回响应写出器，供**流式**场景直接写字节（SSE / chunked / 大文件）。
// 常规响应仍用 JSON / Text —— 它们会设好 Content-Type 与状态码。
//
//	c.SetHeader("Content-Type", "text/event-stream")
//	w := c.Writer()
//	for ev := range events {
//		fmt.Fprintf(w, "data: %s\n\n", ev)
//		if err := c.Flush(); err != nil { return err }  // 首刷落 200，此后逐段推送
//	}
//
// 返回的是框架的包装器：状态码与响应体积照常被 AccessLog 采集；包装器保留底层
// 能力（内嵌接口的方法集提升），`http.Flusher` / `http.ResponseController` 一类的
// 用法不受影响。
func (c *Ctx) Writer() http.ResponseWriter { return c.w }

// Status 只**设置**状态码，不立即写出；由 JSON/Text 或引擎收尾时落定。
func (c *Ctx) Status(code int) *Ctx {
	c.status = code
	return c
}

// JSON 写出 JSON 响应。
func (c *Ctx) JSON(code int, v any) error {
	c.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.w.WriteHeader(code)
	return json.NewEncoder(c.w).Encode(v)
}

// Text 写出纯文本响应。
func (c *Ctx) Text(code int, s string) error {
	c.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	c.w.WriteHeader(code)
	_, err := c.w.Write([]byte(s))
	return err
}

// Flush 把已写内容刷到客户端（SSE / 流式响应）。
func (c *Ctx) Flush() error {
	f, ok := c.w.ResponseWriter.(http.Flusher)
	if !ok {
		return errors.New("web: ResponseWriter does not support Flush")
	}
	c.w.WriteHeader(http.StatusOK)
	f.Flush()
	return nil
}

// responseWriter 包装 http.ResponseWriter，采集状态码与响应体积。
// 首次写入生效：重复 WriteHeader 是 no-op（不产生 superfluous WriteHeader 警告）。
type responseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *responseWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
