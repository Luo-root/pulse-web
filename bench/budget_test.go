//go:build !race

package bench

import (
	"testing"

	web "github.com/Luo-root/pulse-web"
)

// 分配预算门禁（设计验收标准第 7 条：请求路径开销进仓库 bench、作为基线不劣化）。
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
// 实测（Go 1.27）：race 检测器**不改分配计数**（22 / 34 / 17 两个模式下完全一致），
// 但会把 B/op 抬高（默认路径 6314 → 6342、collector 6803 → 6846）。所以用
// `//go:build !race` 把它排除在 race 构建外，由 CI 的独立步骤（不带 -race）执行；
// race 构建下不重复跑，省一次校准时间。
//
// # 预算值从哪来
//
// 设计文档表 B（口径 `-benchtime=20000x -count=5`）；Minimal 一行是本次实测补的。
// 断言是 `<=`——分配数下降不该让 CI 红，但**若下降请同步刷新常量与表 B**，
// 否则预算会慢慢失真。有意抬高分配时同理：改常量 + 表 B，并在 PR 里说明代价。
//
// B/op 留 `budgetSlackBytes` 余量：实测同一 commit 重复跑会差 ±1 字节
// （`plugins=50` 在 6314 / 6315 之间跳、minimal 出现过 5858 与 5857）。
// 8 字节的余量仍远小于任何结构性变化——多一次分配至少 16 字节起。
const (
	budgetDefaultAllocs   = 22
	budgetDefaultBytes    = 6314
	budgetCollectorAllocs = 34
	budgetCollectorBytes  = 6803
	budgetMinimalAllocs   = 17
	budgetMinimalBytes    = 5858
	// 默认出口（ConsoleSink）那一档：分配计数与 B/op 都与 nopSink 档**逐项相同**。
	// 出口渲染零分配（渲染器只往 LineSink 自持的缓冲切片里 append，换行与写出都在
	// 上游完成）；`WithImmediate` 下不挂池化缓冲，所以连摊销也没有了。
	// v0.2.4 之前这里比 nopSink 档多 12 字节（6326），那是旧的 `sync.Pool` 每 P
	// 一个缓冲（256 B）在 20000 次请求上的摊销——改成内嵌 LineSink 后归零。
	budgetConsoleSinkAllocs = 22
	budgetConsoleSinkBytes  = 6314
	// JSON 响应那一档：c.JSON 自 #33 起先编码到 bytes.Buffer、成功才写头，
	// 比「直接编码进响应」多一次分配——两棵树同一探针实测 main 23 → 本分支 25
	// allocs/op、B/op 6341 → 6438（+2 allocs / +97 B），代价换「编码失败不再发
	// 200 空体」。其余各档 handler 都用 c.Text，不经过这条路径，所以单列一档把
	// 它纳入基线（review 提出，PR #35）。
	budgetJSONAllocs = 25
	budgetJSONBytes  = 6438
	budgetSlackBytes = 8
)

// TestRequestPathAllocBudget 把表 B 的分配计数固化成断言。
//
// 与 benchmark 共用同一套 harness（enginePathApp* + runRequestPath），所以预算数字
// 与 `-bench` 报出来的是同一个口径，不会出现「门禁测的和文档写的不是一条路」。
func TestRequestPathAllocBudget(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(tb testing.TB) (*web.Engine, func())
		allocs int64
		bytes  int64 // 0 = 不断言
	}{
		{"default", func(tb testing.TB) (*web.Engine, func()) { return enginePathApp(tb, 0) },
			budgetDefaultAllocs, budgetDefaultBytes},
		// 解耦对照：插件树规模不改变分配计数（红线：默认路径零全局 Provide）。
		// B/op 不断言——实测 50 插件比空树多 1 字节（6314 → 6315），常量级差异，
		// 卡它只会带来假红。
		{"plugins=50", func(tb testing.TB) (*web.Engine, func()) { return enginePathApp(tb, 50) },
			budgetDefaultAllocs, 0},
		{"collector", enginePathAppCollector, budgetCollectorAllocs, budgetCollectorBytes},
		{"minimal", enginePathAppMinimal, budgetMinimalAllocs, budgetMinimalBytes},
		// 默认出口那一档：上面的用例都用 nopSink（把「框架请求路径」与「出口
		// 成本」分开量），这一档把默认装配实际用的 ConsoleSink 接回来——出口
		// 是 0 分配，所以它与 nopSink 档的分配计数应当相同，差在这里就是回归。
		{"default+console-sink", enginePathAppConsoleSink, budgetConsoleSinkAllocs, budgetConsoleSinkBytes},
		// JSON 响应那一档：把「先编码到 buffer、成功才写头」这条路径也钉住。
		{"default+json", enginePathAppJSON, budgetJSONAllocs, budgetJSONBytes},
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
					t.Errorf("分配字节劣化：%d > 预算 %d B/op（含 %d 字节抖动余量）"+
						"（若为有意变更，请同步更新本文件的常量与设计文档表 B）",
						got, c.bytes, budgetSlackBytes)
				}
			}
			t.Logf("allocs/op=%d B/op=%d（预算 %d / %d）",
				res.AllocsPerOp(), res.AllocedBytesPerOp(), c.allocs, c.bytes)
		})
	}
}
