package bench

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse/observability"
)

// accessRecord 造一条与 Engine.writeAccessLog 同形的访问记录（字段数与生产一致）。
func accessRecord() observability.Record {
	rec := observability.Record{
		Time:     time.Now(),
		HostID:   "bench",
		TraceID:  "8f2e1a3b4c5d6e7f8a9b0c1d2e3f4a5b",
		Source:   observability.Source("http"),
		Event:    "http.request",
		Status:   "200",
		Duration: 585 * time.Microsecond,
	}
	observability.Set(&rec.Attrs, "http.request.method", "GET")
	observability.Set(&rec.Attrs, "http.route", "/users/{id}")
	observability.Set(&rec.Attrs, "url.path", "/users/42")
	observability.Set(&rec.Attrs, "http.response.body.size", int64(29))
	observability.Set(&rec.Attrs, "client.address", "192.0.2.1:1234")
	return rec
}

// BenchmarkSinkWrite_ConsoleVsUpstream 量「出口渲染」这一段：同一个 Record 喂给
// 默认出口（ConsoleSink）与上游两个现成出口，出口都写 io.Discard——所以测的是
// **格式化 + 锁**，不含磁盘/终端 I/O。
//
// 口径与 bench/budget_test.go 的门禁一致：结论优先看 **allocs/op**（跨运行稳定），
// ns 只做量级参考（跨轮能漂 2–4×）。
func BenchmarkSinkWrite_ConsoleVsUpstream(b *testing.B) {
	rec := accessRecord()

	b.Run("console", func(b *testing.B) {
		s := web.NewConsoleSink(io.Discard, web.WithColor(false))
		b.ReportAllocs()
		for range b.N {
			s.Write(rec)
		}
	})

	b.Run("console-parallel", func(b *testing.B) {
		s := web.NewConsoleSink(io.Discard, web.WithColor(false))
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				s.Write(rec)
			}
		})
	})

	b.Run("slogsink", func(b *testing.B) {
		s := observability.SlogSink{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		b.ReportAllocs()
		for range b.N {
			s.Write(rec)
		}
	})

	b.Run("linesink", func(b *testing.B) {
		s := observability.NewLineSink(io.Discard)
		b.ReportAllocs()
		for range b.N {
			s.Write(rec)
		}
	})
}

// BenchmarkRequestPath_DefaultSink 是「请求路径 + 默认出口」的端到端口径：
// 对照 nopSink，量出默认出口每请求加了多少。
func BenchmarkRequestPath_DefaultSink(b *testing.B) {
	b.Run("console-sink", func(b *testing.B) {
		app, cleanup := enginePathAppConsoleSink(b)
		defer cleanup()
		runRequestPath(b, app)
	})
	b.Run("nop-sink", func(b *testing.B) {
		app, cleanup := enginePathApp(b, 0)
		defer cleanup()
		runRequestPath(b, app)
	})
}

// enginePathAppConsoleSink 与 enginePathApp 同形，但出口换成默认装配实际用的
// ConsoleSink（写 io.Discard：保留每请求的格式化成本，排除终端 I/O）。
// benchmark 与分配预算门禁（budget_test.go）共用它。
func enginePathAppConsoleSink(tb testing.TB) (*web.Engine, func()) {
	tb.Helper()
	app := web.New(web.WithSink(web.NewConsoleSink(io.Discard, web.WithColor(false))))
	app.GET("/ping", func(c *web.Ctx) error { return c.Text(http.StatusOK, "pong") })
	return app, func() { app.Root().Dispose() }
}
