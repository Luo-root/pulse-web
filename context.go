package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"time"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// Handler 是框架的请求处理器：返回的 error 由集中错误映射处理。
type Handler func(*Ctx) error

// Middleware 是洋葱模型中间件：自行决定是否、何时调用 next。
type Middleware func(c *Ctx, next Handler) error

// Key 是请求级 KV 的类型化键（与 kernel.ServiceKey 同构，但独立命名空间）。
type Key[T any] struct{ name string }

// NewKey 定义请求级 KV 键。
func NewKey[T any](name string) Key[T] { return Key[T]{name: name} }

// Ctx 是一次请求的上下文。
//
// 生命周期与线程安全：Ctx 及其绑定的 ResponseWriter **不可跨 goroutine 使用**；
// 请求结束后 kernel scope 静默失效（Get 返回 false、Effect 返回 ErrDisposed）。
type Ctx struct {
	engine  *Engine
	w       *responseWriter
	r       *http.Request
	scope   *kernel.Context
	traceID string
	// spanID 是本次请求的 span 标识（16hex）；**只有装了 WithSpanHook 才非空**。
	// 它由追踪体系分配、经 SpanRef 交回框架（理由见 span.go 顶部注释），
	// 空串的含义是「这次请求没有 span」。
	spanID  string
	started time.Time

	kv  map[string]any
	err error
}

// ---- 请求信息 ----

// Request 返回底层 *http.Request（只读）。
func (c *Ctx) Request() *http.Request { return c.r }

// Context 返回请求的 context.Context（== Request().Context()）。
//
// 把 ctx 交给下游库时用它——`c.Request().Context()` 是纯噪音。取消语义与底层请求
// 一致：客户端断开连接（或服务端强制关掉连接）时被取消。
func (c *Ctx) Context() context.Context { return c.r.Context() }

// Path 返回路径参数值——与 Request().PathValue 同源（ServeMux 写入的同一份），不另存。
func (c *Ctx) Path(name string) string { return c.r.PathValue(name) }

// Query 返回查询参数。
func (c *Ctx) Query(name string) string { return c.r.URL.Query().Get(name) }

// Cookie 返回请求里名为 name 的 cookie；不存在时返回 http.ErrNoCookie。
func (c *Ctx) Cookie(name string) (*http.Cookie, error) { return c.r.Cookie(name) }

// TraceID 返回本请求的 32hex trace 标识。
func (c *Ctx) TraceID() string { return c.traceID }

// SpanID 返回本请求的 16hex span 标识；**没装 WithSpanHook 时是空串**。
//
// 空串的含义是「这次请求没有 span」——不要拿它当占位符去填日志或响应头。
// 它唯一的用途是让调用方自己把请求内的工作与 trace 关联起来（例如后台任务
// 用 c.Detach() 带走 TraceID + SpanID，在追踪体系里建一条显式的 span link）。
func (c *Ctx) SpanID() string { return c.spanID }

// Kernel 返回**请求 scope**：登记 Effect、注册事件监听、派生更深 scope、
// 或传给需要作用域的组件。它是随请求销毁的，不是进程级 kernel ——
// 进程级请用 Engine.Root()。
func (c *Ctx) Kernel() *kernel.Context { return c.scope }

// ---- 请求级 KV（框架自有；不进 kernel 仓库、零广播、O(1)）----

// Set 写入请求级值。
func (c *Ctx) Set[T any](k Key[T], v T) {
	if c.kv == nil {
		c.kv = make(map[string]any, 4)
	}
	c.kv[k.name] = v
}

// Get 读取请求级值。
func (c *Ctx) Get[T any](k Key[T]) (T, bool) {
	v, ok := c.kv[k.name]
	if !ok {
		var zero T
		return zero, false
	}
	tv, ok := v.(T)
	return tv, ok
}

// ---- 全局服务（kernel root 仓库；任何活 scope 都能读）----

// Service 读取全局服务。
func (c *Ctx) Service[T any](k kernel.ServiceKey[T]) (T, bool) {
	return kernel.Get(c.scope, k)
}

// MustService 读取全局服务，缺失即 panic —— 缺失属装配错误，应在启动期暴露。
func (c *Ctx) MustService[T any](k kernel.ServiceKey[T]) T {
	return mustService(c.scope, k)
}

// mustService 是 Ctx.MustService 与 Detached.MustService 的共用实现：
// 两者只在「从哪个 scope 读」上不同，panic 文案是一条语义，不该有两份。
func mustService[T any](scope *kernel.Context, k kernel.ServiceKey[T]) T {
	v, ok := kernel.Get(scope, k)
	if !ok {
		panic("web: service not provided: " + k.Name())
	}
	return v
}

// ---- 观测打点 ----

// Observe 直写一条业务记录到 Sink —— 不经 scope、零广播，与请求共享 TraceID。
// 实现见 writeObservation（与 Detached.Observe 共用）。
func (c *Ctx) Observe(event string, set func(*observability.Attrs)) {
	writeObservation(c.engine.sink, c.engine.hostID, c.traceID, event, set)
}

// ---- 响应写出 ----

// SetHeader 设置响应头（必须在首次写出之前调用）。
func (c *Ctx) SetHeader(key, value string) { c.w.Header().Set(key, value) }

// Writer 返回响应写出器，供**流式**场景直接写字节（SSE / chunked / 大文件）。
// 常规响应仍用 JSON / Text —— 它们会设好 Content-Type 与状态码。
//
//	c.SetHeader("Content-Type", "text/event-stream")
//	w := c.Writer()
//	for ev := range events {
//		fmt.Fprintf(w, "data: %s\n\n", ev)
//		if err := c.Flush(); err != nil { return err }  // 首刷落 Status() 设置（缺省 200），此后逐段推送
//	}
//
// 返回的是框架的包装器：状态码与响应体积照常被 AccessLog 采集（写多少字节就记多少）。
//
// **能力面是一份显式的窄清单**：包装器显式实现并转发底层的四个——`Flush`、`Hijack`、
// `SetWriteDeadline`、`EnableFullDuplex`；`http.Pusher` 与 `interface{ FlushError() error }`
// **不透出**，也**不提供** `Unwrap() http.ResponseWriter`——那等于把底层 writer 整个
// 交出去，连上面两个一起。于是 `http.NewResponseController(c.Writer())` 上这四件事都可用，
// 其余没有（内嵌 `http.ResponseWriter` 只提升 `Header` / `Write` / `WriteHeader`）。
//
// 协议升级（WebSocket）走 `Hijack`：生态库零改动可用（`gorilla/websocket` 直接断言
// `w.(http.Hijacker)`，`coder/websocket` 先断言、再沿 `Unwrap()` 链找），交出连接之后
// 框架不再写这条响应。前提是底层 writer 支持——HTTP/2 不支持，此时返回明确 error。
func (c *Ctx) Writer() http.ResponseWriter { return c.w }

// Status 只**设置**状态码，不立即写出；由 JSON / Text、首刷（直接写字节 / Flush）
// 或引擎收尾时落定。首刷之后响应头已发出，再调 Status 不会改变已落定的状态码。
func (c *Ctx) Status(code int) *Ctx {
	c.w.statusHint = code
	return c
}

// JSON 写出 JSON 响应。
//
// **先编码成功、后写响应头**：编码失败（chan / func / 环）在写头之前返回
// error，交由统一错误映射成 5xx —— 否则 WriteHeader 先生效，「已写响应不被
// 覆盖」规则会把错误吞成一个 200 空体（与 c.HTML 同一课，见其注释）。
// 编码走 json.Encoder → buffer：成功路径的字节与直接编码到响应逐一致
// （含 Encoder 的尾换行）。
func (c *Ctx) JSON(code int, v any) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	c.w.WriteHeader(code)
	_, err := buf.WriteTo(c.w)
	return err
}

// Text 写出纯文本响应。
func (c *Ctx) Text(code int, s string) error {
	c.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	c.w.WriteHeader(code)
	_, err := c.w.Write([]byte(s))
	return err
}

// Blob 写出原始字节：code + 调用方给定的 Content-Type + body。
//
// 与 Text 的差别是**不补 charset、不假设文本**——Content-Type 原样写出，给什么写什么。
//
// **空串的边界**（实测，见 `TestBlobEmptyContentTypeIsEmptyNotSniffed`）：传 `""` 写出的
// 是一条**空的** Content-Type 头——标准库判断要不要嗅探，看的是「这个头在不在」而不是
// 「值空不空」，所以它不会替你猜。反过来，**整个不设**这个头才会触发嗅探（HTML 载荷会
// 被认成 `text/html; charset=utf-8`，那才是更难看的默认）。空串比「不设」安全，但也不是
// 调用方想要的结果——类型不该猜，请给准。
func (c *Ctx) Blob(code int, contentType string, b []byte) error {
	c.w.Header().Set("Content-Type", contentType)
	c.w.WriteHeader(code)
	_, err := c.w.Write(b)
	return err
}

// Redirect 回复一个重定向：设置 Location 并**立即落定 code**。
//
// 走标准库 http.Redirect，语义与它逐字一致——相对路径按**请求路径**补成绝对、
// 非 ASCII 转义成 %XX、GET 请求带一段 HTML 提示体（HEAD 与 POST 不带）。
// code 不做范围校验，非 3xx 也照写（http.Redirect 同样不校验）。
//
// **它与 Status 的差别在落码时机**：这里响应头当场发出，之后再 return error 也不会
// 被错误映射接管。要先做检查再重定向，检查放在调用之前。
//
// 返回值与 File / NoContent 同理：标准库的这条路径不回报写出结果，恒为 nil——
// 留着是为了让「写出即 return」在全部写出方法上形态一致。
func (c *Ctx) Redirect(code int, url string) error {
	http.Redirect(c.w, c.r, url, code)
	return nil
}

// NoContent 声明一个无正文响应（204 / 304 等）。
//
// 与 Status 同源：只设置状态码，由首刷（含引擎收尾）落定——它**不**声称响应已发出，
// handler 之后 return error 仍会被错误映射接管。与 `Status(204)` 的差别只有可读性。
func (c *Ctx) NoContent(code int) error {
	c.Status(code)
	return nil
}

// SetCookie 追加一个 Set-Cookie 响应头（必须在首刷之前调用）。
//
// 走标准库 http.SetCookie：同名 cookie 是**追加**而不是覆盖，一次响应可以写多条；
// cookie 值里的非法字节被标准库**丢掉**（不是报错）并记一条日志，剩余部分照发。
func (c *Ctx) SetCookie(cookie *http.Cookie) { http.SetCookie(c.w, cookie) }

// File 把磁盘文件作为响应体写出（走标准库 http.ServeFile）。
//
// 拿来的是标准库的完整语义：Range 与 If-Modified-Since / If-None-Match、按扩展名与
// 内容嗅探 Content-Type、目录命中 index.html 时的 301。
//
// **失败由标准库直接写响应**——文件不存在写 404、不可读写写 403，**不经**框架的错误
// 映射（响应形态与 `web.NotFound` 那条路不同，观测记录里也没有错误属性）。要在缺失时
// 走统一错误面，自己先 os.Stat 再返回 `web.NotFound`。
//
// 返回值恒为 nil：http.ServeFile 不回报写出结果（它已经决定了响应）。
func (c *Ctx) File(path string) error {
	http.ServeFile(c.w, c.r, path)
	return nil
}

// Attachment 以「下载」形式写出磁盘文件：`Content-Disposition: attachment` + File 的全部语义。
//
// name 是客户端看到的建议文件名，交给 mime.FormatMediaType 编码——ASCII 名字裸写，
// 含非 ASCII 或控制字符时走 RFC 2231（`filename*=utf-8”%E4%B8%AD%E6%96%87.pdf`）。
// 中文名只有这样才带得出去；手拼引号（`filename="中文.pdf"`）在部分客户端上是乱码。
func (c *Ctx) Attachment(path, name string) error {
	c.w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeFile(c.w, c.r, path)
	return nil
}

// Flush 把已写内容刷到客户端（SSE / 流式响应）。
//
// 首刷落 Status() 设置的状态码（缺省 200）——首刷之后响应头已发出，
// 再调 Status 不会改变已落定的状态码。底层 writer 不支持 http.Flusher 时
// 返回明确 error，且不产生任何写出副作用。
func (c *Ctx) Flush() error { return c.w.flush() }

// responseWriter 包装 http.ResponseWriter，采集状态码与响应体积。
// 首次写入生效：重复 WriteHeader 是 no-op（不产生 superfluous WriteHeader 警告）。
//
// # 能力面是一份显式的窄清单
//
// 只实现 `Flush` / `Hijack` / `SetWriteDeadline` / `EnableFullDuplex` 四个，且都
// 转发给底层；`Pusher` / `FlushError` **不透出**。刻意**不**实现
// `Unwrap() http.ResponseWriter`——那等于把底层 writer 整个交出去（连同上面两个
// 不承诺的），窄口就白收了；`http.ResponseController` 认的那几个方法这里逐个显式
// 实现，效果一样而面是可枚举的。
//
// 要升级协议（WebSocket）走 `Hijack`，见它的 godoc。
type responseWriter struct {
	http.ResponseWriter
	status     int // 已落定的状态码（AccessLog 采样）
	statusHint int // Status() 设置的意图：任何隐式落码都用它，0 → 200
	bytes      int
	wrote      bool
	hijacked   bool // 连接已交出（协议升级）：此后不写出、也不统计体积
}

func (w *responseWriter) WriteHeader(code int) {
	if w.wrote || w.hijacked {
		return
	}
	w.wrote = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// writeHeaderNow 落定首刷状态码：吃 Status() 的提示（缺省 200）。
//
// 三条隐式落码路径共用它——Write、flush、引擎收尾。**写一段再 Flush 是流式
// handler 的自然顺序**，若只有 flush 吃提示，`Status(201)` 会被第一次 Write
// 的隐式 200 吃掉（gin 的 WriteHeaderNow() 是同一语义）。
func (w *responseWriter) writeHeaderNow() {
	if w.wrote || w.hijacked {
		return
	}
	code := w.statusHint
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
}

func (w *responseWriter) Write(b []byte) (int, error) {
	if w.hijacked {
		// 连接已交出，框架不再碰它——与 net/http 自己在 hijack 之后的写行为同型。
		return 0, http.ErrHijacked
	}
	w.writeHeaderNow()
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Flush 实现 http.Flusher（供 c.Writer().(http.Flusher) 与
// http.ResponseController.Flush() 路径调用）。接口签名无返回值，无法把
// 「底层不支持」报出——需要明确 error 请用 Ctx.Flush()。
func (w *responseWriter) Flush() { _ = w.flush() }

// flush 是两条 Flush 路径（Ctx.Flush 与包装器 Flush）的唯一实现：
// 首刷落 Status() 提示（缺省 200）；底层不支持 http.Flusher 时返回明确
// error，且不写任何内容（显式失败不产生副作用）。
func (w *responseWriter) flush() error {
	if w.hijacked {
		return http.ErrHijacked
	}
	f, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return errors.New("web: ResponseWriter does not support Flush")
	}
	w.writeHeaderNow()
	f.Flush()
	return nil
}

// Hijack 实现 http.Hijacker：把连接连同缓冲读写器交给调用方——协议升级的门，
// 典型场景是 WebSocket。
//
// # 为什么需要它
//
// 生态里的 websocket 库都要拿走连接，而拿走的方式只有两条，且都在 net/http 的
// 语义之内：`gorilla/websocket` 直接断言 `w.(http.Hijacker)`；`coder/websocket`
// 先断言、找不到再沿 `Unwrap()` 链往下找。此前包装器只实现 `http.Flusher`，
// 两家都拿不到连接（分别以 500 / 501 结束）。本方法是那条缝的唯一出口，**不打开它
// 就没有任何变通**：`Wrap` 交出去的仍是这个包装器，`Adapt` 只改中间件的位置、
// 不改 writer。
//
// # 交出去之后
//
// 连接不再归框架管：框架写不进、也不该写（此后 `Write` / `Flush` 返回
// `http.ErrHijacked`），收尾阶段跳过落码。访问日志照写一条，但**状态码与体积
// 不再是框架能观测的事实**——约定见 fillRequestAttrs：框架侧未落定状态码时按
// 101 记，并给记录打上 `connection.hijacked` 标记；体积不记（记 0 会被读成
// 「响应是空的」）。两条库的时序差异（一个先写 101 再 hijack、一个先 hijack
// 再自己往裸连接写）正是这条约定的由来。
//
// 底层不支持 Hijack 时返回明确 error（HTTP/2 就是这种情况，h2 的 writer 不是
// Hijacker）——不 panic，也不静默放行。
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("web: ResponseWriter does not support Hijack: %w", http.ErrNotSupported)
	}
	conn, buf, err := h.Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.hijacked = true
	return conn, buf, nil
}

// SetWriteDeadline 转发 http.ResponseController 的写超时设置：底层（net/http 的
// *response）本来就有这个能力，此前被包装器挡住。
//
// 它不改框架的采集状态——超时到期后写失败由调用方的 error 处理负责。
func (w *responseWriter) SetWriteDeadline(deadline time.Time) error {
	d, ok := w.ResponseWriter.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		return fmt.Errorf("web: ResponseWriter does not support SetWriteDeadline: %w", http.ErrNotSupported)
	}
	return d.SetWriteDeadline(deadline)
}

// EnableFullDuplex 转发 http.ResponseController 的全双工开关：默认情况下
// net/http 会在开始写响应之前把请求体读完，打开它才允许一边读 body 一边写响应
// （长连接协议、边收边转的场景）。同样的能力此前被包装器挡住。
func (w *responseWriter) EnableFullDuplex() error {
	d, ok := w.ResponseWriter.(interface{ EnableFullDuplex() error })
	if !ok {
		return fmt.Errorf("web: ResponseWriter does not support EnableFullDuplex: %w", http.ErrNotSupported)
	}
	return d.EnableFullDuplex()
}
