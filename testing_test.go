package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 测试工具链（#63）的核心证据：测试入口跑出来的响应与观测记录，与走完整 ServeHTTP
// **逐项一致**——不是"看起来差不多"，而是同一份装配代码的两次执行。

func TestNewTestContextMatchesRealPath(t *testing.T) {
	handler := func(c *Ctx) error {
		return c.JSON(200, H{"id": c.Path("id"), "q": c.Query("q")})
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
	if err := handler(c); err != nil {
		done()
		t.Fatalf("handler 返回 error：%v", err)
	}
	done()

	// ---- 响应面逐项比 ----
	if rec.Code != realRec.Code {
		t.Fatalf("状态码：测试入口 %d vs 真路径 %d", rec.Code, realRec.Code)
	}
	if rec.Body.String() != realRec.Body.String() {
		t.Fatalf("响应体：测试入口 %q vs 真路径 %q", rec.Body.String(), realRec.Body.String())
	}
	// 两个链路头的**值**不同（trace-id 是身份不是行为），但"在不在"必须一致。
	for _, h := range []string{"X-Trace-Id", "Server-Timing"} {
		tv, rv := rec.Header().Get(h), realRec.Header().Get(h)
		if (tv == "") != (rv == "") {
			t.Fatalf("响应头 %s：测试入口 %q vs 真路径 %q", h, tv, rv)
		}
	}

	// ---- 观测记录逐项比 ----
	realAttrs := attrsOf(waitRecord(t, realSink, eventHTTPReq))
	testAttrs := attrsOf(waitRecord(t, testSink, eventHTTPReq))
	if len(realAttrs) == 0 || len(testAttrs) == 0 {
		t.Fatalf("属性为空：真路径 %d 项，测试入口 %d 项", len(realAttrs), len(testAttrs))
	}
	for k, tv := range testAttrs {
		rv, ok := realAttrs[k]
		if !ok {
			t.Fatalf("属性 %s 只在测试入口里有（%v）", k, tv)
		}
		if k == "trace_id" {
			// 身份是随机的，只要求两边都真的分配了。
			if tv == "" || rv == "" {
				t.Fatalf("trace_id 不该为空：测试入口 %q，真路径 %q", tv, rv)
			}
			continue
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

// ServeTest 覆盖「handler + 一组中间件」这条路径，并且 panic 照真路径那样被接管。
func TestServeTestRunsMiddlewareAndRecoversPanic(t *testing.T) {
	e, sink := newTestEngine(t)

	var order []string
	mw := func(name string) Middleware {
		return func(c *Ctx, next Handler) error {
			order = append(order, name)
			return next(c)
		}
	}

	req := httptest.NewRequest("GET", "/x", nil)
	rec := httptest.NewRecorder()
	e.ServeTest(rec, req, func(c *Ctx) error {
		order = append(order, "handler")
		return c.Text(200, "ok")
	}, mw("a"), mw("b"))

	if got := strings.Join(order, ","); got != "a,b,handler" {
		t.Fatalf("执行顺序 = %q，应为 a,b,handler", got)
	}
	if rec.Code != 200 || rec.Body.String() != "ok" {
		t.Fatalf("status = %d，body = %q", rec.Code, rec.Body.String())
	}
	if _, ok := findRecord(sink, eventHTTPReq); !ok {
		t.Fatal("访问记录缺失——ServeTest 应与真路径一样落记录")
	}

	// panic 路径：真路径把它转成 PanicError 走错误映射，这里必须一样。
	rec2 := httptest.NewRecorder()
	e.ServeTest(rec2, httptest.NewRequest("GET", "/boom", nil), func(c *Ctx) error {
		panic("boom")
	})
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应被转成 500，得到 %d（body = %q）", rec2.Code, rec2.Body.String())
	}
}

// 边界：NewTestContext **不**接管 panic——handler 在调用方自己的栈上执行，框架的
// recover 不在那条栈上。这是 godoc 写明的一条，用测试钉住免得日后被"顺手补上"。
func TestNewTestContextDoesNotSwallowPanic(t *testing.T) {
	e, _ := newTestEngine(t)
	c, done := e.NewTestContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))

	got := func() (p any) {
		defer func() { p = recover() }()
		defer done()
		_ = func(c *Ctx) error { panic("boom") }(c)
		return nil
	}()

	if got == nil {
		t.Fatal("panic 应传播给调用方——要覆盖 panic 路径请用 ServeTest")
	}
}

// done() 幂等：重复调用不重复收尾（否则 Sink 里会多出记录、scope 会被二次 dispose）。
func TestNewTestContextDoneIsIdempotent(t *testing.T) {
	e, sink := newTestEngine(t)
	c, done := e.NewTestContext(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
	if err := c.Text(200, "ok"); err != nil {
		t.Fatal(err)
	}

	done()
	done()
	done()

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
