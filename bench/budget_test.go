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
//	default              22 / 6306   22 / 6334
//	collector            34 / 6793   34 / 6837
//	minimal              17 / 5849   17 / 5868
//	default+console-sink 22 / 6305   22 / 6333
//	default+json         25 / 6435   26 / 6565  ← 连分配计数都多一次
//	default+body-limit   23 / 6369   23 / 6397
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
// B/op 是一份**单份**基线（见 budgetBytes）。留 `budgetSlackBytes` 余量：实测同一
// commit 重复跑会差 ±1 字节（`plugins=50` 与 `minimal` 都出现过相邻两字节的跳动）。
// 8 字节的余量仍远小于任何结构性变化——多一次分配至少 16 字节起。
//
// # 为什么迭代数必须钉死
//
// B/op 里有一项按**每次 benchmark 调用**摊开的常数（拟合约 10 KB / N），所以它随 N 漂。
// 实测（同一份二进制、同机 i9-14900HX，windows/amd64 与 linux/amd64 都验过）：
//
//	-benchtime=2000x     6316 B/op
//	-benchtime=20000x    6314 B/op
//	-benchtime=400000x   6314 B/op
//	默认 1 s 窗口        6314 B/op   ← 这一档的 N（本机 60 万级）与 400000x 同量级
//
// 也就是说：**跨环境差的不是平台，是 N 与 P**。「按 GOOS/GOARCH 记两份基线」是把环境差
// 误认成平台差——同一个二进制在 windows 与 linux 上只要 N 固定就逐项相同，而 1 s
// 窗口下两边都能给出不同的数，取决于当时落到的 N 与 P（#71 的定位结论）。
// 把 N 钉死后基线收敛成一份，8 字节 slack 在每个环境都保住灵敏度。
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
//
// **单份，不按平台分。** 曾经的模型是「windows/amd64 比 linux/amd64 高 8–19 字节」，
// 并为此按平台记了两份；#71 用交叉编译出来的 linux/amd64 二进制在本机 WSL 复现后
// 证伪了它：
//
//	跑法                                default  minimal  json
//	WSL linux/amd64，固定 N=20000x        6298     5841    —
//	Windows amd64，固定 N=20000x          6298     5841   6437
//	默认 1 s 窗口（两平台都可能落在）      6289     5833   6418
//
// 上表是 #71 那一轮（span 缝之前）的数；#76 之后每档 +16，当前值见 budgetBytes。
//
// 换成反过来的说法：会变的是 **N**，不是 OS（机制见文件头）。门禁自己把 N 钉在
// fixedIterations 上，所以这里只需要一份基线。
type budgetBytesPerCase struct {
	def         int
	collector   int
	minimal     int
	consoleSink int
	json        int
}

// fixedIterations 是门禁量分配时用的固定迭代数，与设计文档表 B 的口径
// （`-benchtime=20000x -count=5`）同量级——门禁断的与文档写的因此是同一档 N。
//
// 取 20000：足够让「按每次调用摊开的那约 10 KB」摊到 0.5 B/op 以下（不影响 8 字节
// 余量的判断），又不至于让 CI 的这一步跑成分钟级。
const fixedIterations = 20000

// fixedProcs 是门禁量分配时钉住的 GOMAXPROCS。
//
// B/op 的第二个自变量是 P 数，杠杆比 N 大（同一份二进制、同一台机器、固定
// N=20000x；#76 之后重测：2 P → 6304、4 P → 6305、8 P → 6306、32 P → 6314，
// 即 2 P 换到 32 P 差 10 字节，而 N 在 2000x…400000x 之间只差 2 字节）。同一个 P 下
// windows/amd64 与 linux/amd64 逐项相同（#71 轮 4 P：两边都是
// 6290 / 6777 / 5833 / 6289 / 6418 / 6353）——所以「平台差」从来不存在，存在的是
// 「N 差 + P 差」，而这两样都能钉。
//
// 取 4：与 CI runner（ubuntu-latest 4 vCPU）同档，于是本地与 CI 报同一个数。
const fixedProcs = 4

// 2026-09-16（#76，span 缝）：五个档位 B/op 一律 +16 —— `Ctx` 多了一个存 span
// 身份的指针字段，结构体从 96 涨到 112 字节（尺寸类跳档），于是每请求的 Ctx
// 分配多 16 字节，与档位无关。这是**有意**的取舍，不是漂移：
//
//   - 分配**计数**逐档不变（指针为 nil 时不产生分配）——门禁的第一判据没动；
//   - 换来的是「span 数据随请求走」的单一存放点，而不是把身份拆到 context 值里
//     （那会在 404 这类没走到注册处理器的路径上丢数据，见 context.go 注释）；
//   - 不装 WithSpanHook 的宿主同样付这 16 字节（Ctx 定长）——已同步进设计文档表 B。
var budgetBytes = budgetBytesPerCase{
	def:         6306,
	collector:   6793,
	minimal:     5849,
	consoleSink: 6305,
	json:        6435,
}

// measureFixed 在固定迭代数下量每个 op 的分配。
//
// 与 `testing.B` 同一套口径（前后两次 `runtime.MemStats` 的差除以 N），只是把 N
// 钉死——`testing.Benchmark` 的 N 由 `-benchtime` 决定，默认 1 s 窗口会随机器快慢变，
// 那正是这份门禁以前报出两份「平台基线」的原因（见文件头）。
func measureFixed(t *testing.T, iterations int, app *web.Engine) (allocsPerOp, bytesPerOp int64) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	iterateRequestPath(app, iterations)
	runtime.ReadMemStats(&after)
	n := uint64(iterations)
	return int64((after.Mallocs - before.Mallocs) / n),
		int64((after.TotalAlloc - before.TotalAlloc) / n)
}

// TestRequestPathAllocBudget 把表 B 的分配计数固化成断言。
//
// 与 benchmark 共用**同一个循环体**（`iterateRequestPath`），所以门禁测的与文档写的是
// 同一条路；但**迭代数由本文件钉死**（`fixedIterations`），不受 `-benchtime` 影响——
// 那正是这份门禁以前报出两份「平台基线」的原因（见文件头）。
func TestRequestPathAllocBudget(t *testing.T) {
	prev := runtime.GOMAXPROCS(fixedProcs)
	defer runtime.GOMAXPROCS(prev)

	cases := []struct {
		name   string
		setup  func(tb testing.TB) (*web.Engine, func())
		allocs int64
		bytes  int64 // 0 = 不断言
	}{
		{"default", func(tb testing.TB) (*web.Engine, func()) { return enginePathApp(tb, 0) },
			budgetDefaultAllocs, int64(budgetBytes.def)},
		// 解耦对照：插件树规模不改变分配计数（红线：默认路径零全局 Provide）。
		// B/op 不断言——实测 50 插件比空树多 1 字节（本机 6314 → 6315），常量级差异，
		// 卡它只会带来假红。
		{"plugins=50", func(tb testing.TB) (*web.Engine, func()) { return enginePathApp(tb, 50) },
			budgetDefaultAllocs, 0},
		{"collector", enginePathAppCollector, budgetCollectorAllocs, int64(budgetBytes.collector)},
		{"minimal", enginePathAppMinimal, budgetMinimalAllocs, int64(budgetBytes.minimal)},
		// 默认出口那一档：上面的用例都用 nopSink（把「框架请求路径」与「出口
		// 成本」分开量），这一档把默认装配实际用的 ConsoleSink 接回来——出口
		// 是 0 分配，所以它与 nopSink 档的分配计数应当相同，差在这里就是回归。
		{"default+console-sink", enginePathAppConsoleSink, budgetConsoleSinkAllocs, int64(budgetBytes.consoleSink)},
		// JSON 响应那一档：把「先编码到 buffer、成功才写头」这条路径也钉住。
		{"default+json", enginePathAppJSON, budgetJSONAllocs, int64(budgetBytes.json)},
		// 路由级闸门那一档：挂 BodyLimit 的路由每请求多一次分配（MaxBytesReader
		// 包装器），把这条路径也钉住。
		{"default+body-limit", enginePathAppBodyLimit, budgetBodyLimitAllocs, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, cleanup := c.setup(t)
			defer cleanup()

			allocs, bytes := measureFixed(t, fixedIterations, app)

			if allocs > c.allocs {
				t.Errorf("分配计数劣化：%d > 预算 %d allocs/op"+
					"（若为有意变更，请同步更新本文件的常量与设计文档表 B）", allocs, c.allocs)
			}
			if c.bytes > 0 && bytes > c.bytes+budgetSlackBytes {
				t.Errorf("分配字节劣化：%d > 预算 %d B/op（含 %d 字节抖动余量，N=%d）"+
					"（若为有意变更，请同步更新本文件的常量与设计文档表 B）",
					bytes, c.bytes, budgetSlackBytes, fixedIterations)
			}
			t.Logf("allocs/op=%d B/op=%d（预算 %d / %d，N=%d）",
				allocs, bytes, c.allocs, c.bytes, fixedIterations)
		})
	}
}
