package bench

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Luo-root/pulse-web/loadtest/ginapp"
	"github.com/Luo-root/pulse-web/loadtest/pulseapp"
)

// nopWriter 是零分配的响应出口：这里要量的是框架的请求路径，不是
// httptest.ResponseRecorder 的记账开销（它每请求都要建 buffer）。
type nopWriter struct{ h http.Header }

func (w *nopWriter) Header() http.Header         { return w.h }
func (w *nopWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *nopWriter) WriteHeader(int)             {}

func benchPath(b *testing.B, h http.Handler) {
	b.Helper()

	// 请求与 writer 都复用：两者都是「测试脚手架」，不该进被测框架的账。
	// GET 无 body，两侧的 ServeHTTP 都不会改动这个请求。
	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	w := &nopWriter{h: make(http.Header)}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		h.ServeHTTP(w, req)
	}
}

// BenchmarkRequestPath 是两侧同一条路由（GET /users/42 → 同一份 JSON）的
// 逐请求成本。`pulse/obs-nolog` 是**诊断档**：只开 Trace、关掉访问日志，
// 用来把观测那截开销拆开。
func BenchmarkRequestPath(b *testing.B) {
	cases := []struct {
		name string
		h    http.Handler
	}{
		{"gin/bare", ginapp.New(ginapp.ModeBare)},
		{"pulse/bare", pulseapp.New(pulseapp.ModeBare)},
		{"gin/obs", ginapp.New(ginapp.ModeObs)},
		{"pulse/obs-nolog", pulseapp.New(pulseapp.ModeObsNoLog)},
		{"pulse/obs", pulseapp.New(pulseapp.ModeObs)},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) { benchPath(b, tc.h) })
	}
}
