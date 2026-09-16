package otelweb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse/observability"
)

const (
	inboundTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	inboundSpan  = "00f067aa0ba902b7"
	inboundTP    = "00-" + inboundTrace + "-" + inboundSpan + "-01"
)

type harness struct {
	app  *web.Engine
	exp  *tracetest.InMemoryExporter
	sink *observability.MemorySink
}

// newHarness 起一个装了本适配件的真引擎（内存 exporter，同步导出）。
func newHarness(t *testing.T, opts ...web.Option) *harness {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exp),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	sink := &observability.MemorySink{}
	all := append([]web.Option{
		web.WithSink(sink),
		web.WithHostID("otel-test"),
		web.WithSpanHook(New(tp)),
	}, opts...)
	return &harness{app: web.New(all...), exp: exp, sink: sink}
}

func (h *harness) do(t *testing.T, method, target string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.app.ServeHTTP(rec, req)
	return rec
}

func (h *harness) span(t *testing.T) tracetest.SpanStub {
	t.Helper()
	stubs := h.exp.GetSpans()
	if len(stubs) == 0 {
		t.Fatal("没有导出任何 span")
	}
	return stubs[len(stubs)-1]
}

func attrMap(st tracetest.SpanStub) map[string]attribute.Value {
	out := map[string]attribute.Value{}
	for _, kv := range st.Attributes {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func httpReqRecord(t *testing.T, sink *observability.MemorySink) observability.Record {
	t.Helper()
	for _, r := range sink.Snapshot() {
		if r.Event == "http.request" {
			return r
		}
	}
	t.Fatal("没找到 http.request 记录")
	return observability.Record{}
}

// TestServerSpanFromRequest：一次请求 → 一个 semconv 形状的 server span。
func TestServerSpanFromRequest(t *testing.T) {
	h := newHarness(t)
	h.app.GET("/users/{id}", func(c *web.Ctx) error { return c.Text(http.StatusOK, "ok") })
	h.do(t, "GET", "/users/42")

	st := h.span(t)
	if st.Name != "GET /users/{id}" {
		t.Errorf("span 名 = %q，want %q（{method} {http.route}，不得用 URI 路径）", st.Name, "GET /users/{id}")
	}
	if st.SpanKind != trace.SpanKindServer {
		t.Errorf("span kind = %v，want SpanKindServer", st.SpanKind)
	}
	if st.Parent.IsValid() {
		t.Errorf("无入站头时不应有父 span，got %v", st.Parent)
	}
	if st.Status.Code != codes.Unset {
		t.Errorf("200 的 span status = %v，want Unset", st.Status.Code)
	}
	if !st.StartTime.Before(st.EndTime) && !st.StartTime.Equal(st.EndTime) {
		t.Errorf("时间区间不合法：%v → %v", st.StartTime, st.EndTime)
	}

	attrs := attrMap(st)
	for k, want := range map[string]string{
		"http.request.method": "GET",
		"http.route":          "/users/{id}",
		"url.path":            "/users/42",
	} {
		if got := attrs[k].AsString(); got != want {
			t.Errorf("属性 %s = %q，want %q", k, got, want)
		}
	}
	if got := attrs["http.response.status_code"].AsInt64(); got != http.StatusOK {
		t.Errorf("http.response.status_code = %d，want 200", got)
	}
	if _, ok := attrs["client.address"]; !ok {
		t.Error("缺少 client.address 属性（框架与日志共用同一份字段）")
	}

	// trace-id 三方一致：导出的 span、响应头 X-Trace-Id、访问记录。
	rec := httpReqRecord(t, h.sink)
	if st.SpanContext.TraceID().String() != rec.TraceID {
		t.Errorf("span trace-id = %s，记录 trace-id = %s", st.SpanContext.TraceID(), rec.TraceID)
	}
}

// TestServerSpanAdoptsInboundParent：入站 traceparent 决定 trace-id 与父 span。
func TestServerSpanAdoptsInboundParent(t *testing.T) {
	h := newHarness(t)
	h.app.GET("/t", func(c *web.Ctx) error { return c.Text(http.StatusOK, "ok") })
	h.do(t, "GET", "/t", "Traceparent", inboundTP, "Tracestate", "vendor=opaque")

	st := h.span(t)
	if got := st.SpanContext.TraceID().String(); got != inboundTrace {
		t.Errorf("trace-id = %s，want 沿用入站 %s", got, inboundTrace)
	}
	if got := st.Parent.SpanID().String(); got != inboundSpan {
		t.Errorf("parent span-id = %s，want 入站 %s", got, inboundSpan)
	}
	if st.SpanContext.SpanID().String() == inboundSpan {
		t.Error("本请求 span-id 不能等于入站 span-id")
	}
	if !st.SpanContext.TraceFlags().IsSampled() {
		t.Error("入站 sampled=1 必须被继承（否则下游丢采样）")
	}
	if st.SpanContext.TraceState().String() != "vendor=opaque" {
		t.Errorf("tracestate 未透传：%q", st.SpanContext.TraceState().String())
	}
}

// TestStatusSemantics：5xx → Error、4xx/2xx → Unset，且 error.type 的口径明确
// （框架给了就用框架的，没给而 ≥500 时补状态码字符串）。
func TestStatusSemantics(t *testing.T) {
	cases := []struct {
		name      string
		handler   web.Handler
		wantCode  codes.Code
		wantError string // 期望的 error.type；空 = 不应有该属性
	}{
		{
			name:      "5xx 且框架没有 error.type → 补状态码",
			handler:   func(c *web.Ctx) error { return c.Text(http.StatusServiceUnavailable, "down") },
			wantCode:  codes.Error,
			wantError: "503",
		},
		{
			name:      "panic → 500 + 框架的 error.type 优先",
			handler:   func(c *web.Ctx) error { panic("boom") },
			wantCode:  codes.Error,
			wantError: "panic",
		},
		{
			name:     "4xx → Unset",
			handler:  func(c *web.Ctx) error { return web.NotFound("nope", nil) },
			wantCode: codes.Unset,
		},
		{
			name:     "2xx → Unset",
			handler:  func(c *web.Ctx) error { return c.Text(http.StatusOK, "ok") },
			wantCode: codes.Unset,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.app.GET("/t", c.handler)
			h.do(t, "GET", "/t")

			st := h.span(t)
			if st.Status.Code != c.wantCode {
				t.Errorf("status = %v，want %v", st.Status.Code, c.wantCode)
			}
			if c.wantCode == codes.Error && st.Status.Description != "" {
				t.Errorf("5xx 的 status description 应为空（semconv），got %q", st.Status.Description)
			}
			got, has := attrMap(st)["error.type"]
			switch {
			case c.wantError == "" && has:
				t.Errorf("不应有 error.type，got %q", got.AsString())
			case c.wantError != "" && !has:
				t.Errorf("缺少 error.type，want %q", c.wantError)
			case c.wantError != "" && got.AsString() != c.wantError:
				t.Errorf("error.type = %q，want %q", got.AsString(), c.wantError)
			}
		})
	}
}

// TestDownstreamPropagationUsesServerSpanID：请求内部往下游发请求时，注入出的
// traceparent 的 parent-id 必须是**本请求 span 的 id** —— 这是「接得进去」的
// 核心：下游 span 挂在这条链路上，而不是挂在一条不存在的父 span 上。
//
// 这也是「主模块不引 SDK 会不会断层」的实测答案：span context 在 Begin 里就
// 注入了请求 context，handler 用标准 context 就能拿到。
func TestDownstreamPropagationUsesServerSpanID(t *testing.T) {
	h := newHarness(t)
	var injected string
	h.app.GET("/t", func(c *web.Ctx) error {
		carrier := propagation.HeaderCarrier{}
		propagation.TraceContext{}.Inject(c.Request().Context(), carrier)
		injected = carrier.Get("traceparent")
		return c.Text(http.StatusOK, "ok")
	})
	h.do(t, "GET", "/t", "Traceparent", inboundTP)

	st := h.span(t)
	want := "00-" + st.SpanContext.TraceID().String() + "-" + st.SpanContext.SpanID().String() + "-01"
	if injected != want {
		t.Errorf("下游注入的 traceparent = %q，want %q", injected, want)
	}
	if !strings.Contains(injected, st.SpanContext.SpanID().String()) {
		t.Error("注入的 parent-id 不是本请求 span 的 id —— 下游会挂到不存在的父 span 上")
	}
}

// TestAccessRecordCarriesSpanID：日志能与 trace 对上（记录里的 span.id 就是
// 导出 span 的 id）。
func TestAccessRecordCarriesSpanID(t *testing.T) {
	h := newHarness(t)
	h.app.GET("/t", func(c *web.Ctx) error { return c.Text(http.StatusOK, "ok") })
	h.do(t, "GET", "/t")

	st := h.span(t)
	rec := httpReqRecord(t, h.sink)
	var spanID string
	rec.Attrs.Range(func(k string, v any) {
		if k == "span.id" {
			spanID, _ = v.(string)
		}
	})
	if spanID != st.SpanContext.SpanID().String() {
		t.Errorf("记录里的 span.id = %q，导出 span 的 id = %q", spanID, st.SpanContext.SpanID())
	}
}

// TestParsingAgreesWithOfficialPropagator：框架的入站解析与官方 propagator
// 必须对同一批输入给出一致结论——否则同一个请求在「框架记录」与「OTel 链路」
// 里会拿到不同的 trace 身份。差分对照逐条断言，不靠阅读。
func TestParsingAgreesWithOfficialPropagator(t *testing.T) {
	corpus := []string{
		"", // 没有头
		inboundTP,
		"00-" + inboundTrace + "-" + inboundSpan + "-00",
		"00-" + inboundTrace + "-" + inboundSpan + "-02",
		"00-" + inboundTrace + "-" + inboundSpan + "-03",
		"01-" + inboundTrace + "-" + inboundSpan + "-01", // 更高版本
		"00-" + strings.Repeat("0", 32) + "-" + inboundSpan + "-01",
		"00-" + inboundTrace + "-" + strings.Repeat("0", 16) + "-01",
		"00-" + strings.ToUpper(inboundTrace) + "-" + inboundSpan + "-01",
		"ff-" + inboundTrace + "-" + inboundSpan + "-01",
		"00-tooshort-xyz-01",
		"00-" + inboundTrace + "-" + inboundSpan + "-01-extra",
		"00-" + inboundTrace + "-" + inboundSpan + "-04", // 保留 flag 位
		"00-" + inboundTrace + "-" + inboundSpan,         // 缺 flags 段
		"00-" + inboundTrace + "-" + inboundSpan + "_01", // 分隔符错
		"01-" + inboundTrace + "-" + inboundSpan + "-01-extra",
	}

	for _, hdr := range corpus {
		name := hdr
		if name == "" {
			name = "(空)"
		}
		t.Run(name, func(t *testing.T) {
			// 官方实现的口径（真实 propagator，不是我们的复述）。
			carrier := propagation.HeaderCarrier{}
			if hdr != "" {
				carrier.Set("traceparent", hdr)
			}
			official := trace.SpanContextFromContext(
				propagation.TraceContext{}.Extract(context.Background(), carrier),
			)

			// 框架的口径（跑一次真请求，看 span 有没有采纳入站身份）。
			h := newHarness(t)
			h.app.GET("/t", func(c *web.Ctx) error { return c.Text(http.StatusOK, "ok") })
			var hdrs []string
			if hdr != "" {
				hdrs = []string{"Traceparent", hdr}
			}
			h.do(t, "GET", "/t", hdrs...)
			st := h.span(t)

			adopted := st.Parent.SpanID().String() == inboundSpan &&
				st.SpanContext.TraceID().String() == inboundTrace

			if adopted != official.IsValid() {
				t.Fatalf("两边结论不一致：官方认为有效=%v，框架采纳=%v", official.IsValid(), adopted)
			}
			if official.IsValid() {
				if official.TraceID().String() != st.SpanContext.TraceID().String() {
					t.Errorf("trace-id 不一致：官方 %s，框架 %s", official.TraceID(), st.SpanContext.TraceID())
				}
				if official.SpanID().String() != st.Parent.SpanID().String() {
					t.Errorf("parent-id 不一致：官方 %s，框架 %s", official.SpanID(), st.Parent.SpanID())
				}
			}
		})
	}
}

// okHandler 是占位 handler：200 + 一个短响应体。
func okHandler(c *web.Ctx) error { return c.Text(http.StatusOK, "ok") }

// TestSpanNameNeverUsesURIPath 钉住 span 名只有两种合法形态：`{method} {route}`
// 与 `{method}`——**永不**包含 URI 路径。
//
// 这不是洁癖：404 场景下每个打错的路径都会变成独立的 span 名，APM 的索引基数会
// 直接爆掉。semconv 原文是 instrumentation MUST NOT default to using URI path，
// 所以这里逐条比**完整名字**，而不是只查「含不含斜杠」——注册过的路由模板本来就
// 含斜杠，查斜杠等于没查。
func TestSpanNameNeverUsesURIPath(t *testing.T) {
	h := newHarness(t)
	h.app.GET("/users/{id}", okHandler)
	h.app.GET("/healthz", okHandler)

	cases := []struct {
		name, target, want string
	}{
		{"matched-param", "/users/42", "GET /users/{id}"},
		{"matched-static", "/healthz", "GET /healthz"},
		{"unmatched-typo", "/random-typo-path", "GET"},
		{"unmatched-under-known", "/users/42/extra", "GET"},
		{"unmatched-deep", "/a/very/long/typo/path", "GET"},
		{"unmatched-with-query", "/typo?x=1", "GET"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(h.exp.GetSpans())
			h.do(t, "GET", c.target)
			stubs := h.exp.GetSpans()
			if len(stubs) != before+1 {
				t.Fatalf("导出 span 数 = %d，want %d", len(stubs), before+1)
			}
			if got := stubs[before].Name; got != c.want {
				t.Errorf("span 名 = %q，want %q（URI 路径不得进名字）", got, c.want)
			}
		})
	}
}

// TestMethodNormalizedPerSemconv 钉住方法归一：`http.request.method` 只写 semconv
// 的允许值（已知方法大写，不认识的写 `_OTHER`），原始值另放
// `http.request.method_original`；span 名里的 `{method}` 对未知方法退化为 `HTTP`。
//
// 口径与官方 otelhttp 的 standardizeHTTPMethod / HTTPServer.method 逐条对齐，
// 含「大小写不同但认识」这一档（`get` → `GET` + original `get`）。
func TestMethodNormalizedPerSemconv(t *testing.T) {
	cases := []struct {
		method, wantAttr, wantName, wantOriginal string
	}{
		{"GET", "GET", "GET /t", ""},
		{"get", "GET", "GET /t", "get"},
		{"FOO", "_OTHER", "HTTP /t", "FOO"},
		{"PURGE", "_OTHER", "HTTP /t", "PURGE"},
	}
	for _, c := range cases {
		t.Run(c.method, func(t *testing.T) {
			h := newHarness(t)
			h.app.Handle("/t", okHandler)
			h.do(t, c.method, "/t")

			st := h.span(t)
			attrs := attrMap(st)
			if got := attrs["http.request.method"].AsString(); got != c.wantAttr {
				t.Errorf("http.request.method = %q，want %q", got, c.wantAttr)
			}
			if got := attrs["http.request.method_original"].AsString(); got != c.wantOriginal {
				t.Errorf("http.request.method_original = %q，want %q", got, c.wantOriginal)
			}
			if st.Name != c.wantName {
				t.Errorf("span 名 = %q，want %q", st.Name, c.wantName)
			}
		})
	}
}

// TestRecordKeepsRawMethod 把「记录侧与 span 侧取值口径不同」这条设计选择钉住：
// 框架的日志流保留原始方法（`purge` 就写 `purge`，不归一、不大写），span 侧按
// semconv 归一。免得日后有人把它当 bug「顺手修掉」——修掉就丢了日志里那个可读的
// 原始值。用**小写**方法是为了让这条断言有分辨力：任何归一（大写或 `_OTHER`）
// 都会让它红。
func TestRecordKeepsRawMethod(t *testing.T) {
	h := newHarness(t)
	h.app.Handle("/t", okHandler)
	h.do(t, "purge", "/t")

	rec := httpReqRecord(t, h.sink)
	var raw string
	rec.Attrs.Range(func(k string, v any) {
		if k == "http.request.method" {
			raw, _ = v.(string)
		}
	})
	if raw != "purge" {
		t.Errorf("记录侧 http.request.method = %q，want %q（日志保留原始值）", raw, "purge")
	}
}

// TestContextChainSurvivesEdgePaths 端到端验证「Begin 注入的 context 一直活到
// End」——对应评审提的「c.r 被换回原始 req 就断链」。
//
// 判据不是「c.r 指向谁」（那是实现细节），而是可观测结果：End 靠 c.r.Context()
// 取回 Begin 建的 span，链一断就取不回，span 既不会结束也导不出来。所以这里对
// 每条边角路径断言「恰好导出一个**已结束**的 span，且记录里的 span.id 与它一致」。
func TestContextChainSurvivesEdgePaths(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(h *harness, t *testing.T)
		method     string
		target     string
		wantStatus int
		wantName   string
	}{
		{"routed", func(h *harness, _ *testing.T) { h.app.GET("/users/{id}", okHandler) },
			"GET", "/users/42", 200, "GET /users/{id}"},
		{"unmatched-404", func(h *harness, _ *testing.T) { h.app.GET("/known", okHandler) },
			"GET", "/typo", 404, "GET"},
		{"panic", func(h *harness, _ *testing.T) {
			h.app.GET("/p", func(*web.Ctx) error { panic("boom") })
		}, "GET", "/p", 500, "GET /p"},
		{"error-returned", func(h *harness, _ *testing.T) {
			h.app.GET("/e", func(*web.Ctx) error { return web.NotFound("user", nil) })
		}, "GET", "/e", 404, "GET /e"},
		{"stdlib-wrap", func(h *harness, _ *testing.T) {
			h.app.Handle("/w", web.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
			})))
		}, "PUT", "/w", 201, "PUT /w"},
		{"middleware", func(h *harness, t *testing.T) {
			h.app.Use(func(c *web.Ctx, next web.Handler) error {
				if c.SpanID() == "" {
					t.Error("中间件里读不到 span-id——注入没往链上走")
				}
				return next(c)
			})
			h.app.GET("/m", okHandler)
		}, "GET", "/m", 200, "GET /m"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			c.setup(h, t)
			rec := h.do(t, c.method, c.target)

			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d，want %d", rec.Code, c.wantStatus)
			}
			stubs := h.exp.GetSpans()
			if len(stubs) != 1 {
				t.Fatalf("导出 span 数 = %d，want 1（context 断链时 End 取不回 span，就会是 0）", len(stubs))
			}
			st := stubs[0]
			if st.Name != c.wantName {
				t.Errorf("span 名 = %q，want %q", st.Name, c.wantName)
			}
			if st.EndTime.IsZero() {
				t.Error("span 没有结束时间")
			}
			if !st.SpanContext.IsValid() {
				t.Error("span context 非法")
			}

			rec2 := httpReqRecord(t, h.sink)
			if rec2.TraceID != st.SpanContext.TraceID().String() {
				t.Errorf("记录 trace-id = %s，span trace-id = %s", rec2.TraceID, st.SpanContext.TraceID())
			}
			var recSpanID string
			rec2.Attrs.Range(func(k string, v any) {
				if k == "span.id" {
					recSpanID, _ = v.(string)
				}
			})
			if recSpanID != st.SpanContext.SpanID().String() {
				t.Errorf("记录 span.id = %q，导出 span-id = %s", recSpanID, st.SpanContext.SpanID())
			}
		})
	}
}

// TestConcurrentRequestsDoNotCrosstalk 并发下每个 span / 记录必须是自己的那一份：
// 属性如果共用缓冲或被池化复用，路径就会串台。配合 `-race` 一起跑。
func TestConcurrentRequestsDoNotCrosstalk(t *testing.T) {
	h := newHarness(t)
	h.app.GET("/users/{id}", okHandler)

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", fmt.Sprintf("/users/%d", i), nil)
			h.app.ServeHTTP(httptest.NewRecorder(), req)
		}(i)
	}
	wg.Wait()

	stubs := h.exp.GetSpans()
	if len(stubs) != n {
		t.Fatalf("导出 span 数 = %d，want %d", len(stubs), n)
	}
	seen := make(map[string]bool, n)
	for _, st := range stubs {
		p := attrMap(st)["url.path"].AsString()
		if p == "" {
			t.Fatal("有 span 缺 url.path")
		}
		if seen[p] {
			t.Errorf("url.path %q 重复出现——span 属性串台", p)
		}
		seen[p] = true
		if st.EndTime.IsZero() {
			t.Errorf("span %s 没有结束", st.SpanContext.SpanID())
		}
	}

	recSeen := make(map[string]bool, n)
	records := 0
	for _, r := range h.sink.Snapshot() {
		if r.Event != "http.request" {
			continue
		}
		records++
		r.Attrs.Range(func(k string, v any) {
			if k != "url.path" {
				return
			}
			p, _ := v.(string)
			if recSeen[p] {
				t.Errorf("记录里 url.path %q 重复——记录属性串台", p)
			}
			recSeen[p] = true
		})
	}
	if records != n {
		t.Errorf("http.request 记录数 = %d，want %d", records, n)
	}
}

// nopSink 让基准只量请求路径，不把出口渲染算进来。
type nopSink struct{}

func (nopSink) Write(observability.Record) {}

// BenchmarkConvertAttrs 只量「框架属性 → OTel 属性」这一步（不含 SDK），
// 用来把适配件自身的转换代价与 SDK 的 span 机制开销拆开归因。
func BenchmarkConvertAttrs(b *testing.B) {
	var attrs observability.Attrs
	observability.Set(&attrs, "http.request.method", "GET")
	observability.Set(&attrs, "http.route", "/users/{id}")
	observability.Set(&attrs, "url.path", "/users/42")
	observability.Set(&attrs, "http.response.body.size", int64(2))
	observability.Set(&attrs, "client.address", "127.0.0.1:54321")
	observability.Set(&attrs, "error.type", "panic")

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if kvs, _ := convertAttrs(&attrs, true); len(kvs) != 6 {
			b.Fatalf("转换结果 %d 条，want 6", len(kvs))
		}
	}
}

// BenchmarkRequestWithSpan 量适配件在请求路径上的开销（评审关注点：
// 「影子数据结构 + 转换」会不会吃掉 ConsoleSink 的零分配红利）。
//
// 口径：allocs/op 是整数、跨轮稳定，先看它；ns/op 只做量级参考。
func BenchmarkRequestWithSpan(b *testing.B) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	app := web.New(
		web.WithSink(nopSink{}),
		web.WithHostID("bench"),
		web.WithSpanHook(New(tp)),
	)
	app.GET("/users/{id}", func(c *web.Ctx) error { return c.Text(http.StatusOK, "ok") })

	req := httptest.NewRequest("GET", "/users/42", nil)
	rec := httptest.NewRecorder()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		exp.Reset()
		app.ServeHTTP(rec, req)
	}
}

// BenchmarkRequestFrameworkOnly 是同一条请求路径**不装 span 出口**的对照，
// 两者的差就是适配层 + SDK 的全部代价。
func BenchmarkRequestFrameworkOnly(b *testing.B) {
	app := web.New(
		web.WithSink(nopSink{}),
		web.WithHostID("bench"),
	)
	app.GET("/users/{id}", func(c *web.Ctx) error { return c.Text(http.StatusOK, "ok") })

	req := httptest.NewRequest("GET", "/users/42", nil)
	rec := httptest.NewRecorder()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		app.ServeHTTP(rec, req)
	}
}
