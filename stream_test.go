package web

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 流式响应的覆盖：v1 功能面第 11 条（SSE / chunked / 大文件）。
// 入口是 `Ctx.Writer()` + `Ctx.Flush()`——此前两者都没有测试。

// TestCtxFlushStreamsIncrementally 是流式的端到端证据：第一段必须在 handler
// **仍然挂起**时就到达客户端，这只有真 flush 才做得到。
func TestCtxFlushStreamsIncrementally(t *testing.T) {
	e, _ := newTestEngine(t)
	gate := make(chan struct{})

	e.GET("/sse", func(c *Ctx) error {
		c.SetHeader("Content-Type", "text/event-stream")
		w := c.Writer()
		if _, err := w.Write([]byte("data: 1\n\n")); err != nil {
			return err
		}
		if err := c.Flush(); err != nil {
			return err
		}
		<-gate // 挂住：flush 不生效时客户端读不到第一段
		_, err := w.Write([]byte("data: 2\n\n"))
		return err
	})

	srv := httptest.NewServer(e)
	defer srv.Close()

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
		t.Fatalf("第一段的空行 = %q, %v", blank, err)
	}

	close(gate) // 放行 handler

	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rest), "data: 2") {
		t.Fatalf("第二段 = %q，want 含 data: 2", string(rest))
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

// TestResponseWriterKeepsFlusherCapability：框架的包装器不吞掉底层能力——
// `Ctx.Writer()` 仍是 http.Flusher（内嵌接口的方法集提升），且 flush 真的落到
// 底层 writer。
func TestResponseWriterKeepsFlusherCapability(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/f", func(c *Ctx) error {
		f, ok := c.Writer().(http.Flusher)
		if !ok {
			return Internal("no_flusher", nil)
		}
		f.Flush() // 首次 flush 之前没有任何写入：应落 200
		return nil
	})

	rec := doReq(e, "GET", "/f", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，want 200", rec.Code)
	}
	if !rec.Flushed {
		t.Fatal("flush 没有到达底层 writer")
	}
}
