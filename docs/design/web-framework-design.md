# feat: pulse-web 框架设计——kernel 装配内核 + HTTP 语义层（设计票）

> 完整设计文档（v1）。设计票与评审记录见 [Issue #1](https://github.com/Luo-root/pulse-web/issues/1)。
> 包名 `web`，模块 `github.com/Luo-root/pulse-web`——不与 pulse 根包（`package pulse`）冲突。
>
> **引用口径**：本文中 `包/文件.go:NNN` 形式的行号引用针对 **pulse v0.2.4**（#30 已逐条复核：v0.2.2 → v0.2.4 之间，下述四处引用所在文件未变动，行号与符号位置逐条一致）。上游一个 patch 版本就可能让行号整体位移（v0.2.1 → v0.2.2 就让 `observability/collector.go` 位移了 11 行），所以**升依赖时必须整体复核**——用 `git grep -n '\.go:[0-9]'` 取出全部引用，逐个按符号在新版本里重新定位，不要只看 diff 触及的那几条。

## 一句话

用 pulse 的 **kernel** 做装配内核（IoC / 可逆生命周期 / 请求作用域 / 事件），用 **observability** 做一等观测，构建一个与 gin / chi / echo 同级形态的**通用 Go web 服务框架**。卖点是装配能力 + 一等观测，不是极致性能。

## 做什么（v1）

1. **Engine**——路由注册、分组、中间件（闭包组合，洋葱模型）
2. **Ctx**——路径参数（读 `Request.PathValue`，不另存）、查询、请求级 KV、响应写出（JSON / Text / Status / Header / Writer / Flush）
3. **错误模型**——`HTTPError` + `StatusCoder` 接口 + 默认 mapper（脱敏）
4. **请求体绑定**——`Ctx.Bind` 按 Content-Type 分派（JSON / XML / form-urlencoded / multipart，全 stdlib 实现），`BindQuery` 显式 query；可选 `WithMaxBodyBytes` 全局上限（见 [#32](https://github.com/Luo-root/pulse-web/issues/32)）；`BodyLimit(n)` 中间件按路由 / 分组收紧（见 [#38](https://github.com/Luo-root/pulse-web/issues/38)）
5. **请求作用域**——每请求派生 kernel scope，handler 返回即 `Dispose`（在写响应之前）
6. **一等观测**——装配期 `Bootstrap` + 请求期 `Trace` + `AccessLog`；32hex TraceID；`X-Trace-Id` 回写；可选 **span 出口**（`WithSpanHook`，把请求接进 W3C / OTel 追踪体系，见 [#76](https://github.com/Luo-root/pulse-web/issues/76)）
7. **进程级装配面**——`app.Root()` / `web.WithRoot(k)`；其余用 kernel 原生 API（`Provide` / `Use` / `Loader`）
8. **生命周期**——`Run`（信号 → `Server.Shutdown` → `OnShutdown` → `root.Dispose` → Sink flush）、`Serve`、`Handler`
9. **stdlib 互操作**——`Wrap(http.Handler) Handler`，`Handler()` 反向导出
10. **静态文件**——`Static(prefix, dir)`，**静态资源同样经过全局与分组中间件**（与普通路由共用注册路径）
11. **流式响应**——`c.Writer()` 直接写字节 + `c.Flush()` 逐段推送（SSE / chunked / 大文件），仍是一条 AccessLog
12. **HTML 模板**——`c.HTML()`，薄封装 stdlib `html/template`（生产缓存 / 开发热重载）

## 不做什么（v1 明确排除）

前端界面 / ORM / 策略中间件（认证 / 限流 / 熔断）/ 微服务治理 / 第三方路由库 / `HoldScope` / `RunTLS` / `Recover()` 中间件 / 内部 Router 抽象 / TTFB / 流式双记录 / **框架自己编造 span-id**。

**后续计划：WebSocket**——stdlib 没有 WebSocket 实现（`net/http` 只提供 Hijack / Upgrade 机制，帧协议需自实现），引入它必然带第三方依赖（社区域主流是 `coder/websocket`）。因此作为**可选子包**（`pulse-web/ws`）后续加入，核心保持零依赖。

另有一批"评审中被判定为非 v1"的项，列入文末「明确不做」清单（含各自去路）。

## 为什么

**生态位**：Go 缺一个"装配 + Web"一体框架——`wire` 编译期 DI 无运行时生命周期；`fx` 有 DI 无 HTTP；`kratos` / `go-zero` 是微服务全家桶、偏重。pulse-web 的位置是**带 IoC 生命周期与一等观测的服务基座**，目标场景是装配复杂度高的服务（多组件、需要热更新、需要统一观测），而不是极致轻量的 API 网关。

**为什么内核是 kernel**：kernel 的 scope 树 + Effect LIFO 天生就是"请求作用域"容器——Spring 的 request scope bean、Go 的 `context.Value` + `defer` 清理，kernel 用一个机制统一了，且更严格（销毁必回收）。observability 也早已按"每请求 TraceID"设计。

**借鉴 Spring Boot，不照抄**（理念可借鉴，Go 化必须换形态）：

| Java / Spring | Go / pulse-web | 原因 |
|---|---|---|
| 注解扫描 + 反射注入 | 泛型服务键 + 显式装配 | 依赖关系编译期可见 |
| 异常机制 | `error` 返回值 + 集中映射 | Go 错误是一等返回值 |
| ThreadLocal 请求上下文 | kernel scope 树 | Go 并发不是线程绑定 |
| 代码级热加载 | **不做**，Loader 是状态级重载 | Go 无法卸载已加载代码 |
| classpath 扫描 | `main` 显式装配 + 可选 YAML 条目 | 启动确定性优先 |

## 关键决策（已定）

| 决策 | 结论 | 依据 |
|---|---|---|
| 定位 | 通用 web 框架（库形态），非 agent 专用、不含前端 | 项目所有者拍板 |
| 包名 / 模块 | `package web` / `github.com/Luo-root/pulse-web` | 与 pulse 根包 `package pulse` 不冲突 |
| Go 版本 | `go 1.27.0`（pulse 自身保持 1.25，不冲突） | 实测 stdlib 路由 −22%；pulse 22 包测试在 1.27 全绿 |
| API 风格 | echo 式：`func(*Ctx) error` + 集中错误映射 + stdlib 双向兼容 | 与 pulse「不静默吞错」同构、DI 类型安全、可测 |
| 路由 | stdlib `ServeMux`（Go 1.22+ 模式） | 零依赖、190ns/匹配；**不抽 router 接口**（YAGNI，要换时再抽） |
| HTTP 中间件 | 闭包组合 | **不用** kernel `Waterfall`——那是事件 around 链，不是 HTTP 洋葱 |
| kernel 作用 | 每请求派生 scope（实测 84 ns / 2 allocs，表 B）；**请求级数据不占全局仓库** | v0.2.1 `#169` 后服务变更按依赖名索引投递（不再 O(插件树)）；请求级值走 `kernel.Local()` 作用域局部绑定 |
| 依赖方向 | `pulse-web → pulse` 单向 | pulse 永不反向引用 |

### 路由选型的边界（实测，Go 1.27）

| 能力 | 结果 |
|---|---|
| 路径参数 `{id}` / 通配后缀 `{rest...}` | 支持（后者可匹配空段） |
| 方法限定（`GET /m` vs `/m`） | 支持，方法限定优先 |
| 冲突注册（`/a/{x}` 与 `/a/{y}`） | **注册期 panic**——与 pulse「装配期暴露错误」一致 |
| 尾斜杠 | `/dir` 命中 `/dir/{rest...}` 时 **307**；文档明示 |
| 正则约束 `{id:[0-9]+}` | **不支持**（解析 panic）——用应用层校验替代 |
| 分组 / 中间件继承 | ServeMux 没有，但那是**框架层语法糖**（注册期编译进 handler） |

**分组与顺序**（洋葱模型）：

```go
g1 := app.Group("/api", mw1)
g2 := g1.Group("/v1", mw2)
g2.GET("/users", h, mw3)
// 执行：mw1 → mw2 → mw3 → h → mw3 → mw2 → mw1
```

中间件在注册期闭包组合（运行期零额外开销）；冲突 panic 包装为带分组上下文的信息（`g1(/api) → g2(/v1)`）。

## 实测数据（i9-14900HX / 32 逻辑核 / Go 1.27 / Windows amd64）

每张表自带**轮次与口径**：ns 只在该表内可比（跨轮能漂 2–4×，甚至方向相反），分配计数与 B/op 跨运行确定。

### 表 A：v0.2.0 → v0.2.1 升级对照（**历史，两轮各跑**）

口径 `-benchtime=3000x -count=3` 取中位。**这是跨轮对照**——对照层（stdlib 路由，与任何改动完全无关）自己涨了 9%，说明第二轮机器状态偏慢，所以 ns 降幅是保守估计；**跨运行稳定的硬证据是 alloc 计数**。

> 本表的绝对值与表 B **不可比**（口径不同，同一个 benchmark 会差 10~20%）。它只回答「升级买到了什么」。

| 场景 | v0.2.0 | v0.2.1 | Δ |
|---|---|---|---|
| stdlib ServeMux 路由匹配（**对照层**） | 173.6 ns / 5 allocs | 188.8 ns / 5 allocs | +9% |
| scope 派生 + 销毁 | 119.3 ns / 5 allocs | 74.4 ns / **2** allocs | allocs −60% |
| 请求级事件 `EmitLocal` | 27.5 ns / 2 allocs | 23.2 ns / **1** alloc | allocs −50% |
| 全树事件 `Emit`（50 插件） | 2079 ns / 60 allocs | 1778 ns / **9** allocs | allocs −85% |
| kernel 服务读取 `Get` | 11.83 ns | 13.13 ns | **+11%**（`Get` 现在先走一遍局部绑定链） |
| 每请求 `AttachCollector`（空树） | 294.8 ns / 15 allocs | 301.8 ns / 14 allocs | 持平 |
| 同上（10 / 50 / 100 插件树） | 778 / 2647 / 4946 ns | **314 / 350 / 394 ns** | **−60% / −87% / −92%** |
| 同上（50 插件，并行） | 1371 ns / 15 allocs | 531 ns / 14 allocs | −61% |
| Engine 请求路径 `New()` | 2008 ns / 26 allocs | 1961 ns / 22 allocs | allocs −15% |
| Engine 请求路径 `Minimal()` | 1382 ns / 20 allocs | 1476 ns / 17 allocs | allocs −15% |

**结论**：v0.2.1（`#169`）把服务变更从「全树广播」改成「按依赖名索引投递」，每请求 Provide 的成本**与插件树规模彻底解耦**（100 插件 4946 → 394 ns）。这正是红线 2 的原始依据——依据消失，红线按新语义重写。请求级数据改用 `kernel.Local()`（作用域局部绑定：不写全局仓库、不投递变更、随作用域销毁撤除），而不是把全局 `Provide` 当请求级容器。

### 表 B：当前版本的成本分解（**同轮同口径**，2026-09-16，插电，`-benchtime=20000x -count=5`）

> **2026-09-17 · [#76](https://github.com/Luo-root/pulse-web/issues/76) 的 +16 字节**：`Ctx` 多了一个存 span 身份的字段（96 → 112 字节，跨尺寸类），于是**凡请求路径上带 `Ctx` 的档，B/op 一律 +16**——下表 B/op 列已按这条规则更新（6298 → **6314**、6299 → **6315**、6787 → **6803**、5841 → **5858**、6362 → **6379**、6437 → **6453**），**分配计数逐档不变**（字段为 nil 不产生分配；与档位、插件树规模都无关）。本文档其他位置写作这些数的行同样适用这条规则。代价是有意的：换来「span 数据随请求走」的单一存放点，而不是把身份拆进 context 值（那会在 404 这类没走到注册 handler 的路径上丢数据，见 `context.go`）。复采口径同上表头（`-benchtime=20000x -count=5`，32 P）。
>
> **ns 列没有跟着重采，仍是 2026-09-16 那一轮。** 本轮复采时 `LocalBinding` 与 `AttachCollector` 这对**有包含关系**的行出现了倒挂（中位数 376.1 vs 346.7，n=10；上一轮 326.7 vs 341.9，方向相反）——后者 = 前者 + 1 个 Collector 结构，倒挂只能说明这台机器这一轮的 ns 不可跨行比。按本表规矩（ns 只与同轮对照行比）不发布新 ns 列；B/op 与 allocs 是确定性判据，不受影响。

**凡要相减得出 Δ 的，必须取本表内的两行。** 跨轮相减会得到倒挂的结论——本项目踩过一次：拿跨轮的「裸 `Local` 绑定」（325 ns）与「`AttachCollector`」（227 ns）相减，写出「后者更便宜」，而后者 = 前者 + 1 个 Collector 结构，逻辑上不可能。

| 场景 | ns/op | allocs | B/op |
|---|---|---|---|
| stdlib ServeMux 路由匹配（**对照行**，与框架无关） | 198 | 5 | 224 |
| `Derive + Dispose`（基线） | 80.9 | **2** | 192 |
| + 裸 `kernel.Local()` 绑定 | 326.7 | 14 | 649 |
| + `observability.AttachCollector` | 341.9 | 14 | 681 |
| 同上 · 10 / 50 / 100 插件树 | 345 / 361 / 370 | 14 | 681 |
| 同上 · 50 插件并行 | 566 | 14 | 681 |
| Engine 请求路径（`New()`，0 / 10 / 50 插件） | 1774 / 1819 / 1937 | 22 | 6314 |
| Engine 请求路径 + `WithCollector()` | 2123 | **34** | 6803 |
| Engine 请求路径（`Minimal()`） | 1443 | 17 | 5858 |
| Engine 请求路径 + `BodyLimit` 路由 | 1877 | **23** | 6379 |
| Engine 请求路径 + `c.JSON`（JSON 响应档） | —（探针口径不同，见下） | **25** | 6453 |

由表内两行推出的 Δ：

- **`WithCollector()` 的每请求成本** = 2123 − 1774 = **+349 ns / +12 allocs**（端到端）
- **kernel 层 `AttachCollector` 相对基线** = 341.9 − 80.9 = **+261 ns / +12 allocs**
- **包含关系自检（防倒挂）**：`AttachCollector` = 裸绑定 + 1 个 Collector 结构，**B/op 严格更大（681 > 649）**，这是确定性判据。两者 allocs 同为 14——**alloc 单值区分不了这两者**（一次局部绑定写入本身就占十来个分配）。ns 本轮与逻辑同向（341.9 > 326.7）但只差 4.6%，落在噪声带里（裸绑定档 5 次 322.8–330.2、Collector 档 328.3–355.4 有重叠），**别拿 ns 下这个结论**。
- **与插件树规模解耦**：kernel 层 10 / 50 / 100 插件 345 / 361 / 370 ns、allocs 恒为 14；Engine 侧 0 / 10 / 50 插件 1774 / 1819 / 1937 ns、allocs 恒为 22、B/op 6314（50 插件档 6315）。插件树大小不改变每请求成本——这是「装配能力」能当卖点的前提。
- **`c.JSON` 先编码到 buffer 的代价**（两棵树同一探针，`-benchtime=20000x -count=3`）：main 23 → 本版 **25 allocs/op**、B/op 6341 → **6437**，即 **+2 allocs / +96 B 每响应**——换来「编码失败不再发 200 空体」（#33）。这笔账起初没进描述、门禁也覆盖不到（其余档 handler 都是 `c.Text`，压根不经过 `c.JSON`），现已单列 JSON 档纳入基线。**该行的 ns 留空**：那台探针与本表不同轮，按「凡要相减必须取本表内两行」的规矩不混用。
- **挂了 `BodyLimit` 的路由 +1 alloc**：default 档 22 → body-limit 档 **23 allocs/op**，多出来的就是 `http.MaxBytesReader` 返回的包装器本身（绑在请求上，无法复用）。这笔账同样起初没进描述、门禁也覆盖不到（其余档都不挂 `BodyLimit`），现已单列一档纳入基线（review 提出，PR #45）。ns 本轮 +103（1877 vs 1774），仍落在噪声里（该档 5 次 1839–2177）——**分配是确定性判据，ns 不是**。

**回归门禁**：本表的**分配计数与 B/op** 已固化成断言（`bench/budget_test.go`，由 CI 的 `Alloc budget` 步骤执行，不带 `-race` 跑）。**ns 不设阈值**——跨轮会漂 2–4×，拿它做门禁等于把机器状态引进 CI；ns 对比仍走人工 benchstat，口径见表 A / 表 B 各自的说明。断言失败时按提示同步刷新常量与本表。

> **门禁自己钉死 N 与 P**（`bench/budget_test.go` 的 `fixedIterations` / `fixedProcs`，2026-09-16 · [#71](https://github.com/Luo-root/pulse-web/issues/71)）。分配计数是跨平台确定值——同一 commit、同一工具链（go1.27.0），windows/amd64 与 linux/amd64 实测**逐项相同**（22 / 34 / 17 / 22 / 25）。B/op 会变，但变的自变量**不是平台**，是**迭代数 N 与 P 数**：
>
> | 跑法（同一份二进制、同一台 i9-14900HX） | default | minimal | json |
> |---|---|---|---|
> | 固定 N=20000x，**32 P** | 6302 | 5843 | 6438 |
> | 固定 N=20000x，**4 P** | 6290 | 5833 | 6418 |
> | 默认 1 s 窗口（N 随机器快慢变） | 6289 | 5833 | 6418 |
>
> 两条判据：**同一个 P 下两个 OS 逐项相同**（4 P 时 windows 与 linux 都是 `6290 / 6777 / 5833 / 6289 / 6419 / 6353`）；**同一个 OS 换 P 就变**（32 → 8 → 4 → 2：6302 → 6292 → 6290 → 6289）。1 s 窗口里 N 还会跟着机器快慢漂，于是同一份代码能落在 **6289…6303**——上一轮那张「平台差 8–19 字节」的表就是这么来的：网侧的两个数（6298 / 6289）差的其实是 32 P 与 4 P，外加一档 N。
>
> 所以门禁**把两个自变量都钉住**：迭代数固定 20000、`GOMAXPROCS` 固定 4（与 CI runner 同档）。基线随之收敛成**一份**（那一轮是 `6290 / 6777 / 5833 / 6289 / 6419`；[#76](https://github.com/Luo-root/pulse-web/issues/76) 之后是 **`6306 / 6793 / 5849 / 6305 / 6435`**——每档 +16，与 N / P 的敏感度无关）：本地 windows 与 CI linux 报同一个数，8 字节 slack 在每个环境都还保住灵敏度——不必再靠「按平台记两份」绕开环境差，也不会再出现「按 windows 定就白送 9 字节死余量」那种二选一。定位过程与全部对照见 [#71](https://github.com/Luo-root/pulse-web/issues/71)。
>
> **表 B 的绝对值仍按机器默认 P（本机 32）记**，与门禁钉的那一档不同：两份数字各自在自己的口径里成立，**不要互减**（跨口径相减会得到「门禁比表 B 少 8 字节」这种没有意义的差）。

### 测量口径：本机时钟量子（2026-09-16 实测）

这台机器上 `time.Now()` 的推进量子远粗于常见机器：连续采 200 万次只有 **34 个不同的非零 delta**，最小非零 delta **303 µs**（`for time.Now() == t0 {}` 等到的下一跳 642 µs）。探针是个 20 行的 Go 程序（一次性，未入库）。

后果两条：

1. 任何**短于约 0.3 ms** 的区间会被读成 **0 或量化值**。一个真进程内一次 `GET /users/42` 的访问日志在这台机器上就会打出 `0ns`，而同一条路径加 3 ms 的 handler 睡眠打出 `3.62ms`——**那不是框架没计时，是机器读不出来**。凡是看到「耗时 0」先怀疑时钟，再怀疑被测代码。
2. `-benchtime=20000x` 一档整轮约 7 ms，量化误差本身就有几个百分点，与观测到的 5–10% 离散度同量级。**分配计数不受时钟影响**——这是它当判据的又一条理由；要 ns 就上更长 benchtime，并且只跟同轮对照行比。


### 表 C：与 gin 的真实负载对比（2026-09-16 重采，采集工程在 `loadtest/`）

设计验收标准『真实负载与 gin 同级』条的落地。口径与结论在这里；采集工程是仓库里的 `loadtest/`
——**独立 module**，带 gin 依赖，`replace` 回来比的是当前工作副本（根 module 的 `./...` 不过
module 边界，依赖判据不受影响）。

它曾经待在侧分支 `bench/gin-compare` 上，结果是默认出口从 `SlogSink` 换成 `ConsoleSink` 之后，
那一档**无人察觉地继续量旧默认**——不报错、不告警。所以它现在跟着主干走、由 CI 单独覆盖
（[#73](https://github.com/Luo-root/pulse-web/issues/73)）：**证据工程待在不参与 CI 的地方，
迟早会和主干脱节**。

本节 2026-09-14 首采；2026-09-16 换默认出口后重采，**两次独立会话的配对比值逐项吻合**
（`bare` 0.90x / 0.91x、0.95x / 0.95x；`obs` 0.81x / 0.82x、0.92x / 0.93x）——下面登的是第二次，
也就是采集工程挪进仓库、选项集收敛之后的那一轮。

**口径先行**——这类对比最容易变成「谁的数字好看谁赢」，所以先把口径钉死：

- 两侧**独立进程**，共用同一份 `http.ListenAndServe` bootstrap，**只让 handler 是变量**
  （不用 `gin.Run()` / `Engine.Run()`，免得把各自的默认 server 配置引进对比）
- 同一格内两侧**相邻**跑，奇偶轮交换先后，**4 轮**；每格取 **RPS 中位那一轮**的整套分位；
  两侧比值取**逐轮配对比值的中位**
- 路径 `GET /users/42` → 两侧同一份 JSON；压测器自写（固定并发、连接全复用、计时窗口内
  **每个**请求都进分位，不采样）
- 机器：i9-14900HX / 32 逻辑核 / `GOMAXPROCS=32` / Go 1.27 / Windows amd64 / **插电**
- 两侧日志出口都指向空设备：保留每请求的**格式化成本**，排除终端 / 管道 I/O

两个对拍档：`bare` = `gin.New()` ↔ `web.New(web.Minimal())`；`obs` = `gin.Default()` ↔ `web.New()`
（后者走的是**当前默认出口** `ConsoleSink`）。

| 档位 | 并发 | pulse-web RPS | gin RPS | pulse/gin | pulse p99 | gin p99 |
|---|---|---|---|---|---|---|
| bare | 64 | 55426 | 61290 | **0.91x** | 4.86ms | 4.63ms |
| bare | 256 | 63881 | 67156 | **0.95x** | 17.43ms | 21.30ms |
| obs | 64 | 47946 | 59636 | **0.82x** | 5.33ms | 4.82ms |
| obs | 256 | 61107 | 65097 | **0.93x** | 17.51ms | 20.92ms |

- **裸档同级**：0.91–0.95×；并发 256 时 p99 反而更低（17.43ms vs 21.30ms）。
- **绝对值只做量级参考**——同一格换一次会话就能从 49k 变到 78k（±20%），所以本表只认同轮配对比值：
  与表 A / 表 B 的「ns 跨轮漂 2–4×」是同一条纪律。
- **换默认出口的效果**（只与 2026-09-14 那轮比**比值**，不比绝对值）：`obs` 档 0.65x → **0.82x**（c=64）、
  0.88x → **0.93x**（c=256）。方向一致、幅度收窄——省下的是「把 Record 摊平成 `[]any` 再交给 slog」那一步。

**观测开销拆解**（诊断档，只跑 pulse-web 一侧——它们回答「钱花在哪」，不回答「谁快」）：

| 并发 | bare | 关访问日志 | 默认 `ConsoleSink` | `AsyncSink`(默认出口) |
|---|---|---|---|---|
| 64 | 55426 | 50706 (−9%) | **47946 (−13%)** | 51955 (−6%) |
| 256 | 63881 | 62092 (−3%) | **61107 (−4%)** | 60810 (−5%) |

- **默认装配的观测开销**：c=64 掉 13%、c=256 掉 4%；关掉访问日志掉 9% / 3%——**记录这条路本身**
  （TraceID 生成 + 记录组装）的量级就在这儿。
- **「出口」与「记录框架」的分界落在噪声里，本页不给单独归因。** 把同表内「默认出口」与「关访问日志」
  两格相减得 4%（c=64）/ 1%（c=256）；上一轮同一算法给的是 7% / 3%。两次算出来不一致、又都小于
  `obs` 档自身的**轮间极差**（c=64 是 7.5%）——宁可不写，也不给一个看起来精确的假数。
- **异步出口的倾向**：`AsyncSink(默认)` 在 c=64 是四档里最接近 `bare` 的（−6%，它自己的轮间极差
  只有 2.3%）；c=256 四档挤在 −3% ~ −5%，分不出来。低并发下把格式化挪出请求路径划算、高并发下
  队列与调度把那点收益吃掉——这是**倾向**，不是结论。
- **参照**：gin 侧同一档（`gin.Default()` vs `gin.New()`，`Logger` 就是一行文本、没有第二条路可选）
  掉 **2.7% / 3.1%**（c=64 / c=256，同一张主表内两行相减）。
- **选项集只留今天真实存在的**：默认出口，与「默认外面套一层 `AsyncSink`」。历史上的「换 `LineSink`」
  与原型 `fastsink` 两档已从采集工程里删掉——前者的手法被默认出口吸收（`ConsoleSink` 就是行式缓冲
  出口 + 列式版式的合体），后者的实现永远不会发布。**拿它们当选项比较，等于拿基准当选项。**

**条数与内容**：

- **条数一样**：pulse-web 每请求 **1 条**结构化 `Record`（Engine 收尾里的 AccessLog；`c.Observe`
  要业务自己调，默认不写第二条），gin 每请求 **1 行**文本。pulse-web 另有**装配期 3 条**
  （Bootstrap），不是每请求。
- **内容不一样**：pulse-web 带 TraceID（32hex）、路由**模板**（`/users/{id}`）、错误分类（有错才写）；
  gin 那行只有时间 / 状态 / 耗时 / 客户端 / 方法+路径。给 gin 配上等价物要另装第三方中间件——
  所以这一档量的是「各家默认开箱配置」，不是「等价功能的成本」。

**成本分解**（micro-benchmark，**不是胜负承诺**；口径与表 A / 表 B 不同，数字不要跨表相减）：

口径 `-benchtime=100000x -count=5` 取中位轮（2026-09-16，插电）：

| 档位 | ns/op | B/op | allocs/op |
|---|---|---|---|
| gin/bare | 305 | 120 | 5 |
| pulse/bare | 644 | 921 | 13 |
| gin/obs | 1159 | 346 | 15 |
| pulse/obs-nolog | 893 | 986 | 16 |
| pulse/obs-async（默认出口外面套一层 `AsyncSink`） | 1470 | 1699 | 19 |
| pulse/obs（默认出口 `ConsoleSink`） | **1377** | 1378 | 18 |

> 本表 2026-09-14 首采、2026-09-16 重采，**跨轮只能整表替换、不能只换一格**（跨轮相减会给出倒挂结论，本项目
> 为此专门立过规矩）。这一轮换掉两件事：① 默认出口从 `SlogSink` 换成 `ConsoleSink`——`pulse/obs` 那一格由
> `~2850 ns / 2538 B / 35 allocs` 变成 **1377 ns / 1378 B / 18 allocs**，这是 #21 在请求路径上的直接兑现；
> ② 选项集收敛成今天真实存在的两个（默认出口、默认外套一层 `AsyncSink`），历史上的 `pulse/obs-line`
> （换 `LineSink`）与原型 `pulse/obs-fast` 两行一并删掉。
>
> 跨轮有两个可核对项，说明两轮量的是同一条路径：gin 两侧的 **分配计数与 B/op 逐项相同**（`120 B / 5 allocs`、
> `346 B / 15 allocs`）；pulse 那两行各自 **+2 allocs / +96 B**——与表 B 记的 `c.JSON` 代价逐项吻合
> （`6341 → 6437 B` 是另一支探针量到的同一个变化）。机制已知，不是漂移。
>
> 本表与表 A / 表 B 的 ns **不可互减**：探针不同（这里复用请求与 writer，只量框架层；「默认出口」一节的
> `2335 ns / 22 allocs / 6314 B/op` 出自另一支探针）。

- 逐项拆开（相对 `pulse/bare`）：**记录框架**（`obs-nolog` = TraceID + 记录组装）**+3 allocs / +249 ns**；
  **访问日志与出口渲染**再 **+2 allocs / +484 ns**；把默认出口外面套一层 `AsyncSink` 是 **+6 allocs / +826 ns**。
- **`AsyncSink` 在这张表里更贵**（1470 vs 1377 ns）：单协程紧循环里没有「写出口阻塞」可躲，入队与 Attrs 深拷
  是净加的。它的用处在真实负载下才显出来——拆解表 c=64 那一格它是四档里最接近 `bare` 的（−6%，默认出口
  是 −13%）；c=256 四档分不出来。**同一件事在两张表里给出相反的印象，所以本表只当分解，不当胜负。**
- **微基准与真实负载可以给出相反的印象**：pulse-web 的**裸**路径微基准约是 gin 的 2×（644 vs 305 ns，
  多 8 次分配），真实负载下只差 5–10%（请求路径不是瓶颈，网络栈与调度才是）。这正是「不以 micro-benchmark
  胜负作承诺」的实证——拿上面那张表去说谁快，会得到一个与真实负载相反的印象。

## 设计红线

1. 请求路径一律 `EmitLocal`，禁用全树 `Emit`（v0.2.1 实测 23ns vs 1778ns）
2. **请求级数据用 `kernel.Local()`，不写全局 `Provide`**——全局命名空间是**装配面**，请求级值写进去会被并发请求互相覆盖。`WithCollector()` 显式开启后才在请求路径上出现局部绑定（实测 +349 ns / +12 allocs 每请求，与插件树规模无关；口径见表 B）
3. 请求 scope 与"响应是否送达网络"解耦：**`Dispose` 在写响应之前**
4. `Ctx` 不可跨 goroutine；后台任务用 `Detach`（值 + 进程级 root）

## API 面

### 装配（进程级，与卖点对齐的一等公民）

```go
app := web.New()                  // 自建 root + 装 Bootstrap
app := web.New(web.WithRoot(k))   // 接入已有 kernel（与 host 共用同一棵树）
root := app.Root()                // 进程级 root：Provide / Use / 交给需要进程级 kernel 的组件

kernel.Provide(root, dbKey, db)   // 其余用 kernel 原生 API
kernel.Use(root, myPlugin)
```

**`Root()` 是装配叙事的关键**：任何需要"进程级 kernel"的组件（自建插件、第三方接入、跨请求存活的装配）都必须拿到它。**不要把 `c.Kernel()` 当进程级 kernel 用**——那是请求 scope，挂上去的插件与 Effect 会在请求结束时被级联销毁。

其余装配（`Provide` / `Use` / `Loader.Reconcile` / `FiberSnapshots`）直接用 kernel 原生 API，本框架**不包装**。

### Engine 与运行

```go
func (app *Engine) Run(addr string) error          // 阻塞至关闭完成
func (app *Engine) Serve(ln net.Listener) error    // 自定义 listener
func (app *Engine) Handler() http.Handler          // 导出视图（等价自身）
app.Static("/static", "./files")
app.OnShutdown(fn func(ctx context.Context) error) // 单回调
```

- `Run` **阻塞**；`nil` = 收到信号正常关闭，非 nil = 启动失败或关闭期错误。两条路径都完成 `root.Dispose()`——**调用方不能再 Dispose**
- `Run` 返回后 Engine **不可复用**（kernel 已销毁）
- 不提供 `RunTLS`（用 `tls.NewListener` + `Serve`，或反代终止 TLS）

`ServerConfig`（零值即默认）：

| 字段 | 默认 | 说明 |
|---|---|---|
| `ReadHeaderTimeout` | 5s | 防慢头攻击 |
| `ReadTimeout` | 30s | 读完整请求上限 |
| `WriteTimeout` | **0** | **刻意**——非 0 会切断 SSE；非流式服务建议显式设 30s |
| `IdleTimeout` | 120s | keep-alive 空闲上限 |
| `MaxHeaderBytes` | 1 MiB | 请求头上限 |
| `ShutdownTimeout` | 30s | 优雅关闭总预算 |

### Ctx

```go
func (c *Ctx) Path(name string) string   // == c.Request().PathValue(name)（同源，不另存）
func (c *Ctx) Query(name string) string
func (c *Ctx) Bind(v any) error          // 请求体绑定：按 Content-Type 分派；无 body 落 query（#32）
func (c *Ctx) BindQuery(v any) error     // query 参数绑定（GET / 过滤场景）
func (c *Ctx) Request() *http.Request
func (c *Ctx) Context() context.Context  // ≡ Request().Context()（#69）
func (c *Ctx) Cookie(name string) (*http.Cookie, error)
func (c *Ctx) TraceID() string

// 请求级 KV（框架自有 map —— 不用 kernel.Local()，依据见下「关系澄清」）
func (c *Ctx) Get[T any](k Key[T]) (T, bool)
func (c *Ctx) Set[T any](k Key[T], v T)

// 全局服务（kernel root 仓库）
func (c *Ctx) Service[T any](k kernel.ServiceKey[T]) (T, bool)
func (c *Ctx) MustService[T any](k kernel.ServiceKey[T]) T

func (c *Ctx) Kernel() *kernel.Context   // 请求 scope：挂 Effect / 事件监听 / 传给需要作用域的组件
func (c *Ctx) Observe(event string, set func(*observability.Attrs))
func (c *Ctx) Detach() Detached

// 响应
func (c *Ctx) JSON(code int, v any) error
func (c *Ctx) Text(code int, s string) error
func (c *Ctx) Blob(code int, contentType string, b []byte) error // Content-Type 原样写出（#69）
func (c *Ctx) Redirect(code int, url string) error               // http.Redirect：当场落码（#69）
func (c *Ctx) NoContent(code int) error                          // ≡ Status(code)：只设置（#69）
func (c *Ctx) File(path string) error                            // http.ServeFile：Range / 条件请求 / 嗅探（#69）
func (c *Ctx) Attachment(path, name string) error                // File + Content-Disposition（非 ASCII 名走 RFC 2231）（#69）
func (c *Ctx) Status(code int) *Ctx       // 只设置，不立即写头
func (c *Ctx) SetHeader(k, v string)
func (c *Ctx) SetCookie(cookie *http.Cookie) // 同名是追加（#69）
func (c *Ctx) Writer() http.ResponseWriter // 流式：直接写字节（包装器，状态码与体积照常采集）
func (c *Ctx) Flush() error                // 流式：首刷落 Status() 设置（缺省 200），此后逐段推送；底层不支持 Flusher 时返回明确 error
```

**「首刷」= 第一次写出**：`c.Writer().Write`、`c.Flush()`、handler 返回后的引擎收尾——三处里的最先一个落定状态码（吃 `Status()` 的提示、缺省 200）；此后响应头已发出，再调 `Status` 无效。三条路径共用同一实现（`responseWriter.writeHeaderNow`），所以「先写第一段、再 Flush」这条流式 handler 的自然顺序也吃提示（gin 的 `WriteHeaderNow()` 是同一语义）。只让 `Flush` 吃提示是不够的——`Status(201)` 会被第一次 `Write` 的隐式 200 吃掉（review 提出，PR #35）。

**便利 API 的口径（#69）**：`Blob` / `Redirect` / `NoContent` / `File` / `Attachment` / `SetCookie` / `Cookie` / `Context()` 八个方法是标准库的薄壳，不新造语义。三条显式约定：

- **写出方法一律返回 `error`**——与 `Text` / `JSON` / `HTML` 同形，于是 `return c.Redirect(302, "/x")` 这类收尾写法在**全部**写出方法上成立。其中只有 `Blob` 真会出错；`Redirect` / `NoContent` / `File` / `Attachment` 走的是标准库那条不回报写出结果的路径，返回值恒为 nil。
- **`Redirect` 与 `NoContent` 在「何时落码」上相反**：`Redirect` 当场把响应头发出去（此后 `return` 的 error 不被错误映射接管）；`NoContent(code)` ≡ `Status(code)`，只设置、不落码，错误映射照常。两者都遵守既有首刷规则：落码之后不再改变。
  `NoContent` 存在的理由**不是**这层语义（它俩的响应逐字节相同，`TestNoContentEqualsStatusPlusReturnNil` 连响应头都逐项钉着），而是**调用形态**：`Status` 返回 `*Ctx`——它是「设置状态码」这个中间动作，后面通常还要写 body，于是空响应收尾得写 `c.Status(204); return nil` 两行；`NoContent` 把它压成 `return c.NoContent(204)` 一行，与 `Text` / `JSON` / `Blob` 的收尾形状对齐。
- **`File` / `Attachment` 的失败不经错误映射**：文件缺失由 `http.ServeFile` 自己写 404（纯文本错误页），观测记录里也没有错误属性。要统一错误面就自己先 `os.Stat` 再返回 `web.NotFound`——这是「薄」的代价，写进 godoc 与站点指南，不替调用方兜底。

类型约束：`Key[T]` 与 `kernel.ServiceKey[T]` 是不同类型，误用编译期报错——命名是第一道防线，类型是第二道。

**`Writer()` 的能力边界**（有意的窄口）：返回的是框架包装器，**只显式实现 `http.Flusher`**（`Flush()` 落到下层 writer）；底层的 `Hijacker` / `Pusher` / `FlushError` / `SetWriteDeadline` **不透出**——内嵌 `http.ResponseWriter` 只提升 `Header` / `Write` / `WriteHeader`，其余能力必须显式实现才有。所以 `http.NewResponseController(c.Writer())` 上只有 `Flush()` 可用，`Hijack()` / `SetWriteDeadline()` / `EnableFullDuplex()` 返回 `http.ErrNotSupported`。要升级协议（WebSocket）需要原始 writer：用 `Wrap` 包一个 stdlib handler，代价是拿不到 `*Ctx`。窄口是有意的——让「流式能干什么」在类型层面一眼可见，而不是靠断言碰运气；要放开 `Hijack` 得先想清楚它与 AccessLog 体积统计的交互（另开票）。

**关系澄清**：kernel **v0.2.1 起存在** scope 局部服务——`kernel.Provide(scope, key, v, kernel.Local())`：绑定存本层，**本 scope 及其后代**可读（`Get` 沿父链近因优先），父 / 兄弟不可读，随作用域销毁撤除；它**不投递服务变更、不参与 fiber 依赖解析**。所以 `c.Service(key)` ≡ `kernel.Get(c.Kernel(), key)`，会先走局部链再回全局仓库。

但**请求级 KV 仍由框架自有 map 承担**，不改用 `Local()`：`Local()` 每条绑定实测 ≈ **+250 ns / +12 allocs**（kernel 层同轮对照：`Derive + Dispose` 84 ns → 挂一条绑定 337 ns，见表 B），而 `Ctx.Set/Get` 只是已分配 map 上的一次 store。`Local()` 的定位是「少量**语义性**绑定」（如 Collector），不是通用容器。

**`observability.Set` 的类型约束**：`Set[T AttrValue]` 只接受 `~string | ~int64 | ~float64 | ~bool`——`u.ID` 若是 `int` 或 UUID 类型需显式转换（`int64(u.ID)` 或 `u.ID.String()`）。

### 错误模型

```go
type HTTPError struct {
    Status  int    // HTTP 状态码
    Code    string // 机器可读业务码
    Message string // 人读消息（默认取状态码标准文案）
}
// cause 不导出：要携带原始错误请用构造器 —— 它只进观测记录，绝不进响应体。
func (e *HTTPError) Error() string
func (e *HTTPError) Unwrap() error

// 可扩展状态码：非 HTTPError 但实现本接口的 error，用其状态码，message 仍走默认脱敏
type StatusCoder interface {
    error
    StatusCode() int
}

func BadRequest(code string, cause error) *HTTPError   // 400
func Unauthorized(code string, cause error) *HTTPError // 401
func Forbidden(code string, cause error) *HTTPError    // 403
func NotFound(code string, cause error) *HTTPError     // 404
func Conflict(code string, cause error) *HTTPError     // 409
func Internal(code string, cause error) *HTTPError     // 500

type ErrorHandler func(c *Ctx, err error) error
app := web.New(web.WithErrorHandler(myMapper))
```

- 需要 429 / 422 / 503 等：实现 `StatusCoder`，或直接构造 `&web.HTTPError{Status: n, Code: "…"}`
- 默认响应体：`{"error": {"code": "user", "message": "not found"}}`
- **安全默认**：`cause` 只进观测记录；**非 `HTTPError` 的普通 error 一律 500** + 通用文案
- panic 有专用类型：`PanicError{Value, Stack}`——默认 mapper 一律 **500**（栈只进记录）。**陷阱要显眼写**：`panic(pulse.Unauthorized(…))` 不会返回 401，要 4xx 请 `return`

### 请求体上限（`WithMaxBodyBytes` + `BodyLimit`）

两道闸，一道全局、一道按路由 / 分组：

```go
app := web.New(web.WithMaxBodyBytes(2 << 20))            // 全局：所有路由
app.Group("/upload", web.BodyLimit(100<<20)).POST("/avatar", h)  // 分组：只有这块放宽
app.POST("/api/export", h, web.BodyLimit(1<<20))                 // 单条路由：收紧
```

- **闸门做成中间件，不新增注册参数**：`Group(prefix, mw...)` 与 `GET/POST(..., mw...)` 本来就吃中间件，`BodyLimit` 就是一个中间件——不动路由签名、不引入注册期状态。生态同形：echo 的 `middleware.BodyLimit` 同样是中间件。
- **闸门只能收紧，不能放宽**：引擎级先套一层 `MaxBytesReader`，路由级再套一层，实际生效的是两者中**更小的**那个。`BodyLimit(1<<20)` 挂在 `WithMaxBodyBytes(1<<10)` 的路由里仍被 1 KiB 掐住。不做「就近覆盖 / 可 raise」——去掉内层包装意味着换掉整个 body reader，代价与风险都不成比例。这条是**显式口径**，别让调用方以为可以 raise。
- **两条路径，同一类错误**：有 `Content-Length` 时先按声明值快速失败（**不读 body**）；声明未知（chunked）或声明偏小时由读取闸门兜底。两条路径都产出 413 + `body_too_large`，cause 都是 `*http.MaxBytesError`——与 `WithMaxBodyBytes` 的两条路径同型，自定义 `ErrorHandler` 用 `errors.As` 只认这一个类型即可覆盖全部超限路径。边界口径：body **恰好 n 字节通过**，n+1 起 413。**读取闸门是惰性的**：超限只在 handler 真的去读 body 时才触发，所以「声明未知 + handler 完全不读 body」的请求会以 200 结束——这一点与引擎级那道闸、以及 echo 的 `middleware.BodyLimit` 同形（`net/http` 自己在读侧也会兜住，不会无限灌），要不要读 body 是 handler 的事。
- **`n <= 0` 表示不限**（直通，也不包 body），与 `WithMaxBodyBytes(0)` 同口径。
- **`MaxBytesReader` 的 `w` 必须传原始 writer**：它**只在超限时用**——stdlib 会调 `w.(requestTooLarger).requestTooLarge()` 给响应加 `Connection: close` 并在回复后关连接（防继续灌数据、防 keep-alive 把残留 body 当成下一个请求）。这个未导出接口只有 server 内部的 `*response` 实现，而 `c.Writer()` 是框架包装器——用户自己写中间件时传它就**静默丢掉**这个语义。原始 writer 只有包内实现拿得到，所以这件事该由框架做（引擎级那道闸同理）。
- 因此补了导出构造器 `TooLarge(code, cause)`：内置构造器原先只有 400 / 401 / 403 / 404 / 409 / 500，**没有 413**，而 `HTTPError.cause` 不导出——外部想返回**带 cause 的 413** 没有正规路径。

### 中间件与 stdlib 互操作

```go
type Handler func(*Ctx) error
func Wrap(h http.Handler) Handler   // stdlib → 框架
```

- `Wrap` 后的 handler **无法访问** `*Ctx`（`c.Path` / 请求级 KV / `c.Observe`），但可用 **`r.PathValue("id")`**（与 `c.Path` 同源，ServeMux 写入的同一份）；中间件链照常经过它
- panic 由 Engine 兜底接（见下）；响应已写出则只记录不重写

### HTML 模板

薄封装 stdlib `html/template`（不自己实现模板引擎）：

```go
app := web.New(web.WithTemplates(web.TemplateConfig{
    Root:      "./views",   // 模板根目录
    Pattern:   "*.html",    // glob 模式（默认值）
    DevReload: false,       // true = 每次请求重新解析（开发用）；生产保持缓存
}))

app.GET("/", func(c *web.Ctx) error {
    return c.HTML(200, "index.html", web.H{
        "Title": "首页",
        "User":  user,
    })
})
```

- **生产模式**：启动时解析一次并缓存（并发安全）
- **`DevReload: true`**：每请求重新 `ParseGlob`（每请求磁盘扫描 + 写锁）；仅供开发
- `web.H` = `map[string]any` 的类型别名（模板数据便利写法，也可传任意 struct）
- **`c.HTML` 的三条错误路径一律返回明确 error**（不静默 500）：未配置模板 / 模板名不存在 / 执行期报错。实现上**先渲染到内存缓冲、成功后才写响应头**——`WriteHeader` 一旦先生效，「已写响应不被覆盖」规则会把错误吞成 200 空页。代价是模板输出不流式（需要流式请用 `c.Writer()` + `c.Flush()`）
- 模板自动补 `Content-Type: text/html; charset=utf-8`

### 静态文件与中间件

`Static(prefix, dir)` **走与普通路由同一条注册路径**——静态资源同样经过全局与分组中间件（auth / CORS / 限流不会有例外），也照常进 Trace 与 AccessLog 收尾。内部注册前缀模式 `<prefix>/`，交给 `http.FileServer` 自行处理 404 / 405。

> ⚠️ 再注册 `GET <prefix>/{file...}` 会遮蔽静态服务（方法限定模式优先）；要放行个别路径请用字面量（字面量赢通配），如 `app.GET("/static/health", h)`。

## 运行时契约

### 请求作用域的生命周期（关键时序）

```
中间件链 → handler 返回
  → scope.Dispose()              ← 业务态结束：请求级 Effect 立即 LIFO 回收
  → 错误映射 + 序列化 + 写响应     ← 网络态
  → AccessLog 落盘               ← 响应侧字段此时才就绪
```

**`Dispose` 在写响应之前**：否则慢客户端会占着请求级资源（DB 事务、连接）直到 TCP 超时。（小响应通常只写 `bufio` 缓冲不阻塞，风险面是"大响应 + 慢客户端"；但边界要显式定义。）

**流式响应下同一条规则**：SSE 的 handler 不返回 → scope 活到流结束——这是**正确语义**（scope 生命周期 = 业务逻辑生命周期），不是冲突。使用指引：长连接不要把"用完该马上还"的资源（DB 事务、分布式锁）登记为请求级 Effect。

### 错误映射器契约

映射发生在 `scope.Dispose()` **之后**，所以"能碰什么"是显式契约：

| API | 可用 | 说明 |
|---|---|---|
| `c.Get` / `c.Set`（请求级 KV） | ✅ | KV 挂在 **Ctx** 上，生命周期覆盖到 Engine 收尾 |
| `c.Observe` | ✅ | 直写 Sink，不经 scope |
| `c.TraceID` / `c.Path` / `c.Query` | ✅ | 纯数据 |
| 响应写出 | ✅ | 映射器职责 |
| `c.Kernel()` / `c.Service` | ❌ | 请求 scope 已 Dispose：`Get` 返回 false、`Effect` 返回 `ErrDisposed`（**返回的是死 scope，不是 nil**，避免 nil panic） |

- `err` 参数是**原始 error（只读）**；`ErrorHandler` 返回值 = **映射器自身失败**（响应未写则兜底 500；已写则只进内部日志）
- **panic 兜底在 Engine 的 `defer`**（不是中间件）——所以 `Minimal()` 下 panic 依然有 500 响应（**记录**则取决于装配：Minimal 无 Sink、无 AccessLog，Sink 里不会有这一条）

### Ctx 生命周期与后台任务

`Ctx` 及其 `ResponseWriter` **不可跨 goroutine**（`ResponseWriter` 非线程安全；kernel scope Dispose 后静默失效——`Get` false、`Effect` 返回 `ErrDisposed`，比 panic 更隐蔽）。

**Ctx 不池化**（明确决策）：池化只省一次分配，代价是契约升级为"绝对不能保留引用"——用户存进全局 map 后对象复用会造成静默数据串扰。请求路径预算容得下这次分配。

**后台任务的正确姿势**：`Detach` 是**值袋子**，不拥有 scope：

```go
type Detached struct {
    TraceID string
    HostID  string
    Sink    observability.Sink
    Root    *kernel.Context   // 进程级
}
func (d Detached) Observe(event string, set func(*observability.Attrs))
func (d Detached) Service[T any](k kernel.ServiceKey[T]) (T, bool)
func (d Detached) MustService[T any](k kernel.ServiceKey[T]) T
```

```go
app.POST("/jobs", func(c *web.Ctx) error {
    bg := c.Detach()
    go func() {
        bg.Observe("job.start", nil)      // 直写 Sink，进同一 TraceID
        svc := bg.MustService(jobKey)     // 全局服务：读 root，不需要 scope
        scope, _ := bg.Root.Derive()      // 需要 scope 就自己派生
        defer scope.Dispose()             // 生命周期自己管
        svc.Do(context.Background(), input)
        bg.Observe("job.done", nil)
    }()
    return c.JSON(202, "accepted")
})
```

并发契约一句话：**`Detached` 是值，不拥有 scope**——scope 的派生与回收由调用方自理，不存在共享状态的竞态。

**`Detach` 不携带请求级 KV**：要传递的数据在 Detach 前显式取出（`user := c.MustService(userKey)` 式的显式取值），让"哪些数据进了后台"一眼可见。

### 优雅关闭时序

**kernel 的 `Context.Dispose()` 是级联截断，不是 drain**——递归销毁所有子 scope、静默 `forceUnload` 所有 fiber（`kernel/context.go:191-251`），**不等待在途工作**。drain 必须由 `net/http` 承担：

```
信号（SIGINT / SIGTERM）
  → ① http.Server.Shutdown(timeoutCtx)   ← drain 在这里
  → ② 超时 → srv.Close() 强制断开
  → ③ OnShutdown 回调                     ← 单回调，用户等待后台任务
  → ④ root.Dispose()                     ← 终局：级联截断
  → ⑤ Sink flush（**独立预算** `sinkFlushTimeout` = 3s，
       在 `root.Dispose()` 之后另起，**不计入 `ShutdownTimeout`**：
       总关闭时长上限 = ShutdownTimeout + 3s）
```

- ①②③ 共享**同一个 deadline**（这三段总时长 ≤ `ShutdownTimeout`，与 k8s `terminationGracePeriodSeconds` 对齐）；HTTP 用光预算时回调拿到已过期 ctx，记录"后台任务未等待"；⑤ 的 Sink flush 另起独立预算（见上，总上限 = ShutdownTimeout + 3s）
- **⑤ 的 flush 探测同时认两种出口形态**：`Flush(context.Context) error`（上游 `AsyncSink`）与 `Flush() error`（上游 `LineSink`）。只认其中一种会**静默跳过**另一种——`WithSink(observability.NewLineSink(...))` 下关闭前未达阈值的最后一批记录会随进程消失，且不报错、不告警。flush 返回的错误记 `slog.Warn`（与 ③ 的 `OnShutdown` 错误同一处理：不静默吞，也不让关闭失败）
- **⑤ 只 flush、不 `Close`**：出口的所有权属装配方。`Detach` 明确允许进程级后台任务继续写同一个 Sink，框架在关闭时 `Close` 它会静默丢弃这些记录。关闭时序只负责"把**已产生**的记录写完"
- **未注册等待的后台任务会被 ④ 截断**——明示行为
- `Detached.Root` 派生出的 scope 是 root 的子 scope，同样受 ④ 影响

## 观测设计

### 默认装配与组合

| 配置 | Bootstrap | Trace | AccessLog | panic→500 | Sink |
|---|---|---|---|---|---|
| `New()` | 开 | 开 | 开 | 开（不可关） | `ConsoleSink` → stdout（给人读，见下） |
| `Minimal()` | 关 | 关 | 关 | **开** | 无 |
| `Minimal(), WithSink(s)` | 关 | 关 | 关 | **开** | `s`（仅供 `c.Observe` 与用户自装中间件） |
| `New(WithCollector())` | 开 | 开 | 开 | 开 | 默认 Sink |
| `New(WithSpanHook(h))` | 开 | 开 | 开 | 开 | 默认 Sink（**不改观测，只多一份 span 数据**：`h` 拥有 span，框架采用它给的 id） |

**`WithSink` 只换出口，不复活 Trace / AccessLog**——要观测就别用 `Minimal()`。框架**没有**可单独挂载的 `Trace()` / `AccessLog()` 中间件（本文档早期版本写过 `app.Use(web.Trace())`，那个 API 不存在，已删）：默认装配是一体的，`Minimal()` 下要自己写中间件 + `c.Observe()` 打点。

`WithCollector()` 每请求把 `observability.Collector` 装进请求作用域（上游 v0.2.1 起是 `kernel.Local()` 作用域局部绑定；实测 **+349 ns / +12 allocs 每请求**，**与插件树规模无关**——口径见表 B）。

**它服务的是「库作者」，不是「应用作者」**：应用作者（自己的 controller / service / dao）把 `c` 或 `c.Observe` 往下传就够；需要它的是那种「想同时活在 web 请求与 CLI / worker 里、因此不能 import pulse-web、只收 `*kernel.Context`」的库对象——**且必须由宿主把请求 scope 显式传进去**。边界见下方两处。无 Sink 时装配期 panic（`Minimal()` 且未 `WithSink` 即此组合）。

`WithoutAccessLog()` 关闭访问日志（用户已接自己的日志系统时用）——只关 AccessLog，`Trace` 与 panic 兜底（500 响应）不受影响。**关闭后 panic 不再写观测记录**：AccessLog 是访问级记录的唯一人工出口；`PanicError.Stack` 只经 `Record.Err` 抵达 Sink，默认出口不打印栈——需要时由宿主自定义 Sink 用 `errors.As` 从 `Record.Err` 取 `*PanicError`。

v1 选项面：`New()` / `Minimal()` / `WithSink` / `WithoutAccessLog` / `WithRoot` / `WithHostID` / `WithServer` / `WithErrorHandler` / `WithTemplates` / `WithMaxBodyBytes` / `WithTrustedTraceHeader` / `WithCollector` / `WithSpanHook` / `OnShutdown`。注意 **`AsyncSink` 不在选项面**——它是**出口实现**，用 `WithSink(observability.NewAsyncSink(...))` 接入，框架不另造缓冲层。**`BodyLimit` 同样不在选项面**——它是**中间件**，按路由 / 分组挂（见「请求体上限」）。

### 默认出口：给人读的控制台列式（`ConsoleSink`）

`New()` 的默认出口是 `web.ConsoleSink`（写 stdout），一行列式、列宽固定：

```
PULSE | 2026/09/14 - 08:30:00 | 200 |   585.1µs | 192.0.2.1:1234  | GET     /users/42 | route=/users/{id} | size=29 | host=pulse-web | trace=8f2e1a3b4c5d6e7f8a9b0c1d2e3f4a5b
```

（行首标识 `PULSE` 是上游缺省值（`observability.DefaultLinePrefix`）——pulse 与 pulse-web 同根同源，同一进程树里两个出口的行首一致，`grep PULSE` 就能拿到全部行。
尾段的 `route=` / `size=` / `host=` / 错误 / `trace=` 是各自独立的 ` | ` 字段，有才出现；
逐字版本由 `console_sink_test.go` 的 `TestConsoleSinkHTTPLine` 钉住——改版式时以那条断言为准。）

**为什么换掉 `SlogSink`**（决策与实测见 [#20](https://github.com/Luo-root/pulse-web/issues/20)）：`SlogSink` 是**给机器读**的结构化出口——接宿主 logger、要 JSON、喂采集器时用它。上游 v0.2.3 起它已与 `LineSink` 对齐字段与顺序（`duration_ms` 是**不截断**的毫秒数值、Attrs 走插入序、不自己输出 `time`），所以这不是「`SlogSink` 有缺陷」，而是**默认位置该给人读**：`0.585` 与 `585.1µs` 是同一条耗时，开机第一眼要的是后者；再叠上 18 allocs vs 0 的成本差（同会话渲染实测，见表）。`Record` 里的字段一个不少，缺的只是渲染层。

| | 渲染一条（io.Discard） | 请求路径（同会话配对） |
|---|---|---|
| `ConsoleSink`（默认） | 236 ns / **0 allocs** | 2335 ns / **22 allocs** / 6314 B/op |
| `SlogSink`（旧默认） | 1304 ns / 18 allocs | （观测档，见 #18 的真实负载对比） |
| `LineSink`（上游缺省版式） | 259 ns / **0 allocs** | — |

口径 `-benchtime=20000x -count=10` 取中位轮（2026-09-16；原表 `~190 ns` / `1531 ns / 19 allocs` / `~262 ns / 1 alloc` 是 v0.2.4 之前的会话，**只能整表替换**，不能只换一格）。请求路径那一列对照的是 `nopSink` 档（1963 ns / 22 allocs / 6315 B/op，`bench/budget_test.go` 的 `default+console-sink` 一档是它的门禁）：**分配计数与 B/op 都逐项相同**——出口渲染既不新增分配、也不改变请求路径的分配形状；ns 上多出的那一段就是渲染（2335 − 1963 = 372 ns）。

> 站点性能页的「出口值多少」用的是**另一条口径**（默认 benchtime、`-count=5`，单档迭代数到百万级，更接近稳态），所以两处的 ns 会有几十纳秒的差（本轮同一个 benchmark：`ConsoleSink` 236 vs 235、`SlogSink` 1304 vs 1398）——那是口径差异，不是漂移。**分配计数与 B/op 两处逐项相同**，这也正是本仓库拿它们当判据的原因。

渲染那一列要三格一起看：`ConsoleSink` 比**上游缺省版式的 `LineSink`** 还快，不是因为它绕过了谁（它内嵌的就是 `LineSink`，见下），而是域知识让它能少写字——同一条记录 `ConsoleSink` 出 **171 字节**，上游缺省版式出 **268 字节**。两者行首标识相同，差在版式：缺省版式为了容下 event 列把状态列撑到 12 列（多 9 个空格）、自带一个 `http.request` event 列、一个 `source=http`，attrs 段还是全限定键名（`http.request.method=GET`）。渲染成本与输出长度同阶，省下的就是这部分。

**行为口径**：

- **按 event 分版式**：`http.request` 走列式；装配期记录（`observability.host_ready` / `pulse.kernel.fiber_state`）与业务打点走 `时间 | event | k=v …`——它们没有 status / route，硬套列式只会渲染出一堆空列。
- **固定列盖不住的属性不丢**：未知键按插入序附在行尾（`| llm.model=… k=v`）。
- **颜色**只在目的地是终端时出现（状态列按区间），重定向到文件 / 管道自动关；`NewConsoleSink(w, WithColor(true))` 可强制。
- **不缓冲**：写完即落 `io.Writer`——终端要即时，缓冲会把安静应用的日志扣在内存里。要吞吐 / 异步 / 机器可读就 `WithSink(…)` 换出口（`AsyncSink` / `SlogSink` / `NewLineSink`），**`WithSink` 只换出口，装配不变**。
- **写错误不抛，但可查**：`Sink` 接口没有错误通道，所以默认路径不会因为磁盘满而中断请求；`ConsoleSink` 内嵌上游 `LineSink`，`Err()` 报出**首次**写失败（stdout 管道被关掉、journald socket 满、磁盘满都从这里读），失败后**继续尝试写**、不静默退出、不改缓冲策略——要错误可见就查它，不必为了 `Err()` 换成别的出口。框架关闭时序第 ⑤ 步已经替你读过一次（记 `slog.Warn`），自己接的场合是「进程不退出也想告警」。
- **并发下渲染与写出共用一把锁**：`LineSink` 用一个自持缓冲换零分配，代价是 `WithRenderer` 在临界区内被调用。实测同一条记录并行写比单线程慢 ~75 ns/条（`ConsoleSink` 228 → 310、上游缺省版式 266 → 340，两者增量同级，说明这个代价属于上游的锁范围而非本渲染器）。代价落在出口的临界区上，不进请求路径——上面「请求路径」一列的分配与 B/op 与 `nopSink` 档逐项相同。

实现分工（pulse v0.2.4 起）：本出口只留**版式知识**（列序与列宽、状态配色、http / 非 http 两条分支、尾段顺序），行首标识（取上游缺省 `PULSE`）、结尾换行、缓冲、颜色判定、耗时与引号口径、写错误收集全部交回 `observability.LineSink`。**渲染器是包级函数不是闭包**——闭包嵌在另一个函数值里时 `Attrs.Range` 的逐值装箱消不掉（实测 6 属性记录每条 5 次分配），包级函数 + `Get[T]` 才是 0。

迁移带来**五处变化**（除这五处外逐字节不变；#27 的 44 组对照 + review 时补的 49 条边界语料）：

1. **行首标识**：无 → 上游缺省 `PULSE`。pulse 与 pulse-web 同根同源，同一进程树里两个出口的行首一致，`grep PULSE` 就能拿到全部行
2. **耗时列**：浮点四舍五入 → 上游 `AppendDuration` 的整数截断（`585199ns` 旧 `585.2µs` → 新 `585.1µs`）。`TestConsoleSinkDurationColumn` 钉住
3. **列补齐**：`utf8.RuneCount` → `observability.DisplayWidth`。ASCII 下等价，**全角下是新版才对**——`客户端-甲:1234` 是 10 rune / 14 显示列，旧实现补 5 个空格让该列占 19 列、后续列整体推右 4 格；新版补 1 个、占满 15 列。`TestConsoleSinkPaddingUsesDisplayWidth` 钉住
4. **兜底组与事件行的键名按需加引号**：新实现走 `AppendAttrs`，键名也过 `AppendTextValue`；旧实现 `appendField` / `appendKeyValue` 裸写键名。（`| weird key=1 k=v=2` → `| "weird key"=1 "k=v"=2`）
5. **`color=on` 且状态列短于 3 显示列时的 ANSI 码位置**：从「补空格之后」挪到「补空格之前」（`|   \x1b[31m5\x1b[0m |` → `| \x1b[31m  5\x1b[0m |`），新顺序与上游一致

前三条是「把本地第二份实现换成上游的一致性资产」，**后两条是同一个动作的副作用**。第 4、5 条**引擎自己产出的记录触发不到**（状态恒为 3 字符、属性键是 OTel 点分名），实际影响为零。之所以一条不少地列出来：这条声明从「唯一一处」一路走到「五处」，**每一步都是被语料推翻的**——最初的 44 组对照全是 ASCII 状态 + 点分键名，**结构上就覆盖不到第 3、4、5 条**，直到补了非 ASCII / 类型不符 / 需引号键名 / 短状态 + color 的边界语料才逐条现形。断言的覆盖面就是语料的覆盖面，写「只有 N 处」之前先问手里的语料能不能证伪它。

### 日志落地与持久化（框架不负责那一半）

**默认档只写 stdout，它本身不是持久化**：进程把行写进 fd 1，落盘 / 轮转 / 保留归平台。
这条划分是有意的（12-factor 第 11 条）：应用不管文件系统，平台管，两者用 stdout 解耦。

| 路径 | 做法 | 轮转 / 保留归谁 |
|---|---|---|
| 容器 / k8s | 默认出口即可 | 运行时：docker `log-opts max-size/max-file`、kubelet `containerLogMaxSize/containerLogMaxFiles`——**默认值往往很小或无限增长，要显式设** |
| systemd（单机） | 默认出口即可（stdout 默认进 journald） | `journald.conf` 的 `SystemMaxUse` / `MaxRetentionSec`；查用 `journalctl -u <svc>` |
| 直接落文件 | `os.OpenFile(…, O_APPEND\|O_CREATE\|O_WRONLY, 0o644)` + `WithSink(NewConsoleSink(f))`；要同时给人看和送采集器用上游 `observability.MultiSink{…}` | **宿主自备**（框架零依赖，不内置轮转） |

**丢失窗口按缓冲层级**：`ConsoleSink` 无缓冲（只丢内核 page cache 未回写的那一段）→
`LineSink` 32 KiB（引擎在优雅关闭时会 `Flush`）→ `AsyncSink` 队列（满时按策略丢）。
要更强保证得自己 `fsync`（每请求毫秒级代价，access log 通常不值）；要「一条不丢」的
语义，那不是日志通道的事。

**明确不做**：轮转 / 保留 / 压缩（属宿主或第三方，引 lumberjack 是独立决定）、fsync 策略、
采样丢弃、把写错误塞进请求路径。这些要动，先开票（见 #22）。

### 打点入口（复用上游，不新造协议）

| 场景 | 实现 |
|---|---|
| 请求内业务打点 | `Ctx.Observe` **直写 Sink**（`writeObservation`，信封填充与 `Collector.write` 同构）——不注册 Collector，因此零 scope 开销 |
| 宿主交付请求 scope 的库对象 | `WithCollector()` → `observability.AttachCollector`（作用域局部绑定，随 scope 销毁撤除）；插件**自己取不到**（宿主交付才可用），理由见下 |
| AccessLog / panic | 宿主**直写 `observability.Record`**——需要 `Duration` / `Err`，而 Collector 明确不带这两项（`collector.go:76` 注释：状态型事实） |

**`WithCollector()` 的可见性边界**（实测，五条）：

| 读方 | 读得到 |
|---|---|
| 请求 scope 自身 | ✅ |
| 请求 scope 的后代 | ✅ |
| 宿主 root | ❌ |
| 插件私有 scope（与请求 scope 是**兄弟**） | ❌ |
| 并发请求各自的 scope | ✅（互不遮蔽，各持自己的 TraceID） |

**同一形状还堵死了插件的另一条路**：插件在自己 scope 上 `kernel.On` 注册的监听器**收不到**请求 scope 的 `EmitLocal`（只派发本层）；插件收得到的是全树 `Emit`，而它比 `EmitLocal` 慢 **77 倍**（1778 ns vs 23 ns，50 插件树同轮实测），是红线 1 明令禁用的。

**所以：kernel 插件在请求路径上没有「自取」通道。** 这不是缺陷——局部绑定兄弟不可见是上游 `#170` 的有意修复（避免并发串台），`EmitLocal` 只派发本层是它的定义；但两者叠加的结果此前无人文档化，读者要自己画作用域树才能推出来。

已上报上游 [pulse#189](https://github.com/Luo-root/pulse/issues/189)，上游**核销为不改代码**，并把表述收正为「没有**交付通道**」——机制一直都在，缺的是插件在请求路径上如何拿到那份请求身份。**插件要参与，走宿主交付，交付物二选一**：上表那两条——**请求上下文**（同步调用给 `*Ctx`、跨 goroutine 给值袋子 `Detached`；两者都是 `Observe` 直写 Sink、与请求共享 TraceID，零 scope 开销。`Ctx` 不可跨 goroutine，见运行时契约），或**请求 scope**（`WithCollector()` + `kernel.Get(CollectorKey)`，每请求 +349 ns / +12 allocs）。两条都不要求插件自己 lookup，也不用碰 `EmitLocal` 的传播范围。

上游 #189 的探针实测了同形结论：插件只吃一份 per-request cfg（`kernel/flow/observer_record.go:34` 的 `NewRecordObserver(cfg)` 形态）即可参与，且与宿主在请求 scope 上的直写落在**同一个 TraceID** 下。

**不存在自定义的 Observe 协议**——`c.Observe` / `Detached.Observe` 都是 `Collector.write` 的薄包装。

### `http.request` 记录字段（对齐 OTel HTTP 语义约定）

| 位置 | 字段 | 说明 |
|---|---|---|
| `Record.HostID` | — | **必填**（D3 运行期记录约定）；来自 `WithHostID`，空则 `"pulse-web"` |
| `Record.TraceID` | — | 必填，32hex |
| `Record.Source` | `"http"` | 宿主自定义；**不用** `SourceAdapter`（历史名，字面值 `"bridge"`） |
| `Record.Event` | `http.request` | |
| `Record.Status` | — | 状态码字符串 |
| `Record.Duration` | — | 总耗时 |
| Attrs | `http.request.method` / `http.route` / `url.path` / `http.response.body.size` / `client.address` | |
| Attrs | `error.type` | 错误分类串；**与 `Record.Err` 并存**（`Err` 供出口输出错误文本、`error.type` 供聚合查询，OTel 亦然） |

**隐私边界**：Attrs 只有标量，无 payload 逃生舱——能记 method / status / duration / bytes，**记不了**请求体、响应体、prompt。

**与 OTel HTTP 语义约定的覆盖对照**（上游 semconv：HTTP server span）。「对齐」是指**同名字段用同一套语义**，不等于把 OTel 的属性表抄全：

| OTel 属性 | 等级 | pulse-web |
|---|---|---|
| `http.request.method` | Required | ✅ |
| `url.path` | Required | ✅ |
| `url.scheme` | Required | ❌ 不记（反代 / 终止 TLS 后框架这一层看到的 scheme 未必是真值） |
| `http.response.status_code` | Conditional | ⚠️ 记在 **`Record.Status`**（状态码字符串），不是 attr——与 `Duration` / `Err` 同理：状态型事实走记录字段 |
| `http.route` | Conditional | ✅（含路径模板，低基数） |
| `error.type` | Conditional | ✅（有错才写，与 `Record.Err` 并存） |
| `url.query` | Conditional | ❌ 不记（基数高，且常带敏感参数） |
| `client.address` | Recommended | ✅（对端地址，不解析 `X-Forwarded-For`——信任边界交给反代） |
| `network.protocol.version` / `server.address` / `user_agent.original` | Recommended | ❌ 不记（v1 范围外） |
| `http.request.body.size` / `http.response.body.size` | **Opt-In** | 只记**响应**侧：框架包了 `ResponseWriter`（顺手得到字节数），**没有包 `r.Body`**——量请求体要拦 body 读取，破坏流式语义且每请求多一层 |
| `http.request.header.*` / `http.response.header.*` / `http.request.size` / `http.response.size` | **Opt-In** | ❌ 不做（默认不记头 / 体：体积按请求数放大、会把 token / cookie 写进日志、破低基数聚合）。要记就在中间件里 `c.Observe()` 显式记，自己把关 |

需要「请求进来那一刻」的记录（而不是收尾那一条）**没有现成开关**：框架的访问日志刻意写在收尾（要 `status` / `duration`），要入口记录就在中间件里 `c.Observe()`——框架**不提供** `Trace()` / `AccessLog()` 这类可单独挂载的中间件，默认装配是一体的（`WithSink` 只换出口，不复活被 `Minimal()` 关掉的观测）。

### trace 头兼容与 span 出口

**入站**：优先读 W3C `traceparent` 的 32hex trace-id；其次 B3 `X-B3-TraceId`（**32hex 原样、16hex（Zipkin 64-bit）左垫 0 归一为 32hex**）；**全零按不存在处理**（W3C 明文禁止全零，B3 未禁止但同样按不存在——否则这些请求会在日志里共享同一条 TraceID，比链路断裂更难排查）；都没有则**框架自带生成器产出 32hex**（不用 `observability.NewTraceID()` 的异构格式——`traceid.go:19` 明确"返回值无契约语义、宿主可自带格式"）。

`traceparent` 的字段校验逐条对齐 W3C Trace Context 与官方 otel-go 的 `propagation.TraceContext`（[#76](https://github.com/Luo-root/pulse-web/issues/76)）：**两边的接受集合必须相同**，否则同一个请求在「框架的记录」与「宿主的 OTel 链路」里会拿到不同的 trace 身份。

| 字段 | 规则 |
|---|---|
| `version` | 2 位**小写** hex；`ff` 非法；更高版本按 `00` 的格式解析（尾部字段忽略） |
| `trace-id` | 32 位小写 hex，**全零非法** |
| `parent-id` | 16 位小写 hex，**全零非法** |
| `trace-flags` | 2 位小写 hex；`00` 版本不允许保留位（`> 3` 非法） |
| 长度 | `00` 版本必须恰为 55 字符（该版本不允许尾部字段） |

大写 hex 一律非法（规范写的是 `HEXDIGLC`）。**任何一处不合法 → 整条忽略、起新 trace**，不做部分采纳——只认 trace-id 而丢掉非法 parent-id 会让父子关系静默错位。解析失败时**不解析 `tracestate`**（W3C 明文要求），所以 `SpanInfo.TraceState` 只在 traceparent 通过校验时才透传。

B3 是历史兼容路径：单头 `X-B3-TraceId` 里**没有** span-id，因此走这条路的请求只有 trace-id、没有 parent——它不是 W3C 的等价物，span 出口拿到的是 root span。

**自生成 trace-id 的随机源不换（[#78](https://github.com/Luo-root/pulse-web/issues/78) 的实测结论）。** 曾建议换成更快的源，量下来三条实现（`go test -run '^$' -bench BenchmarkGenerateTraceID -benchtime=20000x -count=5 .`，同轮、32 P）：`crypto/rand + hex.EncodeToString`（现状）**91.5 ns / 32 B / 1 alloc**、`crypto/rand + 手写 nibble` 85.7 ns / 32 B / 1 alloc、`math/rand/v2 + 手写 nibble` 30.6 ns / 32 B / 1 alloc。三条**分配计数完全相同**——那一次分配是返回的字符串本身，`hex.EncodeToString` 的中间 buffer 被编译器证明留在栈上，手写 hex 也省不出来；而对照同轮的默认请求路径（1869 ns / 22 allocs），换源只能省约 61 ns（**3.3%**）却要放弃「不可预测」——那是 W3C 对 random-trace-id 的语义要求，`math/rand/v2` 明文不用于安全用途。结论：**不换**。三条实现都留在 `traceid_bench_test.go`（含被否掉的那条），下次有人再提这条建议时先看那张表。

**出站是两个响应头，别混**（[#76](https://github.com/Luo-root/pulse-web/issues/76)）：

| 头 | 形态 | 什么时候写 |
|---|---|---|
| `X-Trace-Id` | 32hex trace-id，**不含 span** | 任何观测开启的模式（`Minimal()` 关掉 trace 时没有）。本框架的**既有契约**，自定义头、非标准 |
| `Server-Timing: trace;desc=00-<trace-id>-<本请求 span-id>-<flags>` | W3C Trace Context 定义的**响应侧**绑定（规范里 `desc` 就是 `traceparent` 那四个字段） | 只有拿到 span 身份才有：装了 `WithSpanHook` 且 hook 给出了 span-id |

**响应侧不写 `traceparent`**：W3C 只在**请求**侧定义它，客户端也不会去读响应里的这个头——响应侧的标准形态就是 `Server-Timing` 的 `trace` 指标。旧口径「不写 traceparent、不编造假 span-id」的前半句仍然成立，后半句在本票之后收正为**框架不编造 span-id**（理由见下）：框架没有 span 时，两个头里带 span-id 的那个**不写**，而不是填一个假的。

**被代理剥离时的排查顺序**：`Server-Timing` 是标准头，但 WAF / CDN / 老网关对不认得的响应头并不都原样透传；`X-Trace-Id` 是自带头，被剥掉的概率低得多。所以文档里给的排查顺序是**访问日志的 `trace=` 与 `span.id` → `X-Trace-Id` → 最后才怀疑 `Server-Timing`**——这也是保留 `X-Trace-Id` 这条既有契约的现实理由之一（不只是兼容）。

### span 出口：`WithSpanHook`

**谁会用它**：把 pulse-web 接进**既有追踪体系**的宿主装配方——手里已经有一个 OTel `TracerProvider`（或自研 APM 的 span 模型），要的是「这个请求在那套体系里成为一条真实 span」，而框架的 `Record` 只是附带的日志产出。框架自己不引任何追踪 SDK（主模块零第三方依赖是红线），所以这里只留一条**缝**；官方适配在独立 nested module [`otel/`](https://github.com/Luo-root/pulse-web/tree/main/otel)。

```go
type SpanHook interface {
    Begin(ctx context.Context, in SpanInfo) (context.Context, SpanRef)
    End(ctx context.Context, sp Span)
}

app := web.New(web.WithSpanHook(otelweb.New(tracerProvider)))
```

`Begin` 做两件事，缺一不可：① 建 span、把它的 context 注入 `ctx` 并返回——下游支持 OTel 的库（出站 HTTP 客户端 / otelsql / otelgrpc…）因此自动接上这条链路；② 把 span 身份回给框架。`End` 拿到的是完整请求事实（状态码 / 耗时 / 路由模板 / 错误分类 / 属性）。

三条契约：

1. **框架不编造 span-id。** SDK 没有「指定 span-id」的入口（id 来自 provider 的 `IDGenerator`），而 `Server-Timing`、记录里的 `span.id`、下游 client 注入的 `traceparent` 三处必须是**同一个真实存在的 id**——框架自己造一个，会让下游把它当成父 span 挂到一条不存在的链路上。所以框架在 `Begin` 里向 hook **索要**身份（`SpanRef`）并采用它；`SpanRef.SpanID` 为空表示本次请求没有 span，框架就不写 `Server-Timing`、不记 `span.id`。
2. **默认不装。** 不装时请求路径与本选项出现之前逐字节相同（分配门禁量的就是这一档）；装与不装的差别是「请求路径上有没有一份 span 数据」，不是「有没有观测」。传 `nil` 在装配期 panic——装了却没有出口是配置错误，不是静默关闭。
3. **一个存放点。** span 身份存在 `Ctx` 里（`Ctx.SpanID()`，未装 hook 时是空串），不拆进 context 值——否则 404 这类**没走到注册 handler** 的路径会丢数据（见 `context.go` 注释）。代价是 `Ctx` 定长多 16 字节，见「表 B」的说明。

**代价（可复跑）**：`go test -run '^$' -bench 'BenchmarkEngineRequestPath$|BenchmarkEngineRequestPath_SpanHook' -benchtime=20000x -count=5 ./bench/` —— 同一请求路径装与不装一个「只回身份」的 nop hook 之差是 **+315 ns / +505 B / +4 allocs 每请求**（1838 → 2153 ns、6314 → 6819 B、**22 → 26 allocs**）。这个差里含两次 hook 调用、第二遍属性填充（span 那份不带 `span.id`，记录那份带，见 `emitSpan`）与 `Server-Timing` 的字符串拼装。适配件与 SDK 那一侧的完整代价（含属性转换的 6 次分配）在 `otel/` 的基准里量，两边不要相加——口径不同。

**span 属性与访问日志同源**：两边由同一个 `fillRequestAttrs` 填出，所以不可能各说一套。span 名按 semconv 用 `{method} {http.route}`——**不得退回 URI 路径**（semconv 明文 `MUST NOT`，404 场景下每个打错的路径都会变成独立的 span 名，APM 索引基数会爆），路由未知时退化为 `{method}`。

**方法值归一只在 span 侧做**（口径与官方 otelhttp 的 `standardizeHTTPMethod` / `HTTPServer.method` 逐条对齐）：已知方法规范化成大写（`get` → `GET`），**不认识的写 `_OTHER`**（原始值另放 `http.request.method_original`），未知方法在 span 名里退化为 `HTTP`（不是 `HTTP GET` 那种拼法）。框架的**记录侧保留原始方法不归一**——日志给人读，`purge` 就该写成 `purge`。两边属性名相同、取值口径这一处不同是**有意的**，所以钉了 `TestRecordKeepsRawMethod` 守着，免得日后被当成 bug 修掉。

**4xx 不写 `error.type`**（semconv：成功完成的请求 SHOULD NOT 设该属性），5xx 才写——框架日志侧的 `error_` 分类是另一套词表，两边各有其主。

**有意不记的三个 semconv 属性（对规范的显式偏差，不是漏记）**：`url.scheme` / `server.address` / `network.protocol.version` 在 semconv 里都是 Recommended（非 Required），本框架不产出。理由是它们记的是「我这台服务自己的监听信息」而不是请求事实：反代后面 `url.scheme` 拿到的是内网 scheme，照记等于写错；`server.address` 同理是监听地址/Host，不是调用方看到的地址；`network.protocol.version` 只对 HTTP/1.1 与 HTTP/2 有意义且与业务无关。要这三个事实，宿主在**边缘那一层**记才是对的。补记任何一个之前先想清楚「谁才是这个事实的事实源」——同步写在 `fillRequestAttrs` 的注释里。

**`c.Detach()` 的后台任务用 link、不用父子**：后台任务活过请求，做成子节点会让父 span 的时长语义失真（父早已结束、子还在跑）。所以框架**不替后台任务建 span**，而是把身份交出去：`Detached` 值袋子带 `TraceID` + `SpanID`（两者都与请求同期失效无关，是拷贝出来的字符串），宿主在追踪体系里建一条**显式 link** 指回那条 span。两端的测试：`TestDetachCarriesSpanIdentity`（值袋子确实带的是发起请求的那个 span，未装 hook 时 `SpanID` 为空串——`TestDetachWithoutSpanHook`）与 `otel/otelweb_test.go` 的 `TestDetachedWorkLinksToRequestSpan`（真 SDK：link 指向请求 span、后台 span **没有** parent、框架没有多导一条 span）。

**`Minimal()` 与 span 出口互不相干**：`Minimal()` 关的是框架自己的 trace 与访问日志（不写 `X-Trace-Id`、不写记录），装了 `WithSpanHook` 照样走 `Begin`/`End` 并写 `Server-Timing`——那是 span 出口给的，不归 `Minimal()` 管。`TestMinimalWithSpanHook` 把这条分工钉住。

**`c.Detach()` 的后台任务用 link、不用父子**：后台任务活过请求，做成子节点会让父 span 的时长语义失真（父早已结束、子还在跑）。框架**不替后台任务建 span**，而是把身份交出去：`Detached` 带 `TraceID` + `SpanID`（拷贝出来的字符串，跨 goroutine 安全），宿主用它建一条**显式 link** 指回那条 span。两端各有用例：主模块 `TestDetachCarriesSpanIdentity` / `TestDetachWithoutSpanHook`（没装 span 出口时 `SpanID` 是空串），`otel/` 的 `TestDetachedWorkLinksToRequestSpan`（真 SDK：link 指向请求 span、后台 span **没有** parent、也没有多导出第三条 span）。

**APM 拓扑降级声明**（不装 span 出口时）：span 树型后端（Jaeger / Zipkin / SkyWalking）里，pulse-web 的记录是该 trace 下的**独立节点**；日志检索型后端（ELK / Loki）不受影响。装了 span 出口即补上父子边（本服务那条 span 的 parent 来自入站 `traceparent`）。

### 装配诊断

v1 只做当前视图：`app.Debug("/debug/pulse")` 输出 `kernel.FiberSnapshots()` 的 JSON（默认关）。历史 loader 动作**不另存**——`Bootstrap` 已把 `loader_action` 写进 Sink，去日志看。

## 明确不做（v1 之外，各有去路）

| 项 | 去路 |
|---|---|
| `AsyncSink`（队列 / Drop / flushTimeout） | **上游已提供**（`observability.NewAsyncSink`，v0.2.1）——web 不另造缓冲层，`WithSink` 接入即可。注意组合语义：`AsyncSink.Flush` 只排空**它自己的**队列、不级联 inner 的 `Flush`，所以异步化的正确组合是 `NewAsyncSink(SlogSink)`；用 `AsyncSink` 包另一个缓冲出口（如 `LineSink`）会留下未落盘的内层缓冲，框架无从代劳 |
| `Sink.Close`（停协程） | 不做——出口所有权属装配方：`Detach` 允许进程级后台任务继续写同一 Sink，框架在关闭时 `Close` 它会静默丢弃这些记录。关闭时序只负责 flush 并记错误 |
| 流式双记录（Flush 启发式） | 不做——普通 handler / 中间件的 `Flush()` 会误判；SSE 的语义已由"handler 不返回 ⇒ AccessLog 晚写"覆盖。需要"流开始"再显式另开票 |
| 响应侧 `traceparent` 回写 | 不做——W3C 没有给响应定义这个绑定，响应侧用 `Server-Timing`（见上）；框架也**不编造 span-id**，没有 span 就不写带 span-id 的那个头 |
| TTFB / Content-Type 观测 | 不做（TTFB 依赖包装器状态，与"避免额外分配"冲突），另开票 |
| `Detached` 的迷你生命周期（锁 / 懒派生 scope / ErrDetachedDisposed） | 不做——值袋子 + 调用方自理 |
| 内部 `Router` 接口 | 不做（YAGNI）：要换底层时再抽 |
| `Debug` 的 loader 历史环形缓冲 | 不做（Bootstrap 已入 Sink） |
| 多钩 `OnShutdown` + 独立预算 | 不做——单回调 + 同一 deadline 已够 |
| `Recover()` 中间件 | 不做（Engine `defer` 兜底已覆盖） |
| `HoldScope()` / `Release()` | 不做（延长请求 scope 会破坏"请求结束即回收"） |
| `RunTLS` | 不做（`tls.NewListener` + `Serve` 或反代） |
| 请求级数据对 kernel 插件**通用**可见 | 不做——上游 v0.2.1 已给出 `kernel.Local()`（这条"属上游改动"的阻塞已解除），但把整个请求 KV 袋挂进 scope 是每条绑定 ≈ +250 ns / +12 allocs（同轮实测，表 B），且语义上把"通用容器"当成"语义性绑定"。只做 `WithCollector()` 这一处显式、边界清楚的用例 |
| 内置 agent / LLM 相关的观测与装配接线 | 不做——pulse-web **只依赖 kernel 与 observability**，与 pulse 其余组件（llm / loop / host / toolset…）无耦合；需要时由调用方在自己的装配代码里显式接入 |

## 仓库结构

```
pulse-web/
├── README.md                  # 英文主版：结论与入口（展示性内容归站点，见下）
├── README_zh.md               # 中文版；与主版的结构等价由 docs_test.go 的守卫保证
├── go.mod                     # module github.com/Luo-root/pulse-web（package web）
├── doc.go                     # 包文档（定位 / 快速开始 / 运行时契约）
├── engine.go                  # Engine、选项、Root()、路由注册与分组、Static、
│                              # ServeHTTP（时序）、Engine 层收尾、
│                              # Run / Serve（信号注册）/ serve（执行体）、Handler、OnShutdown
├── context.go                 # Ctx、请求级 KV、响应写出（Writer / Flush / JSON / Text）、responseWriter 包装器
├── bind.go                    # 请求体 / query 绑定：Content-Type 分派 + form / query 映射器（#32）
├── bodylimit.go               # BodyLimit 中间件：按路由 / 分组限请求体（#38）
├── errors.go                  # HTTPError / StatusCoder / PanicError / 默认 mapper
├── observe.go                 # TraceID 生成与上游头解析（32hex）
├── wrap.go                    # stdlib 互操作（Wrap）
├── detach.go                  # Detached 值袋子（跨 goroutine 的安全值）
├── templates.go               # html/template 薄封装 + web.H
├── console_sink.go            # 默认出口：给人读的列式单行（见「默认出口」——薄壳 + 版式渲染器）
├── debug.go                   # 装配诊断端点（FiberSnapshots 的 JSON 视图）
├── testing.go                 # 测试入口：NewTestContext / ServeTest（装配与真路径同源，#63）
├── *_test.go                  # 与源文件同包（无独立 xxx_test 包），黑盒走 Engine 入口
├── assets/
│   ├── logo.svg               # 品牌 mark（48×48，currentColor；参数见「品牌标识」）
│   ├── banner.svg             # README 头部锁定：mark + 字标（mark 是同一份几何，内联）
│   └── favicon.svg            # 16px 简化版（5 柱）+ 明暗自适应
├── bench/                     # 性能回归基线 + 分配预算门禁（go test ./bench/）
│   └── muxprobe/              # 路由选型的一次性实测程序（「路由选型的边界」的数据来源）
├── docs/design/               # 设计文档（本文件）
├── LICENSE                    # MIT
├── CONTRIBUTING.md            # 贡献流程：Issue 五段 / PR 六段 / 本地门禁 / review 约定（中英双语）
├── SECURITY.md                # 漏洞上报与范围界定（中英双语）
├── CODE_OF_CONDUCT.md         # Contributor Covenant v2.1（官方英文原文 + 中文导读）
├── AGENTS.md                  # 给 AI coding agent 的仓库指南
└── .github/
    ├── PULL_REQUEST_TEMPLATE.md
    ├── ISSUE_TEMPLATE/        # bug_report.yml / feature_request.yml / config.yml
    └── workflows/ci.yml       # build / vet / gofmt 判空 / test -race / 分配预算 / bench 编译检查
```

**协作规范面**（`LICENSE` 与四个规范文件、`.github` 下的模板）不是框架设计的一部分，但同样是仓库的事实源：流程规则改**这些文件**，不要只在 Issue 评论里约定——评论会沉，文件不会。四份文件的契约关系是：`CONTRIBUTING.md` 管代码怎么提，`CODE_OF_CONDUCT.md` 管人怎么相处，`SECURITY.md` 管漏洞往哪报，`AGENTS.md` 管 agent 怎么在这个仓库里干活。

**文档分层**：**设计文档 = 数据与契约的事实源，站点 = 公开展示面，README = 结论与入口**。推论有三：① 性能跑分、对比表、图表这类**展示性内容不进 README**（进站点），部署运维细节（容器日志轮转、journald 配置）同理；README 只留一句结论 + 链接。② README 分中英两版（`README.md` 英文主版 / `README_zh.md` 中文版），两版的**结构等价**由守卫保证（`##`/`###` 数、各语言代码块计数、相对引用目标集合、互相链接）——措辞各语言自己地道，骨架不许各有各的。③ README **不写状态计数**（「已实现 N/N」这类）：它是内部进度的话术，且每次清单增删都要跟着改；进度的事实源是本文档的验收清单。

## 品牌标识

mark 的走势**直接沿用 pulse**（平段 → 上升 → 峰值 → 深谷 → 回升 → 平段）：两个库并排出现时读作同一族（同一条心跳），但形态不同（折线 → 柱阵），不会被认错。**为什么是柱而不是折线**——pulse 的折线是「信号」，柱阵是「信号被切成一根根样本」，与 pulse-web 的观测口径（每请求一条记录）同构。

**参数口径**（48×48 画布，`assets/logo.svg`）：

| 参数 | 值 |
|---|---|
| 基线 | y = 27（与 pulse 原折线同一位置） |
| 柱数 / 柱宽 / 间距 | 9 / 3 / 1.2 |
| 振幅序列（左→右，向上为正） | +4, +6, +12, +23, −10, −17, −8, +4, +4 |
| 落差 | 40px（峰 +23 → 谷 −17） |
| 圆角 | 1.2 |
| 峰值黑点 | **不保留**（pulse 原 mark 有，本 mark 去掉） |

**颜色**：mark 走 `currentColor`，由宿主决定——内联使用时跟随 `color`，站点可用选择器覆盖。作为 `<img>` 独立渲染时（README / social preview）没有宿主 color 可继承，所以 SVG 内嵌 `svg { color: … }` 给出明暗两档中性色（`#17181a` / `#f4f3f0`，按 `prefers-color-scheme` 切）；**强调色不在这里定**，留给站点那一轮。

**字标**：`Pulse-Web`，**与 pulse 同一套 sans 栈**（`-apple-system, 'Segoe UI', 'Helvetica Neue', Arial, sans-serif`，700 字重）——两库并排时是同一族字面；连字符用字体自带字形（bold sans 的连字符本身就是与字重匹配的短横条）。字号 / 基线取 pulse banner 的同一口径（46 / y=64），文字用 `textLength` + `lengthAdjust="spacing"` 钉成定长（**只调字距、不变形字形**）：sans 的实际宽度随平台字体（Segoe UI / SF / Helvetica）略变，定长既不用为最宽的那家留大片空白，也不会在窄的那家溢出被裁。两种落地：README 头部的**锁定**用 `assets/banner.svg`（mark + 字标一体，对齐 pulse 的呈现方式，不把 mark 单独留在标题上方）；站点首页用**自绘落地页 + CSS 字标**（`.pw-wordmark`，46px / 700 / 同一套 sans 栈）实现同一规格。

**站点首页不用默认主题的 `hero:` / `features:` 模板**：那套是「大标题 + 卡片 + 阴影」，与本站的版式口径（中性面为底、一条 hairline 分栏、mono 编号）不是一路。落地页改成 `layout: page` + 自绘标记（mark 与字标锁定、规格条、能力面 hairline 网格、默认输出终端块、入口宫格），颜色全部走 VitePress 主题变量。`site_test.go` 把字标规格做成断言（字体栈 / 字号 / 字重逐字等于 `assets/banner.svg` 里的 `<text>`），「改了图没改站点」会先红。banner 里的 mark 与 `assets/logo.svg` 是**同一份几何**（同坐标、同圆角，只是内联并缩放到 1.3——测试会逐项比对）。

**favicon**（`assets/favicon.svg`）：48 单位画布缩到 16px 只剩 1/3，9 根柱每根 1 像素宽、缝 0.4 像素，抗锯齿会把墨摊薄（笔画发灰、缝半填）——形状仍读得出，但对比度掉一截，且不同渲染器/DPI 的降采样策略不一致。故降到 **5 根柱、柱宽 6**（同尺寸下柱心实黑）、保留峰与谷两个关键柱；颜色同 logo 写死明暗两档（favicon 不继承宿主 CSS）。**两份文件不是冗余**：一个是 96px+ 的宿主可控色 mark，一个是 16px 的独立自足图标，各解各的尺寸。

**同步面**：README 顶部引用 `assets/banner.svg`（mark + 字标一体）；站点那一轮从 `assets/` 复制到 `site/public/`（favicon 与 social preview 同源），**不在两处各维护一份**。改图后以 `assets/logo.svg` 为准回改本节参数表——`assets_test.go` 会逐项比对（表 ↔ 坐标、banner 里的 mark ↔ logo、favicon 的简化规则、README 相对路径可达），漂移时测试先响，不靠人工比对。

## 验收标准

> 每条的**证据**都写成可复跑的样子：测试名可直接 `go test -run <名> ./...`；实测记录指到对应章节。
>
> 引用这些条目时**用条目名，不要用序号**——写成 `设计验收标准『性能回归』条`，名字逐字取本条开头的加粗短语。序号不是标识而是**位置**：清单是插队长的，第 10 位插一条，其后所有序号后移，而引用它的文件不会自己更新（本清单从 10 条长到 13 条，期间 3 处序号引用全漂）。名字只在条目被改名时失效，而那会立刻被 `TestDesignCriterionNamesAreUsedInReferences` 抓住；该用例同时反向断言全树不再出现按序号的写法——覆盖 `验收标准第 N 条` / `设计验收标准第 N 条` / `v1 功能面第 N 条` 三种历史形态，扫描面是 `.go` / `.md` / `.yml`。

- [x] **垂直切片可跑**：`app.Run()` 起服务，路由 / 中间件 / JSON / 优雅关闭全通
  证据：`TestServeSignalRunsFullShutdownChain`（注入信号 → drain 在途请求 → `OnShutdown` → `root.Dispose` → Sink flush 全链路）、`TestServeReturnsServerError`（server 出错透出）、`TestRunListenFailureDisposesEngine`（监听失败回收引擎）；路由 / 中间件 / JSON 见 `TestRouterJSONAndPathParam`、`TestGroupAndMiddlewareOrder`。
- [x] **WithRoot 接入既有 kernel 树**：双方 `Provide` 的服务彼此可见（同一 IoC 容器）
  证据：`TestWithRootAcceptsPreinstalledTree`——外部树先装插件、web 侧 `Provide` 后 handler 读得到，Bootstrap 仍产出快照记录。
- [x] **默认路径零全局 Provide**（benchmark 不随插件数线性涨）；`WithCollector()` 后是作用域局部绑定，实测同样与插件树规模无关
  证据：分配门禁 `bench/budget_test.go` 的 `plugins=50` 档与空树同为 22 allocs/op；表 B 的 10 / 50 / 100 插件三档 allocs 恒为 14。
- [x] **业务打点与 AccessLog 同 Sink**：同一 `TraceID` / `HostID`；Source 分别为 `"http"`（AccessLog）与 `"bridge"`（`c.Observe` **直写**——该字面值是上游 `observability.SourceAdapter`，此路径不注册 Collector）
  证据：`TestAccessLogRecordFields` 与 `observe_test.go` 里的业务打点断言（同一 Sink、同一 TraceID、Source 为 `SourceAdapter`）。
- [x] **观测贯穿**：单请求 TraceID 在 router → handler → Sink 一致；后台任务共享同一 TraceID
  证据：`TestTraceIDGeneratedAndSharedAcrossRecord`、`TestDetachSharesTraceAndRootAccess`；入站头采纳另见 `TestTraceparentAdopted` / `TestB3TraceIDAdopted` / `TestB3TraceID16HexNormalized`（16hex 左垫归一）/ `TestZeroTraceIDTreatedAsAbsent`（全零视为不存在）/ `TestMalformedTraceHeadersIgnored`。
- [x] **span 出口可接**（[#76](https://github.com/Luo-root/pulse-web/issues/76)）：`WithSpanHook` 把请求的 span 身份交给追踪体系（官方适配在 `otel/` nested module）；**框架不编造 span-id**，两个响应头各写各的，入站 `traceparent` 的校验与官方 propagator 同口径
  证据：`span_test.go` 17 条——请求事实与访问日志同源（`TestSpanHookCarriesRequestFacts`）、注入到达中间件与 handler（`TestSpanHookInjectionReachesHandler`）、**404 也保住注入的 context**（`TestSpanInjectionOnUnmatchedRoute`）、`X-Trace-Id` 与 `Server-Timing` 各写各的（`TestSpanTwoResponseHeaders`）、采用 hook 的身份（`TestSpanAdoptsHookIdentity`）、入站 parent 与 flags（`TestSpanParentFromInboundTraceparent`）、严格解析 4 合法 / 10 非法逐条（`TestTraceparentStrictParsing`）、B3 只给 trace-id（`TestB3GivesTraceIDOnly`）、panic 与映射错误后的状态码（`TestSpanOnPanicAndMappedErrors`）、`nil` 装配期 panic（`TestSpanHookNilPanics`）、handler 读得到 span-id（`TestSpanIDIsReadableInHandler`）；`otel/otelweb_test.go` 12 条——server span 形状（`TestServerSpanFromRequest`）、入站父（`TestServerSpanAdoptsInboundParent`）、状态语义四档（`TestStatusSemantics`）、**下游注入的 parent-id 就是本请求 span-id**（`TestDownstreamPropagationUsesServerSpanID`）、记录里的 `span.id` 与导出 span 一致（`TestAccessRecordCarriesSpanID`）、**与官方 propagator 的差分对照 16 条**（`TestParsingAgreesWithOfficialPropagator`）、**span 名永不使用 URI 路径**（`TestSpanNameNeverUsesURIPath`，含 404 的几种形态）、**方法归一三档**（`TestMethodNormalizedPerSemconv`）、**注入的 context 走完边角路径**（`TestContextChainSurvivesEdgePaths`：未匹配路由 / panic / 映射错误 / `Wrap` / 中间件，判据是 span 被正常结束并导出）、**并发不串台**（`TestConcurrentRequestsDoNotCrosstalk`，64 并发 + `-race`）、**没有 span 身份时不编造**（`TestSpanNoIdentityWritesNoSpanID`：不写 `Server-Timing`、记录里没有 `span.id`）、**`Minimal()` 与 span 出口的分工**（`TestMinimalWithSpanHook`）、**`Detached` 带的是发起请求的那个 span**（`TestDetachCarriesSpanIdentity` / `TestDetachWithoutSpanHook`）、**关掉入站头信任后 span 侧同样新起 trace**（`TestUntrustedTraceHeaderSkipsInboundForSpan`）、**tracestate 原样透传**（`TestTraceStatePassedThroughVerbatim`）、**后台任务建 link 而不是父子**（`TestDetachedWorkLinksToRequestSpan`，真 SDK），外加钉住「记录侧保留原始方法」的 `TestRecordKeepsRawMethod`。
- [x] **标准库兼容**：挂载 stdlib 中间件无侵入；`Wrap` 双向适配
  证据：`TestWrapStdlibHandler`、`TestEngineUnderStdlibMiddleware`、`TestWrapPanicCaughtByEngine`。
- [x] **ServerConfig 契约**：6 个默认值 + 「非零覆盖、零值保持默认」+ 配置**真的**落到 `http.Server` 上
  证据：`TestDefaultServerConfigValues`、`TestWithServerMergesNonZeroFields`、`TestServerConfigReachesHTTPServer`（1 KiB 上限下超限请求头被拒 431）。
- [x] **流式响应可用**：`c.Writer()` + `c.Flush()` 逐段推送（SSE），首刷（= 第一次写出：`Write` / `Flush` / 引擎收尾）落 `Status()` 设置（缺省 200）；底层不支持 `http.Flusher` 时返回明确 error；**仍是一条 AccessLog**（状态码与体积照常采集）
  证据：`TestCtxFlushStreamsIncrementally`（第一段在 handler 仍挂起时已到达客户端——只有真 flush 做得到；同一条用例断 `Status="200"` 与 `http.response.body.size=18`）、`TestFlushFirstWriteUsesStatus` / `TestFlushFirstWriteDefaultsTo200` / `TestStatusAfterFlushIgnored`（首刷吃 `Status`，首刷后不可改）、`TestStatusAppliedOnFirstWrite` / `TestStatusAfterWriteIgnored`（直接写字节同样是首刷）、`TestCtxFlushWithoutFlusherReturnsError`、`TestResponseWriterKeepsFlusherCapability`（能力边界：`Flusher` ✅，`FlushError` / `Hijacker` / `Pusher` / `SetWriteDeadline` ❌）。
- [x] **请求体绑定完整**：`Ctx.Bind` 按 Content-Type 分派（JSON / XML / form-urlencoded / multipart；无 body 落 query），`BindQuery` 显式 query；`WithMaxBodyBytes` 上限对**全部**读取路径生效（超限 → 413 + `body_too_large`）
  证据：`bind_test.go` 19 条——分派（`TestBindJSON` / `TestBindXML` / `TestBindForm` / `TestBindFormQueryIsNotMerged` / `TestBindMultipart` / `TestBindQuery` / `TestBindUnsupportedMediaType` / `TestBindNoContentTypeFallsBackToForm` / `TestBindContentTypeWithParameters` / `TestBindJSONSlice`）、映射（`TestBindMoreScalarKinds`）、上限（`TestBindMaxBodyBytes` 读取闸门 + Content-Length 预检两路径、`TestBindURLEncodedOverLimit` 10 MiB 解析闸有 / 无 CT、`TestBindUserWrappedMaxBytesReader` 用户自包、`TestBindDefaultNoLimit` 默认不限）、健壮性（`TestBindMalformedInputs` / `TestBindJSONTrailingData` / `TestBindTargetErrors` / `TestBindQueryTargetError`）。
- [x] **按路由 / 分组限请求体**：`BodyLimit(n)` 中间件可挂分组与单条路由，与引擎级 `WithMaxBodyBytes` 叠加时**取最严**（只能收紧、不能放宽）；超限 413 + `body_too_large`，cause 与既有两条超限路径同型（`*http.MaxBytesError`）；补导出构造器 `TooLarge`（#38）
  证据：`bodylimit_test.go` 10 条——分组（`TestBodyLimitOnGroup`）、路由与分组叠加（`TestBodyLimitOnRoute`）、边界（`TestBodyLimitBoundary`：恰好 n 通过 / n+1 得 413）、声明未知 chunked（`TestBodyLimitChunked`）、只能收紧（`TestBodyLimitCannotLoosen`：引擎级 1 KiB 拦得住路由级 1 MiB）、预检不读 body（`TestBodyLimitPrecheckRejectsDeclaredOverLimit`）、cause 同型两条路径（`TestBodyLimitCauseType`）、`n <= 0` 直通（`TestBodyLimitNonPositiveIsPassThrough`）、构造器（`TestTooLargeConstructor`）、真实连接上「超限关连接」语义不丢（`TestBodyLimitKeepsCloseConnection`：断言响应带 `Connection: close`，即 `MaxBytesReader` 的 `w` 拿到的是原始 writer 而非包装器）。
- [x] **写出 / 错误语义收口**：`c.JSON` 先编码成功才写头（失败 → 500 统一错误体，不再 200 空体）；`c.Flush` 与包装器 `Flush` 同一实现、首刷吃 `Status()`；`panic(web.NotFound(...))` 一律 500（要 4xx 请 return）
  证据：`TestJSONEncodeFailureMappedTo500` / `TestJSONBytesStable`（尾换行保留）/ `TestPanicHTTPErrorMapsTo500`；Flush 三条见上一条。
- [x] **性能回归**：请求路径开销进入仓库 bench，作为基线不劣化
  证据：`bench/` 全套基准 + 分配预算门禁 `TestRequestPathAllocBudget`（CI 的 `Alloc budget` 步骤，不带 `-race` 执行）。
- [x] **真实负载与 gin 同级**（不以 micro-benchmark 胜负作承诺）——**已复采三轮、结论一致**：裸档 0.91× / 0.95×、观测档 0.82× / 0.93×（见「表 C」；采集工程在 `loadtest/`，CI 覆盖它的 build / vet / test）
