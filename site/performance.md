# 性能

**先读口径，再看数字。** 性能对比最容易变成「谁的数字好看谁赢」，所以三条纪律摆在最前面。

## 三条口径

1. **ns 跨轮不可比。** 同一格换一次会话，真实负载能从 49k RPS 变到 78k（±20%），微基准跨轮能漂 2–4×。**要相减得出 Δ 的，必须取同一张表里的两行**——本项目踩过一次：拿跨轮的 325 ns 与 227 ns 相减，写出「后者更便宜」，而后者 = 前者 + 一个 Collector 结构，逻辑上不可能。
2. **确定性判据是分配计数，不是耗时。** 下表 B 的 `allocs/op` 与 `B/op` 已经固化成 CI 断言（`bench/budget_test.go`，由 CI 的分配预算步骤执行）；**ns 不设阈值**——拿 ns 做门禁等于把机器状态引进 CI。
3. **绝对值只做量级参考，比的是同轮配对比值。** 三张表口径各不相同，**不要跨表相减**。

## 表 A：pulse v0.2.0 → v0.2.1 升级对照（历史）

这张表回答「**为什么请求路径的成本不随装配规模增长**」——它是本项目「内核成本与插件树解耦」这条结论的原始依据。

口径 `-benchtime=3000x -count=3` 取中位，**跨轮对照**。对照层（stdlib 路由，与任何改动完全无关）自己涨了 9%，说明第二轮机器状态偏慢，所以 ns 降幅是保守估计；**跨运行稳定的硬证据是 alloc 计数**。

| 场景 | v0.2.0 | v0.2.1 | Δ |
|---|---|---|---|
| stdlib ServeMux 路由匹配（**对照层**） | 173.6 ns / 5 allocs | 188.8 ns / 5 allocs | +9% |
| scope 派生 + 销毁 | 119.3 ns / 5 allocs | 74.4 ns / **2** allocs | allocs −60% |
| 请求级事件 `EmitLocal` | 27.5 ns / 2 allocs | 23.2 ns / **1** alloc | allocs −50% |
| 全树事件 `Emit`（50 插件） | 2079 ns / 60 allocs | 1778 ns / **9** allocs | allocs −85% |
| kernel 服务读取 `Get` | 11.83 ns | 13.13 ns | **+11%**（`Get` 先走一遍局部绑定链） |
| 每请求 `AttachCollector`（空树） | 294.8 ns / 15 allocs | 301.8 ns / 14 allocs | 持平 |
| 同上（10 / 50 / 100 插件树） | 778 / 2647 / 4946 ns | **314 / 350 / 394 ns** | **−60% / −87% / −92%** |
| 同上（50 插件，并行） | 1371 ns / 15 allocs | 531 ns / 14 allocs | −61% |
| Engine 请求路径 `New()` | 2008 ns / 26 allocs | 1961 ns / 22 allocs | allocs −15% |
| Engine 请求路径 `Minimal()` | 1382 ns / 20 allocs | 1476 ns / 17 allocs | allocs −15% |

**结论**：v0.2.1 把服务变更从「全树广播」改成「按依赖名索引投递」，每请求 Provide 的成本**与插件树规模彻底解耦**（100 插件 4946 → 394 ns）。请求级数据改用作用域局部绑定（不写全局仓库、不投递变更、随作用域销毁撤除），而不是把全局 Provide 当请求级容器用。

::: warning 本表的绝对值与表 B 不可比
两者口径不同，同一个 benchmark 会差 10–20%。这一张只回答「升级买到了什么」。
:::

复现方式是两个版本各跑一遍：

```bash
git worktree add --detach ../pulse-web-v020 aca2ea2   # go.mod 指 pulse v0.2.0
git worktree add --detach ../pulse-web-v021 9b5e1bb   # go.mod 指 pulse v0.2.1
cd ../pulse-web-v021
go test -run '^$' -bench 'BenchmarkStdlibServeMux_Route|BenchmarkScopeCycle_EmptyHost$|BenchmarkEmitLocal|BenchmarkEmitFullTree|BenchmarkServiceGet|BenchmarkRequestCycle_Collector' -benchtime=3000x -count=3 ./bench/
```

`v0.2.0` 那一版的 bench 包里还没有 Engine 请求路径两档（它们是随 v0.2.1 一起加的），所以表里最后两行是**同一份探针只换 `go.mod` 里的 pulse 版本**测的。

## 表 B：当前版本的成本分解（同轮同口径）

口径 `-benchtime=20000x -count=5`。**凡要相减得出 Δ 的，必须取本表内的两行。**

| 场景 | ns/op | allocs | B/op |
|---|---|---|---|
| `Derive + Dispose`（基线） | 84.3 | **2** | 192 |
| + 裸作用域局部绑定 | 337.2 | 14 | 649 |
| + `observability.AttachCollector` | 353.9 | 14 | 681 |
| 同上 · 10 / 50 / 100 插件树 | 338 / 366 / 407 | 14 | 681 |
| 同上 · 50 插件并行 | 565 | 14 | 681 |
| Engine 请求路径（`New()`） | 1790 | 22 | 6314 |
| Engine 请求路径 + `WithCollector()` | 2175 | **34** | 6803 |
| Engine 请求路径 + `c.JSON`（JSON 响应档） | 2137 | **25** | 6438 |
| Engine 请求路径 + `BodyLimit` 路由（只列分配口径，见下） | — | **23** | 6362 |

由表内两行推出的 Δ：

- **`WithCollector()` 的每请求成本** = 2175 − 1790 = **+385 ns / +12 allocs**（端到端）
- **kernel 层 `AttachCollector` 相对基线** = 353.9 − 84.3 = **+270 ns / +12 allocs**
- **包含关系自检（防倒挂）**：`AttachCollector` = 裸绑定 + 1 个 Collector 结构，实测 ns 与 B/op 都严格更大（353.9 > 337.2、681 B > 649 B）。两者 allocs 同为 14——**alloc 单值区分不了这两者**，判包含关系要看 B/op 与 ns。
- **与插件树规模解耦**：10 / 50 / 100 插件 338 / 366 / 407 ns，allocs 恒为 14。
- **`c.JSON` 先编码到 buffer 的代价**：每响应 **+2 allocs / +97 B**，换来「编码失败不再发 200 空体」。
- **挂了 `BodyLimit` 的路由 +1 alloc**：多出来的就是 `http.MaxBytesReader` 返回的包装器本身（绑在请求上，无法复用）。**该行只列分配口径**——ns 未与本表同轮测得，留空等比塞一个跨轮数字更安全。

复现：

```bash
go test -run '^$' -bench 'BenchmarkScopeCycle|BenchmarkRequestCycle' -benchtime=20000x -count=5 ./bench/
go test -run '^$' -bench 'BenchmarkEngineRequestPath' -benchtime=20000x -count=5 ./bench/
go test -run TestRequestPathAllocBudget ./bench/     # 分配门禁——必须不带 -race
```

分配门禁覆盖表 B 的每一档，包括 `default+json` 与 `default+body-limit`。**门禁零变化只说明「被门禁覆盖的那些路径」没变**，所以新增路径要同时进表与门禁。

## 表 C：与 gin 的真实负载对比（一次性验证）

这张回答「**与 gin 同级吗、观测的钱花在哪**」。采集工程**没有进仓库**——`loadtest/` 是带 gin 依赖的独立 module，保留在分支 `bench/gin-compare`；本页只留口径与结论。

口径先行：

- 两侧**独立进程**，共用同一份 `http.ListenAndServe` bootstrap，**只让 handler 是变量**（不用 `gin.Run()` / `Engine.Run()`，免得把各自的默认 server 配置引进对比）
- 同一格内两侧**相邻**跑，奇偶轮交换先后，**只用偶数轮**（先跑位本身有系统性优势）；每格取 **RPS 中位那一轮**的整套分位；两侧比值取**逐轮配对比值的中位**
- 路径 `GET /users/42` → 两侧同一份 JSON；压测器自写（固定并发、连接全复用、计时窗口内**每个**请求都进分位，不采样）
- 机器：i9-14900HX / 32 逻辑核 / `GOMAXPROCS=32` / Go 1.27 / Windows amd64

两个对拍档：`bare` = `gin.New()` ↔ `web.New(web.Minimal())`；`obs` = `gin.Default()` ↔ `web.New()`。

| 档位 | 并发 | pulse-web RPS | gin RPS | pulse/gin | pulse p99 | gin p99 |
|---|---|---|---|---|---|---|
| bare | 64 | 58562 | 61790 | **0.94x** | 4.63ms | 4.71ms |
| bare | 256 | 64986 | 67915 | **0.97x** | 16.80ms | 20.07ms |
| obs | 64 | 38020 | 59127 | **0.65x** | 7.10ms | 4.98ms |
| obs | 256 | 57152 | 65874 | 0.88x | 20.57ms | 21.14ms |

- **裸档同级**：0.94–0.97×，差 3–6%；并发 256 时 p99 反而更低（16.80ms vs 20.07ms）。
- **绝对值只做量级参考**——同一格换一次会话就能从 49k 变到 78k，所以本表只认同轮配对比值。

::: warning `obs` 档是当时口径
它当时用的是**旧默认出口 `SlogSink`**。这个「默认观测档落后」的结论直接促成了默认出口换成 `ConsoleSink`，**所以这一档数字代表旧默认、不代表现状**；换默认后没有重跑，现状量级看下面微基准那一档。
:::

**观测开销拆解**（诊断档，只跑 pulse-web 一侧——它们回答「钱花在哪」，不回答「谁快」）：

| 并发 | bare | 关访问日志 | 默认 `SlogSink` | 换 `LineSink` | `AsyncSink(LineSink)` | 原型 `fastsink` |
|---|---|---|---|---|---|---|
| 64 | 58562 | 54930 (−6%) | **38020 (−35%)** | 48583 (−17%) | 54727 (−7%) | 50621 (−14%) |
| 256 | 64986 | 63739 (−2%) | **57152 (−12%)** | 60711 (−7%) | 62588 (−4%) | 62309 (−4%) |

- **TraceID 生成 + 记录组装本身几乎免费**：关掉访问日志后只掉 6%（c=64）/ 2%（c=256）。
- **钱几乎全花在「把记录写进出口」这一步**，而且换出口差别巨大：默认 `SlogSink` 掉 35%/12%，`LineSink` 掉 17%/7%，`AsyncSink(LineSink)` 只掉 7%/4%。gin 侧同一档只掉 3%（它的 `Logger` 就是一行文本，没有第二条路可选）。
- **渲染本身能便宜一个量级**：原型 `fastsink` 掉 14%/4%——同一批字段，从默认档的 −35% 收到 −14%。这个结论已经由默认出口 `ConsoleSink` 兑现（渲染约 190 ns / 0 allocs）。

复现：

```bash
git worktree add --detach ../pulse-web-gin-compare origin/bench/gin-compare
cd ../pulse-web-gin-compare/loadtest
go run ./cmd/compare -probe -passes 4 -d 8s -warmup 2s   # 记录进文档的那次
go test -bench . -benchmem ./bench/                      # 成本分解（不是胜负承诺）
```

对照工程是独立 module：核心模块零第三方依赖，**不为「跟 gin 比一次」破例**，gin 只出现在那个 module 里。

## 本页与设计文档的分工

本页是三张表的公开版；**口径、推导与完整说明**的事实源是[设计文档](https://github.com/Luo-root/pulse-web/blob/main/docs/design/web-framework-design.md)的「实测数据」一节。数字更新时两处一起改。
