package web

import (
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/Luo-root/pulse/kernel"
	"github.com/Luo-root/pulse/observability"
)

// 请求级 Collector（WithCollector）的可见性边界与并发隔离。
//
// 上游 v0.2.1 起 Collector 是 `kernel.Local()` 作用域局部绑定：绑定所在
// scope 及其后代可读，父 / 兄弟不可读，且不参与 fiber 依赖解析。

func TestWithCollectorScopeBoundary(t *testing.T) {
	e, _ := newTestEngine(t, WithCollector())

	var self, descendant, sibling bool
	e.GET("/probe", func(c *Ctx) error {
		scope := c.Kernel()
		if _, ok := kernel.Get(scope, observability.CollectorKey); ok {
			self = true
		}

		child, err := scope.Derive()
		if err != nil {
			return err
		}
		defer child.Dispose()
		if _, ok := kernel.Get(child, observability.CollectorKey); ok {
			descendant = true
		}

		// 插件私有 scope 的形态：root 的另一个子节点，与请求 scope 同级。
		sib, err := e.Root().Derive()
		if err != nil {
			return err
		}
		defer sib.Dispose()
		if _, ok := kernel.Get(sib, observability.CollectorKey); ok {
			sibling = true
		}

		return c.Text(http.StatusOK, "ok")
	})

	doReq(e, "GET", "/probe", nil)

	if !self {
		t.Error("请求 scope 自身应读得到 Collector")
	}
	if !descendant {
		t.Error("请求 scope 的后代应读得到 Collector")
	}
	if sibling {
		t.Error("插件私有 scope（与请求 scope 是兄弟）不应读得到 Collector")
	}
	if _, ok := kernel.Get(e.Root(), observability.CollectorKey); ok {
		t.Error("宿主 root 不应读得到请求级 Collector")
	}
}

func TestWithCollectorDefaultsOff(t *testing.T) {
	e, _ := newTestEngine(t)

	var found bool
	e.GET("/probe", func(c *Ctx) error {
		_, found = kernel.Get(c.Kernel(), observability.CollectorKey)
		return c.Text(http.StatusOK, "ok")
	})
	doReq(e, "GET", "/probe", nil)

	if found {
		t.Fatal("默认装配不应挂 Collector —— 开了才付每请求成本")
	}
}

func TestWithCollectorRequiresSink(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Minimal() 且无 WithSink 时，WithCollector 应在装配期 panic（对齐「装配期暴露错误」）")
		}
	}()
	_ = New(Minimal(), WithCollector())
}

// TestCollectorIsolatesConcurrentRequests 是**真并发**用例：N 个 goroutine
// 各发一次请求，事后核对「每个请求恰好一条 probe.event」且「每条记录的
// TraceID 命中自己那次请求」。
//
// 顺序发两次测不出串台——即使绑定落在共享位置、并发互相覆盖，顺序跑照样
// 通过。这条用例最初写成了顺序调用却顶着「并发」的名字，已被 review 抓出。
func TestCollectorIsolatesConcurrentRequests(t *testing.T) {
	e, sink := newTestEngine(t, WithCollector())
	e.GET("/probe", func(c *Ctx) error {
		col, ok := kernel.Get(c.Kernel(), observability.CollectorKey)
		if !ok {
			return errors.New("collector missing")
		}
		col.WriteAttrs("probe.event", "", func(a *observability.Attrs) {
			observability.Set(a, "probe.marker", c.TraceID())
		})
		return c.Text(http.StatusOK, c.TraceID())
	})

	const n = 32
	got := make([]string, n) // 每个 goroutine 只写自己的下标，无共享写
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := doReq(e, "GET", "/probe", nil)
			if rec.Code != http.StatusOK {
				t.Errorf("req %d: status = %d", i, rec.Code)
				return
			}
			got[i] = rec.Header().Get("X-Trace-Id")
		}(i)
	}
	wg.Wait()

	seen := make(map[string]int, n)
	for _, r := range sink.Snapshot() {
		if r.Event == "probe.event" {
			seen[r.TraceID]++
		}
	}
	if len(seen) != n {
		t.Fatalf("probe.event 的 TraceID 去重后 = %d，want %d —— 并发请求串台了", len(seen), n)
	}
	for i, want := range got {
		if want == "" {
			t.Fatalf("req %d 没有 TraceID", i)
		}
		if seg := seen[want]; seg != 1 {
			t.Fatalf("req %d 的 TraceID %s 在记录里出现 %d 次，want 1", i, want, seg)
		}
	}
}
