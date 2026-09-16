// Command compare 一条命令跑完 pulse-web 与 gin 的真实负载对比。
//
// 它自己 build 被测服务、顺序起停、预热后计时、多轮遍历、最后打印可直接
// 粘进文档的 markdown 表。不依赖 PowerShell / bash，Windows 与 Linux 同一份行为。
//
// 在 loadtest/ 目录下运行：
//
//	go run ./cmd/compare                 # 3 轮 × 2 档位 × 2 档并发 × (预热 3s + 计时 15s)
//	go run ./cmd/compare -probe          # 追加 pulse-web 的诊断档（拆观测开销）
//	go run ./cmd/compare -quick          # 自检用缩水口径，数字**不要**进文档
package main

import (
	"bytes"
	"cmp"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Luo-root/pulse-web/loadtest/internal/loadgen"
)

const (
	modulePath = "github.com/Luo-root/pulse-web/loadtest"

	fwPulse = "pulse"
	fwGin   = "gin"

	modeBare     = "bare"
	modeObs      = "obs"
	modeObsAsync = "obs-async"
	modeObsNoLog = "obs-nolog"
)

type target struct {
	fw   string
	mode string
}

func (t target) label() string { return t.fw + "/" + t.mode }

// pair 是一组「同一格内相邻跑」的档位。两侧都在的才是对拍；只有一侧的是诊断。
type pair struct {
	mode string
	fws  []string
}

type key struct {
	fw   string
	mode string
	conc int
}

type cell struct {
	runs []loadgen.Result // 一轮一个，顺序与遍历轮次一致
}

// witness 取 RPS **中位**的那一轮，报告它的整套分位。
//
// 不用「最好的一轮」：噪声确实只会让 RPS 变低，但**轮次先后本身**也会带来
// 系统偏差（机器越跑越热 / 越跑越稳），而先后顺序在一轮内是固定的——取最好
// 的一轮等于把「哪一侧恰好排在后面」当成结果。中位那一轮两个方向都吃掉一半。
func (c *cell) witness() loadgen.Result {
	sorted := slices.Clone(c.runs)
	slices.SortFunc(sorted, func(a, b loadgen.Result) int { return cmp.Compare(a.RPS, b.RPS) })
	return sorted[len(sorted)/2]
}

// spread 是轮间 RPS 的相对极差（%），用来看这一格稳不稳。
func (c *cell) spread() float64 {
	lo, hi := c.runs[0].RPS, c.runs[0].RPS
	for _, r := range c.runs[1:] {
		lo = min(lo, r.RPS)
		hi = max(hi, r.RPS)
	}
	if hi == 0 {
		return 0
	}
	return (hi - lo) / hi * 100
}

// ratioMedian 是**逐轮配对**比值的中位：同一轮内两侧先后相邻，机器状态接近，
// 比值比绝对值稳；再对轮数取中位，抵掉整段会话期间的漂移。
func ratioMedian(pu, gi *cell) float64 {
	rs := make([]float64, 0, len(pu.runs))
	for i := range pu.runs {
		if gi.runs[i].RPS > 0 {
			rs = append(rs, pu.runs[i].RPS/gi.runs[i].RPS)
		}
	}
	if len(rs) == 0 {
		return 0
	}
	slices.Sort(rs)
	return rs[len(rs)/2]
}

func main() {
	d := flag.Duration("d", 15*time.Second, "每格计时时长")
	warmup := flag.Duration("warmup", 3*time.Second, "每格预热时长（结果丢弃）")
	concFlag := flag.String("c", "64,256", "并发档位，逗号分隔")
	path := flag.String("path", "/users/42", "压测路径")
	passes := flag.Int("passes", 4, "遍历轮数：奇偶轮交换两侧先后。**用偶数**——奇数轮会让某一侧多占一次「先跑位」，而先跑位本身有系统性优势")
	probe := flag.Bool("probe", false, "追加 pulse-web 的诊断档（obs-nolog，拆观测开销；非对拍）")
	out := flag.String("out", "", "把 markdown 表另存为文件（同时打印到 stdout）")
	quick := flag.Bool("quick", false, "缩水口径（1 轮 / 5s / 2s），自检用，数字不要进文档")
	flag.Parse()

	if *quick {
		*d, *warmup, *passes = 5*time.Second, 2*time.Second, 1
	}
	if *passes < 1 {
		fatal(fmt.Errorf("-passes 至少为 1"))
	}
	if *passes%2 != 0 {
		fmt.Fprintf(os.Stderr, "警告: -passes %d 是奇数——两侧占到的「先跑位」次数不等，先跑位本身有系统性优势；写进文档前请用偶数轮重跑\n", *passes)
	}

	root, err := moduleRoot()
	if err != nil {
		fatal(err)
	}
	concs, err := parseConcs(*concFlag)
	if err != nil {
		fatal(err)
	}

	pairs := []pair{
		{modeBare, []string{fwPulse, fwGin}},
		{modeObs, []string{fwPulse, fwGin}},
	}
	if *probe {
		// 只跑 pulse-web 一侧的诊断档：它们回答「钱花在哪」，不回答「谁快」。
		//
		// 选项集只留**今天真实存在的那几个**：默认出口（`web.New()`，即 ConsoleSink），
		// 以及它外面再套一层 `AsyncSink`。历史上还测过「换 `LineSink`」与原型 `fastsink`
		// ——后者是个不会发布的实现、前者的手法已被默认出口吸收，都不该再出现在对比表里。
		//
		// `obs-nolog` 不是出口选项，是分解用的对照：把「访问日志 + 出口」这段单独摘出来。
		pairs = append(pairs,
			pair{modeObsAsync, []string{fwPulse}},
			pair{modeObsNoLog, []string{fwPulse}},
		)
	}

	fmt.Printf("机器: %s/%s · NumCPU=%d · GOMAXPROCS=%d · %s\n",
		runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.Version())
	fmt.Printf("口径: 路径 %s · 预热 %s · 计时 %s · %d 轮（奇偶轮交换两侧先后）\n",
		*path, *warmup, *d, *passes)
	fmt.Println("聚合: 每格取 RPS 中位那一轮的整套分位；两侧比值取逐轮配对比值的中位")
	fmt.Println("服务: 两侧独立进程，共用同一份 server bootstrap（http.ListenAndServe），只换 handler")

	bin, err := buildServer(root)
	if err != nil {
		fatal(fmt.Errorf("build 被测服务失败：%w", err))
	}
	defer func() { _ = os.Remove(bin) }()

	cells := map[key]*cell{}
	started := time.Now()

	for p := range *passes {
		fmt.Printf("\n—— 第 %d/%d 轮 ——\n", p+1, *passes)
		for _, pr := range pairs {
			for _, c := range concs {
				// 同一格内的两侧**相邻**跑（只隔一次起停），奇偶轮交换先后：
				// 整段会话里的漂移因此摊到两边，而不是全压在同一侧身上。
				order := slices.Clone(pr.fws)
				if p%2 == 1 {
					slices.Reverse(order)
				}
				for _, fw := range order {
					t := target{fw, pr.mode}
					res, err := measure(bin, t, c, *path, *warmup, *d)
					if err != nil {
						fatal(err)
					}
					k := key{fw, pr.mode, c}
					cl := cells[k]
					if cl == nil {
						cl = &cell{}
						cells[k] = cl
					}
					cl.runs = append(cl.runs, res)
					fmt.Printf("  %-16s c=%-4d %s\n", t.label(), c, res.String())
				}
			}
		}
	}

	report := render(pairs, concs, cells)
	fmt.Printf("\n总用时 %s\n\n", time.Since(started).Round(time.Second))
	fmt.Println(report)

	if *out != "" {
		if err := os.WriteFile(*out, []byte(report), 0o644); err != nil {
			fatal(err)
		}
		fmt.Println("已写出:", *out)
	}
}

// measure 起服务 → 等就绪 → 压测 → 杀进程，返回这一格的原始结果。
func measure(bin string, t target, conc int, path string, warmup, d time.Duration) (loadgen.Result, error) {
	port, err := freePort()
	if err != nil {
		return loadgen.Result{}, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	srv, err := startServer(bin, t, addr)
	if err != nil {
		return loadgen.Result{}, err
	}
	defer stopServer(srv)

	return loadgen.Run(loadgen.Config{
		URL:         "http://" + addr + path,
		Concurrency: conc,
		Warmup:      warmup,
		Duration:    d,
	}), nil
}

func startServer(bin string, t target, addr string) (*exec.Cmd, error) {
	cmd := exec.Command(bin, "-fw", t.fw, "-mode", t.mode, "-addr", addr)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard // 启动横幅 / 告警不进压测输出

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second}
	url := "http://" + addr + "/healthz"
	deadline := time.Now().Add(10 * time.Second)
	for {
		if resp, err := client.Get(url); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return cmd, nil
			}
		} else if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, fmt.Errorf("%s 未在 10s 内就绪：%v；stderr: %s", t.label(), err, stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func stopServer(cmd *exec.Cmd) {
	_ = cmd.Process.Kill()
	_ = cmd.Wait() // 等它真的死掉：否则上一格的残留进程会和下一格抢 CPU
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func buildServer(root string) (string, error) {
	name := "pulse-web-loadtest-server"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(os.TempDir(), name)

	cmd := exec.Command("go", "build", "-o", bin, "./cmd/server")
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return bin, nil
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil &&
			strings.Contains(string(data), "module "+modulePath) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("没有找到 %s 模块根：请在 loadtest/ 目录下运行 go run ./cmd/compare", modulePath)
		}
		dir = parent
	}
}

func parseConcs(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("并发档位 %q 不是正整数", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("-c 至少要有一个并发档位")
	}
	return out, nil
}

func render(pairs []pair, concs []int, cells map[key]*cell) string {
	var b strings.Builder

	b.WriteString("### 主表：两侧 RPS（中位轮）与逐轮配对比值\n\n")
	b.WriteString("| 档位 | 并发 | pulse-web RPS | gin RPS | pulse/gin | pulse p99 | gin p99 |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, pr := range pairs {
		if len(pr.fws) < 2 {
			continue // 诊断档只有一侧，进明细表
		}
		for _, c := range concs {
			pu, gi := cells[key{fwPulse, pr.mode, c}], cells[key{fwGin, pr.mode, c}]
			wpu, wgi := pu.witness(), gi.witness()
			fmt.Fprintf(&b, "| %s | %d | %.0f | %.0f | %.2fx | %s | %s |\n",
				pr.mode, c, wpu.RPS, wgi.RPS, ratioMedian(pu, gi),
				loadgen.FmtDur(wpu.P99), loadgen.FmtDur(wgi.P99))
		}
	}

	b.WriteString("\n### 明细：每格的全部数字\n\n")
	b.WriteString("| 档位 | 并发 | 框架 | RPS | p50 | p90 | p99 | p999 | 错误 | 轮间极差 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	for _, pr := range pairs {
		for _, c := range concs {
			for _, fw := range pr.fws {
				cl := cells[key{fw, pr.mode, c}]
				r := cl.witness()
				fmt.Fprintf(&b, "| %s | %d | %s | %.0f | %s | %s | %s | %s | %d | %.1f%% |\n",
					pr.mode, c, fw, r.RPS,
					loadgen.FmtDur(r.P50), loadgen.FmtDur(r.P90), loadgen.FmtDur(r.P99), loadgen.FmtDur(r.P999),
					r.Errors, cl.spread())
			}
		}
	}

	// 拆解：诊断档存在时，把 pulse-web 侧的观测开销摆成一条线——从关掉访问日志
	// 到默认出口，再到「默认外面套一层 AsyncSink」，每格跟上相对同并发 bare 的变化。
	var decomposition bool
	for _, pr := range pairs {
		if pr.mode == modeObsNoLog {
			decomposition = true
		}
	}
	if decomposition {
		b.WriteString("\n### 拆解：pulse-web 的观测开销（诊断档，非对拍）\n\n")
		b.WriteString("括号里是相对同并发 `bare` 的变化。**`obs` 是对拍档**，用的是框架默认出口形态\n")
		b.WriteString("（ConsoleSink，给人读的列式单行）；另外两列是「关掉访问日志」与「默认外面套一层 AsyncSink」。\n\n")
		b.WriteString("| 并发 | bare | 关访问日志 | **默认出口 ConsoleSink** | AsyncSink(默认出口) |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, c := range concs {
			bare := cells[key{fwPulse, modeBare, c}].witness().RPS
			fmt.Fprintf(&b, "| %d | %.0f | %s | %s | %s |\n", c, bare,
				withDelta(cells[key{fwPulse, modeObsNoLog, c}].witness().RPS, bare),
				withDelta(cells[key{fwPulse, modeObs, c}].witness().RPS, bare),
				withDelta(cells[key{fwPulse, modeObsAsync, c}].witness().RPS, bare))
		}
	}
	return b.String()
}

// withDelta 把「绝对值 + 相对基线变化」摆进一格，省得读者自己去减。
func withDelta(v, base float64) string {
	if base <= 0 {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.0f (%+.0f%%)", v, (v-base)/base*100)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "compare:", err)
	os.Exit(1)
}
