package web

import (
	"net/http"
	"testing"

	"github.com/Luo-root/pulse/observability"
)

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
