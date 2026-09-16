// Package otelweb 把 pulse-web 的请求级 span 数据接进 OpenTelemetry。
//
// 它是 pulse-web 主模块与 otel-go 之间唯一的桥：主模块零第三方依赖，只产出
// 结构化数据（web.SpanInfo / web.SpanRef / web.Span）；本包把这些数据翻成
// SDK 类型。装配形态：
//
//	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
//	app := web.New(web.WithSpanHook(otelweb.New(tp)))
//
// 一个请求的生命周期：
//
//	Begin  用官方 propagator 语义解析出的入站身份（web.SpanInfo）建一个
//	       server span，把它注入请求 context —— 中间件、handler、以及它们
//	       往下传的 context 都看得到，出站 HTTP 客户端 / SQL / gRPC 的 OTel
//	       插桩因此自动接上这条链路；span 的 id 同时回给框架（web.SpanRef），
//	       于是响应头 Server-Timing 与访问日志里的 span.id 用的是同一个 id；
//	End    用 `trace.SpanFromContext(ctx)` 取回同一个 span，补上路由模板、
//	       属性与状态，然后结束它。
//
// 与官方 HTTP 插桩（otelhttp）的关系：otelhttp 包在整个服务外面，看不见
// 「错误映射后的状态码」与「路由模板」；pulse-web 的请求收尾在引擎里，所以
// 数据由框架给，本包只做转换与 span 生命周期。
//
// # 各字段的取值口径
//
//   - span 名：`{method} {http.route}`（semconv 的规则）；路由拿不到时退化为
//     `{method}`。**不会**退回 URI 路径——semconv 明文禁止用 URI 路径当名称，
//     那是 404 场景下 APM 基数爆炸的源头。未知方法在名字里退化为 `HTTP`。
//   - 方法：`http.request.method` 归一成 semconv 允许的值（已知方法大写；
//     不认识的写 `_OTHER`），原始值另放 `http.request.method_original`。
//     框架的**记录**侧保留原始方法不归一——见 normalizeMethodAttr 的说明。
//   - 状态：HTTP 5xx → `codes.Error`（描述留空，原因可由 http.response.status_code
//     推出）；4xx / 3xx / 2xx → 保持 Unset（server span 的 4xx 规范要求 MUST unset）。
//   - `http.response.status_code`：由框架映射后的状态码写入（与访问日志同源）。
//   - `error.type`：框架已经给了（`http_5xx` / `panic` / `internal` 这类低基数
//     标识）就用它的；没有而又 ≥500 时，按 semconv 写状态码字符串。
//   - 属性一律来自框架的观测属性（`http.request.method` / `http.route` /
//     `url.path` / `http.response.body.size` / `client.address`），与访问日志
//     同一份来源，避免两处各说一套。
//   - 异常：**不**调 `span.RecordError`。错误原文留在进程内的观测记录里，
//     不随 span 出到追踪后端（与「panic 栈不出响应体」同一条口径）。
package otelweb

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	web "github.com/Luo-root/pulse-web"
	"github.com/Luo-root/pulse/observability"
)

// instrumentationName 是 tracer 名（出现在 instrumentation scope 里）。
const instrumentationName = "github.com/Luo-root/pulse-web/otel"

// methodOther 是 semconv 给「不认识的方法」留的值。
const methodOther = "_OTHER"

// knownMethod 报方法是不是 semconv 认的「已知方法」。
//
// 集合取官方 otelhttp 的 methodLookup：RFC9110 的九个 + PATCH。QUERY（spec 引的
// httpbis-safe-method-w-body）官方实现尚未收录，这里与官方实现保持一致——宁可跟
// 实现同口径，也不自作主张多认一个，否则同一个请求在两套插桩下会得到不同的值。
func knownMethod(m string) bool {
	switch m {
	case "CONNECT", "DELETE", "GET", "HEAD", "OPTIONS", "PATCH", "POST", "PUT", "TRACE":
		return true
	}
	return false
}

// methodAttrs 按 semconv 归一请求方法，返回（写进 http.request.method 的值,
// 原始值）。原始值非空时调用方另写 http.request.method_original。
//
// 三条分支与官方 otelhttp 的 HTTPServer.method 逐条对齐：
//
//	大写已知方法（"GET"）  → 原样，不写 original
//	大小写不同但认识（"get"）→ 规范值 "GET" + original "get"
//	完全不认识（"FOO"）    → "_OTHER" + original "FOO"
//
// 空方法按 `_OTHER` 处理，且不写 original（没有什么可记的）。
func methodAttrs(m string) (value, original string) {
	if m == "" {
		return methodOther, ""
	}
	if knownMethod(m) {
		return m, ""
	}
	if up := strings.ToUpper(m); knownMethod(up) {
		return up, m
	}
	return methodOther, m
}

// spanName 按 semconv 定 span 名：有路由用 `{method} {http.route}`，没有路由
// 只留 `{method}`。
//
// 两个「不得」：**不得**退回 URI 路径（semconv 明文：instrumentation MUST NOT
// default to using URI path），**不得**编出 `HTTP GET` 这种拼法——未知方法时
// `{method}` 就退化为 `HTTP` 这一个词（semconv 原文）。这两条是 404 场景下
// APM 基数爆炸的源头，所以钉在用例里。
func spanName(method, route string) string {
	m := strings.ToUpper(method)
	if !knownMethod(m) {
		m = "HTTP"
	}
	if route == "" {
		return m
	}
	return m + " " + route
}

// Hook 实现 web.SpanHook。
type Hook struct {
	tracer trace.Tracer
}

var _ web.SpanHook = (*Hook)(nil)

// New 用宿主的 TracerProvider 造一个适配件。
//
// tp 就是宿主自己那套（采样器、处理器、导出器都已配好）——本包不改写它，
// 采样决策完全归宿主：入站 sampled=0 的请求在 ParentBased 采样器下不会导出
// span，这是 OTel 的既定语义，不是这里的 bug。
func New(tp trace.TracerProvider, opts ...Option) *Hook {
	h := &Hook{tracer: tp.Tracer(instrumentationName)}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Option 是适配件自己的旋钮（不碰宿主的 SDK 配置）。
type Option func(*Hook)

// Begin 建 span、注入 context，并把身份回给框架。
func (h *Hook) Begin(ctx context.Context, in web.SpanInfo) (context.Context, web.SpanRef) {
	// 此刻还不知道路由，名字先只放方法——路由到 End 时再补全（semconv 允许）。
	ctx, span := h.tracer.Start(withRemoteParent(ctx, in), spanName(in.Method, ""),
		trace.WithSpanKind(trace.SpanKindServer),
	)
	sc := span.SpanContext()
	return ctx, web.SpanRef{
		TraceID: sc.TraceID().String(),
		SpanID:  sc.SpanID().String(),
		Sampled: sc.TraceFlags().IsSampled(),
		Random:  sc.TraceFlags().IsRandom(),
	}
}

// End 补全 span 并结束它。
func (h *Hook) End(ctx context.Context, sp web.Span) {
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		// Begin 没有建 span（例如 hook 复用在了别的路径上）——静默返回，
		// 不要在这里造一个没有起点的 span。
		return
	}

	// 名称：路由这时才知道（semconv 允许在 span 结束前补）。未知方法在这里
	// 退化为 `HTTP`，404 这类没有路由的请求只留方法——不含任何路径成分。
	span.SetName(spanName(sp.Method, sp.Route))

	attrs, hasErrorType := convertAttrs(sp.Attrs, sp.Status >= 500)
	attrs = normalizeMethodAttr(attrs)
	if sp.Status > 0 {
		attrs = append(attrs, attribute.Int("http.response.status_code", sp.Status))
	}
	switch {
	case sp.Status >= 500:
		// 5xx：Error；描述留空（原因可由 http.response.status_code 推出）。
		span.SetStatus(codes.Error, "")
		if !hasErrorType {
			// 框架没给错误标识时按 semconv 补状态码字符串；给了就用框架那个
			// （它更具体、基数同样低）。
			attrs = append(attrs, attribute.String("error.type", strconv.Itoa(sp.Status)))
		}
	case sp.Status >= 400:
		// server span 的 4xx：规范要求保持 Unset（错误由调用方负责）。
	}
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
	span.End(trace.WithTimestamp(sp.End))
}

// withRemoteParent 把入站 traceparent 变成 span 的远端父上下文。
//
// 没有任何入站身份时返回原 ctx —— span 就是一条新 trace 的根。
func withRemoteParent(ctx context.Context, in web.SpanInfo) context.Context {
	if in.ParentID == "" {
		return ctx
	}
	tid, err := trace.TraceIDFromHex(in.TraceID)
	if err != nil {
		return ctx
	}
	pid, err := trace.SpanIDFromHex(in.ParentID)
	if err != nil {
		return ctx
	}
	// tracestate 解析失败不影响 traceparent（W3C 明文），所以忽略 error。
	ts, _ := trace.ParseTraceState(in.TraceState)
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		SpanID:     pid,
		TraceState: ts,
		TraceFlags: flags(in.Sampled, in.Random),
		Remote:     true,
	})
	if !sc.IsValid() {
		return ctx
	}
	return trace.ContextWithRemoteSpanContext(ctx, sc)
}

func flags(sampled, random bool) trace.TraceFlags {
	var f trace.TraceFlags
	if sampled {
		f |= trace.FlagsSampled
	}
	if random {
		f |= trace.FlagsRandom
	}
	return f
}

// normalizeMethodAttr 把框架给的**原始**方法归一成 semconv 允许的值，必要时
// 补一条 http.request.method_original（与官方 otelhttp 的 HTTPServer.method
// 同口径，见 methodAttrs）。
//
// 为什么要在这里归一、而不是在框架侧：框架的记录是**给人读的日志流**，它保留
// 原始方法（`PURGE` / `FOO` 这类直接可读）；span 是给 APM 的、有允许值集合。
// 两边属性名相同、取值口径不同，这一条差异是有意的——同一个人在记录里看到的
// 永远是原始值，在追踪后端不会因为一个冷门方法多出一堆值。框架没带方法属性时
// 这里什么都不做（span 名仍有方法）。
func normalizeMethodAttr(attrs []attribute.KeyValue) []attribute.KeyValue {
	for i := range attrs {
		if attrs[i].Key != "http.request.method" {
			continue
		}
		value, original := methodAttrs(attrs[i].Value.AsString())
		attrs[i] = attribute.String("http.request.method", value)
		if original != "" {
			attrs = append(attrs, attribute.String("http.request.method_original", original))
		}
		return attrs
	}
	return attrs
}

// convertAttrs 把框架的观测属性翻成 OTel 属性。
//
// keepErrorType 为 false 时**丢掉** error.type：框架的记录里 4xx 也会带
// `error.type=http_4xx`（那是它自己的日志词表），而 semconv 说得很清楚——
// 4xx 不算错误，成功完成的请求 SHOULD NOT 设 error.type。两边的差异是有意的：
// 日志按框架词表分类，span 按 semconv。
//
// 第二个返回值表示「保留下来的属性里已经有 error.type」——调用方据此决定要不要
// 按 semconv 补状态码（两处都写会互相覆盖，且丢掉更具体的那一个）。
func convertAttrs(a *observability.Attrs, keepErrorType bool) ([]attribute.KeyValue, bool) {
	if a == nil || a.Len() == 0 {
		return nil, false
	}
	out := make([]attribute.KeyValue, 0, a.Len())
	var hasErrorType bool
	a.Range(func(k string, v any) {
		if k == "error.type" {
			if !keepErrorType {
				return
			}
			hasErrorType = true
		}
		switch t := v.(type) {
		case string:
			out = append(out, attribute.String(k, t))
		case int64:
			out = append(out, attribute.Int64(k, t))
		case float64:
			out = append(out, attribute.Float64(k, t))
		case bool:
			out = append(out, attribute.Bool(k, t))
		default:
			// 值域是类型锁死的开放段（observability.AttrValue），走到这里说明
			// 上游加了新类型而本适配层没跟上——兜底成字符串，不丢字段。
			out = append(out, attribute.String(k, fmt.Sprintf("%v", v)))
		}
	})
	return out, hasErrorType
}
