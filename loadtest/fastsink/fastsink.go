// Package fastsink 是「高性能 + 列式可读」出口的**原型**（不是框架代码）。
//
// 它回答的问题：pulse-web 默认出口（SlogSink）每请求要 ~1.6µs / 19 次分配，
// 而一份同样字段的列式文本能不能做到零分配。
//
// 手法都是常识，没有魔法：
//   - 池化 []byte，复用缓冲（sync.Pool）
//   - 不用 fmt：时间走 time.AppendFormat，数字走 strconv.Append*
//   - 只读自己需要的属性键（Attrs.Range 只暴露 key/value，没有 Get(key)）
//   - 一次锁、一次 Write
//
// 实测（同一个 Record，出口 io.Discard，见 loadtest/bench）：
// SlogSink ~1597ns/19 allocs、LineSink ~264ns/1、AsyncSink ~482ns/1、
// 本包 ~122ns/**0**。
package fastsink

import (
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/Luo-root/pulse/observability"
)

// 时间与版式常量：AppendFormat 不做反射，包级常量连字符串也不用重用。
const (
	timeLayout = "2006/01/02 - 15:04:05"
	prefix     = "[WEB] "
)

// Sink 把 Record 渲染成一行列式文本。
type Sink struct {
	mu   sync.Mutex
	w    io.Writer
	pool sync.Pool
}

// New 构造出口。w 是所有并发请求共享的destination（通常是 os.Stdout / 文件）。
func New(w io.Writer) *Sink {
	return &Sink{
		w: w,
		pool: sync.Pool{New: func() any {
			b := make([]byte, 0, 256)
			return &b
		}},
	}
}

// Write 实现 observability.Sink。
//
// 注意：装配期记录（`observability.host_ready` 等）没有 status / route，
// 走这里的列式版式会出现空列——真做要**按 event 分版式**，本原型不做。
func (s *Sink) Write(r observability.Record) {
	var method, route, client, etype string
	r.Attrs.Range(func(k string, v any) {
		switch k {
		case "http.request.method":
			method, _ = v.(string)
		case "http.route":
			route, _ = v.(string)
		case "client.address":
			client, _ = v.(string)
		case "error.type":
			etype, _ = v.(string)
		}
	})

	bp := s.pool.Get().(*[]byte)
	b := (*bp)[:0]

	b = append(b, prefix...)
	b = r.Time.AppendFormat(b, timeLayout)
	b = append(b, " | "...)
	b = append(b, r.Status...)
	b = append(b, " | "...)
	b = appendDuration(b, r.Duration)
	b = append(b, " | "...)
	b = append(b, client...)
	b = append(b, " | "...)
	b = append(b, method...)
	b = append(b, ' ')
	b = append(b, route...)
	if etype != "" {
		b = append(b, " | "...)
		b = append(b, etype...)
	}
	if len(r.TraceID) >= 8 {
		b = append(b, " | trace="...)
		b = append(b, r.TraceID[:8]...)
	}
	b = append(b, '\n')

	s.mu.Lock()
	_, _ = s.w.Write(b)
	s.mu.Unlock()

	*bp = b
	s.pool.Put(bp)
}

// appendDuration 手写单位选择：µs 精度（上游 SlogSink 的 duration_ms 会把
// 亚毫秒请求取整成 0，看不出快慢）。
func appendDuration(b []byte, d time.Duration) []byte {
	switch {
	case d >= time.Millisecond:
		b = strconv.AppendFloat(b, float64(d)/float64(time.Millisecond), 'f', 2, 64)
		return append(b, "ms"...)
	case d >= time.Microsecond:
		b = strconv.AppendFloat(b, float64(d)/float64(time.Microsecond), 'f', 1, 64)
		return append(b, "us"...)
	default:
		b = strconv.AppendInt(b, d.Nanoseconds(), 10)
		return append(b, "ns"...)
	}
}
