package web

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/observability"
)

// 关闭期出口 flush 的形态覆盖。
//
// 上游有两种出口形态：带 ctx 的（`observability.AsyncSink`）与不带的
// （`observability.LineSink`）。只认其中一种会静默跳过另一种——LineSink
// 形态下「关闭前未达阈值的最后一批」会随进程消失，不报错、不告警。

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
	// 默认出口也是这一形态（内嵌 LineSink）——它是 New() 实际装配的那一个，
	// 漏了它，「关闭前最后一批随进程消失」会落回默认路径。
	var consoleBuf bytes.Buffer
	if _, ok := sinkFlusher(NewConsoleSink(&consoleBuf, WithColor(false))); !ok {
		t.Fatal("默认出口 ConsoleSink 必须被识别")
	}
	async := observability.NewAsyncSink(noFlushSink{})
	defer func() { _ = async.Close(context.Background()) }()
	if _, ok := sinkFlusher(async); !ok {
		t.Fatal("observability.AsyncSink 必须被识别")
	}
}

// TestShutdownFlushesBufferingSink 是缺陷回归：LineSink 缓冲未达阈值时不落盘，
// 关闭时序第 ⑤ 步必须把它 flush 出去。
//
// 复现缺陷的做法（依赖 v0.2.0 的老版本时不能照字面跑本用例——`NewLineSink`
// 在 v0.2.0 不存在，会直接编译失败）：**保持依赖不变、只回退
// `engine.go` 的 `sinkFlusher`**，用一个只实现 `Flush() error` 的等价出口，
// 即可在旧代码上看到 `out.Len() == 0`。本用例断言的就是那个出口。
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
