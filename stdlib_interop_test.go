package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
