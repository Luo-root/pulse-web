package bench

import (
	"testing"

	"github.com/Luo-root/pulse/kernel"
)

// --- 14. 轻量路径的并发扩展性（不 Provide 服务，只用 scope + 请求级监听） ---

func BenchmarkRequestCycle_Light_Parallel(b *testing.B) {
	host := kernel.New()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			sc, err := host.Derive()
			if err != nil {
				b.Fatal(err)
			}
			if _, err := kernel.On(sc, probeEv, func(*int) {}); err != nil {
				b.Fatal(err)
			}
			sc.Dispose()
		}
	})
}

// --- 15. 纯 derive/dispose 的并发扩展性（测量 children 数组在该并发下的行为） ---

func BenchmarkScopeCycle_EmptyHost_Parallel(b *testing.B) {
	host := kernel.New()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			sc, err := host.Derive()
			if err != nil {
				b.Fatal(err)
			}
			sc.Dispose()
		}
	})
}
