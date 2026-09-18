package web

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件覆盖 bodylimit.go：路由 / 分组的请求体闸门、边界值，与 TooLarge 错误的映射。
// 边界：全局上限（WithMaxBodyBytes）走绑定路径那一侧归 bind_test.go；这里只拿它当口径对照。

// bodyLimitReq 发一个带指定 body 的 POST，body 长度即 Content-Length。
func bodyLimitReq(e *Engine, path string, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

// echoBodyLen 是只报告 body 长度的 handler：用例真正关心的是闸门，不是内容。
func echoBodyLen(c *Ctx) error {
	b, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return err
	}
	return c.Text(200, string(rune('0'+len(b)%10)))
}

func TestBodyLimitOnGroup(t *testing.T) {
	// 验收 1：挂分组——组内 413，组外不受影响
	e, _ := newTestEngine(t)
	g := e.Group("/upload", BodyLimit(1<<10))
	g.POST("/a", echoBodyLen)
	e.POST("/other", echoBodyLen)

	if rec := bodyLimitReq(e, "/upload/a", strings.Repeat("x", 2<<10)); rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "body_too_large" {
		t.Fatalf("组内超限：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
	if rec := bodyLimitReq(e, "/upload/a", strings.Repeat("x", 1<<10)); rec.Code != 200 {
		t.Fatalf("组内未超限：status = %d", rec.Code)
	}
	if rec := bodyLimitReq(e, "/other", strings.Repeat("x", 2<<10)); rec.Code != 200 {
		t.Fatalf("组外不该受限：status = %d，body = %s", rec.Code, rec.Body.String())
	}
}

func TestBodyLimitOnRoute(t *testing.T) {
	// 验收 2：单条路由的闸与分组的闸叠加时，更严的生效
	e, _ := newTestEngine(t)
	g := e.Group("/api", BodyLimit(4<<10))
	g.POST("/wide", echoBodyLen)                    // 只吃分组的 4 KiB
	g.POST("/tight", echoBodyLen, BodyLimit(1<<10)) // 路由级再收紧到 1 KiB

	if rec := bodyLimitReq(e, "/api/wide", strings.Repeat("x", 2<<10)); rec.Code != 200 {
		t.Fatalf("分组闸 4 KiB 应放行 2 KiB：status = %d", rec.Code)
	}
	if rec := bodyLimitReq(e, "/api/tight", strings.Repeat("x", 2<<10)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("路由闸 1 KiB 应拦住 2 KiB：status = %d", rec.Code)
	}
	if rec := bodyLimitReq(e, "/api/tight", strings.Repeat("x", 512)); rec.Code != 200 {
		t.Fatalf("路由闸 1 KiB 应放行 512 B：status = %d", rec.Code)
	}
}

func TestBodyLimitBoundary(t *testing.T) {
	// 验收 3：恰好 n 通过，n+1 起 413（与 WithMaxBodyBytes 同口径）
	const n = 1 << 10
	e, _ := newTestEngine(t)
	e.POST("/b", echoBodyLen, BodyLimit(n))

	if rec := bodyLimitReq(e, "/b", strings.Repeat("x", n)); rec.Code != 200 {
		t.Fatalf("恰好 %d 字节应通过：status = %d，body = %s", n, rec.Code, rec.Body.String())
	}
	if rec := bodyLimitReq(e, "/b", strings.Repeat("x", n+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d 字节应 413：status = %d", n+1, rec.Code)
	}
}

func TestBodyLimitChunked(t *testing.T) {
	// 验收 4：声明未知（chunked）时由读取闸门兜底
	e, _ := newTestEngine(t)
	e.POST("/c", echoBodyLen, BodyLimit(1<<10))

	req := httptest.NewRequest("POST", "/c", strings.NewReader(strings.Repeat("x", 2<<10)))
	req.ContentLength = -1 // 模拟 chunked：预检拿不到声明值，只能靠读取闸门
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "body_too_large" {
		t.Fatalf("chunked 超限：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
}

func TestBodyLimitCannotLoosen(t *testing.T) {
	// 验收 5：只能收紧——路由级比引擎级宽，仍被引擎级掐住
	e, _ := newTestEngine(t, WithMaxBodyBytes(1<<10))
	e.POST("/loose", echoBodyLen, BodyLimit(1<<20))
	if rec := bodyLimitReq(e, "/loose", strings.Repeat("x", 2<<10)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("引擎级 1 KiB 更严：status = %d（不该被路由级的 1 MiB 放宽）", rec.Code)
	}

	// 反向：引擎级宽、路由级严 → 路由级生效
	e2, _ := newTestEngine(t, WithMaxBodyBytes(1<<20))
	e2.POST("/tight", echoBodyLen, BodyLimit(1<<10))
	if rec := bodyLimitReq(e2, "/tight", strings.Repeat("x", 2<<10)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("路由级 1 KiB 更严：status = %d", rec.Code)
	}
}

func TestBodyLimitCauseType(t *testing.T) {
	// 验收 6：自定义 ErrorHandler 用 errors.As(*http.MaxBytesError) 同时接住
	// 预检与读取两条路径——框架的 413 与用户自己包的是同一型
	caught := 0
	custom := func(c *Ctx, err error) error {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			caught++
			return c.Text(http.StatusRequestEntityTooLarge, "caught-maxbytes")
		}
		return c.Text(http.StatusInternalServerError, "other")
	}
	e, _ := newTestEngine(t, WithErrorHandler(custom))
	e.POST("/pre", echoBodyLen, BodyLimit(1<<10)) // 预检：声明即超限

	// 预检路径
	rec := bodyLimitReq(e, "/pre", strings.Repeat("x", 2<<10))
	if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != "caught-maxbytes" {
		t.Fatalf("预检路径未被 errors.As 接住：status = %d，body = %q", rec.Code, rec.Body.String())
	}

	// 读取路径（声明未知）
	req := httptest.NewRequest("POST", "/pre", strings.NewReader(strings.Repeat("x", 2<<10)))
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || rec.Body.String() != "caught-maxbytes" {
		t.Fatalf("读取路径未被 errors.As 接住：status = %d，body = %q", rec.Code, rec.Body.String())
	}

	if caught != 2 {
		t.Fatalf("两条路径都应命中 *http.MaxBytesError，实际命中 %d 次", caught)
	}
}

func TestBodyLimitPrecheckRejectsDeclaredOverLimit(t *testing.T) {
	// 预检的语义是「**声明**即超限，不读 body」：声明 4 KiB、实际只发 8 字节也要 413，
	// 且 handler 一个字节都不该读到。
	//
	// 这条守卫是预检路径唯一的守卫——没有它，去掉预检所有用例仍会绿（读取闸门
	// 会兜出同样的 413，只是先读了 body）。
	e, _ := newTestEngine(t)
	read := -1
	e.POST("/d", func(c *Ctx) error {
		b, err := io.ReadAll(c.Request().Body)
		read = len(b)
		if err != nil {
			return err
		}
		return c.Text(200, "ok")
	}, BodyLimit(1<<10))

	req := httptest.NewRequest("POST", "/d", strings.NewReader("12345678"))
	req.ContentLength = 4096 // 声明超限，实际只有 8 字节
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec) != "body_too_large" {
		t.Fatalf("声明超限应 413：status = %d，code = %s", rec.Code, errCode(t, rec))
	}
	if read != -1 {
		t.Fatalf("预检路径不该进 handler，实际读了 %d 字节", read)
	}
}

func TestBodyLimitNonPositiveIsPassThrough(t *testing.T) {
	// n <= 0 = 不限：与 WithMaxBodyBytes(0) 同口径，且不包 body
	e, _ := newTestEngine(t)
	e.POST("/zero", echoBodyLen, BodyLimit(0))
	e.POST("/neg", echoBodyLen, BodyLimit(-1))
	big := strings.Repeat("x", 8<<10)

	if rec := bodyLimitReq(e, "/zero", big); rec.Code != 200 {
		t.Fatalf("BodyLimit(0) 应不限：status = %d", rec.Code)
	}
	if rec := bodyLimitReq(e, "/neg", big); rec.Code != 200 {
		t.Fatalf("BodyLimit(-1) 应不限：status = %d", rec.Code)
	}
}

func TestTooLargeCodeSurvivesDefaultMapper(t *testing.T) {
	// 端到端走**默认 mapper**：调用方传的 code 不能被 cause 的分类吞掉。
	//
	// 陷阱是 errors.As 会沿 HTTPError.Unwrap() 下钻到 cause——若 mapper 把
	// *http.MaxBytesError 的分支排在 HTTPError 之前，TooLarge 的**典型用法**
	// （cause 就是读 body 拿到的 MaxBytesError）会静默变成 body_too_large，
	// 而框架自己那两个调用点传的 code 恰好逐字相同，所以只看框架路径看不出来。
	e, _ := newTestEngine(t)
	e.POST("/quota", func(c *Ctx) error {
		return TooLarge("quota_exceeded", &http.MaxBytesError{Limit: 5})
	})
	e.POST("/nil-cause", func(c *Ctx) error {
		return TooLarge("quota_exceeded", nil)
	})
	e.POST("/bare", func(c *Ctx) error {
		return &http.MaxBytesError{Limit: 5} // 裸读错误：没有 HTTPError 包装
	})

	cases := []struct{ path, want string }{
		{"/quota", "quota_exceeded"}, // cause = MaxBytesError：显式 HTTPError 优先
		{"/nil-cause", "quota_exceeded"},
		{"/bare", codeBodyTooLarge}, // 裸读错误仍归 413 + 统一码
	}
	for _, c := range cases {
		rec := bodyLimitReq(e, c.path, "")
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s: status = %d，应为 413（body = %s）", c.path, rec.Code, rec.Body.String())
		}
		if got := errCode(t, rec); got != c.want {
			t.Fatalf("%s: code = %q，应为 %q", c.path, got, c.want)
		}
	}
}

func TestTooLargeConstructor(t *testing.T) {
	// 验收 8：TooLarge 构造器——413、cause 可 errors.As / errors.Is 追溯
	cause := &http.MaxBytesError{Limit: 8}
	err := TooLarge("body_too_large", cause)

	if err.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d，应为 413", err.Status)
	}
	if err.StatusCode() != http.StatusRequestEntityTooLarge {
		t.Fatalf("StatusCode() = %d", err.StatusCode())
	}
	if got := err.Error(); got != "body_too_large: Request Entity Too Large" {
		t.Fatalf("Error() = %q", got)
	}
	var maxErr *http.MaxBytesError
	if !errors.As(error(err), &maxErr) {
		t.Fatal("cause 不可经 errors.As 追溯到 *http.MaxBytesError")
	}
	if maxErr.Limit != 8 {
		t.Fatalf("追溯到的 Limit = %d", maxErr.Limit)
	}
}

func TestBodyLimitKeepsCloseConnection(t *testing.T) {
	// 验收 7：走**真实连接**——超限读取必须触发 stdlib 的 requestTooLarge 通知，
	// 响应带 Connection: close、回复后关连接。
	//
	// 这条守卫抓的是「MaxBytesReader 的 w 传了包装器」：框架的 responseWriter
	// 不实现未导出的 requestTooLarger 接口，传它则该语义静默丢失（连接会被
	// keep-alive 复用，残留 body 可能被当成下一个请求）。
	e, _ := newTestEngine(t)
	seenLen := int64(0)
	e.POST("/u", func(c *Ctx) error {
		seenLen = c.Request().ContentLength
		_, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return err
		}
		return c.Text(200, "ok")
	}, BodyLimit(16))

	srv := httptest.NewServer(e)
	defer srv.Close()

	// 未知长度的 body：客户端改用 chunked 发送，服务端 Content-Length 声明缺失，
	// 于是必然落到**读取闸门**（预检拿不到声明值）——只有这条路径会触发通知。
	unknownLen := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 4096))}
	req, err := http.NewRequest("POST", srv.URL+"/u", unknownLen)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d，应为 413", resp.StatusCode)
	}
	// 前置条件取**服务端看到的**声明值：只有声明缺失（-1）才说明走的是读取闸门。
	// 若这里 >= 0，请求其实是带 Content-Length 发过来的，那 413 来自预检——
	// 预检不读 body，也就不会触发 requestTooLarge 通知，这条守卫就成了假的。
	if seenLen >= 0 {
		t.Fatalf("前置条件不成立：服务端看到 Content-Length=%d，说明走的是预检路径而非读取闸门", seenLen)
	}
	if !resp.Close {
		t.Fatalf("响应未声明关连接（Connection: %q）——超限关连接的语义丢了（MaxBytesReader 的 w 是不是传了包装器？）",
			resp.Header.Get("Connection"))
	}
}
