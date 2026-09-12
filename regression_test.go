package web

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// TestStaticRunsThroughMiddleware 回归：静态资源必须经过全局中间件链。
func TestStaticRunsThroughMiddleware(t *testing.T) {
	e, _ := newTestEngine(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	var hit bool
	e.Use(func(c *Ctx, next Handler) error {
		hit = true
		return next(c)
	})
	e.Static("/files", dir)

	rec := doReq(e, "GET", "/files/a.txt", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hi" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if !hit {
		t.Fatal("static resource bypassed the middleware chain")
	}
}

// TestGroupMiddlewareAppliesToStatic 分组中间件同样覆盖静态资源。
func TestGroupMiddlewareAppliesToStatic(t *testing.T) {
	e, _ := newTestEngine(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("yo"), 0o600); err != nil {
		t.Fatal(err)
	}

	var hit bool
	g := e.Group("/api", func(c *Ctx, next Handler) error { hit = true; return next(c) })
	g.Static("/assets", dir)

	if rec := doReq(e, "GET", "/api/assets/b.txt", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !hit {
		t.Fatal("group middleware skipped for static resource")
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

// TestGracefulShutdownDrainsInflight 验证 drain 由 net/http 承担：
// 在途请求在 Shutdown 期间正常完成，不被截断（kernel.Dispose 不做这件事）。
func TestGracefulShutdownDrainsInflight(t *testing.T) {
	e, _ := newTestEngine(t)
	started := make(chan struct{})
	release := make(chan struct{})
	e.GET("/slow", func(c *Ctx) error {
		close(started)
		<-release
		return c.Text(http.StatusOK, "drained")
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: e}
	go func() { _ = srv.Serve(ln) }()

	respCh := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			respCh <- "ERR: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		respCh <- string(b)
	}()

	<-started

	shutdownDone := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(shutdownDone)
	}()

	// Shutdown 不应在在途请求结束前返回
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while the request was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case got := <-respCh:
		if got != "drained" {
			t.Fatalf("response = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request did not complete")
	}
	select {
	case <-shutdownDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not complete after drain")
	}
}
