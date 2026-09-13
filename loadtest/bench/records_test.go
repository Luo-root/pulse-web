package bench

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Luo-root/pulse-web/loadtest/ginapp"
	"github.com/Luo-root/pulse-web/loadtest/pulseapp"
	"github.com/Luo-root/pulse/observability"
)

// countingSink 只数条数、留最后一条：它自己不能成为测量对象，
// 所以除了必要的结构拷贝什么也不做。
type countingSink struct {
	mu       sync.Mutex
	n        int
	last     observability.Record
	lastText string
}

func (s *countingSink) Write(r observability.Record) {
	var sb strings.Builder
	r.Attrs.Range(func(key string, val any) {
		fmt.Fprintf(&sb, "%s=%v ", key, val)
	})

	s.mu.Lock()
	s.n++
	s.last = r
	s.lastText = sb.String()
	s.mu.Unlock()
}

func (s *countingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func (s *countingSink) snapshot() (observability.Record, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.lastText
}

// lineCounter 数行数并留最后一行（gin 的 Logger 每请求写一行）。
type lineCounter struct {
	mu    sync.Mutex
	n     int
	cur   bytes.Buffer
	lastL string
}

func (w *lineCounter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			w.n++
			w.lastL = w.cur.String()
			w.cur.Reset()
			continue
		}
		w.cur.WriteByte(b)
	}
	return len(p), nil
}

func (w *lineCounter) lines() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

func (w *lineCounter) lastLine() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastL
}

// TestRecordsPerRequest 钉住「一次请求写几条」这个口径问题。
//
// 两侧都是**每请求 1 条**——pulse-web 是 1 条结构化 Record（Engine 收尾里的
// AccessLog；请求路径上没有第二处写 Sink 的地方，`c.Observe` 要业务自己调），
// gin 是 1 行文本。条数相同，**内容不同**：这条用例把两边的内容都打出来，
// 文档里「不是同一件事」那句得有据可依。
func TestRecordsPerRequest(t *testing.T) {
	const n = 50

	req := httptest.NewRequest(http.MethodGet, "/users/42", nil)
	w := &nopWriter{h: make(http.Header)}

	// pulse-web：装配期 Bootstrap 也会写记录，所以先取基线、只看增量。
	sink := &countingSink{}
	app := pulseapp.NewWithSink(sink)
	base := sink.count()
	t0 := time.Now()
	for range n {
		app.ServeHTTP(w, req)
	}
	elapsed := time.Since(t0)
	got := sink.count() - base
	if got != n {
		t.Errorf("pulse-web: %d 次请求写了 %d 条记录，期望 %d——条数口径要按实际改写", n, got, n)
	}
	last, text := sink.snapshot()
	t.Logf("pulse-web: 装配期 %d 条，每请求 %d 条；%d 次共 %v（均 %v），记录里的 Duration=%dns",
		base, got, n, elapsed, elapsed/n, last.Duration.Nanoseconds())
	t.Logf("pulse-web 记录: Source=%s Event=%s Status=%s TraceID=%d位 Duration=%v",
		last.Source, last.Event, last.Status, len(last.TraceID), last.Duration)
	t.Logf("pulse-web 内容: %s", text)

	// gin：数行数。
	counter := &lineCounter{}
	old := ginapp.LogWriter
	ginapp.LogWriter = counter
	defer func() { ginapp.LogWriter = old }()

	gapp := ginapp.New(ginapp.ModeObs)
	for range n {
		gapp.ServeHTTP(w, req)
	}
	if got := counter.lines(); got != n {
		t.Errorf("gin: %d 次请求写了 %d 行，期望 %d", n, got, n)
	}
	t.Logf("gin: 每请求 %d 行", counter.lines()/n)
	t.Logf("gin 内容: %s", counter.lastLine())
}
