package bench

import (
	"io"
	"log/slog"
	"testing"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse/observability"
)

// BenchmarkSinkWriteParallel 是**并发口径**：单 goroutine 的微基准看不到
// sync.Pool 在 goroutine 迁移下的失效率，也看不到锁竞争——而真实负载正是并发的。
//
// 出口集只列**今天真实存在的选项**：默认出口（`NewConsoleSink`）、上游的行式出口、
// 以及 slog 适配（给要用成熟日志栈的人）。历史上有过一个 `fastsink` 原型出口，
// 它的手法已经被默认出口吸收，再当选项比就是拿不会发布的实现当基准。
func BenchmarkSinkWriteParallel(b *testing.B) {
	rec := sampleRecord()

	b.Run("console", func(b *testing.B) {
		s := web.NewConsoleSink(io.Discard)
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				s.Write(rec)
			}
		})
	})

	b.Run("linesink", func(b *testing.B) {
		s := observability.NewLineSink(io.Discard)
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
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				s.Write(rec)
			}
		})
	})
}
