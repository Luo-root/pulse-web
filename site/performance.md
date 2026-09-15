# 性能

这一页回答两件事：**请求路径值多少**、**观测的钱花在哪**。数字来自本仓库的基准套件，每张表都带复现命令。

**版本**：pulse-web v0.1.0（当前 `main`；依赖 pulse v0.2.4）。打 tag 时这一节随该版本重测一次。

## 怎么读这一页

数字分两类，读法完全不同：

- **确定性的一类**：`allocs/op` 与 `B/op`。它们已经固化成 CI 断言（`bench/budget_test.go`），**跨轮、跨机器都稳定**。
- **浮动的一类**：`ns/op`。同一格换一次会话就能漂 2–4×，所以**只在本轮内可比**，不能跟别的轮次、别的机器比。

为了让「本轮」可辨识，下面每张表都带一条**与框架无关的对照行**（stdlib ServeMux，纯标准库）。对照行变了，说明机器状态变了。

本轮（2026-09-15）实测，这条对照行自己讲了故事：

| 轮次 | 机器状态 | stdlib 对照行 | 同轮 `Engine` 请求路径 | 分配 |
|---|---|---|---|---|
| 2026-09-13（设计文档记录轮） | 插电 | 174 ns | 1790 ns | 22 allocs |
| 2026-09-15（本轮） | **电池**（睿频受限） | **750 ns** | **6592 ns** | **22 allocs** |

同一台机器、同一份代码：**ns 差 4.3 倍，分配计数一模一样**。这就是本页把分配计数当判据、把 ns 只当结构参考的原因。要绝对值就在安静机器上按下面的命令自己跑一轮，并与**同一轮**的对照行比。

## 请求路径的成本分解

口径 `-benchtime=20000x -count=5` 取中位，同轮同口径。**凡要相减得出 Δ 的，必须取本表内的两行。**

| 场景 | ns/op | allocs | B/op |
|---|---|---|---|
| stdlib ServeMux 路由匹配（**对照行**，与框架无关） | 750 | 5 | 224 |
| scope 派生 + 销毁（`Derive + Dispose` 基线） | 213 | **2** | 192 |
| + 裸作用域局部绑定 | 1271 | 14 | 649 |
| + `observability.AttachCollector` | 1197 | 14 | 681 |
| 同上 · 10 / 50 / 100 插件树 | 1301 / 1300 / 1428 | 14 | 681 |
| 同上 · 50 插件并行 | 1888 | 14 | 681 |
| `Engine` 请求路径（`New()`，0 / 10 / 50 插件） | 6592 / 6806 / 7132 | 22 | 6298 |
| `Engine` 请求路径 + `WithCollector()` | 7746 | **34** | 6787 |
| `Engine` 请求路径（`Minimal()`） | 4689 | 17 | 5842 |
| `Engine` 请求路径 + `BodyLimit` 路由 | 6584 | **23** | 6362 |
| `Engine` 请求路径 + `c.JSON` | 见下 | **25** | 6438 |

由表内两行推出的 Δ（同轮，可相减）：

- **`WithCollector()` 的每请求成本** = 7746 − 6592 = **+1154 ns / +12 allocs**（端到端）
- **`BodyLimit` 的每请求成本** = 6584 − 6592 ≈ **0 ns / +1 alloc**——多出来的分配就是 `http.MaxBytesReader` 返回的包装器本身（绑在请求上，无法复用），ns 落在噪声里。
- **装配规模与请求路径无关**：`Engine` 请求路径在 0 / 10 / 50 插件下是 6592 / 6806 / 7132 ns、**allocs 恒为 22、B/op 恒为 6298**；kernel 层 10 / 50 / 100 插件树 **allocs 恒为 14**。插件树大小不改变每请求成本，这是「装配能力」能当卖点的前提。
- **包含关系自检（防倒挂）**：`AttachCollector` = 裸绑定 + 1 个 Collector 结构，判据看 **B/op**（681 > 649）。两者 allocs 同为 14、ns 在本轮落在同一噪声带（1197 vs 1271，顺序反了）——**分配单值与 ns 都区分不了这两者**，这正是「只读结构、不读绝对值」的实例。
- **`c.JSON` 先编码到 buffer**：每响应 **+2 allocs / +97 B**（25 vs 22、6438 vs 6298），换来「编码失败不再发 200 空体」。它的 ns 没有单列——分配口径由门禁覆盖，耗时请自己在同轮里跑对照。

复现：

```bash
# 对照行 + kernel 层
go test -run '^$' -bench 'BenchmarkStdlibServeMux_Route|BenchmarkScopeCycle|BenchmarkRequestCycle' -benchtime=20000x -count=5 ./bench/

# Engine 请求路径（default / collector / minimal / body-limit）
go test -run '^$' -bench 'BenchmarkEngineRequestPath' -benchtime=20000x -count=5 ./bench/

# 分配门禁：把上表的 allocs / B/op 固化成断言（必须不带 -race 跑）
go test -run TestRequestPathAllocBudget ./bench/
```

## 出口（Sink）值多少

观测的钱几乎全花在「把记录送进出口」这一步，而 kernel 的事件派发是全同步的（`Emit` / `EmitLocal` / `Waterfall`，`Parallel` 也等完成）——**出口有多慢，请求路径就有多慢**。

同一个 `Record`、出口都写 `io.Discard`（保留格式化与锁的成本，排除终端/磁盘 I/O）：

| 出口 | ns/op | allocs | B/op |
|---|---|---|---|
| `web.ConsoleSink`（**默认出口**） | 865 | **0** | **0** |
| `observability.LineSink`（上游行式） | 945 | **0** | **0** |
| `observability.SlogSink`（结构化） | 4730 | **18** | 1127 |

端到端（同一条请求路径，只换出口）：默认出口 **8134 ns** vs nop 出口 **7255 ns** = **每请求 +0.9 µs、+0 allocs**。

两点要读出来：默认出口**零分配**（渲染在 `LineSink` 的临界区内单缓冲完成）；而 `SlogSink` 每条要 18 次分配、贵 5 倍——它本来就是**给机器读**的那条路，代价与用途都在这里。

复现：

```bash
go test -run '^$' -bench 'BenchmarkSinkWrite_ConsoleVsUpstream|BenchmarkRequestPath_DefaultSink' -benchmem -count=5 ./bench/
```

出口怎么选见[观测](/guide/observability)；要吞吐就把手慢的出口包进 `observability.NewAsyncSink`。

## 与 gin 的真实负载对比（一次性验证，2026-09-14）

这一节回答「**与 gin 同级吗**」。采集工程（`loadtest/`，带 gin 依赖的独立 module）**没有进仓库**，保留在分支 `bench/gin-compare`；这里只留口径与结论。

口径：两侧独立进程、共用同一份 `http.ListenAndServe` bootstrap、**只让 handler 是变量**；同一格内两侧相邻跑、奇偶轮交换先后、**只用偶数轮**；每格取 RPS 中位那一轮的整套分位，两侧比值取**逐轮配对比值的中位**。压测器自写（固定并发、连接全复用、计时窗口内每个请求都进分位，不采样）。机器：i9-14900HX / 32 逻辑核 / `GOMAXPROCS=32` / Go 1.27 / Windows amd64。

| 档位 | 并发 | pulse-web RPS | gin RPS | pulse/gin | pulse p99 | gin p99 |
|---|---|---|---|---|---|---|
| `bare` | 64 | 58562 | 61790 | **0.94x** | 4.63ms | 4.71ms |
| `bare` | 256 | 64986 | 67915 | **0.97x** | 16.80ms | 20.07ms |

- **裸档同级**：0.94–0.97×，差 3–6%；并发 256 时 p99 反而更低（16.80ms vs 20.07ms）。对拍档是 `gin.New()` ↔ `web.New(web.Minimal())`——两侧都没有默认中间件。
- **绝对值只做量级参考**：同一格换一次会话能从 49k 变到 78k，所以只认同轮配对比值。
- 当时还跑了 `gin.Default()` ↔ `web.New()` 的观测档，但它用的是**旧默认出口**（当时是 `SlogSink`，现已换成 `ConsoleSink`），所以那一档不代表现状，也没有重跑——它留在设计文档里作为「默认出口为什么换」的记录。

复现：

```bash
git worktree add --detach ../pulse-web-gin-compare origin/bench/gin-compare
cd ../pulse-web-gin-compare/loadtest
go run ./cmd/compare -probe -passes 4 -d 8s -warmup 2s   # 记录进文档的那次
```

对照工程是独立 module：核心模块零第三方依赖，**不为「跟 gin 比一次」破例**，gin 只出现在那个 module 里。

## 本页与设计文档的分工

本页给**结论、口径与复现命令**；口径的推导、历史轮次（含 pulse v0.2.0 → v0.2.1 那一轮升级对照）与完整说明的事实源是[设计文档](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md)的「实测数据」一节。数字更新时两处一起改。
