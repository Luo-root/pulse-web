package web

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse/observability"
)

// 本文件覆盖 context.go：Ctx 的便利 API（Cookie / Redirect / Blob / NoContent / File …）与响应写出时序（首刷落码、Flush、Flusher 能力）。

// 便利 API 的覆盖（#69）：每个方法都要过一遍既有契约——首刷锁定状态码、
// 已落码之后不覆盖、未落码时错误映射照常接管。

func TestContextIsRequestContext(t *testing.T) {
	e, _ := newTestEngine(t)

	var got, want any
	e.GET("/ctx", func(c *Ctx) error {
		got, want = c.Context(), c.Request().Context()
		return c.Text(200, "ok")
	})

	if rec := doReq(e, "GET", "/ctx", nil); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if got != want {
		t.Fatalf("Context() = %v，Request().Context() = %v，应为同一个", got, want)
	}
}

func TestCookieReadsAndMisses(t *testing.T) {
	e, _ := newTestEngine(t)

	var value string
	var missErr error
	e.GET("/c", func(c *Ctx) error {
		ck, err := c.Cookie("session")
		if err != nil {
			return err
		}
		value = ck.Value
		_, missErr = c.Cookie("absent")
		return c.Text(200, "ok")
	})

	if rec := doReq(e, "GET", "/c", nil, "Cookie", "session=abc123"); rec.Code != 200 {
		t.Fatalf("status = %d，body = %s", rec.Code, rec.Body.String())
	}
	if value != "abc123" {
		t.Fatalf("Cookie(session) = %q，应为 abc123", value)
	}
	if !errors.Is(missErr, http.ErrNoCookie) {
		t.Fatalf("缺失 cookie 的 error = %v，应为 http.ErrNoCookie", missErr)
	}
}

// SetCookie 是**追加**语义：同名 cookie 写两次是两条头，不是覆盖。
func TestSetCookieAppends(t *testing.T) {
	e, _ := newTestEngine(t)

	e.GET("/c", func(c *Ctx) error {
		c.SetCookie(&http.Cookie{Name: "a", Value: "1"})
		c.SetCookie(&http.Cookie{Name: "a", Value: "2"})
		return c.NoContent(204)
	})

	rec := doReq(e, "GET", "/c", nil)
	got := rec.Result().Header.Values("Set-Cookie")
	if len(got) != 2 || !strings.HasPrefix(got[0], "a=1") || !strings.HasPrefix(got[1], "a=2") {
		t.Fatalf("Set-Cookie = %q，同名应是两条（追加而非覆盖）", got)
	}
}

// 边界：首刷之后写 cookie 送不出去——响应头已经发出，godoc 那句「必须在首刷之前
// 调用」就是这个意思（标准库的 SetCookie 不报错，所以这条只能靠测试钉住）。
func TestSetCookieAfterFirstWriteIsNotDelivered(t *testing.T) {
	e, _ := newTestEngine(t)

	e.GET("/c", func(c *Ctx) error {
		if err := c.Text(200, "written"); err != nil {
			return err
		}
		c.SetCookie(&http.Cookie{Name: "late", Value: "1"})
		return nil
	})

	rec := doReq(e, "GET", "/c", nil)
	if got := rec.Result().Header.Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("首刷之后写的 Set-Cookie 不该送达：%q", got)
	}
	if rec.Body.String() != "written" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestRedirectByMethod(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/r", func(c *Ctx) error { return c.Redirect(302, "/next?a=b") })
	e.POST("/r", func(c *Ctx) error { return c.Redirect(301, "/next") })

	rec := doReq(e, "GET", "/r", nil)
	if rec.Code != 302 || rec.Header().Get("Location") != "/next?a=b" {
		t.Fatalf("GET：status = %d，Location = %q", rec.Code, rec.Header().Get("Location"))
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("GET：Content-Type = %q，应为标准库那一段提示体的类型", ct)
	}
	if !strings.Contains(rec.Body.String(), `href="/next?a=b"`) {
		t.Fatalf("GET：body = %q，应含标准库的提示体", rec.Body.String())
	}

	rec = doReq(e, "POST", "/r", nil)
	if rec.Code != 301 || rec.Body.Len() != 0 {
		t.Fatalf("POST：status = %d，body = %q（标准库对非 GET 不写提示体）", rec.Code, rec.Body.String())
	}
}

// 相对路径按**请求路径**补成绝对——这是 http.Redirect 的语义，不是拼字符串。
func TestRedirectRelativeURLResolvesAgainstRequestPath(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/a/b/old", func(c *Ctx) error { return c.Redirect(302, "next") })

	rec := doReq(e, "GET", "/a/b/old", nil)
	if got := rec.Header().Get("Location"); got != "/a/b/next" {
		t.Fatalf("Location = %q，应为 /a/b/next", got)
	}
}

// 非 3xx 不做校验（与 http.Redirect 同）：传 200 也是照写。
func TestRedirectDoesNotValidateCode(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/r", func(c *Ctx) error { return c.Redirect(200, "/next") })

	if rec := doReq(e, "GET", "/r", nil); rec.Code != 200 {
		t.Fatalf("status = %d，应照写 200（不校验范围）", rec.Code)
	}
}

// 边界：首刷之后 Redirect 不再改变已落定的状态码——与 Status 同一条规则。
// （Location 头仍会进 header map，但响应头早已发出、到不了客户端；body 由标准库追加。）
func TestRedirectAfterFirstWriteKeepsSettledStatus(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/r", func(c *Ctx) error {
		if err := c.Text(201, "written"); err != nil {
			return err
		}
		c.Redirect(302, "/elsewhere")
		return nil
	})

	rec := doReq(e, "GET", "/r", nil)
	if rec.Code != 201 {
		t.Fatalf("status = %d，首刷已落定 201，Redirect 不该改它", rec.Code)
	}
	if !strings.HasPrefix(rec.Body.String(), "written") {
		t.Fatalf("body = %q，应保留已写出的内容", rec.Body.String())
	}
}

func TestNoContentSettlesEmptyResponse(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/nc", func(c *Ctx) error { return c.NoContent(204) })
	e.GET("/nm", func(c *Ctx) error { return c.NoContent(304) })

	for _, tc := range []struct {
		path string
		want int
	}{{"/nc", 204}, {"/nm", 304}} {
		rec := doReq(e, "GET", tc.path, nil)
		if rec.Code != tc.want || rec.Body.Len() != 0 {
			t.Fatalf("%s：status = %d（应为 %d），body = %q", tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// NoContent 与「Status + return nil」是同一件事的两种写法，**响应逐字节相同**：
// NoContent 补的是调用形态（能直接 return），不是新语义。
//
// 现存用法直接印证了这个缺口——`TestStatusOnlyResponse` 写的就是两行
// `c.Status(204); return nil`：`Status` 返回 `*Ctx`（它是「设置状态码」这个中间
// 动作，后面通常还要写 body），拿不到一行收尾的形状。
func TestNoContentEqualsStatusPlusReturnNil(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/two-line", func(c *Ctx) error { c.Status(204); return nil })
	e.GET("/one-line", func(c *Ctx) error { return c.NoContent(204) })

	two := doReq(e, "GET", "/two-line", nil)
	one := doReq(e, "GET", "/one-line", nil)

	if two.Code != http.StatusNoContent || two.Body.Len() != 0 {
		t.Fatalf("两行写法：status = %d，body = %q", two.Code, two.Body.String())
	}
	if two.Code != one.Code || two.Body.String() != one.Body.String() {
		t.Fatalf("两行 %d/%q vs 一行 %d/%q", two.Code, two.Body.String(), one.Code, one.Body.String())
	}

	// 响应头也逐项比（用 Result 的快照，避开 recorder 那张活 map）。
	h2, h1 := two.Result().Header, one.Result().Header
	if len(h2) != len(h1) {
		t.Fatalf("响应头数量不同：%v vs %v", h2, h1)
	}
	for k, v2 := range h2 {
		if v1 := h1[k]; len(v1) != len(v2) {
			t.Fatalf("响应头 %s 不同：%v vs %v", k, v2, v1)
		}
	}
}

// NoContent 只**设置**状态码、不落码——所以它之后返回的 error 仍被错误映射接管。
// 这条与 TestWrittenResponseIsNotOverwritten 互为反面：区别只在有没有真的写出去。
func TestNoContentKeepsErrorMapping(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/x", func(c *Ctx) error {
		c.NoContent(204)
		return NotFound("missing", nil)
	})

	rec := doReq(e, "GET", "/x", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d，应为 404——NoContent 不该吞掉之后返回的错误", rec.Code)
	}
}

// 反面：一旦首刷落定，之后返回的 error 不再覆盖响应。
func TestNoContentThenWriteSwallowsLaterError(t *testing.T) {
	e, _ := newTestEngine(t)
	e.GET("/x", func(c *Ctx) error {
		c.NoContent(204)
		if err := c.Text(200, "written"); err != nil {
			return err
		}
		return NotFound("later", nil)
	})

	rec := doReq(e, "GET", "/x", nil)
	if rec.Code != 200 || rec.Body.String() != "written" {
		t.Fatalf("status = %d，body = %q，应为 200 + written（已落码，后续 error 被吞）", rec.Code, rec.Body.String())
	}
}

// Blob 的 Content-Type 原样写出：不补 charset、不嗅探。
func TestBlobWritesRawBytes(t *testing.T) {
	e, _ := newTestEngine(t)
	payload := []byte{0x00, 0x01, 0xff, 'a'}

	e.GET("/blob", func(c *Ctx) error {
		return c.Blob(206, "application/octet-stream", payload)
	})

	rec := doReq(e, "GET", "/blob", nil)
	if rec.Code != 206 {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q，应原样写出", ct)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Fatalf("body = %v，应为 %v", rec.Body.Bytes(), payload)
	}
}

// Blob 传空 Content-Type 时写出的是**一条空头**，标准库**不会**替它嗅探；反过来，
// 整个不设头才会触发嗅探。
//
// 这条必须用真 server 测：嗅探发生在 net/http 服务端的 writeHeader 里，
// `httptest.ResponseRecorder` 根本不走那段逻辑——用 Recorder 测会得出"两个都空"的
// 假结论（实测过）。
func TestBlobEmptyContentTypeIsEmptyNotSniffed(t *testing.T) {
	// 故意用 HTML 载荷：真发生嗅探时它会被认成 text/html，一眼看得出。
	payload := []byte("<html><body>x</body></html>")

	e, _ := newTestEngine(t)
	e.GET("/blob-empty", func(c *Ctx) error { return c.Blob(200, "", payload) })
	e.GET("/raw", func(c *Ctx) error {
		_, err := c.Writer().Write(payload)
		return err
	})

	srv := httptest.NewServer(e)
	defer srv.Close()

	// 空串：头在、值为空 → 不嗅探（裸 map 才分得出"头存在且为空"与"头不存在"）
	resp, err := http.Get(srv.URL + "/blob-empty")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if v, ok := resp.Header["Content-Type"]; !ok || len(v) != 1 || v[0] != "" {
		t.Fatalf("Blob(200, \"\") 的 Content-Type = %v，应为一条空头（不嗅探）", resp.Header["Content-Type"])
	}

	// 对照：整个不设头 → 嗅探发生。这就是「空串比不设安全」那句话的依据。
	resp2, err := http.Get(srv.URL + "/raw")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if got := resp2.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("不设头时应被嗅探成 text/html，得到 %q", got)
	}
}

func TestFileServesDiskFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello.txt")
	if err := os.WriteFile(path, []byte("hello file"), 0o600); err != nil {
		t.Fatal(err)
	}

	e, _ := newTestEngine(t)
	e.GET("/f", func(c *Ctx) error {
		return c.File(path)
	})

	rec := doReq(e, "GET", "/f", nil)
	if rec.Code != 200 || rec.Body.String() != "hello file" {
		t.Fatalf("status = %d，body = %q", rec.Code, rec.Body.String())
	}
}

// 文件缺失时是**标准库自己写的** 404——纯文本错误页，不是框架错误映射的 JSON。
func TestFileMissingIsStdlibNotFound(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.txt")

	e, _ := newTestEngine(t)
	e.GET("/f", func(c *Ctx) error {
		return c.File(missing)
	})

	rec := doReq(e, "GET", "/f", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d，应为标准库写的 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatalf("body = %q，应为标准库的错误页（不经框架错误映射）", rec.Body.String())
	}
}

// 非 ASCII 文件名走 RFC 2231（`filename*=utf-8”…`）——手拼引号带不出中文。
func TestAttachmentNonASCIIFilenameUsesRFC2231(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(path, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	e, _ := newTestEngine(t)
	e.GET("/dl", func(c *Ctx) error {
		return c.Attachment(path, "2026 年 9 月报告.csv")
	})

	rec := doReq(e, "GET", "/dl", nil)
	if rec.Code != 200 || rec.Body.String() != "a,b\n1,2\n" {
		t.Fatalf("status = %d，body = %q", rec.Code, rec.Body.String())
	}
	got := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(got, "attachment;") {
		t.Fatalf("Content-Disposition = %q，应为 attachment", got)
	}
	_, encoded, ok := strings.Cut(got, "filename*=utf-8''")
	if !ok {
		t.Fatalf("Content-Disposition = %q，应含 RFC 2231 的 filename*", got)
	}
	decoded, err := url.PathUnescape(encoded)
	if err != nil {
		t.Fatalf("filename* 解不开：%v（原文 %q）", err, got)
	}
	if decoded != "2026 年 9 月报告.csv" {
		t.Fatalf("filename* 解码 = %q，应为原文件名", decoded)
	}
}

func TestAttachmentASCIIFilenameStaysPlain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.csv")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	e, _ := newTestEngine(t)
	e.GET("/dl", func(c *Ctx) error {
		return c.Attachment(path, "report.csv")
	})

	rec := doReq(e, "GET", "/dl", nil)
	if got := rec.Header().Get("Content-Disposition"); got != "attachment; filename=report.csv" {
		t.Fatalf("Content-Disposition = %q", got)
	}
}

// 流式响应的覆盖：设计验收标准『流式响应可用』条（SSE / chunked / 大文件）。
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

func (w *plainWriter) WriteHeader(code int) { w.code = code }

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
