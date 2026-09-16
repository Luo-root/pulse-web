// Package loadgen 是一个零依赖的固定并发压测器。
//
// 为什么自己写：外部工具（hey / wrk / vegeta）的分位口径、预热方式、连接
// 复用策略各不相同，换一个工具结论就可能反过来。这里把口径钉死在代码里——
// 固定并发、固定时长、连接全复用、预热后重新计时，计时窗口内的**每个**请求
// 都进分位（不采样）。
package loadgen

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Config 是一次压测的口径。
type Config struct {
	URL         string
	Concurrency int
	Warmup      time.Duration
	Duration    time.Duration
}

// Result 是一次压测的结果。
//
// Requests 只数**成功**请求（也就是进分位的那些），Errors 单独数：
// 混在一起会让「错误很快失败」抬高 RPS，看起来像是变快了。
type Result struct {
	Concurrency int
	Requests    int64
	Errors      int64
	Elapsed     time.Duration
	RPS         float64
	P50         time.Duration
	P90         time.Duration
	P99         time.Duration
	P999        time.Duration
	Max         time.Duration
}

// Run 按 cfg 压测：先跑 Warmup（结果丢弃，目的是把连接池铺开、让服务端进入
// 稳态），再重新计时跑 Duration。
func Run(cfg Config) Result {
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	client := newClient(cfg.Concurrency)

	if cfg.Warmup > 0 {
		_ = drive(client, cfg.URL, cfg.Concurrency, cfg.Warmup)
	}

	start := time.Now()
	b := drive(client, cfg.URL, cfg.Concurrency, cfg.Duration)
	elapsed := time.Since(start)

	res := Result{
		Concurrency: cfg.Concurrency,
		Requests:    int64(len(b.latencies)),
		Errors:      b.errors,
		Elapsed:     elapsed,
	}
	if elapsed > 0 {
		res.RPS = float64(res.Requests) / elapsed.Seconds()
	}
	if n := len(b.latencies); n > 0 {
		slices.Sort(b.latencies)
		res.P50 = pct(b.latencies, 50)
		res.P90 = pct(b.latencies, 90)
		res.P99 = pct(b.latencies, 99)
		res.P999 = pct(b.latencies, 99.9)
		res.Max = b.latencies[n-1]
	}
	return res
}

// String 是给人看的一行摘要。
func (r Result) String() string {
	return fmt.Sprintf("并发 %d · %.0f req/s · p50 %s · p90 %s · p99 %s · p999 %s · 错误 %d · 计时 %s",
		r.Concurrency, r.RPS, FmtDur(r.P50), FmtDur(r.P90), FmtDur(r.P99), FmtDur(r.P999),
		r.Errors, r.Elapsed.Round(time.Millisecond))
}

// FmtDur 把时长格式化成表格里好读的形态。
func FmtDur(d time.Duration) string {
	switch {
	case d >= time.Millisecond:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	case d >= time.Microsecond:
		return fmt.Sprintf("%.0fus", float64(d)/float64(time.Microsecond))
	default:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	}
}

// pct 取最近秩分位（输入必须已排序）。
func pct(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	i := int(p/100*float64(n-1) + 0.5)
	if i >= n {
		i = n - 1
	}
	return sorted[i]
}

type batch struct {
	latencies []time.Duration
	errors    int64
}

// drive 用 concurrency 个 worker 打 d 时长，返回计时窗口内的全部样本。
func drive(client *http.Client, url string, concurrency int, d time.Duration) batch {
	deadline := time.Now().Add(d)
	parts := make([]batch, concurrency)

	var wg sync.WaitGroup
	wg.Add(concurrency)
	for i := range parts {
		go func(b *batch) {
			defer wg.Done()
			// 每个 worker 用自己的缓冲：计时循环里不加锁、不做共享写，
			// 否则压测器自己的竞争会盖过被测框架的差别。
			buf := make([]time.Duration, 0, 1<<14)
			for time.Now().Before(deadline) {
				t0 := time.Now()
				err := once(client, url)
				if err != nil {
					b.errors++
					continue
				}
				buf = append(buf, time.Since(t0))
			}
			b.latencies = buf
		}(&parts[i])
	}
	wg.Wait()

	total := 0
	var res batch
	for i := range parts {
		total += len(parts[i].latencies)
		res.errors += parts[i].errors
	}
	res.latencies = make([]time.Duration, 0, total)
	for i := range parts {
		res.latencies = append(res.latencies, parts[i].latencies...)
	}
	return res
}

func once(client *http.Client, url string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	// 必须读干并关闭：否则连接不会回到 idle 池，keep-alive 就成了摆设。
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("意外状态码 %d", resp.StatusCode)
	}
	return nil
}

func newClient(concurrency int) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			// 显式 nil：不走环境变量里的代理。这台机器有代理配置，
			// 一旦被套进去，压测打的就是代理而不是被测服务。
			Proxy: nil,
			// 默认 Transport 的 MaxIdleConnsPerHost 是 2——不铺开就会把
			// 并发掐成「2 条连接排队」，测出来的是串行延迟不是并发能力。
			MaxIdleConns:        concurrency * 2,
			MaxIdleConnsPerHost: concurrency,
			MaxConnsPerHost:     concurrency,
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			IdleConnTimeout:     90 * time.Second,
			// 服务端不发 gzip，客户端也就不必声明可接受压缩。
			DisableCompression: true,
			// 明文 HTTP/1.1：被测的两个服务都是 h2c 关闭的。
			ForceAttemptHTTP2: false,
		},
		// 不设 Timeout：时长由 worker 的循环控制，设了会把慢请求变成
		// 错误，反而把长尾从分位里抹掉——而长尾正是要看的东西。
	}
}
