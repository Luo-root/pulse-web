//go:build !race

package bench

import (
	"runtime"
	"testing"

	web "github.com/Luo-root/pulse-web"
)

// 分配预算门禁（设计验收标准『性能回归』条：请求路径开销进仓库 bench、作为基线不劣化）。
//
// # 为什么只卡分配、不卡 ns
//
// ns 受机器状态与轮次影响：同一个 commit 在热机 / 冷机、插电 / 电池下能差 2–4×
// （本项目踩过跨轮相减得出倒挂的教训）。拿 ns 做阈值等于把机器状态引进 CI，
// 门禁会变成随机红。分配计数与 B/op 跨运行确定——review 时曾独立复跑、逐项吻合
// ——正好覆盖「请求路径上多了一次分配 / 分配变大」这类真回归。
//
// # 为什么整份文件排除在 -race 外
//
// 2026-09-16 实测（Go 1.27，windows/amd64，同一 commit；race 侧是探针把本文件的
// build tag 临时翻成 `race` 跑的同一套 harness）：
//
//	档                     !race        race
//	default              22 / 6298   22 / 6326
//	collector            34 / 6787   34 / 6830
//	minimal              17 / 5841   17 / 5861
//	default+console-sink 22 / 6298   22 / 6326
//	default+json         25 / 6437   26 / 6569  ← 连分配计数都多一次
//	default+body-limit   23 / 6362   23 / 6390
//
// 所以排除在 race 构建外不只是「B/op 会被抬高 +20 … +43」：`c.JSON` 档在 race 下
// **分配计数也会多一次**（25 → 26），拿 race 数字当基线等于把检测器自身的开销钉进去。
// 本文件用 `//go:build !race` 排除，由 CI 的独立步骤（不带 -race）执行；race 构建下
// 不重复跑，省一次校准时间。
//
// # 预算值从哪来
//
// 分配计数取自设计文档表 B（口径 `-benchtime=20000x -count=5`）；Minimal 一行是
// 本次实测补的。断言是 `<=`——分配数下降不该让 CI 红，但**若下降请同步刷新常量与
// 表 B**，否则预算会慢慢失真。有意抬高分配时同理：改常量 + 表 B，并在 PR 里说明代价。
//
// B/op 基线**按平台记**（见下方 budgetBytes）。留 `budgetSlackBytes` 余量：实测同一
// commit 重复跑会差 ±1 字节（`plugins=50` 与 `minimal` 都出现过相邻两字节的跳动）。
// 8 字节的余量仍远小于任何结构性变化——多一次分配至少 16 字节起。
//
// B/op 也**不随 N 漂**：`testing.Benchmark` 默认跑满 1 s（本机 N 约 5×10⁵），换成
// `-benchtime=20000x`（N 小约 30 倍）后逐项相同——没有摊到每个 op 上的启动开销，
// 所以 B/op 是可比判据，而与「与表 B 口径不同」无关。
const (
	budgetDefaultAllocs   = 22
	budgetCollectorAllocs = 34
	budgetMinimalAllocs   = 17
	// 默认出口（ConsoleSink）那一档：分配计数与 B/op 都与 nopSink 档**逐项相同**。
	// 出口渲染零分配（渲染器只往 LineSink 自持的缓冲切片里 append，换行与写出都在
	// 上游完成）；`WithImmediate` 下不挂池化缓冲，所以连摊销也没有了。
	// v0.2.4 之前这里比 nopSink 档多 12 字节（6326），那是旧的 `sync.Pool` 每 P
	// 一个缓冲（256 B）在 20000 次请求上的摊销——改成内嵌 LineSink 后归零。
	budgetConsoleSinkAllocs = 22
	// JSON 响应那一档：c.JSON 自 #33 起先编码到 bytes.Buffer、成功才写头，
	// 比「直接编码进响应」多一次分配——两棵树同一探针实测 main 23 → 本分支 25
	// allocs/op、B/op 6341 → 6437（+2 allocs / +96 B），代价换「编码失败不再发
	// 200 空体」。其余各档 handler 都用 c.Text，不经过这条路径，所以单列一档把
	// 它纳入基线（review 提出，PR #35）。
	budgetJSONAllocs = 25
	// 路由挂了 BodyLimit 那一档：比 default 档多一次分配，就是
	// `http.MaxBytesReader` 返回的包装器本身（每请求一个，无法复用——它绑在请求上）。
	// 这笔账起初没进描述、门禁也覆盖不到（其余档都不挂 BodyLimit），现已单列一档
	// 纳入基线（review 提出，PR #45）。B/op 不断言：+1 alloc 的字节数随
	// maxBytesReader 结构大小走，卡它只会带来假红。
	budgetBodyLimitAllocs = 23
	budgetSlackBytes      = 8
)

// budgetBytesPerCase 是五个断言档的 B/op 基线。
type budgetBytesPerCase struct {
	def         int
	collector   int
	minimal     int
	consoleSink int
	json        int
}

// B/op 基线**按平台记**。
//
// 分配计数是跨平台确定值——同一 commit、同一工具链（go1.27.0），windows/amd64 与
// linux/amd64 实测逐项相同（22 / 34 / 17 / 22 / 25）。B/op 不是：
//
//	档                     windows/amd64   linux/amd64   差
//	default                        6298          6289    −9
//	collector                      6787          6777   −10
//	minimal                        5841          5833    −8
//	default+console-sink           6298          6289    −9
//	default+json                   6437          6418   −19
//
// windows 值 = 本机（表 B 口径与门禁默认 1 s 两口径逐项相同）；linux 值 = CI run
// 35067192791 的 `Alloc budget` 步骤日志（ubuntu-latest / go1.27.0）。
//
// 拿单一常量卡两头会二选一地失灵：按 windows 定，ubuntu 上会多出 9 字节死余量，
// 「多一次 16 字节分配」正好从缝里溜过；按 linux 定，本机跑本地门禁直接假红
// （6298 > 6289 + 8）。各记一份，两边都保住 8 字节余量的灵敏度。
//
// 平台差的**成因尚未定位**（分配计数相同、只有字节不同，且 json 档的差比其他档大一倍）
// ——定位它是独立的一件事。在定位之前不要把它当余量，也不要为此放宽 slack。
var budgetBytes = map[string]budgetBytesPerCase{
	"windows/amd64": {6298, 6787, 5841, 6298, 6437},
	"linux/amd64":   {6289, 6777, 5833, 6289, 6418},
}

// platformBudgetBytes 取本平台的 B/op 基线。
//
// 未实测的平台退回**已知平台里最宽的那一组**：宁可松，也不要让门禁在新平台上按一个
// 偏低的基线假红。known=false 时测试会打日志提示把本平台实测值补进来；本地想看到
// 自己测到的值，跑 `go test -run TestRequestPathAllocBudget ./bench/ -v`。
func platformBudgetBytes() (b budgetBytesPerCase, known bool) {
	if v, ok := budgetBytes[runtime.GOOS+"/"+runtime.GOARCH]; ok {
		return v, true
	}
	for _, v := range budgetBytes {
		if v.def > b.def {
			b = v
		}
	}
	return b, false
}

// TestRequestPathAllocBudget 把表 B 的分配计数固化成断言。
//
// 与 benchmark 共用同一套 harness（enginePathApp* + runRequestPath），所以预算数字
// 与 `-bench` 报出来的是同一个口径，不会出现「门禁测的和文档写的不是一条路」。
func TestRequestPathAllocBudget(t *testing.T) {
	bb, known := platformBudgetBytes()
	if !known {
		t.Logf("平台 %s/%s 没有实测的 B/op 基线，退回已知平台里最宽的一组"+
			"（default 档 %d B/op）。首次在此平台运行请把实测值补进 budgetBytes，"+
			"并同步设计文档表 B", runtime.GOOS, runtime.GOARCH, bb.def)
	}

	cases := []struct {
		name   string
		setup  func(tb testing.TB) (*web.Engine, func())
		allocs int64
		bytes  int64 // 0 = 不断言
	}{
		{"default", func(tb testing.TB) (*web.Engine, func()) { return enginePathApp(tb, 0) },
			budgetDefaultAllocs, int64(bb.def)},
		// 解耦对照：插件树规模不改变分配计数（红线：默认路径零全局 Provide）。
		// B/op 不断言——实测 50 插件比空树多 1 字节（本机 6298 → 6299），常量级差异，
		// 卡它只会带来假红。
		{"plugins=50", func(tb testing.TB) (*web.Engine, func()) { return enginePathApp(tb, 50) },
			budgetDefaultAllocs, 0},
		{"collector", enginePathAppCollector, budgetCollectorAllocs, int64(bb.collector)},
		{"minimal", enginePathAppMinimal, budgetMinimalAllocs, int64(bb.minimal)},
		// 默认出口那一档：上面的用例都用 nopSink（把「框架请求路径」与「出口
		// 成本」分开量），这一档把默认装配实际用的 ConsoleSink 接回来——出口
		// 是 0 分配，所以它与 nopSink 档的分配计数应当相同，差在这里就是回归。
		{"default+console-sink", enginePathAppConsoleSink, budgetConsoleSinkAllocs, int64(bb.consoleSink)},
		// JSON 响应那一档：把「先编码到 buffer、成功才写头」这条路径也钉住。
		{"default+json", enginePathAppJSON, budgetJSONAllocs, int64(bb.json)},
		// 路由级闸门那一档：挂 BodyLimit 的路由每请求多一次分配（MaxBytesReader
		// 包装器），把这条路径也钉住。
		{"default+body-limit", enginePathAppBodyLimit, budgetBodyLimitAllocs, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := testing.Benchmark(func(b *testing.B) {
				app, cleanup := c.setup(b)
				defer cleanup()
				runRequestPath(b, app)
			})

			if got := res.AllocsPerOp(); got > c.allocs {
				t.Errorf("分配计数劣化：%d > 预算 %d allocs/op"+
					"（若为有意变更，请同步更新本文件的常量与设计文档表 B）", got, c.allocs)
			}
			if c.bytes > 0 {
				if got := res.AllocedBytesPerOp(); got > c.bytes+budgetSlackBytes {
					t.Errorf("分配字节劣化：%d > 预算 %d B/op（含 %d 字节抖动余量；平台 %s/%s）"+
						"（若为有意变更，请同步更新本文件的常量与设计文档表 B）",
						got, c.bytes, budgetSlackBytes, runtime.GOOS, runtime.GOARCH)
				}
			}
			t.Logf("allocs/op=%d B/op=%d（预算 %d / %d，平台 %s/%s）",
				res.AllocsPerOp(), res.AllocedBytesPerOp(), c.allocs, c.bytes,
				runtime.GOOS, runtime.GOARCH)
		})
	}
}
