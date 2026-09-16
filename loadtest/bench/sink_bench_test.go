package bench

import (
	"io"
	"log/slog"
	"testing"
	"time"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse-web/loadtest/pulseapp"
	"github.com/Luo-root/pulse/observability"
)

// discardLogger 是「slog 适配出口 + 空目的地」——量 `SlogSink` 用，
// **不是**框架的默认出口（默认是 `ConsoleSink`，见 pulseapp.ModeObs）。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sampleRecord 造一条和访问日志同等形状的记录（字段数一致）。
func sampleRecord() observability.Record {
	var attrs observability.Attrs
	observability.Set(&attrs, "http.request.method", "GET")
	observability.Set(&attrs, "http.route", "/users/{id}")
	observability.Set(&attrs, "url.path", "/users/42")
	observability.Set(&attrs, "http.response.body.size", int64(29))
	observability.Set(&attrs, "client.address", "192.0.2.1:1234")

	return observability.Record{
		Time:     time.Now(),
		HostID:   "pulse-web",
		TraceID:  "fe7d39b7606f9b4b71630724f749e365",
		Source:   observability.Source("http"),
		Event:    "http.request",
		Status:   "200",
		Duration: 585 * time.Microsecond,
		Attrs:    attrs,
	}
}

// BenchmarkSinkWrite 只量「出口渲染」这一段：同一个 Record 喂给几个出口。
//
// 出口都写 io.Discard，所以这测的是**格式化 + 锁**，不含磁盘——
// 与真实负载对比里 obs 档的口径一致。
//
// 第一行是**框架默认出口本身**（`web.NewConsoleSink`），不是它的复刻：历史上这里跑过
// 一个 `columnSink` 原型，默认出口做出来之后就没必要再拿复刻当基准了（复刻还会漂）。
func BenchmarkSinkWrite(b *testing.B) {
	rec := sampleRecord()

	b.Run("console", func(b *testing.B) {
		s := web.NewConsoleSink(io.Discard)
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

	b.Run("asyncsink", func(b *testing.B) {
		s := observability.NewAsyncSink(web.NewConsoleSink(io.Discard))
		b.ReportAllocs()
		for range b.N {
			s.Write(rec)
		}
	})

	b.Run("slogsink", func(b *testing.B) {
		s := observability.SlogSink{Logger: discardLogger()}
		b.ReportAllocs()
		for range b.N {
			s.Write(rec)
		}
	})
}

// BenchmarkDefaultVSAsync 是端到端口径：同一条请求路径，只换出口——
// 就是那对真实存在的选择（默认出口，与默认外面再套一层 AsyncSink）。
func BenchmarkDefaultVSAsync(b *testing.B) {
	b.Run("pulse/obs", func(b *testing.B) {
		benchPath(b, pulseapp.New(pulseapp.ModeObs))
	})
	b.Run("pulse/obs-async", func(b *testing.B) {
		benchPath(b, pulseapp.New(pulseapp.ModeObsAsync))
	})
}
