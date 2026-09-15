package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

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

// resolveTraceID 优先采纳上游链路头，否则新生成。
func (e *Engine) resolveTraceID(r *http.Request) string {
	if id := traceIDFromHeader(r); id != "" {
		return id
	}
	return generateTraceID()
}

// traceIDFromHeader 读取 W3C traceparent / B3 的 trace-id。
// traceparent 的 trace-id 必须是 32 位 hex（W3C 规定）；B3 接受 32hex 与
// 16hex（Zipkin 64-bit，左垫 0 归一）。格式不符（长度、字符集）一律视为
// 不存在 —— 不信任畸形输入。
func traceIDFromHeader(r *http.Request) string {
	if tp := strings.TrimSpace(r.Header.Get("Traceparent")); tp != "" {
		// version-traceid-parentid-flags
		parts := strings.Split(tp, "-")
		if len(parts) >= 3 && isHex32(parts[1]) {
			return strings.ToLower(parts[1])
		}
	}
	if b3 := strings.TrimSpace(r.Header.Get("X-B3-TraceId")); b3 != "" {
		return normalizeB3(b3)
	}
	return ""
}

// normalizeB3 归一 B3 trace-id：32hex（128-bit）原样小写；16hex（Zipkin
// 64-bit）左垫 16 个 0 归一为 32hex；其余返回空串（视为不存在）。
func normalizeB3(s string) string {
	switch len(s) {
	case 32:
		if isHex(s) {
			return strings.ToLower(s)
		}
	case 16:
		if isHex(s) {
			return "0000000000000000" + strings.ToLower(s)
		}
	}
	return ""
}

func isHex32(s string) bool { return len(s) == 32 && isHex(s) }

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}
