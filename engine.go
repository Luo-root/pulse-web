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
	maxBodyBytes     int64
	spanHook         SpanHook
	spanHookSet      bool
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

// WithSink 指定观测出口（默认 `ConsoleSink` → stdout，给人读的列式单行）。
// 它只换出口，不开启任何被 Minimal 关掉的观测。
//
// 要机器可读 / 接既有日志管道：`WithSink(observability.SlogSink{Logger: …})`；
// 要行式缓冲：`observability.NewLineSink(w)`；要异步：
// `observability.NewAsyncSink(inner)`。
func WithSink(sink observability.Sink) Option {
	return func(cfg *config) { cfg.sink = sink }
}

// newDefaultSink 造默认装配的出口：**给人读**的控制台列式出口，写 stdout。
//
// 选它的依据（Issue #20 的实测）：默认出口是开箱体验，而旧默认
// `observability.SlogSink`（→ stderr）是**给机器读**的结构化出口——接宿主 logger、
// 要 JSON、喂采集器时才是它。#18 的真实负载对比里（采集口径），观测档掉
// 35%（c=64）/ 12%（c=256）吞吐，而 `ConsoleSink` 掉到 14% / 4%。
//
// 渲染同一条访问记录（`io.Discard`、`-benchtime=20000x -count=10` 同会话配对、
// 取中位轮）：`SlogSink` ~1310 ns / 18 allocs，`ConsoleSink` ~236 ns / **0 allocs**
// ——省下的是「把 Record 摊平成 []any 再交给 slog」那一步。
//
// 两者不是「同一样东西换个格式」：上游 v0.2.3 起 `SlogSink` 已与 `LineSink` 对齐
// 字段与顺序（`duration_ms` 是**不截断**的毫秒数值、Attrs 走插入序、不自己输出
// `time`），差别只剩**同一事实的机器面与人读面**——`0.585` 与 `585.1µs` 是同一条
// 耗时，开机第一眼要的是后者。
// 要切回结构化出口用 WithSink —— 只换出口，装配不变。
func newDefaultSink() observability.Sink {
	return NewConsoleSink(os.Stdout)
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

// WithMaxBodyBytes 设置请求体读取上限（字节）：读取超过 n 的部分立即失败
// （*http.MaxBytesError），`Ctx.Bind` 将其映射为 413 + code body_too_large。
//
// 默认 0 = 不限——上限值是业务策略（JSON API 2MB 与文件上传 100MB 不可能
// 同值），与 gin / echo 的默认形态一致（gin 无默认限制；echo 的 BodyLimit
// 中间件需显式启用）。生产建议显式设置，或依赖前置反代
// （nginx 默认 client_max_body_size 1m）。
//
// 实现分层：请求入口先按 Content-Length 快速失败（声明即超限时不读 body），
// 声明未知（chunked）或声明偏小时由 http.MaxBytesReader 读取闸门兜底。
// 上限对**所有**读取路径生效（绑定、用户自读 body、中间件读 body），
// 不依赖调用方记得包一层。
func WithMaxBodyBytes(n int64) Option {
	return func(cfg *config) { cfg.maxBodyBytes = n }
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

// WithCollector 每请求把 `observability.Collector` 装进请求作用域，让宿主交付
// 请求 scope 的**库对象**能用 `kernel.Get(scope, observability.CollectorKey)`
// 把记录写进本次请求的 TraceID。
//
// # 它服务的是「库作者」，不是「应用作者」
//
// 应用作者（自己的 controller / service / dao）把 `c` 或 `c.Observe` 往下传就够，
// 不需要它。需要它的是那种「想同时活在 web 请求与 CLI / worker 里、因此不能
// import pulse-web、只收 `*kernel.Context`」的包：
//
//	// reconcile 包：既被 web 请求调用，也被后台 worker 调用
//	func Run(scope *kernel.Context, batch []Order) error {
//		// 在 web 请求里跑：有 Collector → 记录挂到本次请求的 trace_id
//		if col, ok := kernel.Get(scope, observability.CollectorKey); ok {
//			col.WriteAttrs("reconcile.done", "ok", nil)
//		}
//		// 在 CLI / worker 里跑：没有 → 静默跳过（那边有自己的日志）
//		return nil
//	}
//
// **必须由宿主把请求 scope 显式传进去**。这条选项本身不服务 kernel 插件（插件自取不到）：
//
//	请求 scope 自身       ✅      请求 scope 的后代    ✅
//	宿主 root             ❌      插件私有 scope       ❌（与请求 scope 是兄弟）
//	并发请求各自的 scope   ✅（互不遮蔽，各持自己的 TraceID）
//
// 插件的私有 scope 是 root 的另一个子节点，与请求 scope 同级，永远读不到；用
// `kernel.Require(CollectorKey)` 声明依赖的插件会**静默**停在 inactive（探针
// 实测：`Get` 返回 false、`Apply` 从未调用）。插件在请求路径上没有**自取**通道
// ——连请求 scope 的 `EmitLocal` 也收不到（只派发本层），收得到的全树 `Emit`
// 比它慢 77 倍、是请求路径禁用的。上游 pulse#189 已核销：不改代码，表述收正为
// 「没有交付通道」。
//
// 插件要参与请求级观测，走**宿主交付**，交付物按调用形态选：
//
//	同步调用        `*Ctx`（`c.Observe` 直写 Sink，零 scope 开销）
//	跨 goroutine    `Detached`（`c.Detach()` 拷贝的值袋子，见 detach.go；`Ctx` 不可跨 goroutine）
//	要 scope 查找   本选项（宿主交付请求 scope，接收方 `kernel.Get(CollectorKey)`，成本见下）
//
// 成本（实测，表 B 口径）：约 **+349 ns / +12 allocs 每请求**（端到端；kernel 层
// `AttachCollector` 相对基线 +261 ns / +12 allocs），且与插件树规模解耦
// （100 插件下 370 ns）。默认关。
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
	maxBodyBytes     int64
	spanHook         SpanHook

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
		sink = newDefaultSink()
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

	// 同上：装了 span 出口却没有 hook 是配置错误（写成 WithSpanHook(nil) 多半是
	// 上游变量没初始化），装配期暴露比运行期静默没 span 好排查。
	if cfg.spanHook == nil && cfg.spanHookSet {
		panic("web: WithSpanHook(nil) — span hook must not be nil")
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
		maxBodyBytes:     cfg.maxBodyBytes,
		spanHook:         cfg.spanHook,
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

// ServeHTTP 实现 http.Handler；装配与收尾见 withCtx。
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.withCtx(w, r, func(c *Ctx) {
		e.mux.ServeHTTP(c.w, c.r)
	})
}

// begin 是**请求级装配的唯一实现**：派生请求作用域、造 Ctx、解析链路身份、挂请求级
// collector、把 Ctx 注入 context。返回的 end 负责收尾（dispose scope + 写访问日志）。
//
// ServeHTTP 与测试入口（#63）都从它出发——「构造出来的 Ctx 与真路径同构」因此不是靠
// 纪律维持的，而是结构上只有这一处装配代码。
//
// kernel 已销毁（进程关闭中）时返回 nil，此时 503 已经写到 w。
func (e *Engine) begin(w http.ResponseWriter, r *http.Request) *Ctx {
	scope, err := e.kernel.Derive()
	if err != nil {
		// kernel 已销毁（进程关闭中）
		http.Error(w, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		return nil
	}

	rw := &responseWriter{ResponseWriter: w}
	c := &Ctx{
		engine:  e,
		w:       rw,
		r:       r,
		scope:   scope,
		started: time.Now(),
	}
	// 链路身份：trace-id（沿用入站或新起）；装了 SpanHook 时再向 hook 要 span
	// 身份——span-id 由追踪体系分配（SDK 没有「指定 span-id」的入口），框架只
	// 采用它，这样 Server-Timing、记录里的 span.id、下游注入的 traceparent
	// 用的是同一个真实存在的 id（自己编一个会让下游挂到不存在的父 span 上）。
	//
	// 两个响应头分工不同，别混（详见 span.go 里 serverTimingHeader 的注释）：
	//   X-Trace-Id    既有契约：自定义头，只有 trace-id，任何模式都写。
	//   Server-Timing W3C 定义的响应侧绑定：带**本请求 span-id**，拿到身份才写。
	var beginCtx context.Context
	if e.trace || e.spanHook != nil {
		in := e.resolveSpanInfo(r)
		in.Method = r.Method
		c.traceID = in.TraceID
		if e.spanHook != nil {
			ctx, ref := e.spanHook.Begin(r.Context(), in)
			beginCtx = ctx
			if ref.TraceID != "" {
				// 追踪体系是身份的事实源：它可能采纳了别的链路头（宿主配了
				// B3 / Jaeger propagator 时），记录与响应头跟着它走。
				c.traceID = ref.TraceID
			}
			c.spanID = ref.SpanID
			if v := serverTimingValue(ref); v != "" {
				rw.Header().Set(serverTimingHeader, v)
			}
		}
		if e.trace && e.traceHeader != "" {
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

	// span 注入点：hook 在路由与 handler 之前建好 span 并把它注入 ctx，
	// 这样中间件、handler，以及它们往下传的 context 都能看到——下游支持 OTel
	// 的库（出站 HTTP 客户端 / SQL / gRPC）因此自动接上这条链路。
	//
	// 注入发生在 ServeMux 之前，所以 http.route 这时还不知道；按 semconv，
	// 路由后拿到再补是允许的（只要在 span 结束前）。
	base := r.Context()
	if beginCtx != nil {
		base = beginCtx
	}

	req := r.WithContext(context.WithValue(base, ctxKey{}, c))
	// c.r 也指向 req：没走到注册处理器的请求（ServeMux 直接给 404）不会经过
	// register，只有在这里换，End 才能顺着 c.r.Context() 取回 hook 注入的
	// 那条 context 链（span 就挂在那里）。匹配到的请求随后会被 register
	// 换成同一个 request（ServeMux 只是把 Pattern 写在它上面）。
	c.r = req

	return c
}

// end 与 begin 配对收尾。**是方法不是闭包**——闭包每次请求都会逃逸到堆，正好撞上
// 分配预算门禁（实测：返回闭包的版本让七个档位各 +1 alloc）。
func (e *Engine) end(c *Ctx) {
	c.scope.Dispose()
	e.finish(c, c.w)
}

// withCtx 在装配之上负责**执行与收尾**：请求体闸门 → call → recover → end。
//
// 执行顺序（关键时序）：
//
//	中间件链 → handler 返回
//	  → scope.Dispose()       业务态结束（请求级 Effect 立即回收，先于写响应）
//	  → 错误映射 + 写响应      网络态
//	  → AccessLog 落盘
func (e *Engine) withCtx(w http.ResponseWriter, r *http.Request, call func(*Ctx)) {
	c := e.begin(w, r)
	if c == nil {
		return
	}

	defer func() {
		if p := recover(); p != nil {
			c.setErr(&PanicError{Value: p, Stack: debug.Stack()})
		}
		e.end(c)
	}()

	// 请求体上限（WithMaxBodyBytes）：与路由级 BodyLimit 共用 limitBody ——
	// 声明即超限则快速失败（不读 body），否则由读取闸门兜底。传的是这里拿到的
	// **原始 writer**（理由见 limitBody 的 godoc）。
	//
	// 注意传 c.r（WithContext 的浅拷贝）而不是 r：body 要装在 ServeMux 收到的
	// 那个 request 上。
	if e.maxBodyBytes > 0 {
		if err := limitBody(w, c.r, e.maxBodyBytes); err != nil {
			c.setErr(err)
			return
		}
	}

	call(c)
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

	// 收尾落码与 Write / Flush 同源（writeHeaderNow）：吃 Status() 提示、缺省 200。
	rw.writeHeaderNow()

	// 路由模板只算一次，喂给 span 与访问记录两边——`Span.Route` 与 `http.route`
	// 属性是同一个值，各算一遍迟早会漂（routePattern 现在只是切字符串，但那是
	// 实现细节，不是契约）。
	route := routePattern(c.r)

	// span 先于记录：End 落在响应写完这一刻，不被 sink 的写入耗时污染。
	if e.spanHook != nil {
		e.emitSpan(c, rw, route)
	}

	if e.accessLog && e.sink != nil {
		e.writeAccessLog(c, rw, route)
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

func (e *Engine) writeAccessLog(c *Ctx, rw *responseWriter, route string) {
	rec := observability.Record{
		HostID:   e.hostID,
		TraceID:  c.traceID,
		Source:   observability.Source(sourceHTTP),
		Event:    eventHTTPReq,
		Status:   strconv.Itoa(rw.status),
		Duration: time.Since(c.started),
		Err:      c.err,
	}
	fillRequestAttrs(c, rw, route, &rec.Attrs)
	if c.spanID != "" {
		// 日志 ↔ trace 的关联键：只有拿到 span 身份时才存在（没有 span 就没有
		// span-id 可指），所以它是**有条件**出现的字段。
		observability.Set(&rec.Attrs, attrSpanID, c.spanID)
	}
	e.sink.Write(rec)
}

// fillRequestAttrs 填本请求的观测属性。
//
// 访问日志与 span **共用这一个来源**：同一批字段写两遍必然漂移，而「日志里
// 的状态码与 span 里的对不上」是最难发现的那类不一致。route 由调用方算好传进来
// ——Span.Route 与 `http.route` 属性是同一个值，两处各算一遍迟早会漂。
//
// # 有意不记的三个 semconv 属性（对规范的**显式偏差**）
//
// `url.scheme` / `server.address` / `network.protocol.version` 在 semconv 里是
// Recommended（不是 Required），本框架**不记**，理由是它们记录的是「我这台服务
// 自己的监听信息」而不是请求事实：
//
//	url.scheme               反代后面拿到的是内网 scheme（http），照记等于写错；
//	                         真要它，宿主在边缘那层记才是对的
//	server.address           同源：监听地址 / Host，不是调用方看到的地址
//	network.protocol.version 只对 HTTP/1.1 与 HTTP/2 有意义，且与业务无关
//
// 这是一条**声明过的偏差**（设计文档「trace 头兼容与 span 出口」一节同一句话），
// 不是漏记——补记任何一个之前先想清楚「谁才是这个事实的事实源」。
func fillRequestAttrs(c *Ctx, rw *responseWriter, route string, attrs *observability.Attrs) {
	observability.Set(attrs, attrHTTPMethod, c.r.Method)
	observability.Set(attrs, attrHTTPRoute, route)
	observability.Set(attrs, attrURLPath, c.r.URL.Path)
	observability.Set(attrs, attrHTTPBodySize, int64(rw.bytes))
	observability.Set(attrs, attrClientAddr, c.r.RemoteAddr)
	if c.err != nil {
		observability.Set(attrs, attrErrorType, errCategory(c.err))
	}
}

// emitSpan 把本次请求的 span 数据交给 SpanHook（只在装了 hook 时调用）。
//
// 三个字段要注意来源：Status 是**映射后**的状态码（与访问日志同源，不是
// handler 里写的那个）、Route 是 ServeMux 写在 request 上的路由模板、
// Attrs 与访问日志同一份（fillRequestAttrs），不含 span.id。
//
// 这里的 Attrs 与 writeAccessLog 里那份是**两次独立填充**（不是共用一块缓冲）：
// 记录侧要多一个 `span.id` 关联键，共用就得原地改写 span 手里那份。代价实测
// （同轮、装一个只回身份的 nop hook 与不装对照）22 → 26 allocs/op、
// 6314 → 6819 B/op——这一段差里既有这一遍填充，也有 Server-Timing 的字符串
// 拼装；换掉的是「两者谁先读谁后读」这类隐性耦合。
func (e *Engine) emitSpan(c *Ctx, rw *responseWriter, route string) {
	var attrs observability.Attrs
	fillRequestAttrs(c, rw, route, &attrs)
	e.spanHook.End(c.r.Context(), Span{
		TraceID: c.traceID,
		SpanID:  c.spanID,
		Method:  c.r.Method,
		Route:   route,
		Path:    c.r.URL.Path,
		Status:  rw.status,
		Start:   c.started,
		End:     time.Now(),
		Err:     c.err,
		Attrs:   &attrs,
	})
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
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	return e.serve(ln, sigCh)
}

// serve 是 Serve 的执行体：把 ServerConfig 装进 http.Server，跑到 server 出错
// 或收到关闭信号为止。
//
// 信号源是参数、不在这里 signal.Notify ——「收到信号 → 优雅关闭」是公开入口的
// 关键路径，而真实信号无法在测试里确定性地制造（Windows 上给自身进程投递
// os.Interrupt 不可行）。拆成参数后，测试注入一个受控 channel 即可跑通全链路，
// 且不引入包级可变状态。
func (e *Engine) serve(ln net.Listener, sigCh <-chan os.Signal) error {
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
