package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 测试工具链（#63）的核心证据：测试入口跑出来的响应与观测记录，与走完整 ServeHTTP
// **逐项一致**——不是"看起来差不多"，而是同一份装配代码的两次执行。
//
// 每条用例都标注它盖的是票面验收清单里的哪一项（path / query / body / KV / Detach /
// 错误映射 / 观测记录 / panic）。

// fixedIDHook 给每个请求一个固定 span 身份。没有它，「Server-Timing 在不在」那两行
// 断言是**空转**的——不装 hook 时两条路径都是空串，恒等。
type fixedIDHook struct{}

func (fixedIDHook) Begin(ctx context.Context, in SpanInfo) (context.Context, SpanRef) {
	return ctx, SpanRef{TraceID: in.TraceID, SpanID: "0123456789abcdef"}
}

func (fixedIDHook) End(context.Context, Span) {}

// ---- 同构对比：响应面 + 观测面 ----

func TestNewTestContextMatchesRealPath(t *testing.T) {
	handler := func(c *Ctx) error {
		return c.JSON(http.StatusOK, H{"id": c.Path("id"), "q": c.Query("q")})
	}

	// 真路径：注册 + 走 ServeHTTP
	realEngine, realSink := newTestEngine(t)
	realEngine.GET("/users/{id}", handler)
	realRec := doReq(realEngine, "GET", "/users/42?q=1", nil)

	// 测试入口：同一段 handler，不起 server。路径参数与路由模板由调用方补在 request
	// 上——标准库把它们放在 `Request.PathValue` 与导出的 `Request.Pattern` 里，不走
	// ServeMux 自然就没人替你填。
	testEngine, testSink := newTestEngine(t)
	req := httptest.NewRequest("GET", "/users/42?q=1", nil)
	req.Pattern = "GET /users/{id}"
	req.SetPathValue("id", "42")

	rec := httptest.NewRecorder()
	c, done := testEngine.NewTestContext(rec, req)
	done(handler(c))

	// ---- 响应面 ----
	if rec.Code != realRec.Code {
		t.Fatalf("状态码：测试入口 %d vs 真路径 %d", rec.Code, realRec.Code)
	}
	if rec.Body.String() != realRec.Body.String() {
		t.Fatalf("响应体：测试入口 %q vs 真路径 %q", rec.Body.String(), realRec.Body.String())
	}

	// ---- 观测面：记录本体 + 属性集 ----
	realRecord := waitRecord(t, realSink, eventHTTPReq)
	testRecord := waitRecord(t, testSink, eventHTTPReq)

	// trace-id 是身份不是行为，值必然不同——比的是**两边都真的分配了 32hex**。
	// 注意它在 `Record.TraceID` 上，**不在** `Attrs` 里（`attrsOf` 只 Range Attrs）。
	if len(realRecord.TraceID) != 32 || len(testRecord.TraceID) != 32 {
		t.Fatalf("trace-id 应为 32hex：真路径 %q，测试入口 %q", realRecord.TraceID, testRecord.TraceID)
	}
	if realRecord.HostID != testRecord.HostID {
		t.Fatalf("HostID：真路径 %q vs 测试入口 %q", realRecord.HostID, testRecord.HostID)
	}

	realAttrs, testAttrs := attrsOf(realRecord), attrsOf(testRecord)
	if len(realAttrs) == 0 {
		t.Fatal("属性为空，守卫不能因为没查所以通过")
	}
	for k, tv := range testAttrs {
		rv, ok := realAttrs[k]
		if !ok {
			t.Fatalf("属性 %s 只在测试入口里有（%v）", k, tv)
		}
		if tv != rv {
			t.Fatalf("属性 %s：测试入口 %v vs 真路径 %v", k, tv, rv)
		}
	}
	for k, rv := range realAttrs {
		if _, ok := testAttrs[k]; !ok {
			t.Fatalf("属性 %s 只在真路径里有（%v）", k, rv)
		}
	}
}

// 错误映射（票面验收点）：`done(err)` 把 handler 的返回值交给 mapper，与真路径
// `return err` 逐项一致——状态码、错误体、记录里的 error 属性。
func TestNewTestContextMapsHandlerError(t *testing.T) {
	handler := func(c *Ctx) error { return NotFound("user", nil) }

	realEngine, realSink := newTestEngine(t)
	realEngine.GET("/u/{id}", handler)
	realRec := doReq(realEngine, "GET", "/u/7", nil)

	testEngine, testSink := newTestEngine(t)
	req := httptest.NewRequest("GET", "/u/7", nil)
	req.Pattern = "GET /u/{id}"
	req.SetPathValue("id", "7")
	rec := httptest.NewRecorder()
	c, done := testEngine.NewTestContext(rec, req)
	done(handler(c))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d，应为 404——done(err) 必须走统一错误映射", rec.Code)
	}
	if rec.Body.String() != realRec.Body.String() {
		t.Fatalf("错误体：测试入口 %q vs 真路径 %q", rec.Body.String(), realRec.Body.String())
	}

	ra := attrsOf(waitRecord(t, realSink, eventHTTPReq))
	ta := attrsOf(waitRecord(t, testSink, eventHTTPReq))
	if ra["error.type"] != ta["error.type"] {
		t.Fatalf("error.type：真路径 %v vs 测试入口 %v", ra["error.type"], ta["error.type"])
	}
	if ra["error.type"] == nil {
		t.Fatal("真路径都没记 error.type，这条断言在空转")
	}
}

// 装了 SpanHook 的档：两条路径都写 `Server-Timing`，且形状一致（span-id 段固定可判）。
func TestNewTestContextWritesServerTimingWithHook(t *testing.T) {
	handler := func(c *Ctx) error { return c.Text(http.StatusOK, "ok") }

	realEngine, _ := newTestEngine(t, WithSpanHook(fixedIDHook{}))
	realEngine.GET("/x", handler)
	realRec := doReq(realEngine, "GET", "/x", nil)

	testEngine, _ := newTestEngine(t, WithSpanHook(fixedIDHook{}))
	rec := httptest.NewRecorder()
	c, done := testEngine.NewTestContext(rec, httptest.NewRequest("GET", "/x", nil))
	done(handler(c))

	rt, tt := realRec.Header().Get(serverTimingHeader), rec.Header().Get(serverTimingHeader)
	if rt == "" || tt == "" {
		t.Fatalf("装了 hook 时两条路径都该写 %s：真路径 %q，测试入口 %q", serverTimingHeader, rt, tt)
	}
	if !strings.Contains(tt, "0123456789abcdef") {
		t.Fatalf("测试入口的 %s = %q，应带 hook 给的 span-id", serverTimingHeader, tt)
	}
	if len(tt) != len(rt) {
		t.Fatalf("两条路径的 %s 长度不同：%q vs %q", serverTimingHeader, tt, rt)
	}
}

// ---- 票面点名的其余覆盖面 ----

func TestNewTestContextBindsBody(t *testing.T) {
	e, _ := newTestEngine(t)
	type payload struct {
		Name string `json:"name"`
	}

	req := httptest.NewRequest("POST", "/u", strings.NewReader(`{"name":"a"}`))
	req.Header.Set("Content-Type", "application/json")
	c, done := e.NewTestContext(httptest.NewRecorder(), req)

	var in payload
	err := c.Bind(&in)
	done(err)

	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if in.Name != "a" {
		t.Fatalf("绑定结果 = %+v", in)
	}
}

func TestNewTestContextCarriesRequestKV(t *testing.T) {
	e, _ := newTestEngine(t)
	key := NewKey[string]("k")

	c, done := e.NewTestContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	c.Set(key, "v")
	got, ok := c.Get(key)
	done(c.Text(http.StatusOK, "ok"))

	if !ok || got != "v" {
		t.Fatalf("请求级 KV：got=%q ok=%v", got, ok)
	}
}

func TestNewTestContextDetachSharesTrace(t *testing.T) {
	e, sink := newTestEngine(t)

	rec := httptest.NewRecorder()
	c, done := e.NewTestContext(rec, httptest.NewRequest("GET", "/x", nil))
	trace := c.TraceID()
	d := c.Detach()
	done(c.Text(http.StatusOK, "ok"))

	if trace == "" {
		t.Fatal("TraceID 不该为空")
	}
	if d.TraceID != trace {
		t.Fatalf("Detach 带走的 TraceID = %q，Ctx 上是 %q", d.TraceID, trace)
	}
	if got := waitRecord(t, sink, eventHTTPReq).TraceID; got != trace {
		t.Fatalf("访问记录的 TraceID = %q，应为 %q", got, trace)
	}
}

// engine 已销毁（进程关闭中）：返回 nil Ctx，503 已经写到 w；done 仍要能安全调用。
func TestNewTestContextOnDisposedEngine(t *testing.T) {
	e, _ := newTestEngine(t)
	e.dispose()

	rec := httptest.NewRecorder()
	c, done := e.NewTestContext(rec, httptest.NewRequest("GET", "/x", nil))
	done(nil) // 先调：不该 panic

	if c != nil {
		t.Fatal("engine 已销毁时应返回 nil Ctx")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，应为 503", rec.Code)
	}
}

// done 幂等：**第一次调用生效**。重复调用既不重复收尾，也不让后到的 error 改写结果。
func TestNewTestContextDoneIsIdempotent(t *testing.T) {
	e, sink := newTestEngine(t)
	rec := httptest.NewRecorder()
	c, done := e.NewTestContext(rec, httptest.NewRequest("GET", "/x", nil))

	done(c.Text(http.StatusOK, "ok"))
	done(nil)
	done(NotFound("late", nil)) // 后两次都该被忽略

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，应为 200（后到的 error 不该改写已定的响应）", rec.Code)
	}
	n := 0
	for _, r := range sink.Snapshot() {
		if r.Event == eventHTTPReq {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("访问记录 %d 条，应为 1 条", n)
	}
}

// 边界：NewTestContext **不**接管 panic——handler 在调用方自己的栈上执行。这是 godoc
// 写明的一条，用测试钉住免得日后被"顺手补上"。
func TestNewTestContextDoesNotSwallowPanic(t *testing.T) {
	e, _ := newTestEngine(t)
	c, done := e.NewTestContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	got := func() (p any) {
		defer func() { p = recover() }()
		defer done(nil)
		_ = func(c *Ctx) error { panic("boom") }(c)
		return nil
	}()

	if got == nil {
		t.Fatal("panic 应传播给调用方——要覆盖 panic 路径请用 ServeTest")
	}
}

// ---- ServeTest ----

// 传入的 mw 是**追加**：`Use` 的全局中间件照旧在链上（与真路径同一个 chainMW）。
func TestServeTestChainsGlobalUse(t *testing.T) {
	e, _ := newTestEngine(t)
	var order []string

	e.Use(func(c *Ctx, next Handler) error {
		order = append(order, "use")
		return next(c)
	})

	e.ServeTest(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil),
		func(c *Ctx) error {
			order = append(order, "handler")
			return c.Text(http.StatusOK, "ok")
		},
		func(c *Ctx, next Handler) error {
			order = append(order, "route")
			return next(c)
		})

	if got := strings.Join(order, ","); got != "use,route,handler" {
		t.Fatalf("执行顺序 = %q，应为 use,route,handler", got)
	}
}

// ServeTest 走 withCtx，所以请求体闸门照真路径生效（NewTestContext 不走那条路）。
func TestServeTestEnforcesBodyLimit(t *testing.T) {
	e, _ := newTestEngine(t, WithMaxBodyBytes(16))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/x", strings.NewReader(strings.Repeat("x", 64)))
	e.ServeTest(rec, req, func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d，应为 413（体闸门在 withCtx 里）", rec.Code)
	}
}

// ServeTest 覆盖 panic：与真路径一样转成 PanicError 走错误映射。
func TestServeTestRecoversPanic(t *testing.T) {
	e, sink := newTestEngine(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/boom", nil)
	e.ServeTest(rec, req, func(c *Ctx) error { panic("boom") })

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应被转成 500，得到 %d（body = %q）", rec.Code, rec.Body.String())
	}
	if _, ok := findRecord(sink, eventHTTPReq); !ok {
		t.Fatal("访问记录缺失——ServeTest 应与真路径一样落记录")
	}
}
