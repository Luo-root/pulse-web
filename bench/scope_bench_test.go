// Package bench 的 kernel 层基线：请求级作用域（每请求 Derive → 挂观测 →
// Dispose）的成本边界，回答「装配期抽象能否扛住请求路径强度」。
//
// 这里也是设计文档「实测数据」一节的来源。**有包含关系的两条基准必须留在
// 同一文件**（如 AttachCollector 与裸 Local 绑定），否则会被跨轮相减，
// 得出倒挂的结论。
package bench

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// nopSink 丢弃记录，避免 MemorySink 的锁竞争污染并发测量。
type nopSink struct{}

func (nopSink) Write(observability.Record) {}

var (
	benchCfg = observability.ObserveConfig{Sink: nopSink{}, HostID: "bench", TraceID: "bench-trace"}
	probeEv  = kernel.NewEventKey[int]("bench.probe")
)

// filler 模拟应用树上的普通插件：Apply 里注册事件监听 + 登记一条资源效应。
// 经 kernel.Use 装载后，它同时订阅了宿主服务变更（Use 的内部行为）。
type filler struct{}

func (filler) Inject() []kernel.Dependency { return nil }

func (filler) Apply(c *kernel.Context) error {
	if _, err := kernel.On(c, probeEv, func(*int) {}); err != nil {
		return err
	}
	_, err := c.Effect(func() (func(), error) { return func() {}, nil })
	return err
}

// newHostWith 建一个挂了 n 个插件的宿主（模拟真实应用装配后的规模）。
func newHostWith(n int) *kernel.Context {
	host := kernel.New()
	for i := 0; i < n; i++ {
		if _, err := kernel.Use(host, filler{}); err != nil {
			panic(err)
		}
	}
	return host
}

// --- 1. 纯 scope 派生/销毁（空树） ---

func BenchmarkScopeCycle_EmptyHost(b *testing.B) {
	host := kernel.New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sc, err := host.Derive()
		if err != nil {
			b.Fatal(err)
		}
		sc.Dispose()
	}
}

// --- 2. 请求作用域 + 若干 Effect（模拟请求级资源登记） ---

func BenchmarkScopeCycle_WithEffects(b *testing.B) {
	host := kernel.New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sc, err := host.Derive()
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 5; j++ {
			if _, err := sc.Effect(func() (func(), error) { return func() {}, nil }); err != nil {
				b.Fatal(err)
			}
		}
		sc.Dispose()
	}
}

// --- 3. 官方式请求观测接线：每请求 AttachCollector（v0.2.1 起为作用域局部绑定） ---

func BenchmarkRequestCycle_Collector(b *testing.B) {
	host := kernel.New()
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

// --- 4. 对照：裸 kernel.Local() 绑定（无 Collector 分配） ---
//
// **必须与上一条在同一轮跑**：AttachCollector = 1 次 Local 绑定 + 1 个
// Collector 分配，所以它的 Δ 必须 ≥ 裸绑定。两个数分两轮测就会倒挂——
// 设计文档里曾经把一个跨轮比较（裸绑定 325ns vs AttachCollector 227ns）
// 当成两个结论写进去，被 review 抓出「有包含关系的操作成本倒挂」。

var benchLocalKey = kernel.NewServiceKey[string]("bench.local")

func BenchmarkRequestCycle_LocalBinding(b *testing.B) {
	host := kernel.New()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sc, err := host.Derive()
		if err != nil {
			b.Fatal(err)
		}
		if _, err := kernel.Provide(sc, benchLocalKey, "value", kernel.Local()); err != nil {
			b.Fatal(err)
		}
		sc.Dispose()
	}
}

// --- 5. 同上，但宿主挂了 50 个插件（v0.2.1 前量全树广播的规模效应；现已解耦） ---

func BenchmarkRequestCycle_Collector_PluginTree50(b *testing.B) {
	host := newHostWith(50)
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

// --- 6. 并发：同规模下并行（量请求级局部绑定的 root 锁竞争） ---

func BenchmarkRequestCycle_Collector_PluginTree50_Parallel(b *testing.B) {
	host := newHostWith(50)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			sc, err := host.Derive()
			if err != nil {
				b.Fatal(err)
			}
			if _, err := observability.AttachCollector(sc, benchCfg); err != nil {
				b.Fatal(err)
			}
			sc.Dispose()
		}
	})
}

// --- 7. 请求级事件派发：EmitLocal（只本层） ---

func BenchmarkEmitLocal(b *testing.B) {
	host := kernel.New()
	sc, err := host.Derive()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := kernel.On(sc, probeEv, func(p *int) { *p++ }); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kernel.EmitLocal(sc, probeEv, i)
	}
}

// --- 8. 全树事件派发：Emit（收集整棵树监听器） ---

func BenchmarkEmitFullTree(b *testing.B) {
	host := newHostWith(50)
	sc, err := host.Derive()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := kernel.On(sc, probeEv, func(p *int) { *p++ }); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		kernel.Emit(host, probeEv, i)
	}
}

// --- 9. 对照基准：纯 net/http 空 handler ---

func BenchmarkStdlibHTTPBaseline(b *testing.B) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	req := httptest.NewRequest("GET", "/", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
}
