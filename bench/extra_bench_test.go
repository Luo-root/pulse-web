package bench

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// --- 9. 轻量请求路径：只派生 scope + 挂请求级监听（不 Provide 服务） ---

func BenchmarkRequestCycle_Light(b *testing.B) {
	host := kernel.New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sc, err := host.Derive()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := kernel.On(sc, probeEv, func(*int) {}); err != nil {
			b.Fatal(err)
		}
		sc.Dispose()
	}
}

// --- 10/11. 规模线性度：树规模 10 / 100 ---

func benchCycleWithPlugins(b *testing.B, n int) {
	host := newHostWith(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sc, err := host.Derive()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := observability.AttachCollector(sc, benchCfg); err != nil {
			b.Fatal(err)
		}
		sc.Dispose()
	}
}

func BenchmarkRequestCycle_Collector_PluginTree10(b *testing.B)  { benchCycleWithPlugins(b, 10) }
func BenchmarkRequestCycle_Collector_PluginTree100(b *testing.B) { benchCycleWithPlugins(b, 100) }

// --- 12. 路由基线：标准库 ServeMux（Go 1.22+ 方法+通配模式） ---

func BenchmarkStdlibServeMux_Route(b *testing.B) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = r.PathValue("id")
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest("GET", "/api/v1/users/12345", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
}

// --- 13. 服务查找（Get）开销：请求路径上每次读服务都要过的路径 ---

func BenchmarkServiceGet(b *testing.B) {
	host := kernel.New()
	if _, err := kernel.Provide(host, benchKey, "value"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := kernel.Get(host, benchKey); !ok {
			b.Fatal("missing")
		}
	}
}

var benchKey = kernel.NewServiceKey[string]("bench.kv")
