package web

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Luo-root/pulse/observability"
)

// 访问日志的属性键（OTel HTTP 语义约定）。engine 写入与出口读取共用同一组
// 常量——同一批字符串在两处各写一遍，改一处漏一处。
const (
	attrHTTPMethod   = "http.request.method"
	attrHTTPRoute    = "http.route"
	attrURLPath      = "url.path"
	attrHTTPBodySize = "http.response.body.size"
	attrClientAddr   = "client.address"
	attrErrorType    = "error.type"
)

// 列宽与版式常量。列宽按**显示宽度（rune 数）**算，不是字节数：`µ` 是 2 字节
// 1 列，按 len() 补齐会错位。
const (
	consoleTimeLayout   = "2006/01/02 - 15:04:05" // 21 列
	consoleSep          = " | "
	colStatus           = 3
	colDuration         = 9
	colClient           = 15
	colMethod           = 7 // 方法列宽；再追加一个空格 ⇒ 含分隔共 8 宽，7 字符方法（OPTIONS）也对齐
	consoleBufSize      = 256
	consoleMaxPooledBuf = 4 << 10
)

// ANSI 颜色码。只在目的地是终端时使用（见 isTerminal）——重定向到文件 / 管道时
// 上色等于往日志里混转义序列。
const (
	ansiReset  = "\x1b[0m"
	ansiDim    = "\x1b[90m"
	ansiRed    = "\x1b[31m"
	ansiYellow = "\x1b[33m"
	ansiGreen  = "\x1b[32m"
	ansiCyan   = "\x1b[36m"
)

// ConsoleSink 是框架默认出口：把 Record 渲染成一行**给人读**的文本。
//
// # 为什么有它
//
// 上游默认出口（`observability.SlogSink`）是**给机器读**的：固定前缀
// `time=… level=INFO msg=pulse.observability` 每行都一样、字段按字母序排、
// 亚毫秒耗时被取整成 `duration_ms=0`，而且每条都要 `make([]any, 0, 18)` 再交给
// slog。默认体验因此是「又贵又难扫」——但 `Record` 里的字段一个不少，
// 缺的只是一个渲染层（实测与选型见仓库 Issue #20）。
//
// # 版式
//
// http 请求（一行，列宽固定；下面是 `TestConsoleSinkHTTPLine` 钉住的那一行）：
//
//		2026/09/14 - 08:30:00 | 200 |   585.1µs | 192.0.2.1:1234  | GET     /users/42 | route=/users/{id} | size=29 | host=pulse-web | trace=8f2e1a3b4c5d6e7f8a9b0c1d2e3f4a5b
//
//	  - 时间列是**完成时刻**（与 gin 的 Logger 一致：请求结束后才写这一行）；
//	  - 状态列按区间上色（2xx 绿 / 3xx 青 / 4xx 黄 / 5xx 红），仅在终端生效；
//	  - 耗时带单位且**不取整**（`820ns` / `585.1µs` / `7.62ms` / `1.23s`）——
//	    `duration_ms=0` 看不出快慢；
//	  - 路径列给**具体路径**（`/users/42`，与 gin/chi 的直觉一致），路由模板在
//	    两者不同时另起 `route=` 附在后面（模板是聚合维度，路径是复现维度）；
//	  - 尾段的 `route=` / `size=` / `host=` / 错误 / `trace=` 是**各自独立的
//	    ` | ` 字段**：有才出现、缺就少一段（`host=` 只在配置了 `WithHostID` 时出现）；
//	  - 非 http 记录（装配期 `observability.host_ready` / `fiber_state`、业务
//	    `Ctx.Observe`）退回 `时间 | event | k=v …`：它们没有 status / route，
//	    硬套列式只会渲染出一堆空列；
//	  - 固定列盖不住的属性**不丢**：未知键按插入序附在 `|` 之后（如 `llm.model=…`）。
//
// # 成本
//
// 渲染一条 **~190 ns / 0 allocs**（`-benchtime=20000x`；同一套 bench 里
// `SlogSink` 是 1531 ns / 19 allocs、`LineSink` 是 ~262 ns / 1 alloc），
// 并发 32 路 ~167 ns / 0 allocs。手法都是常识：池化缓冲、不用 fmt
// （时间走 `time.AppendFormat`、数字走 `strconv.Append*`）、一次锁一次 `Write`。
//
// 错误行会调用 `Err.Error()`（错误对象怎么拼字符串不归出口管），可能带一次分配；
// 没有错的请求路径是 0 alloc。
//
// # 与其它出口的关系
//
// 它**不缓冲**：写完即落 `io.Writer`——终端要即时，缓冲会把安静应用的日志扣在
// 内存里等下一次触发。要吞吐或异步就 `WithSink(observability.NewAsyncSink(…))`；
// 要机器可读就 `WithSink(observability.SlogSink{…})` / `NewLineSink`。
// `WithSink` 只换出口，不改变装配。
//
// 写错误被忽略（`Sink` 接口没有错误通道，与 gin / chi 的控制台输出一致）；
// 需要错误可见的场合用 `observability.LineSink`（有 `Err()`）。
type ConsoleSink struct {
	mu    sync.Mutex
	w     io.Writer
	color bool
	pool  sync.Pool
}

// ConsoleOption 配置 ConsoleSink。
type ConsoleOption func(*ConsoleSink)

// WithColor 强制开 / 关 ANSI 颜色。缺省按目的地自动判断：`w` 是 `*os.File`
// 且为字符设备（终端）才上色。
func WithColor(on bool) ConsoleOption {
	return func(s *ConsoleSink) { s.color = on }
}

// NewConsoleSink 构造控制台出口。w 为 nil 视为编程错误（构造期 panic）；
// w 只需被本出口串行调用（内部已加锁），不需要自身并发安全。
func NewConsoleSink(w io.Writer, opts ...ConsoleOption) *ConsoleSink {
	if w == nil {
		panic("web: ConsoleSink requires a non-nil io.Writer")
	}
	s := &ConsoleSink{w: w, color: isTerminal(w)}
	s.pool.New = func() any {
		b := make([]byte, 0, consoleBufSize)
		return &b
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Write 实现 observability.Sink。并发安全，只读传入的 Record。
func (s *ConsoleSink) Write(r observability.Record) {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}

	bp := s.pool.Get().(*[]byte)
	b := (*bp)[:0]
	if r.Event == eventHTTPReq && r.Status != "" {
		b = s.appendHTTPLine(b, r)
	} else {
		b = s.appendEventLine(b, r)
	}

	s.mu.Lock()
	_, _ = s.w.Write(b)
	s.mu.Unlock()

	if cap(b) > consoleMaxPooledBuf {
		b = nil // 异常长的一行不该把大缓冲钉在池里
	}
	*bp = b
	s.pool.Put(bp)
}

// appendHTTPLine 渲染 http 请求记录（固定列 + 尾段）。
func (s *ConsoleSink) appendHTTPLine(b []byte, r observability.Record) []byte {
	method, okMethod := observability.Get[string](r.Attrs, attrHTTPMethod)
	route, okRoute := observability.Get[string](r.Attrs, attrHTTPRoute)
	path, okPath := observability.Get[string](r.Attrs, attrURLPath)
	client, okClient := observability.Get[string](r.Attrs, attrClientAddr)
	errType, okErrType := observability.Get[string](r.Attrs, attrErrorType)
	size, okSize := observability.Get[int64](r.Attrs, attrHTTPBodySize)

	// 时间列（暗淡，扫读时不该跟正文抢注意力）
	b, painted := s.paint(b, ansiDim)
	b = r.Time.AppendFormat(b, consoleTimeLayout)
	b = s.unpaint(b, painted)

	// 状态列（右对齐）
	b = append(b, consoleSep...)
	status := r.Status
	b = appendPadding(b, colStatus-utf8.RuneCountInString(status))
	b, painted = s.paint(b, statusColor(status))
	b = append(b, status...)
	b = s.unpaint(b, painted)

	// 耗时列（右对齐，带单位）。先渲染进栈上小缓冲，再按列宽补前导空格。
	// 16 字节按 `appendDuration` 最长一档定：秒档 `9223372036.85s` = 14 字节。
	b = append(b, consoleSep...)
	var scratch [16]byte
	dur := appendDuration(scratch[:0], r.Duration)
	b = appendPadding(b, colDuration-utf8.RuneCount(dur))
	b = append(b, dur...)

	// 客户端列（左对齐）
	b = append(b, consoleSep...)
	if client == "" {
		client = "-"
	}
	b = append(b, client...)
	b = appendPadding(b, colClient-utf8.RuneCountInString(client))

	// 方法 + 路径列
	b = append(b, consoleSep...)
	if method == "" {
		method = "-"
	}
	b = append(b, method...)
	b = appendPadding(b, colMethod-utf8.RuneCountInString(method))
	b = append(b, ' ')
	shown := path
	if shown == "" {
		shown = route // 没有具体路径（未匹配路由）时退回模板
	}
	if shown == "" {
		shown = "-"
	}
	b = append(b, shown...)

	// 尾段：模板 / 响应体大小 / host / 错误 / trace
	//
	// 只在**路径与模板都非空且不同**时才补 `route=`：路径为空时列里已经展示模板
	// （见上），再补一次就是同一信息打两遍。
	if route != "" && path != "" && route != path {
		b = append(b, consoleSep...)
		b = append(b, "route="...)
		b = append(b, route...)
	}
	if okSize {
		b = append(b, consoleSep...)
		b = append(b, "size="...)
		b = strconv.AppendInt(b, size, 10)
	}
	if r.HostID != "" {
		b = append(b, consoleSep...)
		b = append(b, "host="...)
		b = append(b, r.HostID...)
	}
	if r.Err != nil {
		b = append(b, consoleSep...)
		b, painted = s.paint(b, ansiRed)
		if errType != "" {
			b = append(b, errType...)
			b = append(b, ' ')
		}
		b = appendQuoted(b, r.Err.Error())
		b = s.unpaint(b, painted)
	} else if errType != "" {
		// 有分类没有错误对象：仍然写出来，不因为「没 err 可打」就丢字段。
		b = append(b, consoleSep...)
		b = append(b, errType...)
	}
	if r.TraceID != "" {
		b = append(b, consoleSep...)
		b, painted = s.paint(b, ansiDim)
		b = append(b, "trace="...)
		b = append(b, r.TraceID...)
		b = s.unpaint(b, painted)
	}

	// 固定列盖不住的属性**不丢**：未知键按插入序附成一组 ` | k=v k=v`
	// （中间件 / 业务往访问记录里加的字段走这里）。
	//
	// 传给兜底路径的是「**真的渲染成了列**的键」（consumed），不是按名字猜：
	// 列键类型不符时列并没有渲染它，它就必须落进这一组——否则固定列与兜底组
	// 都跳过它，整条属性静默消失。
	//
	// 先比条数再进闭包是有意的：条数相等即无未知键，常见路径完全不进闭包
	// （闭包捕获缓冲会把缓冲顶到堆上，实测每写一次多 4 次分配）。
	consumed := [numColumns]bool{}
	consumed[colIdxMethod] = okMethod
	consumed[colIdxRoute] = okRoute
	consumed[colIdxPath] = okPath
	consumed[colIdxClient] = okClient
	consumed[colIdxErrType] = okErrType
	consumed[colIdxSize] = okSize
	known := 0
	for _, ok := range consumed {
		if ok {
			known++
		}
	}
	if r.Attrs.Len() > known {
		b = appendExtraAttrs(b, r.Attrs, consumed)
	}
	return append(b, '\n')
}

// 固定列的下标。用命名下标而不是裸序号，是为了让「哪一列取到了值」与
// httpColumnKeys 的对应关系写死在编译期看得见的地方。
const (
	colIdxMethod = iota
	colIdxRoute
	colIdxPath
	colIdxClient
	colIdxErrType
	colIdxSize
	numColumns
)

// httpColumnKeys 是列式版式的固定列键，下标见 colIdx* 常量。
//
// 这是「固定列有哪些」的**唯一**一份表：写入侧用它判类型并取下标，兜底侧用它
// 判「这个键是否已经由某一列渲染掉」。同一集合在一个函数里用两种方式表达
// （布尔列表计数 vs 键名 switch）会让判定不等价的那一格两头都不落。
var httpColumnKeys = [numColumns]string{
	colIdxMethod:  attrHTTPMethod,
	colIdxRoute:   attrHTTPRoute,
	colIdxPath:    attrURLPath,
	colIdxClient:  attrClientAddr,
	colIdxErrType: attrErrorType,
	colIdxSize:    attrHTTPBodySize,
}

// httpColumnIndex 返回键对应的固定列下标。
func httpColumnIndex(key string) (int, bool) {
	for i, k := range httpColumnKeys {
		if k == key {
			return i, true
		}
	}
	return 0, false
}

// appendExtraAttrs 渲染固定列没吃掉的属性：一组 ` | k=v k=v`。
//
// consumed 是「该列真的渲染了值」的标记；`httpColumnIndex` 给的是「这个键属于
// 某一列」。两者都有才跳过——只看后者会让类型不符的列键在固定列与兜底组里
// 都被跳过（属性消失）。
//
// 单独一个函数（而不是写进调用方的闭包）是有意的：闭包捕获调用方的缓冲会让那个
// 缓冲逃逸到堆上，把「0 分配」的常见路径变成每写 4 次分配。
func appendExtraAttrs(b []byte, attrs observability.Attrs, consumed [numColumns]bool) []byte {
	first := true
	attrs.Range(func(k string, v any) {
		if i, ok := httpColumnIndex(k); ok && consumed[i] {
			return
		}
		if first {
			b = append(b, consoleSep...)
			b = appendKeyValue(b, k, v)
			first = false
			return
		}
		b = appendField(b, k, v)
	})
	return b
}

// appendEventLine 渲染非 http 记录：`时间 | event | k=v …`。
//
// 装配期记录与业务打点没有 status / route，固定列会全是空位——所以这一路按
// 「先说什么事，再列字段」的顺序写，字段名保留上游出口的命名（fiber / state /
// loader / entry / plugin），便于与结构化出口对照 grep。
func (s *ConsoleSink) appendEventLine(b []byte, r observability.Record) []byte {
	b, painted := s.paint(b, ansiDim)
	b = r.Time.AppendFormat(b, consoleTimeLayout)
	b = s.unpaint(b, painted)
	b = append(b, consoleSep...)

	event := r.Event
	if event == "" {
		event = string(r.Source)
	}
	if event == "" {
		event = "-"
	}
	b = append(b, event...)

	if r.HostID != "" {
		b = appendStringField(b, "host", r.HostID)
	}
	if r.FiberName != "" {
		b = appendStringField(b, "fiber", r.FiberName)
	}
	if r.From != "" || r.To != "" {
		b = append(b, ' ')
		b = append(b, "state="...)
		b = append(b, r.From...)
		b = append(b, "→"...)
		b = append(b, r.To...)
	}
	if r.LoaderKind != "" {
		b = appendStringField(b, "loader", r.LoaderKind)
	}
	if r.EntryID != "" {
		b = appendStringField(b, "entry", r.EntryID)
	}
	if r.PluginName != "" {
		b = appendStringField(b, "plugin", r.PluginName)
	}
	if r.Status != "" {
		b = appendStringField(b, "status", r.Status)
	}
	if r.Duration != 0 {
		b = append(b, ' ')
		b = append(b, "dur="...)
		b = appendDuration(b, r.Duration)
	}
	if r.Err != nil {
		b = append(b, ' ')
		b, painted = s.paint(b, ansiRed)
		b = append(b, "err="...)
		b = appendQuoted(b, r.Err.Error())
		b = s.unpaint(b, painted)
	}

	// Attrs 是开放段：全部按插入序写出（这一路没有「固定列」可替代）。
	if r.Attrs.Len() > 0 {
		r.Attrs.Range(func(k string, v any) {
			b = appendField(b, k, v)
		})
	}

	if r.TraceID != "" {
		b = append(b, ' ')
		b, painted = s.paint(b, ansiDim)
		b = append(b, "trace="...)
		b = append(b, r.TraceID...)
		b = s.unpaint(b, painted)
	}
	return append(b, '\n')
}

// paint 前置颜色码；返回是否真的上了色（没上色时不用补 reset）。
func (s *ConsoleSink) paint(b []byte, code string) ([]byte, bool) {
	if !s.color || code == "" {
		return b, false
	}
	return append(b, code...), true
}

func (s *ConsoleSink) unpaint(b []byte, painted bool) []byte {
	if !painted {
		return b
	}
	return append(b, ansiReset...)
}

// statusColor 按状态码区间取色：2xx 绿 / 3xx 青 / 4xx 黄 / 5xx 红 / 其余不着色。
func statusColor(status string) string {
	if status == "" {
		return ""
	}
	switch status[0] {
	case '2':
		return ansiGreen
	case '3':
		return ansiCyan
	case '4':
		return ansiYellow
	case '5':
		return ansiRed
	}
	return ""
}

// appendDuration 追加带单位的耗时。
//
// 刻意**不取整到毫秒**：旧默认出口把亚毫秒请求写成 `duration_ms=0`，快慢全看不出来
// （gin 那边是 `585.1µs`）。单位选择按量级，保留一位（µs）或两位（ms / s）小数。
func appendDuration(dst []byte, d time.Duration) []byte {
	switch {
	case d >= time.Second:
		dst = strconv.AppendFloat(dst, d.Seconds(), 'f', 2, 64)
		return append(dst, 's')
	case d >= time.Millisecond:
		dst = strconv.AppendFloat(dst, float64(d)/float64(time.Millisecond), 'f', 2, 64)
		return append(dst, "ms"...)
	case d >= time.Microsecond:
		dst = strconv.AppendFloat(dst, float64(d)/float64(time.Microsecond), 'f', 1, 64)
		return append(dst, "µs"...)
	default:
		dst = strconv.AppendInt(dst, d.Nanoseconds(), 10)
		return append(dst, "ns"...)
	}
}

// appendField 追加 ` key=value`（前导空格；行内并列字段用它）。
func appendField(b []byte, key string, val any) []byte {
	return appendKeyValue(append(b, ' '), key, val)
}

// appendStringField 是字符串专用的 ` key=value`：走 appendField 会把 string
// 装箱成 any，每写一次多一次分配（实测装配期记录那条路径正是这么来的）。
func appendStringField(b []byte, key, val string) []byte {
	b = append(b, ' ')
	b = append(b, key...)
	b = append(b, '=')
	return appendQuoted(b, val)
}

// appendKeyValue 追加 `key=value`（不含前导空格）。值域是 Attrs 的标量集
// （string / int64 / float64 / bool，见 observability.AttrValue）；
// 兜底交给 fmt.Append——同样不额外分配。
func appendKeyValue(b []byte, key string, val any) []byte {
	b = append(b, key...)
	b = append(b, '=')
	switch v := val.(type) {
	case string:
		return appendQuoted(b, v)
	case int64:
		return strconv.AppendInt(b, v, 10)
	case int:
		return strconv.AppendInt(b, int64(v), 10)
	case float64:
		return strconv.AppendFloat(b, v, 'g', -1, 64)
	case bool:
		return strconv.AppendBool(b, v)
	default:
		return fmt.Append(b, v)
	}
}

// appendQuoted 按需加引号：空串、含空格 / 等号 / 引号 / 控制字符才加——
// 与上游出口（`observability` 的 appendTextValue）同一规则，避免同一条记录在
// 两个出口下引号口径不一致。上游那个助手没有导出，这里只能重写一份，
// 要共享得先把编码助手推到上游（见 Issue #20 后续）。
func appendQuoted(b []byte, s string) []byte {
	if !needsQuoting(s) {
		return append(b, s...)
	}
	return strconv.AppendQuote(b, s)
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c <= ' ', c == '"', c == '=', c == 0x7f:
			return true
		}
	}
	return false
}

// appendPadding 追加 n 个空格（n <= 0 时什么都不做）。
func appendPadding(b []byte, n int) []byte {
	for ; n > 0; n-- {
		b = append(b, ' ')
	}
	return b
}

// isTerminal 判断目的地是不是终端：只有 `*os.File` 且为字符设备才算。
// 重定向到文件 / 管道时为 false（不上色）；`/dev/null` 也是字符设备，
// 那时多几个转义序列没人看得见，无害。
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
