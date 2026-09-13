package bench

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse/kernel"
)

// nopWriter 避免 httptest.ResponseRecorder 的分配干扰测量。
type nopWriter struct{ h http.Header }

func (w *nopWriter) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}
func (w *nopWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *nopWriter) WriteHeader(int)             {}

// enginePathApp 建「默认请求路径」的 Engine：n 个插件宿主 + nopSink + 一条 /ping。
//
// benchmark 与分配预算门禁（budget_test.go）共用它，避免两处各写一份请求路径定义。
func enginePathApp(tb testing.TB, plugins int) (*web.Engine, func()) {
	tb.Helper()
	root := kernel.New()
	for i := 0; i < plugins; i++ {
		if _, err := kernel.Use(root, filler{}); err != nil {
			tb.Fatal(err)
		}
	}
	app := web.New(web.WithRoot(root), web.WithSink(nopSink{}))
	app.GET("/ping", func(c *web.Ctx) error { return c.Text(http.StatusOK, "pong") })
	return app, root.Dispose
}

// enginePathAppCollector 同上，但开 WithCollector()。
func enginePathAppCollector(tb testing.TB) (*web.Engine, func()) {
	tb.Helper()
	app := web.New(web.WithSink(nopSink{}), web.WithCollector())
	app.GET("/ping", func(c *web.Ctx) error { return c.Text(http.StatusOK, "pong") })
	return app, func() { app.Root().Dispose() }
}

// enginePathAppMinimal 同上，但用 Minimal()（无 Trace / AccessLog / Sink）。
func enginePathAppMinimal(tb testing.TB) (*web.Engine, func()) {
	tb.Helper()
	app := web.New(web.Minimal())
	app.GET("/ping", func(c *web.Ctx) error { return c.Text(http.StatusOK, "pong") })
	return app, func() { app.Root().Dispose() }
}

// runRequestPath 是「一次请求」的循环体：benchmark 与分配预算门禁共用。
//
// 每轮在循环内重建请求：`httptest.NewRequest` 的开销同时计入 ns/op 与 allocs/op
// （复用同一个 *http.Request 会让其 body reader 首轮被读空，之后每轮走不同路径）。
func runRequestPath(b *testing.B, app *web.Engine) {
	w := &nopWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		app.ServeHTTP(w, httptest.NewRequest("GET", "/ping", nil))
	}
}

// BenchmarkEngineRequestPath 验证设计红线：默认请求路径零 Provide，
// 成本不随宿主插件树规模线性增长。
//
// 测量口径（三处刻意选择，避免 ns/op 与 allocs/op 口径不一致）：
//   - setup（建树、装配、注册路由）在 ResetTimer 之前完成；
//   - 每轮在循环内重建请求（理由见 runRequestPath）；
//   - 结论优先看 **allocs/op 是否恒定**（噪声下比 ns 稳）。
//
// 对照组：BenchmarkRequestCycle_Collector_PluginTree*（v0.2.1 之前每请求
// AttachCollector = O(插件树)；上游 #169 之后应与插件树规模解耦，留作对照）。
// 采样建议：-count=3 起，观察离散度。
//
// 回归门禁：分配计数由 bench/budget_test.go 断言（不用人来记得跑）。
func BenchmarkEngineRequestPath(b *testing.B) {
	for _, n := range []int{0, 10, 50} {
		b.Run(fmt.Sprintf("plugins=%d", n), func(b *testing.B) {
			app, cleanup := enginePathApp(b, n)
			b.Cleanup(cleanup)
			runRequestPath(b, app)
		})
	}
}

// BenchmarkEngineRequestPath_Collector 端到端对照：开启 WithCollector() 的
// 请求路径。
//
// 与 BenchmarkEngineRequestPath 同口径，**必须同轮跑取差值**——设计文档与
// `WithCollector` 的 godoc 发布的「每请求成本」落点就在这里，不要让读者自己
// 去拿跨轮的两个数相减。
func BenchmarkEngineRequestPath_Collector(b *testing.B) {
	app, cleanup := enginePathAppCollector(b)
	b.Cleanup(cleanup)
	runRequestPath(b, app)
}

// BenchmarkEngineRequestPathMinimal 对照：Minimal 模式（无 Trace / AccessLog / Sink）。
func BenchmarkEngineRequestPathMinimal(b *testing.B) {
	app, cleanup := enginePathAppMinimal(b)
	b.Cleanup(cleanup)
	runRequestPath(b, app)
}
