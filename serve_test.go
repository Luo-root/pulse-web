package web

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Luo-root/pulse/observability"
)

// 本文件覆盖 Serve / Run 的关闭链路：信号 → drain → OnShutdown → Dispose → flush，
// 以及 ServerConfig 的合并与透传。
// 边界：请求级装配归 engine_test.go，退出时对出口的 flush 细节归 console_sink_test.go。

// 生命周期入口的覆盖：`Engine.Run` / `Engine.Serve` / `Engine.serve`。
//
// 这三条此前一条都没被执行过——原有的优雅关闭用例自建 `http.Server` 直接调
// `srv.Shutdown`，覆盖的是实现而不是用户会调用的入口；`OnShutdown` 回调、
// `root.Dispose`、Sink flush 三段都在它之外。

// TestServeSignalRunsFullShutdownChain 把公开入口的关闭链路整条跑通：
// 收到信号 → drain 在途请求 → OnShutdown → root.Dispose → Sink flush。
//
// 信号源是注入的 channel（理由见 serve 的注释）：Windows 上无法给自身进程
// 投递 os.Interrupt，真实信号路径不可测。
func TestServeSignalRunsFullShutdownChain(t *testing.T) {
	var out bytes.Buffer
	sink := observability.NewLineSink(&out) // 缓冲出口：未达阈值时不落盘
	e := New(WithSink(sink), WithHostID("serve-test"))
	t.Cleanup(e.dispose)

	started := make(chan struct{})
	release := make(chan struct{})
	e.GET("/slow", func(c *Ctx) error {
		close(started)
		<-release
		return c.Text(http.StatusOK, "drained")
	})

	hook := make(chan struct{})
	e.OnShutdown(func(context.Context) error { close(hook); return nil })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sigCh := make(chan os.Signal, 1)
	serveErr := make(chan error, 1)
	go func() { serveErr <- e.serve(ln, sigCh) }()

	respCh := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			respCh <- "ERR: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		respCh <- string(b)
	}()

	<-started
	sigCh <- os.Interrupt // 触发优雅关闭

	// 在途请求结束前 serve 不应返回（drain 由 net/http 承担）
	select {
	case err := <-serveErr:
		t.Fatalf("在途请求未完成时 serve 就返回了：%v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	select {
	case got := <-respCh:
		if got != "drained" {
			t.Fatalf("在途响应 = %q，want drained", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("在途请求没有完成")
	}

	select {
	case err := <-serveErr:
		if err != nil {
			t.Fatalf("serve 返回错误：%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("收到信号后 serve 没有返回")
	}

	select {
	case <-hook:
	default:
		t.Fatal("OnShutdown 回调没有执行")
	}
	if out.Len() == 0 {
		t.Fatal("关闭期没有 flush 出口缓冲：LineSink 未达阈值的记录会随进程消失")
	}
	if !strings.Contains(out.String(), eventHTTPReq) {
		t.Fatalf("flush 落盘的内容里没有访问记录：%q", out.String())
	}
	// ④ 之后引擎已回收：kernel root 销毁，请求直接 503
	if rec := doReq(e, "GET", "/slow", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("关闭后引擎状态 = %d，want 503（root 未被回收？）", rec.Code)
	}
}

// TestServeReturnsServerError 覆盖 serve 的 server 错误分支：listener 已关闭时
// `srv.Serve` 立即报错，serve 把错误透出（而不是静默返回 nil 假装正常关闭），
// 并回收引擎。这里走的是公开入口 `Serve` 本身。
func TestServeReturnsServerError(t *testing.T) {
	e, _ := newTestEngine(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	if err := e.Serve(ln); err == nil {
		t.Fatal("listener 已关闭时 Serve 应返回错误，得到 nil")
	}
	if rec := doReq(e, "GET", "/x", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("server 出错后引擎未回收：status = %d，want 503", rec.Code)
	}
}

// TestRunListenFailureDisposesEngine 覆盖 `Run` 的监听失败分支：绑不上地址时
// 回收 root 并返回错误，不留一个半死的引擎给调用方。
//
// `Run` 的成功分支 = `net.Listen` + 转 `Serve`；`Serve` 与 `serve` 已由本文件
// 其余用例与 TestServeSignalRunsFullShutdownChain 覆盖。
func TestRunListenFailureDisposesEngine(t *testing.T) {
	e, _ := newTestEngine(t)

	if err := e.Run("127.0.0.1:99999"); err == nil {
		t.Fatal("端口越界时 Run 应返回监听错误，得到 nil")
	}
	if rec := doReq(e, "GET", "/anything", nil); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("监听失败后引擎未回收：status = %d，want 503", rec.Code)
	}
}

// TestDefaultServerConfigValues 钉住 §ServerConfig 契约表的 6 个默认值。
// `WriteTimeout` 是其中最要紧的一个：非 0 会把 SSE / 长连接切断，默认必须是 0。
func TestDefaultServerConfigValues(t *testing.T) {
	got := DefaultServerConfig()
	want := ServerConfig{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ShutdownTimeout:   30 * time.Second,
	}
	if got != want {
		t.Fatalf("DefaultServerConfig() = %+v，want %+v", got, want)
	}
}

// TestWithServerMergesNonZeroFields 覆盖 mergeServer：非零字段生效、零值字段保持默认。
func TestWithServerMergesNonZeroFields(t *testing.T) {
	def := DefaultServerConfig()
	e, _ := newTestEngine(t, WithServer(ServerConfig{
		ReadTimeout:    7 * time.Second,
		MaxHeaderBytes: 2048,
	}))

	if e.server.ReadTimeout != 7*time.Second {
		t.Fatalf("ReadTimeout = %v，want 7s（非零字段应生效）", e.server.ReadTimeout)
	}
	if e.server.MaxHeaderBytes != 2048 {
		t.Fatalf("MaxHeaderBytes = %d，want 2048（非零字段应生效）", e.server.MaxHeaderBytes)
	}
	if e.server.ReadHeaderTimeout != def.ReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %v，want 默认 %v（零值字段应保持默认）",
			e.server.ReadHeaderTimeout, def.ReadHeaderTimeout)
	}
	if e.server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v，want 0（零值字段不得被写坏）", e.server.WriteTimeout)
	}
	if e.server.ShutdownTimeout != def.ShutdownTimeout {
		t.Fatalf("ShutdownTimeout = %v，want 默认 %v", e.server.ShutdownTimeout, def.ShutdownTimeout)
	}
}

// TestServerConfigReachesHTTPServer 证「配置真的落到了 http.Server 上」，而不只是
// 存进 Engine 字段：1 KiB 上限下，超限的请求头必须被拒（431）。
//
// 两条口径先钉住，否则这条断言会假红/假绿：
//
//   - 尺寸要按 net/http 的实际预算给：`MaxHeaderBytes` 只是读取上限，reader 的实际
//     预算是 `maxHeaderBytes + 4096`（bufio 余量），所以 1 KiB 配置下 2 KiB 的头照样放行。
//   - 超限请求必须走**新连接**：实测（Go 1.27，纯 `http.Server` 探针复现，与框架无关）
//     keep-alive 连接上**后续**请求不受此限——同一连接先发一个小请求、再发 8 KiB 头，
//     服务端会正常处理；新连接上的首个请求才返回 431。所以这里用一个禁用 keep-alive
//     的独立 client 发超限请求。
func TestServerConfigReachesHTTPServer(t *testing.T) {
	e, _ := newTestEngine(t, WithServer(ServerConfig{MaxHeaderBytes: 1024}))
	e.GET("/", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sigCh := make(chan os.Signal, 1)
	serveErr := make(chan error, 1)
	go func() { serveErr <- e.serve(ln, sigCh) }()
	defer func() {
		sigCh <- os.Interrupt
		if err := <-serveErr; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()

	// 基线：普通请求通
	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("基线请求 status = %d，want 200", resp.StatusCode)
	}

	// 8 KiB 头 > 1 KiB + 4 KiB 余量，且走新连接：必须被 http.Server 拒掉
	req, err := http.NewRequest("GET", "http://"+ln.Addr().String()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Big", strings.Repeat("a", 8<<10))
	fresh := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}
	resp, err = fresh.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		t.Fatalf("超限请求头 status = %d，want 431 —— ServerConfig 没有传到 http.Server", resp.StatusCode)
	}
}

// TestWithoutAccessLogKeepsTraceAndPanicGuard：这个选项只关访问日志——Trace
// （响应头）与 panic 兜底不受影响。
func TestWithoutAccessLogKeepsTraceAndPanicGuard(t *testing.T) {
	e, sink := newTestEngine(t, WithoutAccessLog())
	e.GET("/ok", func(c *Ctx) error { return c.Text(http.StatusOK, "ok") })
	e.GET("/boom", func(c *Ctx) error { panic("kaboom") })

	rec := doReq(e, "GET", "/ok", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，want 200", rec.Code)
	}
	if rec.Header().Get("X-Trace-Id") == "" {
		t.Fatal("Trace 被 WithoutAccessLog 一起关掉了（应只关访问日志）")
	}
	if _, ok := findRecord(sink, eventHTTPReq); ok {
		t.Fatal("WithoutAccessLog 下不应写 http.request 记录")
	}

	// panic 兜底独立于访问日志
	if r := doReq(e, "GET", "/boom", nil); r.Code != http.StatusInternalServerError {
		t.Fatalf("panic 兜底 status = %d，want 500", r.Code)
	}
}

// TestGracefulShutdownDrainsInflight 验证 drain 由 net/http 承担：
// 在途请求在 Shutdown 期间正常完成，不被截断（kernel.Dispose 不做这件事）。
func TestGracefulShutdownDrainsInflight(t *testing.T) {
	e, _ := newTestEngine(t)
	started := make(chan struct{})
	release := make(chan struct{})
	e.GET("/slow", func(c *Ctx) error {
		close(started)
		<-release
		return c.Text(http.StatusOK, "drained")
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: e}
	go func() { _ = srv.Serve(ln) }()

	respCh := make(chan string, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/slow")
		if err != nil {
			respCh <- "ERR: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		respCh <- string(b)
	}()

	<-started

	shutdownDone := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		close(shutdownDone)
	}()

	// Shutdown 不应在在途请求结束前返回
	select {
	case <-shutdownDone:
		t.Fatal("Shutdown returned while the request was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case got := <-respCh:
		if got != "drained" {
			t.Fatalf("response = %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("in-flight request did not complete")
	}
	select {
	case <-shutdownDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown did not complete after drain")
	}
}
