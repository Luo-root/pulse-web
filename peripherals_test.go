package web

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Luo-root/pulse/kernel"
)

func TestDetachSharesTraceAndRootAccess(t *testing.T) {
	e, sink := newTestEngine(t)
	key := kernel.NewServiceKey[string]("detach.svc")
	if _, err := kernel.Provide(e.Root(), key, "svc-value"); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var gotTrace, gotSvc string
	e.GET("/job", func(c *Ctx) error {
		bg := c.Detach()
		go func() {
			defer close(done)
			gotTrace = bg.TraceID
			gotSvc = bg.MustService(key)
			bg.Observe("job.done", nil)
		}()
		return c.JSON(http.StatusAccepted, map[string]string{"ok": "1"})
	})

	rec := doReq(e, "GET", "/job", nil)
	<-done

	if want := rec.Header().Get("X-Trace-Id"); gotTrace != want {
		t.Fatalf("detached trace %q != request %q", gotTrace, want)
	}
	if gotSvc != "svc-value" {
		t.Fatalf("detached service = %q", gotSvc)
	}
	biz, ok := findRecord(sink, "job.done")
	if !ok {
		t.Fatal("no job.done record")
	}
	if biz.TraceID != gotTrace {
		t.Fatalf("business record trace = %q, want %q", biz.TraceID, gotTrace)
	}
}

func TestDetachedRootDeriveIsIndependent(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/x", func(c *Ctx) error {
		bg := c.Detach()
		scope, err := bg.Root.Derive()
		if err != nil {
			return err
		}
		defer scope.Dispose()
		// 请求 scope 已随请求结束，独立 scope 仍可登记 Effect
		if _, err := scope.Effect(func() (func(), error) { return func() {}, nil }); err != nil {
			return err
		}
		return c.Text(http.StatusOK, "ok")
	})
	if rec := doReq(e, "GET", "/x", nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestTemplatesRender(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.html"), []byte("Hi {{.Name}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ := newTestEngine(t, WithTemplates(TemplateConfig{Root: dir}))
	e.GET("/page", func(c *Ctx) error {
		return c.HTML(http.StatusOK, "hello.html", H{"Name": "web"})
	})

	rec := doReq(e, "GET", "/page", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "Hi web" {
		t.Fatalf("got %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Fatal("missing content-type")
	}
}

func TestHTMLWithoutTemplatesIsLoud(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/page", func(c *Ctx) error { return c.HTML(http.StatusOK, "x.html", nil) })

	rec := doReq(e, "GET", "/page", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (explicit error, not silent)", rec.Code)
	}
}

func TestDebugEndpointIsOptIn(t *testing.T) {
	e, _ := newTestEngine(t)
	if rec := doReq(e, "GET", "/debug/pulse", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("debug must be opt-in, got %d", rec.Code)
	}

	e.Debug("/debug/pulse")
	rec := doReq(e, "GET", "/debug/pulse", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestTrustedTraceHeaderToggle(t *testing.T) {
	const tp = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	const want = "4bf92f3577b34da6a3ce929d0e0e4736"

	trusted, _ := newTestEngine(t)
	trusted.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, c.TraceID()) })
	if got := doReq(trusted, "GET", "/t", nil, "Traceparent", tp).Header().Get("X-Trace-Id"); got != want {
		t.Fatalf("default mode should adopt upstream trace, got %q", got)
	}

	untrusted, _ := newTestEngine(t, WithTrustedTraceHeader(false))
	untrusted.GET("/t", func(c *Ctx) error { return c.Text(http.StatusOK, c.TraceID()) })
	rec := doReq(untrusted, "GET", "/t", nil, "Traceparent", tp)
	if got := rec.Header().Get("X-Trace-Id"); got == want {
		t.Fatal("untrusted mode must ignore the inbound header")
	} else if len(got) != 32 {
		t.Fatalf("generated trace id = %q (len %d)", got, len(got))
	}
}
