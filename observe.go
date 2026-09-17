package web

import (
	"net/http"
	"strings"

	"github.com/Luo-root/pulse/observability"
)

// writeObservation 是 Ctx.Observe 与 Detached.Observe 的共用实现。
//
// 它直写 Sink、不经 scope —— 信封填充与 observability.Collector 的 write 同构，
// 区别是本框架**默认不注册 Collector**（那是 WithCollector() 的显式行为，
// 且 Collector 是作用域局部绑定、只对持有请求 scope 的组件可见），
// 因此这里不带 Collector 的 status 参数：web 场景的状态语义走
// Record.Status 或 Attrs。若上游补了「只构造、不 Provide」的 NewCollector，
// 本函数可直接切换过去。
func writeObservation(sink observability.Sink, hostID, traceID, event string, set func(*observability.Attrs)) {
	if sink == nil {
		return
	}
	rec := observability.Record{
		HostID:  hostID,
		TraceID: traceID,
		Source:  observability.SourceAdapter,
		Event:   event,
	}
	if set != nil {
		set(&rec.Attrs)
	}
	sink.Write(rec)
}

// resolveSpanInfo 解析入站链路头，得到本请求的 trace 身份（span 侧的数据）。
//
// 顺序：W3C `traceparent`（严格校验，规则与官方 otel-go 实现逐条对齐，见 span.go）
// → B3 `X-B3-TraceId`（历史兼容，只给 trace-id，没有 parent）→ 都没有就新起一条。
//
// trustTraceHeader 关掉时整段跳过：不读任何入站头，一律新起 trace。
//
// 自启 trace 时 sampled 与 random 都置位：trace-id 全部来自 crypto/rand（满足
// W3C 对 random-trace-id 的条件），而本服务确实会为这次请求留下记录（访问日志），
// 所以 sampled=1 是事实而不是乐观假设。
func (e *Engine) resolveSpanInfo(r *http.Request) SpanInfo {
	if e.trustTraceHeader {
		if in := parseInboundTrace(r.Header.Get("Traceparent"), r.Header.Get("Tracestate")); in.TraceID != "" {
			return in
		}
		if id := parseB3TraceID(strings.TrimSpace(r.Header.Get("X-B3-TraceId"))); id != "" {
			return SpanInfo{TraceID: id}
		}
	}
	return SpanInfo{TraceID: generateTraceID(), Sampled: true, Random: true}
}
