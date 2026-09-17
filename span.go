package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Luo-root/pulse/observability"
)

// 本文件是请求级 span 的**数据面**：框架按 W3C Trace Context 与 OpenTelemetry
// HTTP 语义约定产出结构化数据，宿主用 WithSpanHook 把它接进自己的追踪体系。
//
// 主模块不引任何追踪 SDK（零第三方依赖是红线）：官方 otel-go 的适配在独立
// nested module `github.com/Luo-root/pulse-web/otel`。
//
// 一个关键分工：**span 的 id 由追踪体系分配，不由框架编造**。SDK 不提供
// 「指定 span-id」的入口（id 来自 provider 的 IDGenerator），而 Server-Timing、
// 记录里的 span.id、下游 client 注入的 traceparent 三处用的都必须是同一个 id，
// 所以框架在 Begin 里向 hook **索要**身份（SpanRef），而不是自己造一个。
type SpanInfo struct {
	// TraceID 是 32 位小写 hex：采纳入站 traceparent 时沿用，否则新起一条。
	TraceID string

	// ParentID 是入站 traceparent 的 span-id（16 位小写 hex）：本请求 span 的父。
	// 空 = root span（没有入站 traceparent，或入站只有 B3 trace-id）。
	ParentID string

	// TraceState 是入站 `tracestate` 头的原样透传（框架不解析）；
	// traceparent 非法时按 W3C 要求不解析 tracestate，这里就是空串。
	TraceState string

	// Sampled / Random 是 trace-flags 的第 0 / 第 1 位。它们必须按位读，
	// 不能拿整个 flags 做等值比较。
	Sampled bool
	Random  bool

	// Method 是请求方法：span 名在路由已知前只能退化为 `{method}`，而这个值在
	// 请求开始时就可用（semconv：没有低基数目标时，名称就用 `{method}`）。
	//
	// 这里**不带路径**：semconv 禁止拿 URI 路径当 span 名，而 Begin 阶段除了
	// 名称之外没有别的用途——请求事实（含 `url.path`）都在 End 的 Span 里给。
	Method string
}

// SpanRef 是 hook 在 Begin 里给出的本请求 span 身份。
//
// 适配层从自己创建的 span 里取这三样（`trace.SpanContext` + flags）——框架
// 拿它写 Server-Timing 与记录里的 span.id，因此三方共用一个真实存在的 id。
//
// SpanID 为空表示「本次请求没有 span」：框架不写 Server-Timing、不记 span.id。
type SpanRef struct {
	TraceID string
	SpanID  string
	Sampled bool
	Random  bool
}

// Span 是一次请求的完整数据（End 的入参）：请求/响应事实 + 观测属性。
//
// 语义对齐 OpenTelemetry HTTP semantic conventions 的 server span：名称用
// `{method} {http.route}`、属性用 semconv 键名（`http.request.method` /
// `http.route` / `url.path` / `http.response.status_code` / `client.address` /
// `error.type` …）。
type Span struct {
	// TraceID / SpanID 是本次请求在追踪体系里的身份（Begin 时由 hook 给出）；
	// hook 自己创建的 span 里也有同一份，这里再带一遍是为了让自定义 hook
	// 不必自己存。
	TraceID string
	SpanID  string

	// Method 是请求方法（`http.request.method`）。
	Method string

	// Route 是路由模板（`http.route`），如 `/users/{id}`；未匹配到路由时为空。
	// span 名应按 semconv 用 `{method} {route}`，**不得**退回 URI 路径
	// （semconv 明文：instrumentation MUST NOT default to using URI path）。
	Route string

	// Path 是实际请求路径（`url.path`），含路径参数的实际值。
	//
	// 官方 otel/ 适配件**不直接读**它——`url.path` 已经随 Attrs 过去，再读一遍
	// 只会多一个取值口径。它服务于自定义 hook 的便利取值（比如要建一条带
	// 路径的 link 事件，不想从 Attrs 里翻）。
	Path string

	// Status 是**映射后**的 HTTP 状态码（与访问日志同源）：handler 返回的错误
	// 经错误映射器决定，panic 记 500。
	Status int

	// Start / End 是请求的开始与结束时刻（End 在响应写完之后）。真实 span 的
	// 起点在 Begin（更早），这里给的是框架能给出的最完整区间。
	Start time.Time
	End   time.Time

	// Err 是 handler 返回的原始错误（已映射为 Status 的那个）。它只在进程内
	// 传递——适配层默认不把它写进 span（异常事件会把错误原文带出进程）。
	Err error

	// Attrs 与访问日志**同一份字段**（`http.request.method` / `http.route` /
	// `url.path` / `http.response.body.size` / `client.address` / 可选
	// `error.type`），由同一个 fillRequestAttrs 填出，所以 span 与日志不可能
	// 各说一套。
	//
	// 它**不含** `span.id`：那是观测记录侧才需要的关联键（见 attrSpanID），所以
	// 引擎在收尾时填**两遍**——span 这一份干净、访问记录那一份多一个关联键。
	// 宁可多填一遍也不共用同一块缓冲：共用会让「记录里追加 span.id」变成对
	// span 数据的原地改写，而两者谁先读谁后读是可变的（代价见 emitSpan）。
	//
	// 指针指向引擎的**栈上局部**，只在 End 调用期间有效——hook 不得把它存到
	// End 返回之后再读（要留就自己拷一份）。适配层只读。
	Attrs *observability.Attrs
}

// SpanHook 让宿主把请求级 span 接进自己的追踪体系。
//
// 两个方法对应请求的头尾，中间隔着路由与 handler：
//
//	Begin(ctx, in) → 中间件 → handler → 响应写出 → End(ctx, sp)
//
// Begin 里做两件事，缺一不可：
//
//  1. 建 span 并把它的 context 注入 ctx（返回新的 ctx）——下游支持 OTel 的库
//     （otelgrpc / otelsql / 出站 http 客户端）因此自动接上这条链路；
//  2. 把 span 身份回给框架——Server-Timing、访问记录里的 span.id 都用它。
//
// End 里用 `trace.SpanFromContext(ctx)` 取回同一个 span，补上名称（路由到这时
// 才知道）、属性与状态，然后结束它。ctx 是**请求 context**（自带 Begin 注入的
// span），所以 hook 不必自己维护「请求 ↔ span」的映射。
//
// 两条对**实现者**的约定（写在接口上，免得只看 Span 的字段注释才看得到）：
//
//   - `sp.Attrs` 是引擎的**栈上局部**的指针，只在本次 End 调用期间有效。要留就
//     当场拷一份（`sp.Attrs.Range` 或者自己 clone），**不得**存下来延后读。
//   - End 是**请求 goroutine 最后一次**看到这个请求的机会：此刻的 ctx 与 sp 都
//     随请求结束失效，别把它们捕获进后台 goroutine（后台任务用 `Ctx.Detach()`
//     拿 TraceID / SpanID，在追踪体系里建 link，不是拿 span 往下走）。
type SpanHook interface {
	Begin(ctx context.Context, in SpanInfo) (context.Context, SpanRef)
	End(ctx context.Context, sp Span)
}

// WithSpanHook 装上 span 出口（默认不装）。
//
// 装与不装的差别是**请求路径上是否多一份 span 数据**，而不是「有没有观测」：
//
//	不装   请求路径与本选项出现之前逐字节相同（分配预算门禁量的是这一档）
//	装     每请求多两次 hook 调用 + 一个 span-id 字符串字段（16 字节，见
//	       bench/budget_test.go 的基线说明）；记录与响应头不变
//
// 与 `WithCollector()` 的分工：那个是**记录**（Record）层面的出口，这个是
// **链路**（span）层面的出口；两者互不依赖，可以同时装。
//
// 传 nil 会在装配期 panic——装了却没有任何出口是配置错误，不是「静默关闭」。
func WithSpanHook(h SpanHook) Option {
	return func(cfg *config) {
		cfg.spanHook = h
		cfg.spanHookSet = true
	}
}

// attrSpanID 是 span 模式下写进**观测记录**的关联键。
//
// 它存在的理由只有一个：日志与 trace 对得上（一条记录能指回它的 span）。
// 它只出现在记录里，不进 span 属性——span 自己的 id 由 span 自身携带。
const attrSpanID = "span.id"

// serverTimingHeader 是 W3C Trace Context 定义的**响应侧**绑定。
//
// 注意它和 X-Trace-Id 是两个不同的东西，别混：
//
//	X-Trace-Id    本框架的既有契约：自定义头，只有 trace-id。任何模式都写，
//	              给的是「这条请求属于哪条 trace」，不含 span。
//	Server-Timing W3C（trace-context §Trace Context Server Timing）定义的标准
//	              形态：`trace;desc=00-<trace-id>-<本请求 span-id>-<flags>`。
//	              只有拿到 span 身份（SpanRef.SpanID 非空）才写——没有 span
//	              就没有 span-id 可写。
//
// 响应侧**不写 traceparent**：W3C 没有给响应定义这个头，客户端也不会读它。
//
// 被代理剥离时按这个顺序排查：访问日志的 `trace=` 与 `span.id` → `X-Trace-Id`
// → 最后才怀疑本头（WAF / CDN / 老网关对不认得的响应头并不都原样透传）。
const serverTimingHeader = "Server-Timing"

// serverTimingMetric 是这条 Server-Timing 指标的名字，W3C trace-context 定死为
// `trace`（客户端按这个名字找链路信息）。
const serverTimingMetric = "trace"

// traceparentVersion 是 W3C 当前唯一的版本号 `00`。
//
// Server-Timing 的 desc 里要把 traceparent 的四个字段按原序写一遍，版本位同样
// 是这个值——所以它既用于入站校验，也用于出站拼装，不各写一份。
const traceparentVersion = "00"

// serverTimingValue 产出 Server-Timing 的指标值（不含头名）。
//
// desc 里的 span-id 是**本服务这次操作**的 span（规范里叫 child-id），不是入站
// 的 parent-id——上游拿它关联自己那条请求、也能顺着 trace-id 找到完整链路。
func serverTimingValue(ref SpanRef) string {
	if ref.TraceID == "" || ref.SpanID == "" {
		return ""
	}
	return serverTimingMetric + ";desc=" + traceparentVersion + "-" +
		ref.TraceID + "-" + ref.SpanID + "-" + flagsHex(ref.Sampled, ref.Random)
}

// trace-flags 是 W3C 定义的两位（外加保留位）：
//
//	flagSampled    0x01  sampled——上游决定要采这条链路
//	flagRandom     0x02  random-trace-id——trace-id 来自随机源（不是历史 id）
//	knownFlagBits  两位都算「已知」，`00` 版本不允许出现其他位（规范：保留位必须
//	               为 0，官方 otel-go 同样按 >3 判非法）
const (
	flagSampled   = 0x01
	flagRandom    = 0x02
	knownFlagBits = flagSampled | flagRandom
)

// flagsHex 只写规范支持的两 bit：sampled 与 random-trace-id。
// W3C 要求出站时把不认识的位写 0，所以这里不做位透传。
func flagsHex(sampled, random bool) string {
	var v byte
	if sampled {
		v |= flagSampled
	}
	if random {
		v |= flagRandom
	}
	const hexDigits = "0123456789abcdef"
	return string([]byte{hexDigits[v>>4], hexDigits[v&0x0f]})
}

// traceparentMinLen 是 W3C 一个合法 traceparent 的最短长度：
// "00-" + 32 + "-" + 16 + "-" + 2 = 55。
const traceparentMinLen = 55

const (
	zeroTraceID = "00000000000000000000000000000000"
	zeroSpanID  = "0000000000000000"
)

// parseInboundTrace 解析入站链路头：W3C `traceparent` 优先，其次 B3 `X-B3-TraceId`。
//
// traceparent 的校验逐条对齐 W3C Trace Context 与官方 otel-go 实现
// （`propagation.TraceContext`）——两边必须接受同一批输入，否则同一个请求在
// 「框架的记录」与「宿主的 OTel 链路」里会拿到不同的 trace 身份：
//
//	version     2 位小写 hex；ff 非法；更高版本按 00 的格式解析（尾部忽略）
//	trace-id    32 位小写 hex，全零非法
//	parent-id   16 位小写 hex，全零非法
//	trace-flags 2 位小写 hex；00 版本不允许保留位（>3）
//
// 大写 hex 非法（W3C：HEXDIGLC）。任何一处不合法 → **整条忽略**、起新 trace，
// 不做部分采纳（只认 trace-id 而丢掉非法 parent-id 会让父子关系静默错位）。
//
// 解析失败时**不解析** tracestate（W3C 明文要求），所以 traceState 只在
// traceparent 通过校验时才回填。
func parseInboundTrace(traceparent, tracestate string) SpanInfo {
	tp := strings.TrimSpace(traceparent)
	if tp == "" {
		return SpanInfo{}
	}
	in, ok := parseTraceparent(tp)
	if !ok {
		return SpanInfo{}
	}
	in.TraceState = strings.TrimSpace(tracestate)
	return in
}

// parseTraceparent 是 traceparent 的字段级解析（规则见 parseInboundTrace）。
func parseTraceparent(tp string) (SpanInfo, bool) {
	if len(tp) < traceparentMinLen {
		return SpanInfo{}, false
	}
	// version 必须是 2 位小写 hex 后面跟 '-'。
	if !isHexLower(tp[0:2]) || tp[0] == 'f' && tp[1] == 'f' || tp[2] != '-' {
		return SpanInfo{}, false
	}
	version := (hexVal(tp[0]) << 4) | hexVal(tp[1])

	traceID := tp[3:35]
	if tp[35] != '-' || !isHexLower(traceID) || traceID == zeroTraceID {
		return SpanInfo{}, false
	}

	parentID := tp[36:52]
	if tp[52] != '-' || !isHexLower(parentID) || parentID == zeroSpanID {
		return SpanInfo{}, false
	}

	flags := tp[53:55]
	if !isHexLower(flags) {
		return SpanInfo{}, false
	}
	flagBits := (hexVal(flags[0]) << 4) | hexVal(flags[1])

	if version == 0 {
		// 00 版本是定长格式：不允许额外的尾部字段，也不允许保留 flag 位
		// （与官方 otel-go 实现同口径）。
		if len(tp) != traceparentMinLen || flagBits > knownFlagBits {
			return SpanInfo{}, false
		}
	}
	return SpanInfo{
		TraceID:  traceID,
		ParentID: parentID,
		Sampled:  flagBits&flagSampled != 0,
		Random:   flagBits&flagRandom != 0,
	}, true
}

// parseB3TraceID 解析 B3 的 trace-id（32hex 原样、16hex 左垫 0 归一为 32hex）。
//
// B3 是历史兼容路径：单头 `X-B3-TraceId` 里**没有** span-id，因此走这条路的
// 请求只有 trace-id、没有 parent——文档里写明了这一点，不要把它当成 W3C 等价物。
func parseB3TraceID(b3 string) string {
	if b3 == "" {
		return ""
	}
	switch {
	case isHex32Lower(b3) && b3 != zeroTraceID && b3 != zeroSpanID:
		return b3
	case isHex16Lower(b3) && b3 != zeroSpanID:
		return "0000000000000000" + b3
	}
	return ""
}

// generateTraceID 生成 32hex 的 W3C 兼容 trace-id。
//
// 不使用 observability.NewTraceID()：它返回 UnixNano-随机段-序号 的异构格式，
// 自生成场景下回写 traceparent 会违反 W3C（trace-id 必须 32 位小写 hex）。
// 自带生成器符合其设计（traceid.go:19 明确「返回值无契约语义、宿主可自带格式」）。
func generateTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func isHexLower(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return len(s) > 0
}

func isHex32Lower(s string) bool { return len(s) == 32 && isHexLower(s) }

func isHex16Lower(s string) bool { return len(s) == 16 && isHexLower(s) }

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return 0
}
