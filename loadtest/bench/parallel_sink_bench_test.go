package bench

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Luo-root/pulse-web/loadtest/fastsink"
	"github.com/Luo-root/pulse/observability"
)

// BenchmarkSinkWriteParallel 是**并发口径**：单 goroutine 的微基准看不到
// sync.Pool 在 goroutine 迁移下的失效率，也看不到锁竞争——而真实负载正是并发的。
//
// 结论预告：单线程 13× 的差距在并发下会大幅收窄，甚至反超。
func BenchmarkSinkWriteParallel(b *testing.B) {
	rec := sampleRecord()

	b.Run("slogsink", func(b *testing.B) {
		s := observability.SlogSink{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
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

	b.Run("fastsink", func(b *testing.B) {
		s := fastsink.New(io.Discard)
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				s.Write(rec)
			}
		})
	})
}
