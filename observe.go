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

// zeroTraceID 是全零 trace-id。W3C traceparent 明文规定 trace-id 不得为全零
// （B3 未禁止）——两条入站路径统一按「不存在」处理：否则带该头的请求会在日志里
// 共享同一条 TraceID，比链路断裂更难排查。
const zeroTraceID = "00000000000000000000000000000000"

// traceIDFromHeader 读取 W3C traceparent / B3 的 trace-id。
// traceparent 的 trace-id 必须是 32 位 hex（W3C 规定）；B3 接受 32hex 与
// 16hex（Zipkin 64-bit，左垫 0 归一）。格式不符（长度、字符集）或全零一律
// 视为不存在 —— 不信任畸形输入。
func traceIDFromHeader(r *http.Request) string {
	if tp := strings.TrimSpace(r.Header.Get("Traceparent")); tp != "" {
		// version-traceid-parentid-flags
		parts := strings.Split(tp, "-")
		if len(parts) >= 3 && isHex32(parts[1]) {
			if id := strings.ToLower(parts[1]); id != zeroTraceID {
				return id
			}
		}
	}
	if b3 := strings.TrimSpace(r.Header.Get("X-B3-TraceId")); b3 != "" {
		if id := normalizeB3(b3); id != zeroTraceID {
			return id
		}
	}
	return ""
}

// normalizeB3 归一 B3 trace-id：32hex（128-bit）原样小写；16hex（Zipkin
// 64-bit）左垫 16 个 0 归一为 32hex；其余返回空串（视为不存在）。
//
// 补零方向取「高 64 位为零」这一多数 tracer 的约定 —— B3 规格只要求
// 「32 或 16 个 hex 字符、标识符不透明」，**未规定** 64→128 的补零方向
// （openzipkin/b3-propagation 的 README 未涉及）。将来若要与某个 128-bit
// 上游按位对齐，以对方的位序为准。
func normalizeB3(s string) string {
	switch {
	case isHex32(s):
		return strings.ToLower(s)
	case isHex16(s):
		return "0000000000000000" + strings.ToLower(s)
	}
	return ""
}

func isHex32(s string) bool { return len(s) == 32 && isHex(s) }

func isHex16(s string) bool { return len(s) == 16 && isHex(s) }

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
