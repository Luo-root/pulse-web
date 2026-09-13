package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Luo-root/pulse/kernel"
)

// assertLoudError 断言框架给出的是**明确映射过的错误响应**，而不是静默空响应。
//
// 只断言状态码是不够的：panic、模板执行失败、写响应失败都会得到 500——
// 状态码区分不了「明确错误」与「静默 500」。必须落到响应体上的 errorPayload 信封。
func assertLoudError(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Body.Len() == 0 {
		t.Fatal("响应体为空：错误被吞掉了，没有经过统一错误映射")
	}
	var body errorPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体不是错误信封: %v（%q）", err, rec.Body.String())
	}
	if body.Error.Code == "" || body.Error.Message == "" {
		t.Fatalf("错误信封缺 code/message: %+v", body.Error)
	}
}

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
	assertLoudError(t, rec)
}

// TestHTMLMissingTemplateIsLoud 钉住「已配置模板、但模板名不存在」这条路径。
//
// 它曾经落到 200 + 空 body：WriteHeader 先于 ExecuteTemplate 生效，
// 「已写响应不被覆盖」规则把错误吞成一个看起来正常的空页。
// 写错模板名是开发期第一天就会遇到的错误，这条比「未配置模板」更常见。
func TestHTMLMissingTemplateIsLoud(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.html"), []byte("Hi {{.Name}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ := newTestEngine(t, WithTemplates(TemplateConfig{Root: dir}))
	e.GET("/page", func(c *Ctx) error {
		return c.HTML(http.StatusOK, "nope.html", H{"Name": "web"})
	})

	rec := doReq(e, "GET", "/page", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500（模板名写错必须是明确错误，不能是 200 空页）", rec.Code)
	}
	assertLoudError(t, rec)
}

// boomData 的 Boom 方法恒返回错误，用来制造确定性的**执行期**（非解析期）失败。
type boomData struct{}

func (boomData) Boom() (string, error) { return "", errors.New("boom") }

// TestHTMLExecutionErrorIsLoud 钉住执行期报错：模板解析成功、渲染途中失败，
// 同样必须在写响应头之前返回 error。
func TestHTMLExecutionErrorIsLoud(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "boom.html"), []byte("{{.Boom}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ := newTestEngine(t, WithTemplates(TemplateConfig{Root: dir}))
	e.GET("/page", func(c *Ctx) error {
		return c.HTML(http.StatusOK, "boom.html", boomData{})
	})

	rec := doReq(e, "GET", "/page", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500（执行期报错必须仍是明确错误）", rec.Code)
	}
	assertLoudError(t, rec)
}

// TestHTMLDevReloadConcurrent 覆盖 DevReload 分支的并发渲染：该分支每请求
// 走一次 ParseGlob + 写锁，是模板实现里唯一有写并发的地方。
func TestHTMLDevReloadConcurrent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.html"), []byte("Hi {{.Name}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	e, _ := newTestEngine(t, WithTemplates(TemplateConfig{Root: dir, DevReload: true}))
	e.GET("/page", func(c *Ctx) error {
		return c.HTML(http.StatusOK, "hello.html", H{"Name": "web"})
	})

	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := doReq(e, "GET", "/page", nil)
			if rec.Code != http.StatusOK || rec.Body.String() != "Hi web" {
				t.Errorf("got %d %q", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
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
