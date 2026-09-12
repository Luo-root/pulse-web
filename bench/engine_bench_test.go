package bench

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	web "github.com/Luo-root/pulse-web"
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

// BenchmarkEngineRequestPath 验证设计红线：默认请求路径零 Provide，
// 成本不随宿主插件树规模线性增长。
//
// 测量口径（三处刻意选择，避免 ns/op 与 allocs/op 口径不一致）：
//   - setup（建树、装配、注册路由）在 ResetTimer 之前完成；
//   - 每轮在循环内重建请求：`httptest.NewRequest` 的开销同时计入 ns/op 与
//     allocs/op（复用同一个 *http.Request 会让其 body reader 首轮被读空，
//     之后每轮走不同路径）；
//   - 结论优先看 **allocs/op 是否恒定**（噪声下比 ns 稳）。
//
// 对照组：BenchmarkRequestCycle_Collector_PluginTree*（每请求 Provide = O(插件树)）。
// 采样建议：-count=3 起，观察离散度。
func BenchmarkEngineRequestPath(b *testing.B) {
	for _, n := range []int{0, 10, 50} {
		b.Run(fmt.Sprintf("plugins=%d", n), func(b *testing.B) {
			root := kernel.New()
			for i := 0; i < n; i++ {
				if _, err := kernel.Use(root, filler{}); err != nil {
					b.Fatal(err)
				}
			}
			app := web.New(web.WithRoot(root), web.WithSink(nopSink{}))
			app.GET("/ping", func(c *web.Ctx) error { return c.Text(http.StatusOK, "pong") })
			b.Cleanup(func() { root.Dispose() })

			w := &nopWriter{}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				app.ServeHTTP(w, httptest.NewRequest("GET", "/ping", nil))
			}
		})
	}
}

// BenchmarkEngineRequestPathMinimal 对照：Minimal 模式（无 Trace / AccessLog / Sink）。
func BenchmarkEngineRequestPathMinimal(b *testing.B) {
	app := web.New(web.Minimal())
	app.GET("/ping", func(c *web.Ctx) error { return c.Text(http.StatusOK, "pong") })
	b.Cleanup(func() { app.Root().Dispose() })

	w := &nopWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		app.ServeHTTP(w, httptest.NewRequest("GET", "/ping", nil))
	}
}
