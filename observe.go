package web

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
)

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
// 格式不符（长度、字符集）一律视为不存在 —— 不信任畸形输入。
func traceIDFromHeader(r *http.Request) string {
	if tp := strings.TrimSpace(r.Header.Get("Traceparent")); tp != "" {
		// version-traceid-parentid-flags
		parts := strings.Split(tp, "-")
		if len(parts) >= 3 && isHex32(parts[1]) {
			return strings.ToLower(parts[1])
		}
	}
	if b3 := strings.TrimSpace(r.Header.Get("X-B3-TraceId")); isHex32(b3) {
		return strings.ToLower(b3)
	}
	return ""
}

func isHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
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
