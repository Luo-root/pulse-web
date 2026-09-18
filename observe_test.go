package web

import (
	crand "crypto/rand"
	"fmt"
	mrand "math/rand/v2"
	"net/http"
	"testing"
	"time"

	"github.com/Luo-root/pulse/observability"
)

// 本文件覆盖 observe.go：trace-id 的产生与入站链路头的解析（含基准测试）。
// 边界：span 身份与两个响应头的分工归 span_test.go。

func findRecord(sink *observability.MemorySink, event string) (observability.Record, bool) {
	for _, r := range sink.Snapshot() {
		if r.Event == event {
			return r, true
		}
	}
	return observability.Record{}, false
}

func TestTraceIDGeneratedAndSharedAcrossRecord(t *testing.T) {
	e, sink := newTestEngine(t)
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, c.TraceID()) })

	rec := doReq(e, "GET", "/t", nil)

	header := rec.Header().Get("X-Trace-Id")
	if len(header) != 32 {
		t.Fatalf("X-Trace-Id = %q (len %d), want 32hex", header, len(header))
	}
	if body := rec.Body.String(); body != header {
		t.Fatalf("handler TraceID %q != response header %q", body, header)
	}

	logRec, ok := findRecord(sink, eventHTTPReq)
	if !ok {
		t.Fatal("no http.request record")
	}
	if logRec.TraceID != header {
		t.Fatalf("record TraceID %q != %q", logRec.TraceID, header)
	}
	if logRec.HostID != "test-host" {
		t.Fatalf("record HostID = %q", logRec.HostID)
	}
}

func TestTraceparentAdopted(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, c.TraceID()) })

	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	rec := doReq(e, "GET", "/t", nil, "Traceparent", tp)

	const want = "4bf92f3577b34da6a3ce929d0e0e4736"
	if got := rec.Header().Get("X-Trace-Id"); got != want {
		t.Fatalf("X-Trace-Id = %q, want %q", got, want)
	}
	if got := rec.Body.String(); got != want {
		t.Fatalf("handler TraceID = %q, want %q", got, want)
	}
}

func TestB3TraceIDAdopted(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, c.TraceID()) })

	const b3 = "0af7651916cd43dd8448eb211c80319c"
	rec := doReq(e, "GET", "/t", nil, "X-B3-TraceId", b3)
	if got := rec.Header().Get("X-Trace-Id"); got != b3 {
		t.Fatalf("X-Trace-Id = %q, want %q", got, b3)
	}
}

func TestB3TraceID16HexNormalized(t *testing.T) {
	// Zipkin 的 B3 也允许 16hex（64-bit）；左垫 0 归一为 32hex 后采纳——
	// 网关后链路不会因为上游用 16hex 而断裂（#33）。
	e, _ := newTestEngine(t)
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, c.TraceID()) })

	const b3 = "0af7651916cd43dd"
	const want = "00000000000000000af7651916cd43dd"
	rec := doReq(e, "GET", "/t", nil, "X-B3-TraceId", b3)
	if got := rec.Header().Get("X-Trace-Id"); got != want {
		t.Fatalf("X-Trace-Id = %q, want %q", got, want)
	}
	if got := rec.Body.String(); got != want {
		t.Fatalf("handler TraceID = %q, want %q", got, want)
	}
}

func TestMalformedTraceHeadersIgnored(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, c.TraceID()) })

	rec := doReq(e, "GET", "/t", nil, "Traceparent", "00-tooshort-xyz-01")
	got := rec.Header().Get("X-Trace-Id")
	if got == "tooshort" || len(got) != 32 {
		t.Fatalf("malformed header should be ignored, got %q", got)
	}
}

// TestZeroTraceIDTreatedAsAbsent：全零 trace-id 一律视为不存在（两条入站路径同一口径）。
//
// W3C traceparent 明文规定 trace-id 不得为全零；B3 未禁止，但同样按不存在处理——
// 否则带该头的请求会在日志里共享同一条 TraceID（比链路断裂更难排查）。review 提出，PR #35。
func TestZeroTraceIDTreatedAsAbsent(t *testing.T) {
	const zero = "00000000000000000000000000000000"
	cases := []struct{ name, header, value string }{
		{"traceparent 全零", "Traceparent", "00-" + zero + "-1111111111111111-01"},
		{"B3 32hex 全零", "X-B3-TraceId", zero},
		{"B3 16hex 全零", "X-B3-TraceId", "0000000000000000"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, _ := newTestEngine(t)
			e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, c.TraceID()) })

			rec := doReq(e, "GET", "/t", nil, c.header, c.value)
			got := rec.Header().Get("X-Trace-Id")
			if got == zero {
				t.Fatalf("全零 trace-id 被采纳了：%q（应视为不存在、回落到框架生成器）", got)
			}
			if len(got) != 32 {
				t.Fatalf("X-Trace-Id = %q，want 32hex（框架生成）", got)
			}
		})
	}
}

func TestAccessLogRecordFields(t *testing.T) {
	e, sink := newTestEngine(t)
	e.GET("/things/{id}", func(c *Ctx) error {
		c.Observe("biz.event", func(a *observability.Attrs) {
			observability.Set(a, "user.id", "u-1")
		})
		return c.JSON(http.StatusCreated, map[string]string{"id": c.Path("id")})
	})

	rec := doReq(e, "GET", "/things/7", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}

	logRec, ok := findRecord(sink, eventHTTPReq)
	if !ok {
		t.Fatal("no http.request record")
	}
	if logRec.Source != observability.Source(sourceHTTP) {
		t.Fatalf("source = %q, want %q", logRec.Source, sourceHTTP)
	}
	if logRec.Status != "201" {
		t.Fatalf("status = %q", logRec.Status)
	}
	if logRec.Duration < 0 {
		t.Fatalf("duration = %v", logRec.Duration)
	}
	if logRec.Err != nil {
		t.Fatalf("unexpected err: %v", logRec.Err)
	}

	attrs := attrsOf(logRec)
	if attrs["http.request.method"] != "GET" {
		t.Fatalf("method attr = %v", attrs["http.request.method"])
	}
	if attrs["http.route"] != "/things/{id}" {
		t.Fatalf("route attr = %v", attrs["http.route"])
	}
	if attrs["url.path"] != "/things/7" {
		t.Fatalf("path attr = %v", attrs["url.path"])
	}
	if _, hasErrType := attrs["error.type"]; hasErrType {
		t.Fatal("error.type should be absent on success")
	}

	// 业务打点：同 Sink、同 TraceID，Source 为 Collector 直写面的 SourceAdapter
	bizRec, ok := findRecord(sink, "biz.event")
	if !ok {
		t.Fatal("no biz.event record")
	}
	if bizRec.TraceID != logRec.TraceID {
		t.Fatalf("biz TraceID %q != %q", bizRec.TraceID, logRec.TraceID)
	}
	if bizRec.Source != observability.SourceAdapter {
		t.Fatalf("biz source = %q, want %q", bizRec.Source, observability.SourceAdapter)
	}
	if got := attrsOf(bizRec)["user.id"]; got != "u-1" {
		t.Fatalf("user.id = %v", got)
	}
}

func TestPanicRecordedWithErrorType(t *testing.T) {
	e, sink := newTestEngine(t)
	e.GET("/boom", func(c *Ctx) error { panic("kaboom") })

	rec := doReq(e, "GET", "/boom", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); got == "" || got == "kaboom" {
		t.Fatalf("body = %q, want sanitized payload", got)
	}

	logRec, ok := findRecord(sink, eventHTTPReq)
	if !ok {
		t.Fatal("no http.request record for panicking request")
	}
	if logRec.Err == nil {
		t.Fatal("panic should be recorded as Err")
	}
	if got := attrsOf(logRec)["error.type"]; got != "panic" {
		t.Fatalf("error.type = %v, want panic", got)
	}
}

// trace-id 生成的三条实现对照（#78 的实测落点）。
//
// 结论是**不换随机源**（理由见设计文档「trace 头兼容与 span 出口」一节）：
// 三条实现的 allocs/op **完全相同**——那一次分配是返回的字符串本身，
// `hex.EncodeToString` 的中间 buffer 被编译器证明留在栈上，手写 nibble 也省不
// 出来。换 `math/rand/v2` 只省约 50 ns（占默认请求路径 ns 的 ~2.7%），却要放
// 弃「不可预测」，而那是 W3C 对 random-trace-id 的语义要求。
//
// 把**被否掉的方案**留在基准里，是为了下次有人再提「换个更快的随机源」时先看
// 这张表，而不是重新推一遍。
//
// 跑法（N 钉死，避免默认窗口让 N 随机器快慢漂）：
//
//	go test -run '^$' -bench BenchmarkGenerateTraceID -benchtime=200000x -count=5 .
//
// 关注 allocs/op；ns 只做量级参考。
func BenchmarkGenerateTraceID(b *testing.B) {
	b.Run("crypto+EncodeToString", func(b *testing.B) { benchTraceID(b, generateTraceID) })
	b.Run("crypto+manualHex", func(b *testing.B) { benchTraceID(b, traceIDCryptoManualHex) })
	b.Run("mathrand+manualHex-rejected", func(b *testing.B) { benchTraceID(b, traceIDMathRandManualHex) })
}

func benchTraceID(b *testing.B, fn func() string) {
	b.ReportAllocs()
	var s string
	for i := 0; i < b.N; i++ {
		s = fn()
	}
	traceIDSink = s // 防止整个调用被优化掉
}

var traceIDSink string

// traceIDCryptoManualHex 是「保留 crypto/rand、手工写十六进制」的对照实现
// （`hex.EncodeToString` 的替代），实测与现状同为 1 alloc / 32 B——所以现状
// 不用改。
func traceIDCryptoManualHex() string {
	var raw [16]byte
	if _, err := crand.Read(raw[:]); err != nil {
		return fallbackTraceID()
	}
	var out [32]byte
	const hexDigits = "0123456789abcdef"
	for i, v := range raw {
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out[:])
}

// traceIDMathRandManualHex 是**已否决**的备选源：更快，但不是密码学安全的随
// 机源（`math/rand/v2` 明文不用于安全用途），而 W3C 要求 trace-id 不可预测。
func traceIDMathRandManualHex() string {
	hi, lo := mrand.Uint64(), mrand.Uint64()
	var out [32]byte
	const hexDigits = "0123456789abcdef"
	for i := 0; i < 16; i++ {
		var v byte
		if i < 8 {
			v = byte(hi >> (56 - 8*i))
		} else {
			v = byte(lo >> (56 - 8*(i-8)))
		}
		out[i*2] = hexDigits[v>>4]
		out[i*2+1] = hexDigits[v&0x0f]
	}
	return string(out[:])
}

// fallbackTraceID 与 generateTraceID 的兜底同源（随机源不可用时）。
func fallbackTraceID() string { return fmt.Sprintf("%032x", time.Now().UnixNano()) }
