package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// 本文件覆盖 pulse v0.2.1 采纳后的两处行为：关闭期出口 flush 的形态覆盖，
// 与请求级 Collector（WithCollector）的可见性边界。

// ---- 关闭期 flush：形态覆盖 ----

// noFlushSink 连 Flush 都没有，用来确认探测不会误判。
type noFlushSink struct{}

func (noFlushSink) Write(observability.Record) {}

// ctxFlushRecorder / noCtxFlushRecorder 分别实现两种 flush 形态。
type ctxFlushRecorder struct{ called bool }

func (f *ctxFlushRecorder) Write(observability.Record)  {}
func (f *ctxFlushRecorder) Flush(context.Context) error { f.called = true; return nil }

type noCtxFlushRecorder struct{ called bool }

func (f *noCtxFlushRecorder) Write(observability.Record) {}
func (f *noCtxFlushRecorder) Flush() error               { f.called = true; return nil }

func TestSinkFlusherCoversBothShapes(t *testing.T) {
	ctxSink := &ctxFlushRecorder{}
	flush, ok := sinkFlusher(ctxSink)
	if !ok {
		t.Fatal("带 ctx 的形态（observability.AsyncSink）未被识别")
	}
	if err := flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ctxSink.called {
		t.Fatal("带 ctx 的 Flush 未被调用")
	}

	noCtx := &noCtxFlushRecorder{}
	flush, ok = sinkFlusher(noCtx)
	if !ok {
		t.Fatal("不带 ctx 的形态（observability.LineSink）未被识别——这正是会静默丢记录的那一种")
	}
	if err := flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !noCtx.called {
		t.Fatal("不带 ctx 的 Flush 未被调用")
	}

	if _, ok := sinkFlusher(noFlushSink{}); ok {
		t.Fatal("无 Flush 的出口不应被判定为可 flush")
	}
	if _, ok := sinkFlusher(nil); ok {
		t.Fatal("nil 出口不应被判定为可 flush")
	}

	// 真实出口：把上游的两个实现钉在探测面上。
	var buf bytes.Buffer
	if _, ok := sinkFlusher(observability.NewLineSink(&buf)); !ok {
		t.Fatal("observability.LineSink 必须被识别")
	}
	async := observability.NewAsyncSink(noFlushSink{})
	defer func() { _ = async.Close(context.Background()) }()
	if _, ok := sinkFlusher(async); !ok {
		t.Fatal("observability.AsyncSink 必须被识别")
	}
}

// TestShutdownFlushesBufferingSink 是缺陷回归：LineSink 缓冲未达阈值时不落盘，
// 关闭时序第 ⑤ 步必须把它 flush 出去。修复前这条用例在 main 上 FAIL
// （探测只认 Flush(context.Context) error，LineSink 不被匹配，缓冲随进程消失）。
func TestShutdownFlushesBufferingSink(t *testing.T) {
	var out bytes.Buffer
	e, _ := newTestEngine(t, WithSink(observability.NewLineSink(&out)))
	e.GET("/x", func(c *Ctx) error {
		c.Observe("probe.event", nil)
		return c.Text(http.StatusOK, "ok")
	})

	if rec := doReq(e, "GET", "/x", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if out.Len() != 0 {
		t.Fatalf("LineSink 未达阈值时不应落盘，实际已写 %d 字节", out.Len())
	}

	if err := e.shutdown(&http.Server{}); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if out.Len() == 0 {
		t.Fatal("关闭后出口缓冲仍为空：flush 探测没覆盖 LineSink 形态")
	}
	if !strings.Contains(out.String(), "probe.event") {
		t.Fatalf("关闭后落盘内容不含业务记录: %q", out.String())
	}
}

// ---- 请求级 Collector：可见性边界 ----

func TestWithCollectorScopeBoundary(t *testing.T) {
	e, _ := newTestEngine(t, WithCollector())

	var self, descendant, sibling bool
	e.GET("/probe", func(c *Ctx) error {
		scope := c.Kernel()
		if _, ok := kernel.Get(scope, observability.CollectorKey); ok {
			self = true
		}

		child, err := scope.Derive()
		if err != nil {
			return err
		}
		defer child.Dispose()
		if _, ok := kernel.Get(child, observability.CollectorKey); ok {
			descendant = true
		}

		// 插件私有 scope 的形态：root 的另一个子节点，与请求 scope 同级。
		sib, err := e.Root().Derive()
		if err != nil {
			return err
		}
		defer sib.Dispose()
		if _, ok := kernel.Get(sib, observability.CollectorKey); ok {
			sibling = true
		}

		return c.Text(http.StatusOK, "ok")
	})

	doReq(e, "GET", "/probe", nil)

	if !self {
		t.Error("请求 scope 自身应读得到 Collector")
	}
	if !descendant {
		t.Error("请求 scope 的后代应读得到 Collector")
	}
	if sibling {
		t.Error("插件私有 scope（与请求 scope 是兄弟）不应读得到 Collector")
	}
	if _, ok := kernel.Get(e.Root(), observability.CollectorKey); ok {
		t.Error("宿主 root 不应读得到请求级 Collector")
	}
}

func TestWithCollectorDefaultsOff(t *testing.T) {
	e, _ := newTestEngine(t)

	var found bool
	e.GET("/probe", func(c *Ctx) error {
		_, found = kernel.Get(c.Kernel(), observability.CollectorKey)
		return c.Text(http.StatusOK, "ok")
	})
	doReq(e, "GET", "/probe", nil)

	if found {
		t.Fatal("默认装配不应挂 Collector —— 开了才付每请求成本")
	}
}

func TestWithCollectorRequiresSink(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Minimal() 且无 WithSink 时，WithCollector 应在装配期 panic（对齐「装配期暴露错误」）")
		}
	}()
	_ = New(Minimal(), WithCollector())
}

// TestCollectorCarriesOwnRequestTrace 确认并发请求不串台：每个请求的
// Collector 自带自己的 TraceID，且作用域销毁后绑定撤除。
func TestCollectorCarriesOwnRequestTrace(t *testing.T) {
	e, sink := newTestEngine(t, WithCollector())
	var afterDispose bool

	e.GET("/probe", func(c *Ctx) error {
		col, ok := kernel.Get(c.Kernel(), observability.CollectorKey)
		if !ok {
			return errors.New("collector missing")
		}
		col.WriteAttrs("probe.event", "", func(a *observability.Attrs) {
			observability.Set(a, "probe.marker", c.TraceID())
		})
		return c.Text(http.StatusOK, c.TraceID())
	})

	first := doReq(e, "GET", "/probe", nil)
	second := doReq(e, "GET", "/probe", nil)

	var got []string
	for _, r := range sink.Snapshot() {
		if r.Event == "probe.event" {
			got = append(got, r.TraceID)
		}
	}
	if len(got) != 2 {
		t.Fatalf("probe.event 记录数 = %d, want 2", len(got))
	}
	if got[0] != first.Header().Get("X-Trace-Id") || got[1] != second.Header().Get("X-Trace-Id") {
		t.Fatalf("Collector 记录的 TraceID 与各自请求不符: %v / %q / %q",
			got, first.Header().Get("X-Trace-Id"), second.Header().Get("X-Trace-Id"))
	}
	if got[0] == got[1] {
		t.Fatal("两次请求 TraceID 相同——并发请求会串台")
	}

	// 请求 scope 已销毁；绑在它上面的 Collector 应一并撤除（这里只能间接验：
	// 新请求拿到的是新实例，且上面的断言已确认 TraceID 各自独立）。
	e.GET("/again", func(c *Ctx) error {
		col, ok := kernel.Get(c.Kernel(), observability.CollectorKey)
		if !ok {
			return errors.New("collector missing on second request")
		}
		afterDispose = col != nil
		return c.Text(http.StatusOK, "ok")
	})
	doReq(e, "GET", "/again", nil)
	if !afterDispose {
		t.Fatal("后续请求应拿到自己的 Collector 实例")
	}
}
