package web

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/observability"
)

// 流式响应的覆盖：v1 功能面第 11 条（SSE / chunked / 大文件）。
// 入口是 `Ctx.Writer()` + `Ctx.Flush()`——此前两者都没有测试。

// waitRecord 等 Engine 收尾把记录写进 Sink（响应读完与记录落盘之间有极小的时序差）。
func waitRecord(t *testing.T, sink *observability.MemorySink, event string) observability.Record {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if rec, ok := findRecord(sink, event); ok {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("等不到 %s 记录", event)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestCtxFlushStreamsIncrementally 是流式的端到端证据：第一段必须在 handler
// **仍然挂起**时就到达客户端，这只有真 flush 才做得到。
func TestCtxFlushStreamsIncrementally(t *testing.T) {
	e, sink := newTestEngine(t)

	var once sync.Once
	gate := make(chan struct{})
	release := func() { once.Do(func() { close(gate) }) }

	e.GET("/sse", func(c *Ctx) error {
		c.SetHeader("Content-Type", "text/event-stream")
		w := c.Writer()
		if _, err := w.Write([]byte("data: 1\n\n")); err != nil {
			return err
		}
		if err := c.Flush(); err != nil {
			return err
		}
		// 挂住，好让「第一段已到达客户端」可证——flush 没生效时客户端根本读不到它。
		// 客户端断开也要收工（真实 SSE handler 就该这么写）。
		select {
		case <-gate:
		case <-c.Request().Context().Done():
			return nil
		}
		_, err := w.Write([]byte("data: 2\n\n"))
		return err
	})

	srv := httptest.NewServer(e)
	defer srv.Close()
	// release 必须比 srv.Close **先**执行：断言失败时 httptest.Server.Close 会等
	// 未完成请求，不解锁就会从「干净失败」变成挂到 go test 超时——而 flush 坏掉
	// 正是这条用例最该抓的失败。defer 是 LIFO，晚注册 = 先执行。
	defer release()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(srv.URL + "/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q，want text/event-stream（Writer 不应覆盖它）", ct)
	}

	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("第一段没读到（flush 没生效？）：%v", err)
	}
	if line != "data: 1\n" {
		t.Fatalf("第一段 = %q，want %q", line, "data: 1\n")
	}
	if blank, err := br.ReadString('\n'); err != nil || blank != "\n" {
		t.Fatalf("第一段的空行 = %q，%v", blank, err)
	}

	release() // 放行 handler

	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "data: 2") {
		t.Fatalf("第二段 = %q，want 含 data: 2", string(rest))
	}

	// 「仍是一条 AccessLog」：流式路径的状态码与体积照常采集——
	// 两段各 9 字节 = 18，状态码是首刷落下的 200。
	rec := waitRecord(t, sink, eventHTTPReq)
	if rec.Status != "200" {
		t.Fatalf("访问记录 Status = %q，want 200", rec.Status)
	}
	if got := attrsOf(rec)[attrHTTPBodySize]; got != int64(18) {
		t.Fatalf("http.response.body.size = %v，want 18（两段各 9 字节）", got)
	}
}

// plainWriter 是最小 ResponseWriter：只实现必需三方法，**不**实现 http.Flusher。
type plainWriter struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (w *plainWriter) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}
func (w *plainWriter) WriteHeader(code int)        { w.code = code }
func (w *plainWriter) Write(b []byte) (int, error) { return w.buf.Write(b) }

// TestFlushFirstWriteUsesStatus 验证首刷落 Status() 设置的状态码：此前 Flush
// 硬编码 200，`c.Status(201); c.Flush()` 会写出 200，与 Status 的公开语义打架（#33）。
func TestFlushFirstWriteUsesStatus(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/s", func(c *Ctx) error {
		c.Status(http.StatusCreated)
		return c.Flush()
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/s")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d，want 201", resp.StatusCode)
	}
}

// TestFlushFirstWriteDefaultsTo200：未设 Status 时首刷仍是 200。
func TestFlushFirstWriteDefaultsTo200(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/s", func(c *Ctx) error { return c.Flush() })
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/s")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，want 200", resp.StatusCode)
	}
}

// TestStatusAfterFlushIgnored：首刷之后响应头已发出——再调 Status 不会改变
// 已落定的状态码（HTTP 固有语义，文档写明）。
func TestStatusAfterFlushIgnored(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/s", func(c *Ctx) error {
		c.Status(http.StatusCreated)
		if err := c.Flush(); err != nil {
			return err
		}
		c.Status(http.StatusInternalServerError)
		return nil
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/s")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d，want 201（首刷已落定）", resp.StatusCode)
	}
}

// TestStatusAppliedOnFirstWrite：首刷不只是 Flush —— 直接往包装器写字节同样是
// 首刷，Status() 的提示必须在这里就落定。
//
// review 提出（PR #35）：流式 handler 的自然顺序是「先写第一段、再 Flush」，若只有
// Flush 吃提示，`Status(201)` 会被第一次 Write 的隐式 200 吃掉，README 那句
// 「在首刷之前设好状态码」也就给不出有效指引。
func TestStatusAppliedOnFirstWrite(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/s", func(c *Ctx) error {
		c.Status(http.StatusCreated)
		if _, err := c.Writer().Write([]byte("x")); err != nil {
			return err
		}
		return c.Flush()
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/s")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d，want 201（Write 也是首刷）", resp.StatusCode)
	}
}

// TestStatusAfterWriteIgnored：Write 落定后与 Flush 同规则——状态码不再可改。
func TestStatusAfterWriteIgnored(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/s", func(c *Ctx) error {
		c.Status(http.StatusCreated)
		if _, err := c.Writer().Write([]byte("x")); err != nil {
			return err
		}
		c.Status(http.StatusInternalServerError)
		return nil
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/s")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d，want 201（Write 已落定）", resp.StatusCode)
	}
}

// TestCtxFlushWithoutFlusherReturnsError：底层不支持 Flush 时给出明确 error，
// 而不是静默当作成功。
func TestCtxFlushWithoutFlusherReturnsError(t *testing.T) {
	e, _ := newTestEngine(t)
	var flushErr error
	e.GET("/plain", func(c *Ctx) error {
		flushErr = c.Flush()
		return c.Text(http.StatusOK, "ok")
	})

	w := &plainWriter{}
	e.ServeHTTP(w, httptest.NewRequest("GET", "/plain", nil))

	if flushErr == nil {
		t.Fatal("不实现 http.Flusher 的 writer 上 Flush 应返回错误，得到 nil")
	}
	if !strings.Contains(flushErr.Error(), "Flush") {
		t.Fatalf("错误文案应点明 Flush 不支持：%v", flushErr)
	}
}

// TestResponseWriterKeepsFlusherCapability 钉住包装器的**能力边界**：它只显式实现
// `http.Flusher`；`FlushError` / `Hijacker` / `Pusher` / `SetWriteDeadline` 都不透出。
//
// 边界要钉全，是因为「包装器＝底层能力都在」是错的，而类型断言静默失败很难查：
// 内嵌 `http.ResponseWriter` 只提升 `Header` / `Write` / `WriteHeader`，Flusher 来自
// `responseWriter` 自己实现的 `Flush()`。要升级协议（WebSocket）得走 `Wrap` 拿原始 writer。
//
// 用真实服务器：`httptest.ResponseRecorder` 本来就不支持 Hijacker，
// 测不出「包装器把底层有的能力过滤掉了」这件事。
func TestResponseWriterKeepsFlusherCapability(t *testing.T) {
	e, _ := newTestEngine(t)

	type caps struct {
		flusher    bool
		flushError bool
		hijacker   bool
		pusher     bool
		rcFlush    error
		rcDeadline error
	}
	got := make(chan caps, 1)

	e.GET("/caps", func(c *Ctx) error {
		w := c.Writer()
		var cs caps
		_, cs.flusher = w.(http.Flusher)
		_, cs.flushError = w.(interface{ FlushError() error })
		_, cs.hijacker = w.(http.Hijacker)
		_, cs.pusher = w.(http.Pusher)
		rc := http.NewResponseController(w)
		cs.rcFlush = rc.Flush()
		cs.rcDeadline = rc.SetWriteDeadline(time.Now().Add(time.Second))
		got <- cs
		return nil
	})

	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/caps")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，want 200", resp.StatusCode)
	}

	cs := <-got
	if !cs.flusher {
		t.Fatal("http.Flusher 必须可用——这是流式的前提")
	}
	if cs.flushError {
		t.Fatal("FlushError 不该透出：ResponseController.Flush() 只能退回 Flusher.Flush()，flush 失败拿不到 error")
	}
	if cs.hijacker {
		t.Fatal("Hijacker 不该透出：升级协议请走 Wrap 拿原始 writer")
	}
	if cs.pusher {
		t.Fatal("Pusher 不该透出")
	}
	if cs.rcFlush != nil {
		t.Fatalf("ResponseController.Flush() = %v，want nil", cs.rcFlush)
	}
	if !errors.Is(cs.rcDeadline, http.ErrNotSupported) {
		t.Fatalf("ResponseController.SetWriteDeadline() = %v，want http.ErrNotSupported", cs.rcDeadline)
	}
}
