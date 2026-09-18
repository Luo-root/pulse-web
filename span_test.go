package web

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Luo-root/pulse/observability"
)

// 本文件覆盖 span.go：span 身份的来源（hook 与入站 traceparent）、两个响应头的分工，
// 以及 Detach 出来的后台任务带走什么。
// 边界：没挂 span hook 时的 trace-id 产生与入站解析归 observe_test.go。

// testSpanID 是测试 hook 分配的 span-id（真实适配层用 SDK 分配的，这里固定值
// 便于断言「框架用的是 hook 给的 id」）。
const testSpanID = "0123456789abcdef"

// recordingHook 是测试用的 SpanHook：记下框架交来的每一份数据，并在 Begin 里
// 模拟追踪体系的行为（沿用入站的 trace-id、分配自己的 span-id）。
type recordingHook struct {
	infos  []SpanInfo
	refs   []SpanRef
	spans  []Span
	spanID string

	// traceID 非空时用它替换框架给的 trace-id（模拟「追踪体系采纳了别的链路头」）。
	traceID string
	// inject 非 nil 时，Begin 把它塞进返回的 context —— 验证注入真的交到
	// 中间件与 handler 手上（下游 OTel 库自动串联的前提）。
	inject any
}

type injectKey struct{}

func (h *recordingHook) Begin(ctx context.Context, in SpanInfo) (context.Context, SpanRef) {
	h.infos = append(h.infos, in)
	ref := SpanRef{TraceID: in.TraceID, SpanID: testSpanID, Sampled: in.Sampled, Random: in.Random}
	if h.spanID != "" {
		ref.SpanID = h.spanID
	}
	if h.traceID != "" {
		ref.TraceID = h.traceID
	}
	h.refs = append(h.refs, ref)
	if h.inject != nil {
		return context.WithValue(ctx, injectKey{}, h.inject), ref
	}
	return ctx, ref
}

func (h *recordingHook) End(_ context.Context, sp Span) { h.spans = append(h.spans, sp) }

func (h *recordingHook) begin(t *testing.T) SpanInfo {
	t.Helper()
	if len(h.infos) != 1 {
		t.Fatalf("Begin 调用次数 = %d，want 1", len(h.infos))
	}
	return h.infos[0]
}

func (h *recordingHook) last(t *testing.T) Span {
	t.Helper()
	if len(h.spans) != 1 {
		t.Fatalf("End 调用次数 = %d，want 1", len(h.spans))
	}
	return h.spans[0]
}

func isHexLowerN(s string, n int) bool { return len(s) == n && isHexLower(s) }

// TestSpanHookCarriesRequestFacts：End 拿到的必须是与访问日志同源的请求事实
// ——状态码是映射后的、路由是模板而不是实际路径、属性与日志同一份。
func TestSpanHookCarriesRequestFacts(t *testing.T) {
	h := &recordingHook{}
	e, sink := newTestEngine(t, WithSpanHook(h))
	e.GET("/users/{id}", func(c *Ctx) error { return c.Text(http.StatusOK, "user:"+c.Path("id")) })

	rec := doReq(e, "GET", "/users/42", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	in, sp := h.begin(t), h.last(t)
	if !isHexLowerN(in.TraceID, 32) {
		t.Errorf("自启 trace 的 TraceID = %q，want 32hex 小写", in.TraceID)
	}
	if in.ParentID != "" {
		t.Errorf("无入站 traceparent 时 ParentID 应为空（root span），got %q", in.ParentID)
	}
	if sp.TraceID != in.TraceID {
		t.Errorf("End 的 TraceID = %q，want 与 Begin 一致（%q）", sp.TraceID, in.TraceID)
	}
	if sp.SpanID != h.refs[0].SpanID {
		t.Errorf("End 的 SpanID = %q，want 采用 hook 给的 %q", sp.SpanID, h.refs[0].SpanID)
	}
	if sp.Method != "GET" {
		t.Errorf("Method = %q", sp.Method)
	}
	if sp.Route != "/users/{id}" {
		t.Errorf("Route = %q，want 路由模板（不是实际路径 /users/42）", sp.Route)
	}
	if sp.Path != "/users/42" {
		t.Errorf("Path = %q", sp.Path)
	}
	if sp.Status != http.StatusOK {
		t.Errorf("Status = %d", sp.Status)
	}
	if sp.Start.IsZero() || sp.End.Before(sp.Start) {
		t.Errorf("时间戳不合法：start=%v end=%v", sp.Start, sp.End)
	}
	if sp.Err != nil {
		t.Errorf("正常请求 Err 应为 nil，got %v", sp.Err)
	}
	if sp.Attrs == nil {
		t.Fatal("Attrs 为 nil")
	}
	attrs := attrsOf(observability.Record{Attrs: *sp.Attrs})
	for k, want := range map[string]any{
		attrHTTPMethod: "GET",
		attrHTTPRoute:  "/users/{id}",
		attrURLPath:    "/users/42",
	} {
		if attrs[k] != want {
			t.Errorf("span 属性 %s = %v，want %v", k, attrs[k], want)
		}
	}
	if _, ok := attrs[attrClientAddr]; !ok {
		t.Errorf("span 属性缺少 %s", attrClientAddr)
	}
	// span 属性里**不含** span.id：那是记录侧的关联键。
	if _, ok := attrs[attrSpanID]; ok {
		t.Errorf("span 属性不应包含 %s（它只进记录）", attrSpanID)
	}

	// 记录侧：span.id 有条件出现，且与 span 的 id 一致。
	// （sink 里还有 Bootstrap 的装配记录，按事件名取访问记录。）
	var logs []observability.Record
	for _, r := range sink.Snapshot() {
		if r.Event == eventHTTPReq {
			logs = append(logs, r)
		}
	}
	if len(logs) != 1 {
		t.Fatalf("访问记录条数 = %d，want 1", len(logs))
	}
	if got := attrsOf(logs[0])[attrSpanID]; got != sp.SpanID {
		t.Errorf("记录里的 %s = %v，want %q", attrSpanID, got, sp.SpanID)
	}
	if logs[0].TraceID != sp.TraceID {
		t.Errorf("记录 TraceID = %q，span TraceID = %q", logs[0].TraceID, sp.TraceID)
	}
}

// TestSpanHookInjectionReachesHandler：Begin 返回的 context 必须真的交给
// 中间件与 handler（这是「下游 OTel 库自动串联」的机制本身，不是约定）。
func TestSpanHookInjectionReachesHandler(t *testing.T) {
	h := &recordingHook{inject: "otel-span-context"}
	e, _ := newTestEngine(t, WithSpanHook(h))

	var seen, seenFromMW any
	e.Use(func(c *Ctx, next Handler) error {
		seenFromMW = c.Request().Context().Value(injectKey{})
		return next(c)
	})
	e.GET("/t", func(c *Ctx) error {
		seen = c.Request().Context().Value(injectKey{})
		return c.Text(http.StatusNoContent, "")
	})
	doReq(e, "GET", "/t", nil)

	if seen != "otel-span-context" {
		t.Errorf("handler 没看到注入值：%v", seen)
	}
	if seenFromMW != "otel-span-context" {
		t.Errorf("中间件没看到注入值：%v", seenFromMW)
	}
}

// TestSpanInjectionOnUnmatchedRoute：没匹配到路由（ServeMux 直接 404）时，
// 注入的 context 也必须走到收尾——End 顺着它取回 span。
func TestSpanInjectionOnUnmatchedRoute(t *testing.T) {
	// ctxProbeHook 在 End 里检查「请求 context 里还有没有 Begin 注入的标记」。
	type key struct{}
	h := &probeHook{key: key{}}
	e, _ := newTestEngine(t, WithSpanHook(h))
	e.GET("/known", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })

	rec := doReq(e, "GET", "/unknown-path", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d，want 404", rec.Code)
	}
	if !h.endSawMarker {
		t.Error("404 请求的 End 里看不到 Begin 注入的 context —— 请求 context 链断了")
	}
	if h.endStatus != http.StatusNotFound {
		t.Errorf("End.Status = %d，want 404", h.endStatus)
	}
}

type probeHook struct {
	key          any
	endSawMarker bool
	endStatus    int
}

func (h *probeHook) Begin(ctx context.Context, _ SpanInfo) (context.Context, SpanRef) {
	return context.WithValue(ctx, h.key, true), SpanRef{TraceID: generateTraceID(), SpanID: testSpanID}
}

func (h *probeHook) End(ctx context.Context, sp Span) {
	v, _ := ctx.Value(h.key).(bool)
	h.endSawMarker = v
	h.endStatus = sp.Status
}

// TestSpanTwoResponseHeaders：X-Trace-Id 与 Server-Timing 是两个不同的东西，
// 同时存在时必须各自正确（用户点名的区分点，改动此处请同步 README / 设计文档）。
func TestSpanTwoResponseHeaders(t *testing.T) {
	t.Run("装了 hook", func(t *testing.T) {
		h := &recordingHook{}
		e, _ := newTestEngine(t, WithSpanHook(h))
		e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusNoContent, "") })
		rec := doReq(e, "GET", "/t", nil)
		ref, sp := h.refs[0], h.last(t)

		if got := rec.Header().Get("X-Trace-Id"); got != ref.TraceID {
			t.Errorf("X-Trace-Id = %q，want trace-id %q", got, ref.TraceID)
		}
		want := "trace;desc=00-" + ref.TraceID + "-" + ref.SpanID + "-" + flagsHex(ref.Sampled, ref.Random)
		if got := rec.Header().Get(serverTimingHeader); got != want {
			t.Errorf("Server-Timing = %q，want %q", got, want)
		}
		// 自启 trace：sampled 与 random 都置位（理由见 resolveSpanInfo 的 godoc）。
		if !ref.Sampled || !ref.Random {
			t.Errorf("自启 trace 的 flags = sampled:%v random:%v，want 两个都 true", ref.Sampled, ref.Random)
		}
		if sp.SpanID != ref.SpanID {
			t.Errorf("End 的 SpanID = %q，want %q", sp.SpanID, ref.SpanID)
		}
	})

	t.Run("没装 hook", func(t *testing.T) {
		e, _ := newTestEngine(t)
		e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, c.SpanID()) })
		rec := doReq(e, "GET", "/t", nil)

		if got := rec.Header().Get(serverTimingHeader); got != "" {
			t.Errorf("没装 hook 不应写 %s，got %q（没有 span-id 可写）", serverTimingHeader, got)
		}
		if got := rec.Header().Get("X-Trace-Id"); len(got) != 32 {
			t.Errorf("X-Trace-Id = %q，want 32hex（既有契约不受影响）", got)
		}
		if got := rec.Body.String(); got != "" {
			t.Errorf("Ctx.SpanID() = %q，没装 hook 时必须是空串", got)
		}
	})
}

// TestSpanAdoptsHookIdentity：追踪体系是身份的事实源——hook 换掉的 trace-id
// 必须被框架采用（记录与响应头都跟着走），否则日志与链路会对不上。
func TestSpanAdoptsHookIdentity(t *testing.T) {
	const adopted = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h := &recordingHook{traceID: adopted, spanID: "bbbbbbbbbbbbbbbb"}
	e, sink := newTestEngine(t, WithSpanHook(h))
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusNoContent, "") })
	rec := doReq(e, "GET", "/t", nil)

	if got := rec.Header().Get("X-Trace-Id"); got != adopted {
		t.Errorf("X-Trace-Id = %q，want 采用 hook 的 %q", got, adopted)
	}
	if got := rec.Header().Get(serverTimingHeader); !strings.Contains(got, adopted) {
		t.Errorf("Server-Timing = %q，want 含 hook 的 trace-id", got)
	}
	for _, r := range sink.Snapshot() {
		if r.Event == eventHTTPReq && r.TraceID != adopted {
			t.Errorf("记录 TraceID = %q，want %q", r.TraceID, adopted)
		}
	}
}

// TestSpanParentFromInboundTraceparent：W3C 入站头的四个字段都要交给 hook
// ——trace-id 沿用、span-id 作为本请求的父、flags 继承、tracestate 透传。
func TestSpanParentFromInboundTraceparent(t *testing.T) {
	const (
		traceID  = "4bf92f3577b34da6a3ce929d0e0e4736"
		parentID = "00f067aa0ba902b7"
	)
	h := &recordingHook{}
	e, _ := newTestEngine(t, WithSpanHook(h))
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusNoContent, "") })

	rec := doReq(e, "GET", "/t", nil,
		"Traceparent", "00-"+traceID+"-"+parentID+"-01",
		"Tracestate", "vendor=opaque")

	in := h.begin(t)
	if in.TraceID != traceID {
		t.Errorf("TraceID = %q，want 沿入站 %q", in.TraceID, traceID)
	}
	if in.ParentID != parentID {
		t.Errorf("ParentID = %q，want 入站 span-id %q", in.ParentID, parentID)
	}
	if !in.Sampled || in.Random {
		t.Errorf("flags 继承错：sampled=%v random=%v（入站是 01）", in.Sampled, in.Random)
	}
	if in.TraceState != "vendor=opaque" {
		t.Errorf("TraceState = %q，want 原样透传", in.TraceState)
	}
	if ref := h.refs[0]; ref.SpanID == parentID {
		t.Error("本请求 span-id 不能等于入站 span-id（W3C 要求新生成）")
	}
	if got := rec.Header().Get(serverTimingHeader); !strings.HasSuffix(got, "-01") {
		t.Errorf("Server-Timing 的 flags 应回写入站的 01，got %q", got)
	}
}

// TestTraceparentStrictParsing：traceparent 的任何一处不合法都必须**整条忽略**
// （起新 trace），不做部分采纳——规则与官方 otel-go 的 propagator 对齐，
// 否则同一个请求在「框架记录」与「宿主 OTel 链路」里会拿到不同的 trace 身份。
func TestTraceparentStrictParsing(t *testing.T) {
	const (
		tid = "4bf92f3577b34da6a3ce929d0e0e4736"
		pid = "00f067aa0ba902b7"
	)
	valid := []struct {
		name   string
		header string
	}{
		{"标准头", "00-" + tid + "-" + pid + "-01"},
		{"sampled 位为 0", "00-" + tid + "-" + pid + "-00"},
		{"random 位", "00-" + tid + "-" + pid + "-02"},
		{"更高版本按 00 格式解析", "01-" + tid + "-" + pid + "-01"},
	}
	invalid := []struct {
		name   string
		header string
	}{
		{"全零 trace-id", "00-" + zeroTraceID + "-" + pid + "-01"},
		{"全零 parent-id", "00-" + tid + "-" + zeroSpanID + "-01"},
		{"大写 hex", "00-" + strings.ToUpper(tid) + "-" + pid + "-01"},
		{"版本 ff", "ff-" + tid + "-" + pid + "-01"},
		{"短于 55 字符", "00-tooshort-xyz-01"},
		{"00 版本带额外字段", "00-" + tid + "-" + pid + "-01-extra"},
		{"00 版本带保留 flag 位", "00-" + tid + "-" + pid + "-04"},
		{"版本后缺分隔符", "00" + tid + "-" + pid + "-01"},
		{"flags 段缺分隔符", "00-" + tid + "-" + pid + "01"},
		{"空串", ""},
	}

	for _, c := range valid {
		t.Run("合法/"+c.name, func(t *testing.T) {
			h := &recordingHook{}
			e, _ := newTestEngine(t, WithSpanHook(h))
			e.GET("/t", func(ctx *Ctx) error { return ctx.Text(http.StatusNoContent, "") })
			doReq(e, "GET", "/t", nil, "Traceparent", c.header)

			in := h.begin(t)
			if in.TraceID != tid || in.ParentID != pid {
				t.Errorf("应当采纳入站身份，got trace=%q parent=%q", in.TraceID, in.ParentID)
			}
		})
	}

	for _, c := range invalid {
		t.Run("非法/"+c.name, func(t *testing.T) {
			h := &recordingHook{}
			e, _ := newTestEngine(t, WithSpanHook(h))
			e.GET("/t", func(ctx *Ctx) error { return ctx.Text(http.StatusNoContent, "") })
			doReq(e, "GET", "/t", nil,
				"Traceparent", c.header,
				"Tracestate", "vendor=should-not-survive")

			in := h.begin(t)
			if in.TraceID == tid {
				t.Errorf("非法头被部分采纳：trace-id 仍是入站的 %q", in.TraceID)
			}
			if !isHexLowerN(in.TraceID, 32) {
				t.Errorf("应当新起一条 trace，got TraceID=%q", in.TraceID)
			}
			if in.ParentID != "" {
				t.Errorf("非法头不应产生 parent，got %q", in.ParentID)
			}
			if in.TraceState != "" {
				t.Errorf("traceparent 非法时不得解析 tracestate（W3C），got %q", in.TraceState)
			}
		})
	}
}

// TestB3GivesTraceIDOnly：B3 是历史兼容路径，只给 trace-id，没有 parent。
func TestB3GivesTraceIDOnly(t *testing.T) {
	const b3 = "0af7651916cd43dd8448eb211c80319c"
	h := &recordingHook{}
	e, _ := newTestEngine(t, WithSpanHook(h))
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusNoContent, "") })
	doReq(e, "GET", "/t", nil, "X-B3-TraceId", b3)

	in := h.begin(t)
	if in.TraceID != b3 {
		t.Errorf("TraceID = %q，want %q", in.TraceID, b3)
	}
	if in.ParentID != "" {
		t.Errorf("B3 单头没有 span-id，ParentID 应为空，got %q", in.ParentID)
	}
}

// TestSpanOnPanicAndMappedErrors：span 的状态码是**映射后**的——panic 记 500、
// 业务错误按 mapper 的判定走，与访问日志同源。
func TestSpanOnPanicAndMappedErrors(t *testing.T) {
	cases := []struct {
		name   string
		h      Handler
		status int
	}{
		{"panic → 500", func(c *Ctx) error { panic("boom") }, http.StatusInternalServerError},
		{"NotFound", func(c *Ctx) error { return NotFound("no_such_thing", nil) }, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &recordingHook{}
			e, _ := newTestEngine(t, WithSpanHook(h), WithErrorHandler(defaultErrorHandler))
			e.GET("/t", c.h)
			doReq(e, "GET", "/t", nil)

			sp := h.last(t)
			if sp.Status != c.status {
				t.Errorf("span Status = %d，want %d（映射后的状态码）", sp.Status, c.status)
			}
			if sp.Err == nil {
				t.Error("失败请求的 Span.Err 不应为 nil（适配层据此决定 error.type）")
			}
		})
	}
}

// TestSpanHookNilPanics：装了却没有出口是配置错误，装配期就炸。
func TestSpanHookNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithSpanHook(nil) 应当 panic")
		}
	}()
	New(WithSpanHook(nil))
}

// TestSpanIDIsReadableInHandler：请求内可读到 span-id（后台任务要拿它建 link）。
func TestSpanIDIsReadableInHandler(t *testing.T) {
	h := &recordingHook{}
	e, _ := newTestEngine(t, WithSpanHook(h))
	var inHandler string
	e.GET("/t", func(c *Ctx) error {
		inHandler = c.SpanID()
		return c.Text(http.StatusNoContent, "")
	})
	doReq(e, "GET", "/t", nil)

	if inHandler == "" || inHandler != h.refs[0].SpanID {
		t.Errorf("handler 读到的 span-id = %q，hook 给的是 %q", inHandler, h.refs[0].SpanID)
	}
}

// noIdentityHook 模拟「hook 在、但这次请求没有 span」：给 trace-id、不给 span-id。
type noIdentityHook struct {
	began, ended int
}

func (h *noIdentityHook) Begin(ctx context.Context, in SpanInfo) (context.Context, SpanRef) {
	h.began++
	return ctx, SpanRef{TraceID: in.TraceID}
}

func (h *noIdentityHook) End(context.Context, Span) { h.ended++ }

// TestSpanNoIdentityWritesNoSpanID：hook 在、但没给 span 身份时，框架**不编造**
// ——不写 Server-Timing、记录里没有 span.id；X-Trace-Id 照旧（它是 trace 层的
// 既有契约，与有没有 span 无关）。
func TestSpanNoIdentityWritesNoSpanID(t *testing.T) {
	h := &noIdentityHook{}
	e, sink := newTestEngine(t, WithSpanHook(h))
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })

	rec := doReq(e, "GET", "/t", nil)
	if h.began != 1 || h.ended != 1 {
		t.Fatalf("hook 调用次数 = %d/%d，want 1/1", h.began, h.ended)
	}
	if got := rec.Header().Get(serverTimingHeader); got != "" {
		t.Errorf("没有 span 身份却写了 %s = %q（编造 span-id）", serverTimingHeader, got)
	}
	if got := rec.Header().Get("X-Trace-Id"); !isHexLowerN(got, 32) {
		t.Errorf("X-Trace-Id = %q，want 32hex（它与 span 无关，照旧要写）", got)
	}
	logRec, ok := findRecord(sink, eventHTTPReq)
	if !ok {
		t.Fatal("缺访问记录")
	}
	if _, has := attrsOf(logRec)[attrSpanID]; has {
		t.Error("没有 span 身份时记录里不该出现 span.id")
	}
}

// TestMinimalWithSpanHook：`Minimal()` 关掉的是框架自己的 trace 与访问日志，
// **不管 span 出口**——装了 hook 就要走 Begin/End。两者分工钉在这里：X-Trace-Id
// 不写（trace 关了），Server-Timing 照写（span 出口给的）。
func TestMinimalWithSpanHook(t *testing.T) {
	h := &recordingHook{}
	e, sink := newTestEngine(t, Minimal(), WithSpanHook(h))
	e.GET("/m", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })

	rec := doReq(e, "GET", "/m", nil)
	if len(h.spans) != 1 {
		t.Fatalf("End 调用次数 = %d，want 1（Minimal 不关 span 出口）", len(h.spans))
	}
	if in := h.begin(t); !isHexLowerN(in.TraceID, 32) {
		t.Errorf("hook 应拿到一个自启的 trace-id，got %q", in.TraceID)
	}
	if got := rec.Header().Get("X-Trace-Id"); got != "" {
		t.Errorf("Minimal() 关了 trace，不该写 X-Trace-Id，got %q", got)
	}
	if got := rec.Header().Get(serverTimingHeader); !strings.Contains(got, testSpanID) {
		t.Errorf("%s = %q，应带 hook 给的 span-id", serverTimingHeader, got)
	}
	if _, ok := findRecord(sink, eventHTTPReq); ok {
		t.Error("Minimal() 关了访问日志，不该有 http.request 记录")
	}
}

// TestDetachCarriesSpanIdentity：后台任务的 span link 靠 `Detached` 带走身份
// ——它必须带上**发起这次请求的 span**的 id，且请求结束后仍可读（值袋子，不是
// Ctx 的引用）。这是「后台任务用 link、不用父子」这条规则的可执行部分：框架
// 不替后台建 span，但得把建 link 需要的东西交出去。
func TestDetachCarriesSpanIdentity(t *testing.T) {
	h := &recordingHook{}
	e, _ := newTestEngine(t, WithSpanHook(h))

	var bg Detached
	e.GET("/bg", func(c *Ctx) error {
		bg = c.Detach()
		return c.Text(http.StatusOK, "ok")
	})
	doReq(e, "GET", "/bg", nil)

	sp := h.last(t)
	if bg.TraceID != sp.TraceID {
		t.Errorf("Detached.TraceID = %q，want %q", bg.TraceID, sp.TraceID)
	}
	if bg.SpanID != sp.SpanID || bg.SpanID != testSpanID {
		t.Errorf("Detached.SpanID = %q，want %q（发起请求的那个 span）", bg.SpanID, sp.SpanID)
	}
}

// TestDetachWithoutSpanHook：不装 span 出口时 SpanID 是空串——没有 span 可指，
// 别拿它当 trace-id 用（那种场景用 TraceID）。
func TestDetachWithoutSpanHook(t *testing.T) {
	e, _ := newTestEngine(t)

	var bg Detached
	e.GET("/bg", func(c *Ctx) error {
		bg = c.Detach()
		return c.Text(http.StatusOK, "ok")
	})
	doReq(e, "GET", "/bg", nil)

	if !isHexLowerN(bg.TraceID, 32) {
		t.Errorf("Detached.TraceID = %q，want 32hex", bg.TraceID)
	}
	if bg.SpanID != "" {
		t.Errorf("没装 span 出口时 Detached.SpanID = %q，want 空串", bg.SpanID)
	}
}

// TestUntrustedTraceHeaderSkipsInboundForSpan：关掉入站头信任后，span 侧同样
// 一律新起 trace——不采纳入站 trace-id，也不把它的 span-id 当父（信任开关是
// 安全边界，不能只在 X-Trace-Id 那条路上生效）。
func TestUntrustedTraceHeaderSkipsInboundForSpan(t *testing.T) {
	const inbound = "4bf92f3577b34da6a3ce929d0e0e4736"
	const tp = "00-" + inbound + "-00f067aa0ba902b7-01"

	h := &recordingHook{}
	e, _ := newTestEngine(t, WithSpanHook(h), WithTrustedTraceHeader(false))
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })

	doReq(e, "GET", "/t", nil, "Traceparent", tp)
	in := h.begin(t)
	if in.TraceID == inbound {
		t.Error("关掉信任后不得采纳入站 trace-id")
	}
	if !isHexLowerN(in.TraceID, 32) {
		t.Errorf("TraceID = %q，want 自启的 32hex", in.TraceID)
	}
	if in.ParentID != "" {
		t.Errorf("关掉信任后不应有 parent，got %q", in.ParentID)
	}
}

// TestTraceStatePassedThroughVerbatim：tracestate 的内容框架不解析（W3C 允许
// vendor 段随意），所以畸形值也原样透传——只做外层空白 trim。它不参与
// traceparent 的合法性判定（那条另有用例：traceparent 非法时整条不解析）。
func TestTraceStatePassedThroughVerbatim(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	const weird = ",,vendor===x,foo bar"

	h := &recordingHook{}
	e, _ := newTestEngine(t, WithSpanHook(h))
	e.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })

	doReq(e, "GET", "/t", nil, "Traceparent", tp, "Tracestate", "  "+weird+"  ")
	if got := h.begin(t).TraceState; got != weird {
		t.Errorf("TraceState = %q，want %q（原样透传，只 trim 外层空白）", got, weird)
	}
}
