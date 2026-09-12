package web

import (
	"context"
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
	root         *kernel.Context
	sink         observability.Sink
	hostID       string
	trace        bool
	accessLog    bool
	traceHeader  string
	minimal      bool
	errorHandler ErrorHandler
	server       ServerConfig
}

func defaultConfig() config {
	return config{
		hostID:      defaultHostID,
		trace:       true,
		accessLog:   true,
		traceHeader: "X-Trace-Id",
		server:      DefaultServerConfig(),
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

	sink         observability.Sink
	hostID       string
	trace        bool
	accessLog    bool
	traceHeader  string
	server       ServerConfig
	errorHandler ErrorHandler

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

	e := &Engine{
		kernel:       root,
		mux:          http.NewServeMux(),
		sink:         sink,
		hostID:       cfg.hostID,
		trace:        cfg.trace,
		accessLog:    cfg.accessLog,
		traceHeader:  cfg.traceHeader,
		server:       cfg.server,
		errorHandler: handler,
		life:         &lifecycle{},
	}

	if sink != nil && !cfg.minimal {
		// Bootstrap 必须是宿主树最先 Use 的插件：后装只能靠快照横幅兜底。
		// Minimal 表示"零默认观测"，此时连装配期记录也不装。
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
// 注意：再注册 "GET <prefix>/{file...}" 会遮蔽本静态服务（方法限定模式优先）；
// 要放行个别路径请用字面量（字面量赢通配），如 app.GET("/static/health", h)。
func (e *Engine) Static(prefix, dir string) {
	full := e.prefix + prefix
	e.mux.Handle(full+"/", http.StripPrefix(full, http.FileServer(http.Dir(dir))))
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
		c.traceID = e.resolveTraceID(r)
		if e.traceHeader != "" {
			rw.Header().Set(e.traceHeader, c.traceID)
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
			// 映射器自身失败：兜底 500，且不影响 AccessLog 对原始 error 的记录。
			writeFallback500(rw)
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

func writeFallback500(rw *responseWriter) {
	if rw.wrote {
		return
	}
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(http.StatusInternalServerError)
	_, _ = rw.Write([]byte(`{"error":{"code":"internal","message":"Internal Server Error"}}`))
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

func errCategory(err error) string {
	var perr *PanicError
	if errors.As(err, &perr) {
		return "panic"
	}
	var herr *HTTPError
	if errors.As(err, &herr) {
		return "http"
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

	// ⑤ Sink flush（若出口实现了 Flusher）
	if f, ok := e.sink.(interface {
		Flush(context.Context) error
	}); ok {
		flushCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = f.Flush(flushCtx)
	}
	return nil
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
