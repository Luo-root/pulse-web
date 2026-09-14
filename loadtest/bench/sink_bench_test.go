package bench

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Luo-root/pulse-web/loadtest/fastsink"
	"github.com/Luo-root/pulse-web/loadtest/pulseapp"
	"github.com/Luo-root/pulse/observability"
)

// discardLogger 是「默认出口形态、空目的地」——与 loadtest 主对比的 obs 档一致。
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

// BenchmarkSinkWrite 只量「出口渲染」这一段：同一个 Record 喂给四个出口。
//
// 出口都写 io.Discard，所以这测的是**格式化 + 锁**，不含磁盘——
// 与 loadtest 主对比里 obs 档的口径一致。
func BenchmarkSinkWrite(b *testing.B) {
	rec := sampleRecord()

	b.Run("slogsink", func(b *testing.B) {
		s := observability.SlogSink{Logger: discardLogger()}
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
		s := observability.NewAsyncSink(observability.NewLineSink(io.Discard))
		b.ReportAllocs()
		for range b.N {
			s.Write(rec)
		}
	})

	b.Run("columnsink-fmt", func(b *testing.B) {
		s := &columnSink{w: io.Discard}
		b.ReportAllocs()
		for range b.N {
			s.Write(rec)
		}
	})

	b.Run("fastsink", func(b *testing.B) {
		s := fastsink.New(io.Discard)
		b.ReportAllocs()
		for range b.N {
			s.Write(rec)
		}
	})
}

// BenchmarkFastSinkVSDefault 是端到端口径：同一条请求路径，只换出口。
func BenchmarkFastSinkVSDefault(b *testing.B) {
	b.Run("pulse/obs-slog", func(b *testing.B) {
		benchPath(b, pulseapp.NewWithSink(observability.SlogSink{Logger: discardLogger()}))
	})
	b.Run("pulse/obs-fast", func(b *testing.B) {
		benchPath(b, pulseapp.NewWithSink(fastsink.New(io.Discard)))
	})
}
