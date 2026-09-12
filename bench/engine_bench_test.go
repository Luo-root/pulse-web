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

// BenchmarkEngineRequestPath 验证设计红线：默认请求路径零 Provide，
// 成本不随宿主插件树规模线性增长。
//
// 对照基线见 BenchmarkRequestCycle_Collector_PluginTree*（每请求 Provide =
// O(插件树)，≈ +47ns/插件）；本基准应保持平缓。
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

			req := httptest.NewRequest("GET", "/ping", nil)
			w := &nopWriter{}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				app.ServeHTTP(w, req)
			}
		})
	}
}

// BenchmarkEngineRequestPathMinimal 对照：Minimal 模式（无 Trace/AccessLog/Sink）。
func BenchmarkEngineRequestPathMinimal(b *testing.B) {
	app := web.New(web.Minimal())
	app.GET("/ping", func(c *web.Ctx) error { return c.Text(http.StatusOK, "pong") })
	b.Cleanup(func() { app.Root().Dispose() })

	req := httptest.NewRequest("GET", "/ping", nil)
	w := &nopWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		app.ServeHTTP(w, req)
	}
}
