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
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

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

func (teapotError) Error() string  { return "i am a teapot" }
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
