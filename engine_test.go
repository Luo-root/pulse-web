package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// 本文件覆盖 Engine 的面：路由 / 中间件编排 / 错误映射 / 静态 / 生命周期，以及 Collector 选项与注册表、KV、服务查找、错误构造器这层公开面。

func newTestEngine(t *testing.T, opts ...Option) (*Engine, *observability.MemorySink) {
	t.Helper()
	sink := &observability.MemorySink{}
	all := append([]Option{WithSink(sink), WithHostID("test-host")}, opts...)
	e := New(all...)
	t.Cleanup(e.dispose)
	return e, sink
}

func doReq(e *Engine, method, target string, body io.Reader, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func attrsOf(r observability.Record) map[string]any {
	out := map[string]any{}
	r.Attrs.Range(func(k string, v any) { out[k] = v })
	return out
}

// ---- 路由与上下文 ----

func TestRouterJSONAndPathParam(t *testing.T) {
	e, _ := newTestEngine(t)
	type user struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	e.GET("/users/{id}", func(c *Ctx) error {
		return c.JSON(http.StatusOK, user{ID: c.Path("id"), Name: "jiangnan"})
	})

	rec := doReq(e, "GET", "/users/42", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got user
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "42" {
		t.Fatalf("path param = %q, want 42", got.ID)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestRequestKV(t *testing.T) {
	e, _ := newTestEngine(t)
	key := NewKey[string]("test.user")

	e.Use(func(c *Ctx, next Handler) error {
		c.Set(key, "jiangnan")
		return next(c)
	})
	e.GET("/who", func(c *Ctx) error {
		name, ok := c.Get(key)
		if !ok {
			return NotFound("user", nil)
		}
		return c.Text(http.StatusOK, name)
	})

	rec := doReq(e, "GET", "/who", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "jiangnan" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

func TestGlobalServiceFromRoot(t *testing.T) {
	e, _ := newTestEngine(t)
	key := kernel.NewServiceKey[string]("test.greeting")
	if _, err := kernel.Provide(e.Root(), key, "hello"); err != nil {
		t.Fatalf("provide: %v", err)
	}
	e.GET("/svc", func(c *Ctx) error { return c.Text(http.StatusOK, c.MustService(key)) })

	rec := doReq(e, "GET", "/svc", nil)
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// ---- 分组与中间件 ----

func TestGroupAndMiddlewareOrder(t *testing.T) {
	e, _ := newTestEngine(t)
	var order []string
	mw := func(name string) Middleware {
		return func(c *Ctx, next Handler) error {
			order = append(order, name+">")
			err := next(c)
			order = append(order, "<"+name)
			return err
		}
	}

	g := e.Group("/api", mw("g1"))
	g.GET("/x", func(c *Ctx) error {
		order = append(order, "h")
		return c.Text(http.StatusOK, "ok")
	}, mw("r"))

	rec := doReq(e, "GET", "/api/x", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	want := "g1> r> h <r <g1"
	if got := strings.Join(order, " "); got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
}

func TestMiddlewareShortCircuit(t *testing.T) {
	e, _ := newTestEngine(t)
	reached := false
	e.Use(func(c *Ctx, next Handler) error { return Unauthorized("auth", nil) })
	e.GET("/secret", func(c *Ctx) error {
		reached = true
		return c.Text(http.StatusOK, "nope")
	})

	rec := doReq(e, "GET", "/secret", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if reached {
		t.Fatal("handler should not be reached")
	}
}

// ---- 错误映射 ----

func TestErrorMappingHTTPErrorStripsCause(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/missing", func(c *Ctx) error {
		return NotFound("user", errors.New("db: no rows in result set"))
	})

	rec := doReq(e, "GET", "/missing", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "no rows") {
		t.Fatalf("cause leaked into response: %s", rec.Body.String())
	}
	var body errorPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Error.Code != "user" {
		t.Fatalf("code = %q", body.Error.Code)
	}
}

type teapotError struct{}

func (teapotError) Error() string { return "i am a teapot" }

func (teapotError) StatusCode() int { return http.StatusTeapot }

func TestErrorMappingStatusCoder(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/tea", func(c *Ctx) error { return teapotError{} })

	rec := doReq(e, "GET", "/tea", nil)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d, want 418", rec.Code)
	}
}

func TestPlainErrorMapsTo500WithoutLeak(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/boom", func(c *Ctx) error { return errors.New("secret internal detail") })

	rec := doReq(e, "GET", "/boom", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("internal detail leaked: %s", rec.Body.String())
	}
}

func TestWrittenResponseIsNotOverwritten(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/partial", func(c *Ctx) error {
		_ = c.JSON(http.StatusOK, map[string]string{"ok": "yes"})
		return errors.New("late failure")
	})

	rec := doReq(e, "GET", "/partial", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (first write wins)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "yes") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestStatusOnlyResponse(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/nc", func(c *Ctx) error { c.Status(http.StatusNoContent); return nil })

	rec := doReq(e, "GET", "/nc", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestJSONEncodeFailureMappedTo500(t *testing.T) {
	// 编码失败（不可编码的值）必须在写头之前暴露：客户端拿到 500 + 统一
	// 错误体，而不是 200 空响应（c.HTML 的同一课，见其注释）。
	e, _ := newTestEngine(t)
	e.GET("/j", func(c *Ctx) error { return c.JSON(http.StatusOK, make(chan int)) })

	rec := doReq(e, "GET", "/j", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d，want 500（body=%q）", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":"internal"`) {
		t.Fatalf("body = %q，want 统一错误体", rec.Body.String())
	}
}

func TestJSONBytesStable(t *testing.T) {
	// 成功路径字节与「编码器直写」逐一致（保留 Encoder 的尾换行）。
	e, _ := newTestEngine(t)
	e.GET("/j", func(c *Ctx) error { return c.JSON(http.StatusOK, map[string]int{"a": 1}) })
	rec := doReq(e, "GET", "/j", nil)
	if got, want := rec.Body.String(), "{\"a\":1}\n"; got != want {
		t.Fatalf("body = %q，want %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestPanicHTTPErrorMapsTo500(t *testing.T) {
	// mapper 注释的承诺：panic(web.NotFound(...)) 不返回 404 —— 一律 500
	// （要 4xx 请 return）；业务码不泄露进响应体。
	e, _ := newTestEngine(t)
	e.GET("/p", func(c *Ctx) error { panic(NotFound("gone", nil)) })

	rec := doReq(e, "GET", "/p", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d，want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "gone") {
		t.Fatalf("业务码不应泄露：%q", rec.Body.String())
	}
}

func TestErrorHandlerFailureFallsBackTo500(t *testing.T) {
	e, _ := newTestEngine(t, WithErrorHandler(func(c *Ctx, err error) error {
		panic("mapper exploded")
	}))
	e.GET("/x", func(c *Ctx) error { return errors.New("boom") })

	rec := doReq(e, "GET", "/x", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// ---- 静态文件 ----

func TestStatic(t *testing.T) {
	e, _ := newTestEngine(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello static"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.Static("/files", dir)

	rec := doReq(e, "GET", "/files/a.txt", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello static" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
}

// ---- 装配与关闭 ----

func TestMinimalKeepsPanicGuardWithoutObservability(t *testing.T) {
	e := New(Minimal())
	t.Cleanup(e.dispose)

	if e.sink != nil {
		t.Fatal("Minimal should not install a default sink")
	}
	e.GET("/boom", func(c *Ctx) error { panic("kaboom") })
	rec := doReq(e, "GET", "/boom", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestWithSinkDoesNotReviveMinimalObservability(t *testing.T) {
	e, sink := newTestEngine(t, Minimal())
	e.GET("/plain", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })

	rec := doReq(e, "GET", "/plain", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if n := sink.Len(); n != 0 {
		t.Fatalf("Minimal + WithSink should not log requests, got %d records", n)
	}
}

func TestOnShutdownHookRuns(t *testing.T) {
	e, _ := newTestEngine(t)
	called := false
	e.OnShutdown(func(ctx context.Context) error {
		called = true
		return nil
	})

	srv := &http.Server{}
	if err := e.shutdown(srv); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if !called {
		t.Fatal("shutdown hook not called")
	}
	if !e.life.disposed {
		t.Fatal("kernel should be disposed after shutdown")
	}
}

func TestDisposedEngineReturns503(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })
	e.dispose()

	rec := doReq(e, "GET", "/", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// 请求级 Collector（WithCollector）的可见性边界与并发隔离。
//
// 上游 v0.2.1 起 Collector 是 `kernel.Local()` 作用域局部绑定：绑定所在
// scope 及其后代可读，父 / 兄弟不可读，且不参与 fiber 依赖解析。

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

// TestCollectorIsolatesConcurrentRequests 是**真并发**用例：N 个 goroutine
// 各发一次请求，事后核对「每个请求恰好一条 probe.event」且「每条记录的
// TraceID 命中自己那次请求」。
//
// 顺序发两次测不出串台——即使绑定落在共享位置、并发互相覆盖，顺序跑照样
// 通过。这条用例最初写成了顺序调用却顶着「并发」的名字，已被 review 抓出。
func TestCollectorIsolatesConcurrentRequests(t *testing.T) {
	e, sink := newTestEngine(t, WithCollector())
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

	const n = 32
	got := make([]string, n) // 每个 goroutine 只写自己的下标，无共享写
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := doReq(e, "GET", "/probe", nil)
			if rec.Code != http.StatusOK {
				t.Errorf("req %d: status = %d", i, rec.Code)
				return
			}
			got[i] = rec.Header().Get("X-Trace-Id")
		}(i)
	}
	wg.Wait()

	seen := make(map[string]int, n)
	for _, r := range sink.Snapshot() {
		if r.Event == "probe.event" {
			seen[r.TraceID]++
		}
	}
	if len(seen) != n {
		t.Fatalf("probe.event 的 TraceID 去重后 = %d，want %d —— 并发请求串台了", len(seen), n)
	}
	for i, want := range got {
		if want == "" {
			t.Fatalf("req %d 没有 TraceID", i)
		}
		if seg := seen[want]; seg != 1 {
			t.Fatalf("req %d 的 TraceID %s 在记录里出现 %d 次，want 1", i, want, seg)
		}
	}
}

// 公开 API 面的薄包装：路由方法注册器、Ctx 的两条读取入口、服务查找的 miss
// 分支、内置错误构造器与两种 Error 文本。它们此前没有任何测试调用过。

// TestHTTPMethodRegistrars 覆盖除 GET 外的六个路由方法注册器与底层 Handle。
func TestHTTPMethodRegistrars(t *testing.T) {
	e, _ := newTestEngine(t)
	h := func(c *Ctx) error { return c.Text(http.StatusOK, c.Request().Method) }
	e.POST("/m", h)
	e.PUT("/m", h)
	e.PATCH("/m", h)
	e.DELETE("/m", h)
	e.HEAD("/m", h)
	e.OPTIONS("/m", h)
	e.Handle("GET /raw", func(c *Ctx) error { return c.Text(http.StatusOK, "raw") })

	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		rec := doReq(e, m, "/m", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s /m status = %d，want 200", m, rec.Code)
		}
		// HEAD 的 body 语义由 net/http 承担，这里只断状态码
		if m != "HEAD" && rec.Body.String() != m {
			t.Fatalf("%s /m body = %q，want %q", m, rec.Body.String(), m)
		}
	}

	if rec := doReq(e, "GET", "/raw", nil); rec.Code != http.StatusOK || rec.Body.String() != "raw" {
		t.Fatalf("Handle(\"GET /raw\") = %d %q", rec.Code, rec.Body.String())
	}
}

// TestCtxQueryAndSetHeader：查询参数与响应头两条薄包装。
func TestCtxQueryAndSetHeader(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/q", func(c *Ctx) error {
		c.SetHeader("X-Custom", "v1")
		if got := c.Query("name"); got != "jiangnan" {
			return BadRequest("bad_name", nil)
		}
		if got := c.Query("absent"); got != "" {
			return BadRequest("absent_should_be_empty", nil)
		}
		return c.Text(http.StatusOK, "ok")
	})

	rec := doReq(e, "GET", "/q?name=jiangnan", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，want 200", rec.Code)
	}
	if got := rec.Header().Get("X-Custom"); got != "v1" {
		t.Fatalf("X-Custom = %q，want v1", got)
	}
}

// TestServiceLookupMissReturnsFalse：`Ctx.Service` / `Detached.Service` 在未提供
// 时返回 (zero, false) 而不是 panic —— panic 是 MustService 的语义。
func TestServiceLookupMissReturnsFalse(t *testing.T) {
	e, _ := newTestEngine(t)
	missing := kernel.NewServiceKey[string]("test.missing")

	e.GET("/ctx", func(c *Ctx) error {
		if v, ok := c.Service(missing); ok || v != "" {
			return Internal("unexpected_hit", nil)
		}
		return c.Text(http.StatusOK, "absent")
	})
	e.GET("/detached", func(c *Ctx) error {
		if v, ok := c.Detach().Service(missing); ok || v != "" {
			return Internal("unexpected_hit", nil)
		}
		return c.Text(http.StatusOK, "absent")
	})

	for _, p := range []string{"/ctx", "/detached"} {
		if rec := doReq(e, "GET", p, nil); rec.Code != http.StatusOK || rec.Body.String() != "absent" {
			t.Fatalf("%s = %d %q", p, rec.Code, rec.Body.String())
		}
	}
}

// TestErrorConstructorsAndText：六个内置构造器的状态码与 code 落位，以及
// 两种 Error 文本——文本用于日志（code + 状态标准文案），**不含 cause**。
func TestErrorConstructorsAndText(t *testing.T) {
	cases := []struct {
		name string
		err  *HTTPError
		want int
	}{
		{"BadRequest", BadRequest("bad", nil), http.StatusBadRequest},
		{"Unauthorized", Unauthorized("nope", nil), http.StatusUnauthorized},
		{"Forbidden", Forbidden("denied", nil), http.StatusForbidden},
		{"NotFound", NotFound("gone", nil), http.StatusNotFound},
		{"Conflict", Conflict("dup", nil), http.StatusConflict},
		{"Internal", Internal("boom", nil), http.StatusInternalServerError},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.err.Status != c.want {
				t.Fatalf("Status = %d，want %d", c.err.Status, c.want)
			}
			if got := c.err.StatusCode(); got != c.want {
				t.Fatalf("StatusCode() = %d，want %d", got, c.want)
			}
			if c.err.Code == "" {
				t.Fatal("构造器应写入 code")
			}
			if c.err.Message != "" {
				t.Fatal("构造器不设 Message：默认文案取状态码标准文案")
			}
			if got, want := c.err.Error(), c.err.Code+": "+http.StatusText(c.want); got != want {
				t.Fatalf("Error() = %q，want %q", got, want)
			}
		})
	}

	// 没有 Code / Message 时退回状态码标准文案
	if got, want := (&HTTPError{Status: http.StatusTeapot}).Error(), http.StatusText(http.StatusTeapot); got != want {
		t.Fatalf("Error() = %q，want %q", got, want)
	}

	// cause 只进观测记录，但必须能经 errors.Is 追溯
	cause := errors.New("db is down")
	withCause := Internal("db", cause)
	if !errors.Is(withCause, cause) {
		t.Fatal("errors.Is 追不到 cause")
	}
	if strings.Contains(withCause.Error(), "db is down") {
		t.Fatal("Error() 文本不应包含 cause（它只进观测记录）")
	}

	if got := (&PanicError{Value: "kaboom"}).Error(); got != "panic: kaboom" {
		t.Fatalf("PanicError.Error() = %q", got)
	}
}

// TestPanicAfterResponseWrittenKeepsStatus 回归：panic 时若响应已写出，
// 状态码保持首次写入值（不重写成 500）。
func TestPanicAfterResponseWrittenKeepsStatus(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/half", func(c *Ctx) error {
		_ = c.JSON(http.StatusAccepted, map[string]string{"stage": "half"})
		panic("boom after write")
	})

	rec := doReq(e, "GET", "/half", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (first write wins)", rec.Code)
	}
}

// TestStatusCoderErrorCategory 回归：实现 StatusCoder 的错误按状态码区间分类，
// 不再一律归 internal。
func TestStatusCoderErrorCategory(t *testing.T) {
	e, sink := newTestEngine(t)
	e.GET("/tea", func(c *Ctx) error { return teapotError{} })

	if rec := doReq(e, "GET", "/tea", nil); rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d", rec.Code)
	}
	rec, ok := findRecord(sink, eventHTTPReq)
	if !ok {
		t.Fatal("no http.request record")
	}
	if got := attrsOf(rec)["error.type"]; got != "http_4xx" {
		t.Fatalf("error.type = %v, want http_4xx", got)
	}
}

type extPlugin struct{}

func (extPlugin) Inject() []kernel.Dependency { return nil }

func (extPlugin) Apply(*kernel.Context) error { return nil }

// TestWithRootAcceptsPreinstalledTree 回归：外部树已装插件时 WithRoot 仍可用，
// 双方 Provide 的服务彼此可见（Bootstrap 只能拿到快照，见 New 的注释）。
func TestWithRootAcceptsPreinstalledTree(t *testing.T) {
	root := kernel.New()
	t.Cleanup(root.Dispose)

	if _, err := kernel.Use(root, extPlugin{}); err != nil {
		t.Fatal(err)
	}

	sink := &observability.MemorySink{}
	e := New(WithRoot(root), WithSink(sink), WithHostID("ext"))
	t.Cleanup(e.dispose)

	key := kernel.NewServiceKey[string]("ext.greeting")
	if _, err := kernel.Provide(e.Root(), key, "hi-from-web"); err != nil {
		t.Fatal(err)
	}
	e.GET("/svc", func(c *Ctx) error { return c.Text(http.StatusOK, c.MustService(key)) })

	if rec := doReq(e, "GET", "/svc", nil); rec.Body.String() != "hi-from-web" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if sink.Len() == 0 {
		t.Fatal("expected bootstrap snapshot records on pre-installed tree")
	}
}
