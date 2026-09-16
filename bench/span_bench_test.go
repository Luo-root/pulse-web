package bench

import (
	"context"
	"net/http"
	"testing"

	web "github.com/Luo-root/pulse-web"
)

// nopSpanHook 只回一个身份、什么都不做：把「框架侧 span 开销」从适配件与 SDK
// 的代价里隔离出来。适配件那一侧的实测在 `otel/`（那边才带 SDK 依赖）。
type nopSpanHook struct{}

func (nopSpanHook) Begin(_ context.Context, in web.SpanInfo) (context.Context, web.SpanRef) {
	return context.Background(), web.SpanRef{TraceID: in.TraceID, SpanID: "0123456789abcdef"}
}

func (nopSpanHook) End(context.Context, web.Span) {}

// BenchmarkEngineRequestPath_SpanHook 量「装了 span 出口、但 hook 几乎不干活」
// 时的请求路径成本。对照行是同文件外的 enginePathApp（不装 hook）。
//
// 关注 allocs/op：它是整数、跨轮稳定；ns 只做量级参考（见 budget_test.go 文件头）。
// 这一档不进分配门禁——它是「装了才有」的成本，不是默认路径的预算。
func BenchmarkEngineRequestPath_SpanHook(b *testing.B) {
	app, cleanup := spanHookApp(b)
	defer cleanup()

	b.ReportAllocs()
	b.ResetTimer()
	iterateRequestPath(app, b.N)
}

// spanHookApp 是装了 nopSpanHook 的引擎，配置与 enginePathApp 逐项对齐
// （nopSink + 同一个 handler），这样两个基准的差就是 span 出口本身。
func spanHookApp(tb testing.TB) (*web.Engine, func()) {
	tb.Helper()
	app := web.New(
		web.WithSink(nopSink{}),
		web.WithHostID("bench"),
		web.WithSpanHook(nopSpanHook{}),
	)
	app.GET("/ping", func(c *web.Ctx) error { return c.Text(http.StatusOK, "pong") })
	return app, func() {}
}
