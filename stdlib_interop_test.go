package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ---- stdlib 互操作（设计验收标准『标准库兼容』条）----
//
// 三条路径，前两条是「入」、第三条是「出」：`Wrap` 把 stdlib handler 接进来、
// `Adapt` 把 stdlib 中间件接进洋葱内、`Handler()` 把 Engine 导出去。「无侵入」说的是
// 这三条都在，而不是只有外包一条。
//
// 中间件走 Adapt 还是外包 Handler() 不是风格问题：拦预检的（CORS）、动 body 且会在
// next 返回后无条件收尾的（gzip）必须外包；要读路由模板、要让短路的请求进访问日志
// 的只能走 Adapt。判据见 Adapt 的 godoc 与设计文档「中间件与 stdlib 互操作」一节。

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

// ---- Adapt：把 stdlib 中间件接进洋葱内 ----
//
// 每个用例对应 Adapt 的一条承诺或一条边界。承诺侧的四条是「真假适配器的分水岭」：
// 只调 next、不接管 writer / request 的 naive 实现全都会红。

// statusSpy 代表「包 ResponseWriter 抓状态码」这一类中间件
// （promhttp 的计数器、chi 的 Logger 同形）。
type statusSpy struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusSpy) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusSpy) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// TestAdaptMiddlewareObservesRealStatusAndBytes 钉住第一条承诺：中间件包住
// ResponseWriter 后，抓到的是**真实**的状态码与字节数。
//
// 反证（naive 适配器）：handler 写的是框架的采集层，中间件只包得到自己那一层，
// spy 恒读到 0 / 0——promhttp 的计数器于是全标 code="0"。
func TestAdaptMiddlewareObservesRealStatusAndBytes(t *testing.T) {
	e, _ := newTestEngine(t)
	var spy *statusSpy
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			spy = &statusSpy{ResponseWriter: w}
			next.ServeHTTP(spy, r)
		})
	}))
	const payload = "short and stout"
	e.GET("/teapot", func(c *Ctx) error { return c.Text(http.StatusTeapot, payload) })

	rec := doReq(e, "GET", "/teapot", nil)
	// 前置：中间件没有改动响应本身
	if rec.Code != http.StatusTeapot || rec.Body.String() != payload {
		t.Fatalf("响应被中间件改动了：%d %q", rec.Code, rec.Body.String())
	}
	if spy == nil {
		t.Fatal("中间件没有执行")
	}
	if spy.status != http.StatusTeapot {
		t.Fatalf("中间件抓到的状态码 = %d，want %d（0 即 naive 适配器）", spy.status, http.StatusTeapot)
	}
	if spy.bytes != len(payload) {
		t.Fatalf("中间件抓到的字节数 = %d，want %d", spy.bytes, len(payload))
	}
}

// TestAdaptShortCircuitReachesAccessLog 钉住第二条承诺：中间件短路（不调 next）时，
// 它写出的状态码与字节数记回框架的采集层。
//
// 反证：不补这一步时框架只看到「没写响应」——被限流挡下的 403 在访问日志里记成 200。
func TestAdaptShortCircuitReachesAccessLog(t *testing.T) {
	e, sink := newTestEngine(t)
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "rejected", http.StatusForbidden)
		})
	}))
	e.GET("/x", func(c *Ctx) error { return c.Text(http.StatusOK, "never") })

	rec := doReq(e, "GET", "/x", nil)
	if rec.Code != http.StatusForbidden || rec.Body.String() != "rejected\n" {
		t.Fatalf("短路响应 = %d %q，want 403 %q", rec.Code, rec.Body.String(), "rejected\n")
	}
	logRec := waitRecord(t, sink, eventHTTPReq)
	if logRec.Status != "403" {
		t.Fatalf("访问日志状态码 = %q，want 403（未补采集层时会记成 200）", logRec.Status)
	}
	if got := attrsOf(logRec)[attrHTTPBodySize]; got != int64(len("rejected\n")) {
		t.Fatalf("访问日志 body.size = %v，want %d", got, len("rejected\n"))
	}
}

// TestAdaptReplacedRequestReachesHandler 钉住第三条承诺：中间件换掉的 request 传得下去。
//
// 这也是「中间件怎么把身份传给 handler」的答案——走 request context（stdlib 自己的
// 通道），框架不额外开取值入口。
func TestAdaptReplacedRequestReachesHandler(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, "inside")))
		})
	}))
	e.GET("/who", func(c *Ctx) error {
		v, _ := c.Request().Context().Value(contextKey{}).(string)
		return c.Text(http.StatusOK, "v="+v)
	})

	if got := doReq(e, "GET", "/who", nil).Body.String(); got != "v=inside" {
		t.Fatalf("handler 读到的值 = %q，want v=inside（换过的 request 没传下去）", got)
	}
}

// TestAdaptPassesHandlerErrorThrough 钉住框架契约在 Adapt 里的**全部落点**：handler 返回的
// error 照常穿过适配层交给错误映射器。直通 Adapt（不改 writer、不改 request）不能让 error 消失。
//
// 反证：把 `if called { return err }` 写成 `return nil`——挂了 Adapt 的路由整条错误模型失效，
// 404 变 200，且访问日志跟着记错。
func TestAdaptPassesHandlerErrorThrough(t *testing.T) {
	e, sink := newTestEngine(t)
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) })
	}))
	e.GET("/x", func(c *Ctx) error { return NotFound("missing", nil) })

	rec := doReq(e, "GET", "/x", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d，want 404（handler 的 error 被适配层吞了）", rec.Code)
	}
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("响应不是统一错误信封：%v（body=%q）", err, rec.Body.String())
	}
	if env.Error.Code != "missing" {
		t.Fatalf("信封 code = %q，want missing", env.Error.Code)
	}
	if logRec := waitRecord(t, sink, eventHTTPReq); logRec.Status != "404" {
		t.Fatalf("访问日志 Status = %q，want 404", logRec.Status)
	}
}

// TestAdaptDoesNotSeePreflight 钉住「不搬动路由」这条边界：路由匹配先于中间件，所以只注册了
// GET 时，预检 OPTIONS 根本到不了中间件——405 + Allow，中间件一次都没跑、也没有 CORS 头。
//
// 这正是 CORS 必须外包 `Handler()` 的实证。（godoc / 站点 / 设计文档写了三遍，这里把它钉成红灯。）
func TestAdaptDoesNotSeePreflight(t *testing.T) {
	e, _ := newTestEngine(t)
	called := false
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			w.Header().Set("Access-Control-Allow-Origin", "*")
			next.ServeHTTP(w, r)
		})
	}))
	e.GET("/api", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })

	rec := doReq(e, "OPTIONS", "/api", nil)
	if called {
		t.Fatal("预检跑到了中间件——「路由先于中间件」这条边界不成立了？")
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d，want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("Allow = %q，want 含 GET", allow)
	}
	if acao := rec.Header().Get("Access-Control-Allow-Origin"); acao != "" {
		t.Fatalf("ACAO = %q，want 空（预检没经过中间件）", acao)
	}
	// 正向对照：同一条路由的 GET 确实经过中间件
	if got := doReq(e, "GET", "/api", nil); got.Code != http.StatusOK || !called {
		t.Fatalf("GET status = %d called = %v，want 200 / true", got.Code, called)
	}
}

// TestAdaptCanReadRouteTemplate 钉住 Adapt 相对外包 `Handler()` 的**唯一卖点**：中间件跑在
// 路由之后，所以读得到路由模板（`r.Pattern`）。外包时这里是空的——路由还没匹配。
func TestAdaptCanReadRouteTemplate(t *testing.T) {
	e, _ := newTestEngine(t)
	var inside string
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inside = r.Pattern
			next.ServeHTTP(w, r)
		})
	}))
	e.GET("/users/{id}", func(c *Ctx) error { return c.Text(http.StatusOK, c.Path("id")) })

	if got := doReq(e, "GET", "/users/42", nil).Body.String(); got != "42" {
		t.Fatalf("body = %q，want 42", got)
	}
	// stdlib 的 Pattern 带方法前缀（引擎内部也这么读，再去掉前缀得到 http.route）。
	if inside != "GET /users/{id}" {
		t.Fatalf("r.Pattern = %q，want %q（洋葱内读不到路由模板）", inside, "GET /users/{id}")
	}
}

// gzipWriter / gzipMW 是**改写 body 的最小中间件**——不是 chi Compress。
//
// 它在 next 返回后**无条件**收尾（`defer zw.Close()`），这正是 godoc「压缩类推外包」
// 所指的那种踩坑形态：幸福路没问题，但 handler 返回 error 时会把状态锁成 200（见
// TestAdaptUnconditionalFinalizeLocksStatus）。chi Compress 是「真写过才收尾」，
// 所以它经 Adapt 与外包完全一致——那是特例，不是通例。
type gzipWriter struct {
	http.ResponseWriter
	zw *gzip.Writer
}

func (w *gzipWriter) Write(b []byte) (int, error) { return w.zw.Write(b) }

func gzipMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		defer func() { _ = zw.Close() }()
		next.ServeHTTP(&gzipWriter{ResponseWriter: w, zw: zw}, r)
	})
}

// TestAdaptBodyRewritingMiddlewareKeepsResponseValid 钉住 writer 的层序：框架的采集层
// 必须落在中间件 writer 的**外面**，handler 的写出才真的穿得过中间件。
//
// 反证（naive 适配器）：handler 绕过中间件直接写底层，于是
// `Content-Encoding: gzip` 配着明文 body 发出去，客户端报 gzip: invalid header。
func TestAdaptBodyRewritingMiddlewareKeepsResponseValid(t *testing.T) {
	e, sink := newTestEngine(t)
	e.Use(Adapt(gzipMW))
	e.GET("/g", func(c *Ctx) error { return c.Text(http.StatusOK, "compress me") })

	rec := doReq(e, "GET", "/g", nil)
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q，want gzip", got)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("响应不是合法 gzip（body 没穿过中间件）：%v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "compress me" {
		t.Fatalf("解压后 = %q，want %q", plain, "compress me")
	}
	// 体积口径：采集层在 gzip **外面**，记的是压缩前字节数（#89 当显式契约）。
	logRec := waitRecord(t, sink, eventHTTPReq)
	if got := attrsOf(logRec)[attrHTTPBodySize]; got != int64(len("compress me")) {
		t.Fatalf("访问日志 body.size = %v，want %d（压缩前）", got, len("compress me"))
	}
}

// TestAdaptUnconditionalFinalizeLocksStatus 钉住 godoc 里那条**已知边界**（不是承诺）：
// 在 next 返回后无条件收尾的中间件（本文件的 gzipMW），碰到 handler 返回 error 时会把
// 状态锁成 200，并把 gzip 尾与**未被压缩**的错误体拼在一起。
//
// 与 TestAdaptPassesHandlerErrorThrough 是一对：直通 Adapt 必须让 error 穿过去；
// 「无条件收尾」则是中间件自己的形态问题——所以压缩类默认推外包。
//
// 用真 server：`httptest.ResponseRecorder` 的 WriteHeader 是「最后一次赢」，会把 200 覆盖成
// 框架随后写的 404，测不出「首刷已落定」这件事（真链路是「第一次赢」）。
func TestAdaptUnconditionalFinalizeLocksStatus(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Use(Adapt(gzipMW))
	e.GET("/boom", func(c *Ctx) error { return NotFound("missing", nil) })

	srv := httptest.NewServer(e)
	defer srv.Close()

	// 关掉 Transport 的透明解压：默认客户端读到「gzip 流后面跟着明文」会直接报
	// `gzip: invalid header`——那是同一个损坏的另一面；这里要看的是原始字节。
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Get(srv.URL + "/boom")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，want 200（无条件收尾的中间件先落码，这是已知边界）", resp.StatusCode)
	}
	if !bytes.Contains(raw, []byte("missing")) {
		t.Fatalf("错误体没被拼进来？raw=%q", raw)
	}
}

// TestAdaptMiddlewareWritingOutsideNextReachesAccessLog 是 #96 的最小复现：中间件
// **调了 next**，但在 next **外面**写响应（`Recoverer` 形状：自己的 recover 接住 panic、
// 补一个 500；兜底 404、自研 recovery 同形）。
//
// 框架自己的 writer 一个字节都没写（handler 直接 panic），而响应已经在客户端那边落定。
// 代理是唯一看见它的地方——记录必须跟着记 500，而不是收尾时的缺省 200。记成 200 的后果
// 不是「数字不准」，是**把一条 5xx 从监控面板上抹掉**：降级、告警、错误率全跟着错。
//
// 与上一条 TestAdaptUnconditionalFinalizeLocksStatus 是一对：那条是「返回 error +
// 收尾型中间件」的既有边界（记录仍交给错误映射器），这条是「没有 error 待映射」时
// 记录向代理对齐。两条一起说明闸门为什么是 `err == nil`。
//
// 记录里**没有** `error.type` / 错误对象：框架没接住任何错误，它不知道这份响应怎么来的
// ——编一个出来就是假信息。这是这条路上有意的观测语义。
func TestAdaptMiddlewareWritingOutsideNextReachesAccessLog(t *testing.T) {
	e, sink := newTestEngine(t)
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if p := recover(); p != nil {
					http.Error(w, "fallback", http.StatusServiceUnavailable)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}))
	e.GET("/panic", func(c *Ctx) error { panic("boom") })

	// 状态码与**字节数**都要收回：只补状态码的写法在体积那一列留下的还是 0，
	// 而客户端明明收到了 9 字节。
	rec := doReq(e, "GET", "/panic", nil)
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "fallback\n" {
		t.Fatalf("响应 = %d %q，want 503 %q（中间件自己写的那份）",
			rec.Code, rec.Body.String(), "fallback\n")
	}
	logRec := waitRecord(t, sink, eventHTTPReq)
	if logRec.Status != "503" {
		t.Fatalf("访问记录 Status = %q，want 503（#96：记成缺省 200 等于把一条 5xx 抹掉）", logRec.Status)
	}
	if got := attrsOf(logRec)[attrHTTPBodySize]; got != int64(len("fallback\n")) {
		t.Fatalf("访问记录 body.size = %v，want %d（只补状态码的写法在这里留下 0）",
			got, len("fallback\n"))
	}
	if got, ok := attrsOf(logRec)["error.type"]; ok {
		t.Fatalf("error.type = %v，want 缺席——框架没接住错误，不该编一个类别出来", got)
	}
	if logRec.Err != nil {
		t.Fatalf("记录里的 Err = %v，want nil（同上）", logRec.Err)
	}
}

// TestAdaptKeepsFlusher 钉住能力面：交给中间件的 writer 支持 http.Flusher，
// SSE 的逐条 flush 在洋葱内照常工作。
func TestAdaptKeepsFlusher(t *testing.T) {
	e, _ := newTestEngine(t)
	sawFlusher := false
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, sawFlusher = w.(http.Flusher)
			next.ServeHTTP(w, r)
		})
	}))
	var flushErr error
	e.GET("/sse", func(c *Ctx) error {
		if _, err := c.Writer().Write([]byte("data: 1\n\n")); err != nil {
			return err
		}
		flushErr = c.Flush()
		return nil
	})

	rec := doReq(e, "GET", "/sse", nil)
	if !sawFlusher {
		t.Fatal("交给中间件的 writer 不支持 http.Flusher")
	}
	if flushErr != nil {
		t.Fatalf("Ctx.Flush() = %v，want nil", flushErr)
	}
	if !rec.Flushed {
		t.Fatal("flush 没有落到真实 writer 上")
	}
	if got := rec.Body.String(); got != "data: 1\n\n" {
		t.Fatalf("body = %q", got)
	}
}

// TestAdaptDoesNotOverclaimFlusher 钉住反向：底层不能 Flush 时，代理**不许**替它
// 虚报能力——否则 Ctx.Flush() 的「不支持」判定会被吃掉，流式 handler 以为自己在
// flush，实际什么都没发生。底层用 context_test.go 的 plainWriter（同款最小 writer）。
func TestAdaptDoesNotOverclaimFlusher(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { next.ServeHTTP(w, r) })
	}))

	var flushErr error
	e.ServeTest(&plainWriter{}, httptest.NewRequest("GET", "/f", nil), func(c *Ctx) error {
		flushErr = c.Flush()
		return nil
	})

	if flushErr == nil {
		t.Fatal("底层不支持 Flush 时 Ctx.Flush() 应返回明确 error（代理虚报了 Flusher）")
	}
	if !strings.Contains(flushErr.Error(), "does not support Flush") {
		t.Fatalf("Flush error = %v", flushErr)
	}
}

// quietServer 起一个测试服务器，把 net/http 自己的告警收进缓冲。
//
// hijack 之后误写响应时，net/http 会往 ErrorLog 打 `http: response.WriteHeader on
// hijacked connection from …`，行号指向**调用方**——经 Adapt 写的那些调用方一律是
// 框架的代理层，于是告警看起来像是框架的毛病。这个缓冲就是「框架有没有把噪音留给
// 使用者」的判据：框架自己挡住时它是空的。
//
// ErrorLog 必须在 Start 之前替换：服务器起来之后再改就是和连接 goroutine 抢字段，
// `-race` 会报。
func quietServer(t *testing.T, h http.Handler) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(buf, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, buf
}

// TestAdaptHijackStopsProxyWrites 钉住「连接交出去之后，代理自己也不许再写」——写在
// 代理层的第二个 Hijack 入口上（中间件自己拿的连接）。
//
// 场景：中间件经 Adapt 拿到连接、升级成功，之后（defer 里的兜底、next 返回后的收尾、
// 或者纯粹写错）又向 writer 写响应。守卫缺失时实测三件事一起发生：
//
//	① net/http 打两行 `on hijacked connection` 告警，行号归属 wrap.go——噪音指向
//	   框架，该去看的却是使用者的中间件；
//	② 代理把自己记成「写过 200」，Adapt 的短路回填把这条升级请求记成 status=200
//	   （该记 101），访问日志从「连接已交出」退化成一条看起来正常的 200；
//	③ 返回值全靠 net/http 兜着（本机实测它确实给 http.ErrHijacked，但那是借来的
//	   保护——换个底层 writer 就没了）。
//
// 判据：Write 明确拒绝且 0 字节、服务端告警为空、客户端只看见裸连接上那份响应、
// 访问日志回到 101 + connection.hijacked。
func TestAdaptHijackStopsProxyWrites(t *testing.T) {
	e, sink := newTestEngine(t)

	written := make(chan writeResult, 1)
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("交给中间件的 writer 不是 http.Hijacker：%T", w)
				return
			}
			conn, buf, err := h.Hijack()
			if err != nil {
				t.Errorf("中间件经代理 Hijack 失败：%v", err)
				return
			}
			defer func() { _ = conn.Close() }()

			// 交出之后的两次误写：先写头、再写体。
			w.WriteHeader(http.StatusInternalServerError)
			n, err := w.Write([]byte("fallback"))
			written <- writeResult{n, err}

			// 裸连接上写一份合法响应，让客户端能干净收尾。
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi")
			_ = buf.Flush()
			// 不调 next：短路分支也要一起验（回填就挂在它上面）。
		})
	}))
	e.GET("/ws", func(c *Ctx) error { return nil })

	srv, serverLog := quietServer(t, e)

	resp, err := http.Get(srv.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hi" {
		t.Fatalf("客户端读到 %q，want %q——裸连接上那份响应必须原样到达，中间没有框架插进来的字节", body, "hi")
	}

	res := awaitWrite(t, written)
	if !errors.Is(res.err, http.ErrHijacked) {
		t.Fatalf("交出连接后 Write = %v，want http.ErrHijacked（代理必须自己挡住）", res.err)
	}
	if res.n != 0 {
		t.Fatalf("交出连接后 Write 写了 %d 字节，want 0", res.n)
	}
	if got := serverLog.String(); got != "" {
		t.Fatalf("服务端出现告警：\n%s（框架自己挡住时这里应当是空的）", got)
	}

	rec := waitRecord(t, sink, eventHTTPReq)
	if rec.Status != "101" {
		t.Fatalf("访问记录 Status = %q，want 101——短路回填把中间件误写的那个码当成事实了", rec.Status)
	}
	attrs := attrsOf(rec)
	if attrs[attrConnHijacked] != true {
		t.Fatalf("connection.hijacked = %v，want true", attrs[attrConnHijacked])
	}
	if v, ok := attrs[attrHTTPBodySize]; ok {
		t.Fatalf("连接已交出，不该记响应体积，得到 %v", v)
	}
}

// writeResult 是中间件那次（应该被拒绝的）写出的返回值，经 channel 送回测试 goroutine
// ——直接写共享变量会让 `-race` 报竞态，而且那种报告是真的：这次写发生在 handler 已经把
// 响应 Flush 给客户端之后。
type writeResult struct {
	n   int
	err error
}

func awaitWrite(t *testing.T, ch <-chan writeResult) writeResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(3 * time.Second):
		t.Fatal("中间件没有走到那次误写——判据没有成立")
		return writeResult{}
	}
}

// TestAdaptProxyYieldsToHandlerHijack 钉住代理读的是**框架侧**那份标记：升级发生在
// 洋葱内的 handler（走 `c.Writer()`，不经过代理），中间件随后照常收尾写响应——代理
// 必须也知道连接没了。
//
// 这是「兜底写响应」型中间件的形状：next 返回后无条件补一个响应。代理只信自己那份
// 标记时，这一写会落到 net/http 上，打两行归属 wrap.go 的告警（实测），而中间件
// 作者在自己的代码里看不到任何异常。
func TestAdaptProxyYieldsToHandlerHijack(t *testing.T) {
	e, sink := newTestEngine(t)

	written := make(chan writeResult, 1)
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			n, err := w.Write([]byte("fallback"))
			written <- writeResult{n, err}
		})
	}))
	e.GET("/ws", func(c *Ctx) error {
		h, ok := c.Writer().(http.Hijacker)
		if !ok {
			t.Errorf("c.Writer() 不是 http.Hijacker")
			return nil
		}
		conn, buf, err := h.Hijack()
		if err != nil {
			t.Errorf("handler Hijack 失败：%v", err)
			return nil
		}
		defer func() { _ = conn.Close() }()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi")
		return buf.Flush()
	})

	srv, serverLog := quietServer(t, e)

	resp, err := http.Get(srv.URL + "/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hi" {
		t.Fatalf("客户端读到 %q，want %q", body, "hi")
	}

	res := awaitWrite(t, written)
	if !errors.Is(res.err, http.ErrHijacked) {
		t.Fatalf("handler 交出连接后，中间件的 Write = %v，want http.ErrHijacked（代理要看见框架侧那份标记）", res.err)
	}
	if res.n != 0 {
		t.Fatalf("交出连接后 Write 写了 %d 字节，want 0", res.n)
	}
	if got := serverLog.String(); got != "" {
		t.Fatalf("服务端出现告警：\n%s（中间件这一写该被框架挡住，而不是交给 net/http 去抱怨）", got)
	}

	rec := waitRecord(t, sink, eventHTTPReq)
	if rec.Status != "101" {
		t.Fatalf("访问记录 Status = %q，want 101", rec.Status)
	}
	if got := attrsOf(rec)[attrConnHijacked]; got != true {
		t.Fatalf("connection.hijacked = %v，want true", got)
	}
}

// TestAdaptHijackKeepsStatusColumnConventional 钉住短路回填那条守卫：**连接交出去
// 之后，状态码列一律是 hijack 的约定值（101）**，不回填中间件写过的任何码。
//
// 用「先写一个普通状态码、再交出连接」这个合法但少见的次序把两条路分开：回填守卫
// 缺失时，这条升级请求在访问日志里显示 418 + connection.hijacked，读者分不清 418 是
// 真写到线上了还是中间件手滑写早了；守卫在则一律 101，这一列的性质由
// connection.hijacked 一句话说清（它本来就只是约定值，见 statusHijacked）。
func TestAdaptHijackKeepsStatusColumnConventional(t *testing.T) {
	e, sink := newTestEngine(t)

	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot) // 先按普通响应落一个码
			h, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("交给中间件的 writer 不是 http.Hijacker：%T", w)
				return
			}
			conn, _, err := h.Hijack()
			if err != nil {
				t.Errorf("中间件经代理 Hijack 失败：%v", err)
				return
			}
			_ = conn.Close() // 交出之后不写任何东西：客户端拿到的是一个残缺响应
		})
	}))
	e.GET("/ws", func(c *Ctx) error { return nil })

	srv, _ := quietServer(t, e)
	if resp, err := http.Get(srv.URL + "/ws"); err == nil {
		_ = resp.Body.Close()
	}

	rec := waitRecord(t, sink, eventHTTPReq)
	if rec.Status != "101" {
		t.Fatalf("访问记录 Status = %q，want 101——回填把中间件写的 418 当成这条请求的事实了", rec.Status)
	}
	if got := attrsOf(rec)[attrConnHijacked]; got != true {
		t.Fatalf("connection.hijacked = %v，want true", got)
	}
}

// muteWriter 模拟「收尾型」中间件交出去的 writer：收尾之后不再接受写入。
type muteWriter struct {
	http.ResponseWriter
	mute bool
}

func (w *muteWriter) WriteHeader(code int) {
	if w.mute {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *muteWriter) Write(b []byte) (int, error) {
	if w.mute {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

// TestAdaptRestoresCtxOnPanic 钉住「还原必须走 defer」：handler panic 时，框架的收尾
// （错误映射 + 访问记录）必须写回**原始** writer，而不是中间件已经收尾的那一个。
//
// 反证：还原写成顺序语句时会被 panic 跳过——实测后果是 500 整个丢掉、客户端拿到
// 200 空响应（日志里只剩 `error handler failed: flate: closed writer`）。
func TestAdaptRestoresCtxOnPanic(t *testing.T) {
	e, sink := newTestEngine(t)
	e.Use(Adapt(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mw := &muteWriter{ResponseWriter: w}
			defer func() { mw.mute = true }() // 展开路径上也收尾
			next.ServeHTTP(mw, r)
		})
	}))
	e.GET("/boom", func(c *Ctx) error { panic("kaboom") })

	rec := doReq(e, "GET", "/boom", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 路径 status = %d，want 500（错误映射写进了中间件已收尾的 writer）", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("panic 路径 body 为空：错误信封整个丢了")
	}
	if logRec := waitRecord(t, sink, eventHTTPReq); logRec.Err == nil {
		t.Fatal("panic 没有被记录成 Err")
	}
}

// TestAdaptLayersNest 钉住两层 Adapt 的 writer 叠序：外层拿到的 writer 内层接着包，
// 两层各自的观测同时成立（内层 gzip 能解压、外层 spy 抓到真实状态码）。
func TestAdaptLayersNest(t *testing.T) {
	e, _ := newTestEngine(t)
	var outer *statusSpy
	e.Use(Adapt(func(next http.Handler) http.Handler { // 外层：抓状态码
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			outer = &statusSpy{ResponseWriter: w}
			next.ServeHTTP(outer, r)
		})
	}))
	e.Use(Adapt(gzipMW)) // 内层：gzip
	e.GET("/n", func(c *Ctx) error { return c.Text(http.StatusCreated, "nested") })

	rec := doReq(e, "GET", "/n", nil)
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("响应不是合法 gzip：%v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(plain) != "nested" {
		t.Fatalf("解压后 = %q，want nested", plain)
	}
	if outer == nil || outer.status != http.StatusCreated {
		t.Fatalf("外层中间件抓到的状态码 = %v，want 201", outer)
	}
}

// TestAdaptNilMiddlewareFailsFast 钉住装配期守卫：nil 中间件必须在装配时报错，
// 而不是每个请求都 nil-deref 成 500。
func TestAdaptNilMiddlewareFailsFast(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Adapt(nil) 没有 fail-fast")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "non-nil middleware") {
			t.Fatalf("panic = %v，want 带 non-nil middleware 的说明", r)
		}
	}()
	_ = Adapt(nil)
}
