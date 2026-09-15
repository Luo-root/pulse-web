package web

import (
	"io"
	"strconv"
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

// 列宽与版式常量。列宽按**显示宽度**算，不是字节数、也不是 rune 数：`µ` 是
// 2 字节 1 列（按 len() 补齐会错位），全角字符是 1 rune **2 列**（按 rune 补齐会
// 多补、把后续列整体推右）。宽度口径走上游 `observability.DisplayWidth`。
//
// 迁移到 LineSink 之前这里用的是 `utf8.RuneCount`——ASCII 下等价，全角下不等价，
// 属于本次迁移的第二处行为变化（见 `TestConsoleSinkPaddingUsesDisplayWidth`）。
const (
	consoleTimeLayout = "2006/01/02 - 15:04:05" // 21 列
	consoleSep        = " | "
	colStatus         = 3
	colDuration       = 9
	colClient         = 15
	colMethod         = 7 // 方法列宽；再追加一个空格 ⇒ 含分隔共 8 宽，7 字符方法（OPTIONS）也对齐
)

// ANSI 颜色码。只在目的地是终端时使用——**是否上色由 LineSink 判定**（TTY +
// WithColor），渲染器拿到的是结论，所以重定向到文件时不会混进转义序列。
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
// 上游还有一个默认出口 `observability.SlogSink`，但它是**给机器读**的：接宿主
// logger、要 JSON、喂采集器时用它。两条线叠起来它就不适合当开箱默认——渲染一条
// ~1310 ns / 18 allocs（每条都要 `make([]any, 0, 18)` 再交给 slog），而毫秒数值
// `0.585` 也不如带单位的 `585.1µs` 一眼看得懂。**不是它有缺陷，是默认位置该给人读**：
// `Record` 里的字段一个不少，缺的只是一个渲染层（实测与选型见仓库 Issue #20）。
//
// # 它与上游 LineSink 的分工（pulse v0.2.4 起）
//
// 上游 v0.2.4 把出口的公共面开出来了：六条编码原语 + 行体渲染器接缝。于是本出口
// 只留**版式知识**，其余全部交给 `observability.LineSink`：
//
//	| 归 LineSink（上游） | 归本出口（pulse-web） |
//	| --- | --- |
//	| 行首标识 / 结尾换行 / 缓冲 / 即时写出 | 列序与列宽 |
//	| 写错误收集（Err）与 Flush | 状态配色（2xx 绿 / 3xx 青 / 4xx 黄 / 5xx 红） |
//	| 颜色判定（TTY + WithColor） | http / 非 http 两条分支 |
//	| 耗时 / 引号 / 列补齐 / attrs 子集的口径 | 尾段顺序 |
//
// 分工线画在「谁认识业务语义」上：HTTP 的列序与状态配色是 pulse-web 的知识，
// 上游明确不碰（「出口不按 Status 猜语义」）；而「耗时怎么格式化」「含空格的值
// 要不要加引号」是各出口共用的一致性资产，由上游提供，本地不再重写一份。
//
// # 版式
//
// http 请求（一行，列宽固定；下面是 `TestConsoleSinkHTTPLine` 钉住的那一行）：
//
//	PULSE | 2026/09/14 - 08:30:00 | 200 |   585.1µs | 192.0.2.1:1234  | GET     /users/42 | route=/users/{id} | size=29 | host=pulse-web | trace=8f2e1a3b4c5d6e7f8a9b0c1d2e3f4a5b
//
//	  - 行首标识 `PULSE` 走上游缺省值（`observability.DefaultLinePrefix`），
//	    暗淡上色：pulse 与 pulse-web 同根同源，同一进程树里的行首一致，
//	    `grep PULSE` 一把捞出全部行；
//	  - 时间列是**完成时刻**（与 gin 的 Logger 一致：请求结束后才写这一行）；
//	  - 状态列按区间上色（2xx 绿 / 3xx 青 / 4xx 黄 / 5xx 红），仅在终端生效；
//	  - 耗时列右对齐、带单位、**不取整**（`820ns` / `585.1µs` / `7.62ms` / `1.23s`），
//	    口径与上游 `AppendDuration` 同一条（整数运算、不进位）；
//	  - 路径列给**具体路径**（`/users/42`，与 gin/chi 的直觉一致），路由模板在
//	    两者不同时另起 `route=` 附在后面（模板是聚合维度，路径是复现维度）；
//	  - 尾段的 `route=` / `size=` / `host=` / 错误 / `trace=` 是**各自独立的
//	    ` | ` 字段**：有才出现、缺就少一段（`host=` 只在配置了 `WithHostID` 时出现）；
//	  - 非 http 记录（装配期 `observability.host_ready` / `fiber_state`、业务
//	    `Ctx.Observe`）退回 `时间 | event | k=v …`：它们没有 status / route，
//	    硬套列式只会渲染出一堆空列；
//	  - 固定列盖不住的属性**不丢**：未知键按插入序附在 `|` 之后（如 `llm.model=…`）。
//
// # 退出方式
//
// 它**不缓冲**（`WithImmediate`）：写完即落 `io.Writer`——终端要即时，缓冲会把
// 安静应用的日志扣在内存里等下一次触发。要吞吐或异步就 `WithSink(observability.NewAsyncSink(…))`；
// 要机器可读就 `WithSink(observability.SlogSink{…})` / `NewLineSink`。
// `WithSink` 只换出口，不改变装配。
//
// 写错误**不抛**（`Sink` 接口没有错误通道，与 gin / chi 的控制台输出一致），但
// 也不会被吞掉：内嵌的 `LineSink` 把**首次**写失败记在 `Err()` 上——「日志早就不
// 写了却没人知道」是落地场景里最痛的失败形态（stdout 管道被关掉、journald socket
// 满、磁盘满都是这一类）。失败之后仍然继续尝试写，不静默退出、不改缓冲策略。
//
// **框架已经替你读了一次**：`Run()` 的关闭流程第 ⑤ 步会调 `Flush()`，首错会被
// 记成 `slog.Warn("pulse.web: sink flush", …)`。所以只有「进程不退出也想告警」的
// 场合才需要自己接——在健康检查里定期读一次即可：
//
//	if err := sink.Err(); err != nil { /* 告警：访问日志已经写不进去了 */ }
//
// 这个通道现在由内嵌出口白送，不必为了 `Err()` 换成裸 `LineSink`。`Flush()` 在本
// 出口下**不等于**「把缓冲落盘」：`WithImmediate` 让每条写完即出、缓冲区通常是空
// 的，它的实际作用是**把首错交出来**（顺带兜住最后一条未写的记录）。
//
// # 并发
//
// 渲染发生在 `LineSink` 的临界区内（单缓冲换零分配）。实测并行写比单线程慢
// ~75 ns/条，而裸 `LineSink` 并行档的增量与之**完全相同**——这个代价是内嵌上游
// 出口继承来的，不是本渲染器引入的。
type ConsoleSink struct {
	*observability.LineSink
}

// ConsoleOption 配置 ConsoleSink。
type ConsoleOption func(*consoleConfig)

type consoleConfig struct {
	color    bool
	colorSet bool
}

// WithColor 强制开 / 关 ANSI 颜色。缺省由 LineSink 按目的地自动判断：`w` 是
// `*os.File` 且为字符设备（终端）才上色。
func WithColor(on bool) ConsoleOption {
	return func(c *consoleConfig) { c.color, c.colorSet = on, true }
}

// NewConsoleSink 构造控制台出口。w 为 nil 视为编程错误（构造期 panic）；
// w 只需被本出口串行调用（内部已加锁），不需要自身并发安全。
func NewConsoleSink(w io.Writer, opts ...ConsoleOption) *ConsoleSink {
	if w == nil {
		panic("web: ConsoleSink requires a non-nil io.Writer")
	}
	cfg := consoleConfig{}
	for _, opt := range opts {
		opt(&cfg)
	}

	lineOpts := []observability.LineOption{
		// 行首标识走上游缺省值（`PULSE`）：pulse 与 pulse-web 同根同源，同一进程树里
		// 两个出口的行首一致——grep 一个词就能把全部行捞出来，也便于与结构化出口对账。
		observability.WithPrefix(observability.DefaultLinePrefix),
		observability.WithImmediate(), // 终端要即时，见上面「退出方式」
		observability.WithRenderer(consoleRenderer),
	}
	if cfg.colorSet {
		lineOpts = append(lineOpts, observability.WithColor(cfg.color))
	}
	return &ConsoleSink{observability.NewLineSink(w, lineOpts...)}
}

// consoleRenderer 是 ConsoleSink 的行体渲染器，按 event 分两条版式。
//
// 它写成**包级函数**而不是闭包，是有实测依据的：闭包嵌在另一个函数值里时，
// `Attrs.Range` 的逐值装箱消不掉（6 属性记录实测每条 5 次分配）；包级函数 +
// `Get[T]` 的写法实测 0 分配。行体之外的接管见 ConsoleSink 的注释。
func consoleRenderer(dst []byte, r observability.Record, color bool) []byte {
	if r.Event == eventHTTPReq && r.Status != "" {
		return appendHTTPLine(dst, r, color)
	}
	return appendEventLine(dst, r, color)
}

// appendHTTPLine 渲染 http 请求记录（固定列 + 尾段）。
//
// 时间 / 标识 / 结尾换行不归它：LineSink 负责行首标识与换行，本函数只产行体。
func appendHTTPLine(dst []byte, r observability.Record, color bool) []byte {
	method, okMethod := observability.Get[string](r.Attrs, attrHTTPMethod)
	route, okRoute := observability.Get[string](r.Attrs, attrHTTPRoute)
	path, okPath := observability.Get[string](r.Attrs, attrURLPath)
	client, okClient := observability.Get[string](r.Attrs, attrClientAddr)
	errType, okErrType := observability.Get[string](r.Attrs, attrErrorType)
	size, okSize := observability.Get[int64](r.Attrs, attrHTTPBodySize)

	// 时间列（暗淡，扫读时不该跟正文抢注意力）
	painted := false
	dst, painted = paint(dst, color, ansiDim)
	dst = r.Time.AppendFormat(dst, consoleTimeLayout)
	dst = unpaint(dst, painted)

	// 状态列（右对齐；按区间上色）
	dst = append(dst, consoleSep...)
	painted = false
	dst, painted = paint(dst, color, statusColor(r.Status))
	dst = observability.AppendPadding(dst, colStatus-observability.DisplayWidth(r.Status))
	dst = append(dst, r.Status...)
	dst = unpaint(dst, painted)

	// 耗时列（右对齐，带单位）。先渲染进栈上小缓冲，再按列宽补前导空格。
	// 16 字节按最长一档定：秒档 `9223372036.85s` = 14 字节。
	//
	// 这一列是全文唯一按 **rune 数**而不是显示宽度补齐的地方（其余三列走
	// `DisplayWidth`），与上游默认渲染器同一个位置同一种写法：耗时文本只有 ASCII
	// 数字与 `µ`（两者都是 1 列宽），rune 数 == 显示列数，两条口径在这条路径上恒等。
	dst = append(dst, consoleSep...)
	var scratch [16]byte
	dur := observability.AppendDuration(scratch[:0], r.Duration)
	dst = observability.AppendPadding(dst, colDuration-utf8.RuneCount(dur))
	dst = append(dst, dur...)

	// 客户端列（左对齐）
	dst = append(dst, consoleSep...)
	if client == "" {
		client = "-"
	}
	dst = append(dst, client...)
	dst = observability.AppendPadding(dst, colClient-observability.DisplayWidth(client))

	// 方法 + 路径列
	dst = append(dst, consoleSep...)
	if method == "" {
		method = "-"
	}
	dst = append(dst, method...)
	dst = observability.AppendPadding(dst, colMethod-observability.DisplayWidth(method))
	dst = append(dst, ' ')
	shown := path
	if shown == "" {
		shown = route // 没有具体路径（未匹配路由）时退回模板
	}
	if shown == "" {
		shown = "-"
	}
	dst = append(dst, shown...)

	// 尾段：模板 / 响应体大小 / host / 错误 / trace
	//
	// 只在**路径与模板都非空且不同**时才补 `route=`：路径为空时列里已经展示模板
	// （见上），再补一次就是同一信息打两遍。
	if route != "" && path != "" && route != path {
		dst = append(dst, consoleSep...)
		dst = append(dst, "route="...)
		dst = append(dst, route...)
	}
	if okSize {
		dst = append(dst, consoleSep...)
		dst = append(dst, "size="...)
		dst = strconv.AppendInt(dst, size, 10)
	}
	if r.HostID != "" {
		dst = append(dst, consoleSep...)
		dst = append(dst, "host="...)
		dst = append(dst, r.HostID...)
	}
	if r.Err != nil {
		dst = append(dst, consoleSep...)
		painted = false
		dst, painted = paint(dst, color, ansiRed)
		if errType != "" {
			dst = append(dst, errType...)
			dst = append(dst, ' ')
		}
		dst = observability.AppendTextValue(dst, r.Err.Error())
		dst = unpaint(dst, painted)
	} else if errType != "" {
		// 有分类没有错误对象：仍然写出来，不因为「没 err 可打」就丢字段。
		dst = append(dst, consoleSep...)
		dst = append(dst, errType...)
	}
	if r.TraceID != "" {
		dst = append(dst, consoleSep...)
		painted = false
		dst, painted = paint(dst, color, ansiDim)
		dst = append(dst, "trace="...)
		dst = append(dst, r.TraceID...)
		dst = unpaint(dst, painted)
	}

	// 固定列盖不住的属性**不丢**：未知键按插入序附成一组 ` | k=v k=v`
	// （中间件 / 业务往访问记录里加的字段走这里）。
	//
	// 传进 `AppendAttrsExcept` 的必须只是「**真的渲染成了列**」的键，不是
	// 「这个名字属于某一列」：列键类型不符时固定列并没有渲染它，它就必须落进
	// 兜底组——只按名字跳过会让它在固定列与兜底组之间两头落空（属性静默消失）。
	// 所以 `skip` 是按 `okX` 逐个攒的，而不是拿一张列键表整表传进去。
	//
	// 分隔符**先写、再按产出长度决定撤回**，不能拿 `Len()` 判空：`Len()` 问的是
	// 「组非空」，不是「有可渲染项」——列键全被吃掉时 `AppendAttrsExcept` 产出
	// 0 字节（与空组同形），先写的分隔符就成了悬空的 ` | `。上游 godoc 点名了这两条
	// （见 `observability.AppendAttrsExcept`），本仓不另立一套判空口径。
	// 数组长度 = 固定列个数，容量恰好用满：`skip` 只是它的前缀视图，
	// `append` 不触发扩容，因此整条路径仍然 0 分配。
	var skipArr [6]string
	skip := skipArr[:0]
	if okMethod {
		skip = append(skip, attrHTTPMethod)
	}
	if okRoute {
		skip = append(skip, attrHTTPRoute)
	}
	if okPath {
		skip = append(skip, attrURLPath)
	}
	if okClient {
		skip = append(skip, attrClientAddr)
	}
	if okErrType {
		skip = append(skip, attrErrorType)
	}
	if okSize {
		skip = append(skip, attrHTTPBodySize)
	}
	mark := len(dst)
	dst = append(dst, consoleSep...)
	dst = observability.AppendAttrsExcept(dst, r.Attrs, skip...)
	if len(dst) == mark+len(consoleSep) {
		dst = dst[:mark] // 固定列全吃掉了，这一组不写
	}
	return dst
}

// appendEventLine 渲染非 http 记录：`时间 | event | k=v …`。
//
// 装配期记录与业务打点没有 status / route，固定列会全是空位——所以这一路按
// 「先说什么事，再列字段」的顺序写，字段名保留上游出口的命名（fiber / state /
// loader / entry / plugin），便于与结构化出口对照 grep。
func appendEventLine(dst []byte, r observability.Record, color bool) []byte {
	painted := false
	dst, painted = paint(dst, color, ansiDim)
	dst = r.Time.AppendFormat(dst, consoleTimeLayout)
	dst = unpaint(dst, painted)
	dst = append(dst, consoleSep...)

	event := r.Event
	if event == "" {
		event = string(r.Source)
	}
	if event == "" {
		event = "-"
	}
	dst = append(dst, event...)

	if r.HostID != "" {
		dst = consoleField(dst, "host", r.HostID)
	}
	if r.FiberName != "" {
		dst = consoleField(dst, "fiber", r.FiberName)
	}
	if r.From != "" || r.To != "" {
		dst = append(dst, ' ')
		dst = append(dst, "state="...)
		dst = append(dst, r.From...)
		dst = append(dst, "→"...)
		dst = append(dst, r.To...)
	}
	if r.LoaderKind != "" {
		dst = consoleField(dst, "loader", r.LoaderKind)
	}
	if r.EntryID != "" {
		dst = consoleField(dst, "entry", r.EntryID)
	}
	if r.PluginName != "" {
		dst = consoleField(dst, "plugin", r.PluginName)
	}
	if r.Status != "" {
		dst = consoleField(dst, "status", r.Status)
	}
	if r.Duration != 0 {
		dst = append(dst, ' ')
		dst = append(dst, "dur="...)
		dst = observability.AppendDuration(dst, r.Duration)
	}
	if r.Err != nil {
		dst = append(dst, ' ')
		painted = false
		dst, painted = paint(dst, color, ansiRed)
		dst = append(dst, "err="...)
		dst = observability.AppendTextValue(dst, r.Err.Error())
		dst = unpaint(dst, painted)
	}

	// Attrs 是开放段：全部按插入序写出（这一路没有「固定列」可替代）。
	if r.Attrs.Len() > 0 {
		dst = append(dst, ' ')
		dst = observability.AppendAttrs(dst, r.Attrs)
	}

	if r.TraceID != "" {
		dst = append(dst, ' ')
		painted = false
		dst, painted = paint(dst, color, ansiDim)
		dst = append(dst, "trace="...)
		dst = append(dst, r.TraceID...)
		dst = unpaint(dst, painted)
	}
	return dst
}

// consoleField 追加 ` key=value`（前导空格；行内并列字段用它）。
//
// 引号口径走上游 `AppendTextValue`：同一条记录在默认出口与宿主自带出口下必须同形。
func consoleField(dst []byte, key, val string) []byte {
	dst = append(dst, ' ')
	dst = append(dst, key...)
	dst = append(dst, '=')
	return observability.AppendTextValue(dst, val)
}

// paint 前置颜色码；返回是否真的上了色（没上色时不用补 reset）。
func paint(dst []byte, color bool, code string) ([]byte, bool) {
	if !color || code == "" {
		return dst, false
	}
	return append(dst, code...), true
}

func unpaint(dst []byte, painted bool) []byte {
	if !painted {
		return dst
	}
	return append(dst, ansiReset...)
}

// statusColor 按状态码区间取色：2xx 绿 / 3xx 青 / 4xx 黄 / 5xx 红 / 其余不着色。
//
// 这是**本出口自己的域知识**（上游明确不按 Status 猜语义）：换成别的词表就把
// 这段换掉，出口的其余部分不受影响。
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
