package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

const (
	defaultHostID = "pulse-web"
	eventHTTPReq  = "http.request"
	sourceHTTP    = "http"

	// sinkFlushTimeout 是关闭时序第 ⑤ 步（Sink flush）的独立预算：
	// 它在 root.Dispose() 之后另起，**不在 ShutdownTimeout 之内**，
	// 所以总关闭时长上限 = ShutdownTimeout + sinkFlushTimeout。
	sinkFlushTimeout = 3 * time.Second
)

// ServerConfig 是 http.Server 参数；零值字段取默认（见 DefaultServerConfig）。
type ServerConfig struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration // 默认 0——非 0 会切断 SSE / 长连接
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
	ShutdownTimeout   time.Duration // 优雅关闭总预算
}

// DefaultServerConfig 返回默认 Server 参数。
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ShutdownTimeout:   30 * time.Second,
	}
}

// Option 配置 Engine。
type Option func(*config)

type config struct {
	root             *kernel.Context
	sink             observability.Sink
	hostID           string
	trace            bool
	trustTraceHeader bool
	accessLog        bool
	traceHeader      string
	minimal          bool
	errorHandler     ErrorHandler
	server           ServerConfig
	templates        *TemplateConfig
	collector        bool
}

func defaultConfig() config {
	return config{
		hostID:           defaultHostID,
		trace:            true,
		trustTraceHeader: true,
		accessLog:        true,
		traceHeader:      "X-Trace-Id",
		server:           DefaultServerConfig(),
	}
}

// WithRoot 接入已有 kernel root（不新建）。Run 结束时该 root 会被销毁
// （级联回收全部 scope），因此调用方不应在 Run 返回后继续使用它。
func WithRoot(root *kernel.Context) Option {
	return func(cfg *config) { cfg.root = root }
}

// WithSink 指定观测出口（默认 SlogSink → stderr）。
// 它只换出口，不开启任何被 Minimal 关掉的观测。
func WithSink(sink observability.Sink) Option {
	return func(cfg *config) { cfg.sink = sink }
}

// WithTrustedTraceHeader 控制是否采纳入站链路头（W3C traceparent / B3）。
//
//	true （默认）: 采纳入站 trace-id，网关之后的链路在本进程接得上
//	false        : 忽略入站头、总是自生成，客户端无法伪造 trace-id 污染日志
//
// 取舍按部署形态二选一：置于可信网关之后用 true，直接暴露公网用 false。
func WithTrustedTraceHeader(trust bool) Option {
	return func(cfg *config) { cfg.trustTraceHeader = trust }
}

// WithHostID 设置宿主标识（出现在全部观测记录与装配横幅中）。
func WithHostID(id string) Option {
	return func(cfg *config) { cfg.hostID = id }
}

// WithoutAccessLog 关闭访问日志（保留 Trace 与 panic 兜底）。
func WithoutAccessLog() Option {
	return func(cfg *config) { cfg.accessLog = false }
}

// WithErrorHandler 替换默认错误映射器。
func WithErrorHandler(h ErrorHandler) Option {
	return func(cfg *config) { cfg.errorHandler = h }
}

// WithServer 覆盖 Server 参数：非零字段生效，零值字段保持默认。
func WithServer(sc ServerConfig) Option {
	return func(cfg *config) { cfg.server = mergeServer(cfg.server, sc) }
}

// Minimal 关闭默认装配（默认 Sink、Trace、访问日志），保留路由骨架与 panic 兜底。
// 需要观测就显式 WithSink + 自行装配，不要指望 Minimal 复活它们。
func Minimal() Option {
	return func(cfg *config) {
		cfg.minimal = true
		cfg.trace = false
		cfg.accessLog = false
	}
}

// WithCollector 每请求把 `observability.Collector` 装进请求作用域，供
// **被交付请求 scope 的组件**用 `kernel.Get(scope, observability.CollectorKey)` 打点。
//
// 可见性边界（上游 v0.2.1 起改用 `kernel.Local()` 作用域局部绑定，实测）：
//
//	请求 scope 自身       ✅      请求 scope 的后代    ✅
//	宿主 root             ❌      插件私有 scope       ❌（与请求 scope 是兄弟）
//	并发请求各自的 scope   ✅（互不遮蔽，各持自己的 TraceID）
//
// 因此它服务的是「拿到 `c.Kernel()` 的组件」，**不是**「自行 lookup 的插件」——
// 插件的私有 scope 是 root 的另一个子节点，与请求 scope 同级，永远读不到。
//
// 成本（实测，表 B 口径）：约 **+385 ns / +12 allocs 每请求**（端到端；kernel 层
// `AttachCollector` 相对基线 +270 ns / +12 allocs），且与插件树规模解耦
// （100 插件下 407 ns）。默认关。
//
// 无 Sink 时装配期 panic——`Minimal()` 且未 `WithSink` 就是这个组合。
func WithCollector() Option {
	return func(cfg *config) { cfg.collector = true }
}

func mergeServer(base, over ServerConfig) ServerConfig {
	out := base
	if over.ReadHeaderTimeout != 0 {
		out.ReadHeaderTimeout = over.ReadHeaderTimeout
	}
	if over.ReadTimeout != 0 {
		out.ReadTimeout = over.ReadTimeout
	}
	if over.WriteTimeout != 0 {
		out.WriteTimeout = over.WriteTimeout
	}
	if over.IdleTimeout != 0 {
		out.IdleTimeout = over.IdleTimeout
	}
	if over.MaxHeaderBytes != 0 {
		out.MaxHeaderBytes = over.MaxHeaderBytes
	}
	if over.ShutdownTimeout != 0 {
		out.ShutdownTimeout = over.ShutdownTimeout
	}
	return out
}

// Engine 是框架核心：持有 kernel root、路由表与运行时配置。
//
// 经 Group 派生出的 Engine 是浅拷贝（共享 kernel / mux / 配置与生命周期状态），
// 只携带各自的前缀与中间件。
type Engine struct {
	kernel *kernel.Context
	mux    *http.ServeMux

	sink             observability.Sink
	hostID           string
	trace            bool
	trustTraceHeader bool
	accessLog        bool
	traceHeader      string
	server           ServerConfig
	errorHandler     ErrorHandler
	templates        *templateSet
	collector        bool

	prefix string
	mw     []Middleware

	life *lifecycle
}

// lifecycle 承载进程级可变状态（指针共享，避免 Group 浅拷贝拷贝锁）。
type lifecycle struct {
	mu         sync.Mutex
	onShutdown func(context.Context) error
	disposed   bool
}

// New 创建 Engine：自建 kernel root（或接入 WithRoot 传入的），
// 并在有 Sink 时最先装配观测插件 Bootstrap。
//
// 选项冲突在装配期 panic（对齐「装配期暴露错误」）。
func New(opts ...Option) *Engine {
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}

	sink := cfg.sink
	if sink == nil && !cfg.minimal {
		sink = observability.SlogSink{}
	}

	root := cfg.root
	if root == nil {
		root = kernel.New()
	}

	handler := cfg.errorHandler
	if handler == nil {
		handler = defaultErrorHandler
	}

	// 装配期暴露错误：WithCollector 每请求要往作用域里装 Collector，
	// 没有出口时它是空转——而 Minimal() 且未 WithSink 正是 sink == nil 的组合。
	if cfg.collector && sink == nil {
		panic("web: WithCollector requires a Sink (got Minimal() without WithSink)")
	}

	e := &Engine{
		kernel:           root,
		mux:              http.NewServeMux(),
		sink:             sink,
		hostID:           cfg.hostID,
		trace:            cfg.trace,
		trustTraceHeader: cfg.trustTraceHeader,
		accessLog:        cfg.accessLog,
		traceHeader:      cfg.traceHeader,
		server:           cfg.server,
		errorHandler:     handler,
		collector:        cfg.collector,
		life:             &lifecycle{},
	}

	if cfg.templates != nil {
		ts, err := newTemplateSet(*cfg.templates)
		if err != nil {
			panic("web: templates: " + err.Error())
		}
		e.templates = ts
	}

	if sink != nil && !cfg.minimal {
		// Bootstrap 必须是宿主树最先 Use 的插件：kernel 事件不回放，
		// 后装只能靠快照横幅兜底。
		//
		// 已知限制（WithRoot 场景）：传入的树若已装过插件，本次装载拿到的是
		// 当前快照，**此前的 fiber 迁移轨迹缺失**。需要完整装载轨迹时，
		// 应在 WithRoot 传入前不要 Use 任何插件（即由 pulse-web 最先装配）。
		if _, err := kernel.Use(root, observability.Bootstrap(cfg.hostID, sink)); err != nil {
			panic("web: observability bootstrap failed: " + err.Error())
		}
	}
	return e
}

// Root 返回进程级 kernel root —— Provide / Use 原生 API 的入口，
// 也是传给"需要进程级 kernel 的组件"的正确对象。
//
// 不要在这一位置传 Ctx.Kernel()（请求 scope）：挂在它下面的东西会随请求结束被级联销毁。
func (e *Engine) Root() *kernel.Context { return e.kernel }

// ---- 路由注册 ----

// Handle 注册处理器（pattern 直接交给 stdlib ServeMux 解析，含方法前缀与通配）。
func (e *Engine) Handle(pattern string, h Handler, mw ...Middleware) {
	e.register(e.prefix+pattern, h, mw)
}

func (e *Engine) GET(path string, h Handler, mw ...Middleware) {
	e.register("GET "+e.prefix+path, h, mw)
}
func (e *Engine) POST(path string, h Handler, mw ...Middleware) {
	e.register("POST "+e.prefix+path, h, mw)
}
func (e *Engine) PUT(path string, h Handler, mw ...Middleware) {
	e.register("PUT "+e.prefix+path, h, mw)
}
func (e *Engine) PATCH(path string, h Handler, mw ...Middleware) {
	e.register("PATCH "+e.prefix+path, h, mw)
}
func (e *Engine) DELETE(path string, h Handler, mw ...Middleware) {
	e.register("DELETE "+e.prefix+path, h, mw)
}
func (e *Engine) HEAD(path string, h Handler, mw ...Middleware) {
	e.register("HEAD "+e.prefix+path, h, mw)
}
func (e *Engine) OPTIONS(path string, h Handler, mw ...Middleware) {
	e.register("OPTIONS "+e.prefix+path, h, mw)
}

// Use 追加全局中间件（对之后注册的路由生效；请在注册路由前调用）。
func (e *Engine) Use(mw ...Middleware) { e.mw = e.chainMW(mw) }

// Group 派生路由分组：累积路径前缀与中间件。中间件在注册期闭包组合，
// 运行期零额外开销；执行顺序为洋葱模型（先注册的先入、后出）。
func (e *Engine) Group(prefix string, mw ...Middleware) *Engine {
	g := *e
	g.prefix = e.prefix + prefix
	g.mw = e.chainMW(mw)
	return &g
}

// Static 在 prefix 下提供 dir 的静态文件，注册的是前缀模式 "<prefix>/"。
//
// 静态资源同样经过全局与分组中间件（auth / CORS / 限流不会有例外），
// 与普通路由共用同一条注册路径。
//
// 注意：再注册 "GET <prefix>/{file...}" 会遮蔽本静态服务（方法限定模式优先）；
// 要放行个别路径请用字面量（字面量赢通配），如 app.GET("/static/health", h)。
func (e *Engine) Static(prefix, dir string) {
	full := e.prefix + prefix
	fs := http.StripPrefix(full, http.FileServer(http.Dir(dir)))
	e.register(full+"/", Wrap(fs), nil)
}

func (e *Engine) chainMW(extra []Middleware) []Middleware {
	out := make([]Middleware, 0, len(e.mw)+len(extra))
	out = append(out, e.mw...)
	out = append(out, extra...)
	return out
}

func (e *Engine) register(pattern string, h Handler, mw []Middleware) {
	chain := compose(e.chainMW(mw), h)
	e.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := ctxFromRequest(r)
		if c == nil {
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		// 换成 ServeMux 分发的这个 request：路径参数与匹配 pattern 都写在它上面
		// （Request.PathValue / Request.Pattern），Ctx 必须读同一份。
		c.r = r
		if err := chain(c); err != nil {
			c.setErr(err)
		}
	}))
}

// compose 把中间件与处理器编译成单个 Handler（洋葱模型）。
func compose(mw []Middleware, h Handler) Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		next := h
		m := mw[i]
		h = func(c *Ctx) error { return m(c, next) }
	}
	return h
}

// ---- 请求处理 ----

type ctxKey struct{}

func ctxFromRequest(r *http.Request) *Ctx {
	c, _ := r.Context().Value(ctxKey{}).(*Ctx)
	return c
}

// ServeHTTP 实现 http.Handler。
//
// 执行顺序（关键时序）：
//
//	中间件链 → handler 返回
//	  → scope.Dispose()       业务态结束（请求级 Effect 立即回收，先于写响应）
//	  → 错误映射 + 写响应      网络态
//	  → AccessLog 落盘
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	scope, err := e.kernel.Derive()
	if err != nil {
		// kernel 已销毁（进程关闭中）
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return
	}

	rw := &responseWriter{ResponseWriter: w}
	c := &Ctx{
		engine:  e,
		w:       rw,
		r:       r,
		scope:   scope,
		started: time.Now(),
	}
	if e.trace {
		if e.trustTraceHeader {
			c.traceID = e.resolveTraceID(r)
		} else {
			// 不信任入站头：客户端无法伪造 trace-id（代价是网关处链路断裂）
			c.traceID = generateTraceID()
		}
		if e.traceHeader != "" {
			rw.Header().Set(e.traceHeader, c.traceID)
		}
	}

	// 请求级 Collector：作用域局部绑定（上游 kernel.Local()），装完即随
	// scope.Dispose 撤除——并发请求各持自己的实例，互不串台。
	if e.collector {
		if _, err := observability.AttachCollector(scope, observability.ObserveConfig{
			Sink:    e.sink,
			HostID:  e.hostID,
			TraceID: c.traceID,
		}); err != nil {
			// 装配期已校验 Sink 非 nil，scope 也是刚派生出来的活作用域，
			// 走到这里说明状态与预期不符——记一条而不是静默。
			slog.Warn("pulse.web: attach collector failed", "err", err.Error(), "trace_id", c.traceID)
		}
	}

	req := r.WithContext(context.WithValue(r.Context(), ctxKey{}, c))

	defer func() {
		if p := recover(); p != nil {
			c.setErr(&PanicError{Value: p, Stack: debug.Stack()})
		}
		scope.Dispose()
		e.finish(c, rw)
	}()

	e.mux.ServeHTTP(rw, req)
}

// finish 是 Engine 层收尾：错误映射 → 兜底写响应 → 访问日志。
func (e *Engine) finish(c *Ctx, rw *responseWriter) {
	if c.err != nil && !rw.wrote {
		if herr := e.mapErrorSafely(c, c.err); herr != nil {
			// 映射器自身失败：兜底 500（与默认 mapper 同源），
			// 且不影响 AccessLog 对原始 error 的记录。
			writeErrorPayload(rw, http.StatusInternalServerError, "internal", http.StatusText(http.StatusInternalServerError))
			slog.Warn("pulse.web: error handler failed", "err", herr.Error(), "trace_id", c.traceID)
		}
	}

	if !rw.wrote {
		code := c.status
		if code == 0 {
			code = http.StatusOK
		}
		rw.WriteHeader(code)
	}

	if e.accessLog && e.sink != nil {
		e.writeAccessLog(c, rw)
	}
}

// mapErrorSafely 调用错误映射器，并兜住映射器自身的 panic。
func (e *Engine) mapErrorSafely(c *Ctx, err error) (out error) {
	defer func() {
		if p := recover(); p != nil {
			out = fmt.Errorf("error handler panicked: %v", p)
		}
	}()
	return e.errorHandler(c, err)
}

// writeErrorPayload 写出统一格式的错误响应。
// 与默认 mapper 共用 errorPayload —— fallback 与正常映射的响应体形状永远一致，
// 改一处即两处同步。
func writeErrorPayload(w *responseWriter, status int, code, message string) {
	if w.wrote {
		return
	}
	body, err := json.Marshal(errorPayload{Error: errorDetail{Code: code, Message: message}})
	if err != nil {
		body = []byte(`{"error":{"code":"internal","message":"Internal Server Error"}}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (e *Engine) writeAccessLog(c *Ctx, rw *responseWriter) {
	rec := observability.Record{
		HostID:   e.hostID,
		TraceID:  c.traceID,
		Source:   observability.Source(sourceHTTP),
		Event:    eventHTTPReq,
		Status:   strconv.Itoa(rw.status),
		Duration: time.Since(c.started),
		Err:      c.err,
	}
	observability.Set(&rec.Attrs, "http.request.method", c.r.Method)
	observability.Set(&rec.Attrs, "http.route", routePattern(c.r))
	observability.Set(&rec.Attrs, "url.path", c.r.URL.Path)
	observability.Set(&rec.Attrs, "http.response.body.size", int64(rw.bytes))
	observability.Set(&rec.Attrs, "client.address", c.r.RemoteAddr)
	if c.err != nil {
		observability.Set(&rec.Attrs, "error.type", errCategory(c.err))
	}
	e.sink.Write(rec)
}

// routePattern 去掉 ServeMux Request.Pattern 的方法前缀，
// 得到 OTel 语义的 http.route（路由模板，不含方法）。
func routePattern(r *http.Request) string {
	p := r.Pattern
	for i := 0; i < len(p); i++ {
		if p[i] == ' ' {
			return p[i+1:]
		}
	}
	return p
}

// statusCode 从错误里取 HTTP 状态码（*HTTPError 或实现 StatusCoder 的自定义错误）。
func statusCode(err error) int {
	var herr *HTTPError
	if errors.As(err, &herr) {
		return herr.Status
	}
	var sc StatusCoder
	if errors.As(err, &sc) {
		return sc.StatusCode()
	}
	return 0
}

// errCategory 把错误分类为稳定的聚合维度（进 AccessLog 的 error.type）。
// 有状态码的错误按区间分，避免把「客户端 4xx」与「服务端 5xx」混进同一个桶。
func errCategory(err error) string {
	var perr *PanicError
	if errors.As(err, &perr) {
		return "panic"
	}
	switch status := statusCode(err); {
	case status >= 500:
		return "http_5xx"
	case status >= 400:
		return "http_4xx"
	case status >= 300:
		return "http_3xx"
	}
	return "internal"
}

func (c *Ctx) setErr(err error) {
	if c.err == nil {
		c.err = err
	}
}

// ---- 运行与关闭 ----

// Handler 返回标准 http.Handler 视图（等价 Engine 自身），供自带 server 的场景使用。
func (e *Engine) Handler() http.Handler { return e }

// OnShutdown 注册关闭回调（单回调，后设置的生效）：在 HTTP drain 之后、
// root.Dispose 之前执行，用于等待后台任务。共享同一 deadline。
func (e *Engine) OnShutdown(fn func(ctx context.Context) error) {
	e.life.mu.Lock()
	defer e.life.mu.Unlock()
	e.life.onShutdown = fn
}

// Run 监听 addr 并阻塞服务，直到收到 SIGINT / SIGTERM 完成优雅关闭。
//
// 返回 nil 表示正常关闭；非 nil 表示启动失败或关闭期错误。
// 两条路径都会完成 root.Dispose —— 调用方不需要、也不能再 Dispose。
// Run 返回后 Engine 不可复用。
func (e *Engine) Run(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		e.dispose()
		return err
	}
	return e.Serve(ln)
}

// Serve 在给定 listener 上阻塞服务（同样内置信号处理与优雅关闭）。
func (e *Engine) Serve(ln net.Listener) error {
	srv := &http.Server{
		Handler:           e,
		ReadHeaderTimeout: e.server.ReadHeaderTimeout,
		ReadTimeout:       e.server.ReadTimeout,
		WriteTimeout:      e.server.WriteTimeout,
		IdleTimeout:       e.server.IdleTimeout,
		MaxHeaderBytes:    e.server.MaxHeaderBytes,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-errCh:
		e.dispose()
		return err
	case <-sigCh:
		return e.shutdown(srv)
	}
}

// shutdown 执行优雅关闭：drain → OnShutdown → root.Dispose → Sink flush。
func (e *Engine) shutdown(srv *http.Server) error {
	ctx, cancel := context.WithTimeout(context.Background(), e.server.ShutdownTimeout)
	defer cancel()

	// ① 停止接受新连接并等待在途请求结束（drain 由 net/http 负责，kernel 不做这件事）
	if err := srv.Shutdown(ctx); err != nil {
		// ② 超时：强制断开剩余连接
		_ = srv.Close()
	}

	// ③ 用户回调（等待后台任务；共享同一 deadline）
	e.life.mu.Lock()
	fn := e.life.onShutdown
	e.life.mu.Unlock()
	if fn != nil {
		if err := fn(ctx); err != nil {
			slog.Warn("pulse.web: shutdown hook", "err", err.Error())
		}
	}

	// ④ 终局：级联回收全部 kernel 资源
	e.dispose()

	// ⑤ Sink flush（独立预算，见 sinkFlushTimeout）
	if flush, ok := sinkFlusher(e.sink); ok {
		flushCtx, cancel := context.WithTimeout(context.Background(), sinkFlushTimeout)
		defer cancel()
		if err := flush(flushCtx); err != nil {
			// 与 ③ 的 OnShutdown 错误同样处理：不静默吞，但也不让关闭失败。
			slog.Warn("pulse.web: sink flush", "err", err.Error())
		}
	}
	return nil
}

// sinkFlusher 归一出口的 flush 形态。
//
// 上游有两种出口形态：带 ctx 的（observability.AsyncSink）与不带的
// （observability.LineSink）。只认其中一种会**静默跳过**另一种——
// LineSink 形态下「关闭前未达阈值的最后一批」会随进程消失，而
// `WithSink(observability.NewLineSink(...))` 正是最自然的行式输出用法：
// 不报错、不告警，只是丢日志。
//
// 不带 ctx 的形态无法兑现截止时间（内部只有一次 Write），闭包忽略 ctx。
//
// 另注：AsyncSink.Flush 只排空**它自己的**队列，不级联 inner 的 Flush——
// 异步化的正确组合是 `NewAsyncSink(SlogSink)`；用 AsyncSink 包另一个缓冲
// 出口（如 LineSink）会留下未落盘的内层缓冲，框架无从代劳。
func sinkFlusher(sink observability.Sink) (func(context.Context) error, bool) {
	switch f := sink.(type) {
	case interface{ Flush(context.Context) error }:
		return f.Flush, true
	case interface{ Flush() error }:
		return func(context.Context) error { return f.Flush() }, true
	}
	return nil, false
}

func (e *Engine) dispose() {
	e.life.mu.Lock()
	if e.life.disposed {
		e.life.mu.Unlock()
		return
	}
	e.life.disposed = true
	e.life.mu.Unlock()
	e.kernel.Dispose()
}
