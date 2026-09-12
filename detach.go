package web

import (
	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// Detached 是从一次请求解耦出来的**值袋子**。
//
// 契约：它不拥有任何 scope —— 只携带可安全跨 goroutine 使用的只读值。
// 后台任务需要作用域时自行 Root.Derive() 并自理 Dispose；打点则用 Observe
// （直写 Sink，与请求共享 TraceID）。因此 Detached 不存在共享状态的竞态。
type Detached struct {
	TraceID string
	HostID  string
	Sink    observability.Sink
	Root    *kernel.Context // 进程级 root（非请求 scope）
}

// Detach 返回当前请求的值袋子，供后台 goroutine 使用。
//
// 不携带请求级 KV：要传递的数据请在 Detach 前显式取出——让"哪些数据进了后台"
// 在代码里一眼可见，也不把 KV 的生命周期契约拉长。
func (c *Ctx) Detach() Detached {
	return Detached{
		TraceID: c.traceID,
		HostID:  c.engine.hostID,
		Sink:    c.engine.sink,
		Root:    c.engine.kernel,
	}
}

// Observe 直写一条记录到 Sink（不经 scope、零广播），与请求共享 TraceID。
// 与 Ctx.Observe 同构。
func (d Detached) Observe(event string, set func(*observability.Attrs)) {
	if d.Sink == nil {
		return
	}
	rec := observability.Record{
		HostID:  d.HostID,
		TraceID: d.TraceID,
		Source:  observability.SourceAdapter,
		Event:   event,
	}
	if set != nil {
		set(&rec.Attrs)
	}
	d.Sink.Write(rec)
}

// Service 读取全局服务（kernel root 仓库）—— 取全局服务不需要作用域。
func (d Detached) Service[T any](k kernel.ServiceKey[T]) (T, bool) {
	return kernel.Get(d.Root, k)
}

// MustService 读取全局服务，缺失即 panic（属装配错误）。
func (d Detached) MustService[T any](k kernel.ServiceKey[T]) T {
	v, ok := kernel.Get(d.Root, k)
	if !ok {
		panic("web: service not provided: " + k.Name())
	}
	return v
}
