package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- stdlib 互操作（设计验收标准第 6 条）----
//
// 设计只承诺两条路径：`Wrap` 把 stdlib handler 接进来、`Handler()` 把 Engine 导出去。
// **不提供**「框架内挂 stdlib 中间件」的 API——stdlib 中间件包在 Engine 外面即可
// （Engine 本身就是 http.Handler），这就是「无侵入」的具体含义。本文件把这两条钉住。

// contextKey 验证「外层 stdlib 中间件注入的 context 值，框架 handler 读得到」。
type contextKey struct{}

// TestWrapStdlibHandler 钉住 Wrap 的语义：stdlib handler 自写响应、照常经过中间件链、
// 路径参数与框架侧**同源**（都是 ServeMux 写入的那一份），且观测不受影响。
func TestWrapStdlibHandler(t *testing.T) {
	e, sink := newTestEngine(t)

	var sawPath string
	mw := func(c *Ctx, next Handler) error {
		sawPath = c.Path("id") // 中间件在 handler 之前跑
		return next(c)
	}

	called := false
	e.GET("/wrapped/{id}", Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("stdlib:" + r.PathValue("id")))
	})), mw)
	// 对照：框架 handler 用 c.Path 读同一个参数
	e.GET("/native/{id}", func(c *Ctx) error { return c.Text(http.StatusOK, c.Path("id")) })

	rec := doReq(e, "GET", "/wrapped/abc", nil)
	if !called {
		t.Fatal("经过 Wrap 的 stdlib handler 没有被调用")
	}
	if rec.Code != http.StatusCreated || rec.Body.String() != "stdlib:abc" {
		t.Fatalf("wrapped: got %d %q, want 201 %q", rec.Code, rec.Body.String(), "stdlib:abc")
	}
	if sawPath != "abc" {
		t.Fatalf("中间件没经过 Wrap 的 handler：读到的路径参数 = %q, want %q", sawPath, "abc")
	}
	if got := doReq(e, "GET", "/native/abc", nil).Body.String(); got != "abc" {
		t.Fatalf("native: got %q, want %q", got, "abc")
	}
	if _, ok := findRecord(sink, eventHTTPReq); !ok {
		t.Fatal("经过 Wrap 的请求没有 http.request 记录：观测在这条路径上断了")
	}
}

// TestWrapPanicCaughtByEngine 钉住设计里那句「panic 由 Engine 的 defer 兜底接」
// ——Wrap 的 handler 也不例外，且响应体不泄漏 panic 值。
func TestWrapPanicCaughtByEngine(t *testing.T) {
	e, sink := newTestEngine(t)
	e.GET("/boom", Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("from-stdlib")
	})))

	rec := doReq(e, "GET", "/boom", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); got == "" || strings.Contains(got, "from-stdlib") {
		t.Fatalf("body = %q, want sanitized payload", got)
	}
	logRec, ok := findRecord(sink, eventHTTPReq)
	if !ok || logRec.Err == nil {
		t.Fatal("Wrap 内的 panic 没有被记录成 Err")
	}
	if got := attrsOf(logRec)["error.type"]; got != "panic" {
		t.Fatalf("error.type = %v, want panic", got)
	}
}

// TestEngineUnderStdlibMiddleware 钉住反向：Engine 经 Handler() 导出后，可以被 stdlib
// 中间件**包在外层**——框架不需要认识它，这就是「挂载 stdlib 中间件无侵入」。
//
// 同时钉住两件容易被忽略的事：外层中间件注入到 request context 的值，框架 handler
// 读得到（`Ctx.Request()` 暴露原始 *http.Request）；引擎自身的观测在中间件内层照常跑。
func TestEngineUnderStdlibMiddleware(t *testing.T) {
	e, sink := newTestEngine(t)
	e.GET("/ping", func(c *Ctx) error {
		v, _ := c.Request().Context().Value(contextKey{}).(string)
		return c.Text(http.StatusOK, "v="+v)
	})

	stdMW := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Stdlib-MW", "on")
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, "injected")))
		})
	}

	srv := httptest.NewServer(stdMW(e.Handler()))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK || string(body) != "v=injected" {
		t.Fatalf("got %d %q, want 200 %q", resp.StatusCode, body, "v=injected")
	}
	if resp.Header.Get("X-Stdlib-MW") != "on" {
		t.Fatal("外层 stdlib 中间件的响应头丢了")
	}
	if resp.Header.Get("X-Trace-Id") == "" {
		t.Fatal("引擎的 X-Trace-Id 丢了：观测没有在 stdlib 中间件内层跑起来")
	}
	if _, ok := findRecord(sink, eventHTTPReq); !ok {
		t.Fatal("被 stdlib 中间件包住的请求没有 http.request 记录")
	}
}
