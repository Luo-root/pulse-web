# AGENTS.md

给 AI coding agent 的仓库指南。**事实源是代码与 [`docs/design/web-framework-design.md`](docs/design/web-framework-design.md)**；本文件只写「怎么在这个仓库里干活」，不重复设计文档里的决策论证。贡献流程（Issue 五段 / PR 六段 / 发版纪律）见 [CONTRIBUTING.md](CONTRIBUTING.md)，本文件只补它没写的部分。

## 这是什么

`github.com/Luo-root/pulse-web`——基于 [pulse](https://github.com/Luo-root/pulse) 的 **kernel** 与 **observability** 构建的通用 Go web 服务框架。库形态（与 gin / chi / echo 同级），**单包** `package web` 位于仓库根目录，不含前端。目前 pre-1.0（`v0.x`；最新 tag 是 `v0.1.0`，2026-09-16 发布）。卖点是**装配能力 + 一等观测**。

## 构建与测试

需要 **Go 1.27+**（`go.mod` 写 `go 1.27.0`，工具链缺失会自动下载）。没有 Makefile、没有 linter 配置——下面九条就是全部 CI 门禁（`.github/workflows/ci.yml` 的九个 step，一一对应）：

```bash
go build ./...                                            # 编译
go vet ./...                                              # 静态检查
"$(go env GOROOT)/bin/gofmt" -l $(git ls-files '*.go')     # 格式门禁：输出非空即失败
go test -race ./...                                       # 全部测试
go test -run TestRequestPathAllocBudget ./bench/          # 分配预算门禁——必须不带 -race
go test -run '^$' -bench '^$' ./bench/                    # bench 编译检查
(cd loadtest && go build ./... && go vet ./... && go test ./...)   # loadtest 独立 module——根 module 的 ./... 盖不到
(cd otel && go build ./... && go vet ./... && go test ./...)       # otel 适配件同理（带 otel-go 依赖，主模块零依赖靠它保住）
(cd interop && go build ./... && go vet ./... && go test ./...)    # interop 生态互操作对照同理（websocket / CORS / chi …）
```

Windows PowerShell 下格式门禁写成：

```powershell
& (Join-Path (go env GOROOT) 'bin\gofmt.exe') -l (git ls-files '*.go')
```

三条判据不是顺手写的，踩过：

- **`gofmt` 走 `$(go env GOROOT)/bin/gofmt`，不走 `PATH`。** `PATH` 上可能挂着旧版，解析不了泛型方法一类的新语法，表现为**全量**报错。症状是「gofmt 报错但 `go build` 通过」——先怀疑二进制，不是代码。
- **文件集来自 `git ls-files '*.go'`，不是 `gofmt -l .`。** 后者会连未跟踪目录一起扫。
- **分配预算门禁必须不带 `-race`。** race 检测器会抬高 `B/op`；`bench/budget_test.go` 用 `//go:build !race` 排除在 race 构建外，由独立 step 跑。

## 仓库布局

```
go.mod                  # module github.com/Luo-root/pulse-web（package web）
doc.go                  # 包文档：定位 / 快速开始 / 运行时契约
engine.go               # Engine、选项、路由与分组、Static、ServeHTTP 时序、Run / Serve、Handler
context.go              # Ctx、请求级 KV、响应写出（Writer / Flush / JSON / Text / Hijack）、responseWriter 包装
bind.go                 # 请求体 / query 绑定：Content-Type 分派 + form / query 映射器
errors.go               # HTTPError / StatusCoder / PanicError / 默认 mapper
observe.go              # TraceID 生成与入站链路头解析（32hex）
wrap.go                 # stdlib 互操作（Wrap / Adapt；Adapt 的代理透出 Flusher 与 Hijacker）
detach.go               # Detached 值袋子（跨 goroutine 的安全值）
templates.go            # html/template 薄封装 + web.H
console_sink.go         # 默认出口：给人读的列式单行（薄壳 + 版式渲染器）
debug.go                # 装配诊断端点（FiberSnapshots 的 JSON 视图）
testing.go              # 测试入口：NewTestContext / ServeTest（装配与真路径同源）
*_test.go               # 与源文件同包（无独立 xxx_test 包），黑盒走 Engine 入口
assets/                 # 品牌事实源：logo.svg / banner.svg / favicon.svg
bench/                  # 性能回归基线 + 分配预算门禁；muxprobe/ 是路由选型的一次性实测程序
loadtest/               # 与 gin 的真实负载对比（**独立 module**，带 gin 依赖；根 module 的 ./... 不过 module 边界）
otel/                   # 官方 OTel 适配（**独立 module**，带 otel-go 依赖；主模块只产出结构化 span 数据）
interop/                # 生态互操作对照（**独立 module**，带 websocket / CORS / chi 等真依赖；站点上公布的兼容结论由它守着）
docs/design/            # 设计文档（决策与验收清单的事实源）
.github/workflows/ci.yml
```

`assets/` 的三份 SVG 有测试守卫（`assets_test.go` 逐项比对参数表 ↔ 坐标、banner 里的 mark ↔ logo、favicon 的简化规则、README 相对路径可达）。**改图先改 `assets/logo.svg`，再按它回改设计文档的参数表**，别反向操作。

## 红线（review 会挡）

- **核心模块零第三方依赖。** 只允许 `stdlib` + `pulse/kernel` + `pulse/observability`。判据是**真正编译进去的东西**：
  ```bash
  go list -deps . | grep -E '^[^/]+\.[^/]+/'   # 只应有 pulse/kernel、pulse/observability、pulse-web 自身
  ```
  `go list -m all` **不是**这个判据——它列整个 module graph，含其中每个模块的间接依赖。
- **不要 import pulse 的其余部分**（`llm` / `loop` / `host` / `toolset` / `memory` / `skills` / `textsplit` / `flow`）。pulse-web 与它们零耦合；需要时由调用方在自己的装配代码里接。
- **单包，根目录。** 不放 `internal/`、不放 `cmd/`、不切子包——这是库不是可执行程序，发布的每个符号都是公开 API。
- **`Ctx` 不得跨 goroutine**（它与请求及其响应写出器绑定）；后台任务用 `c.Detach()`。
- **handler 签名 `func(*Ctx) error`**，错误靠返回；状态码由错误映射器决定。外部错误用 `web.NotFound` / `web.BadRequest` 这类构造器包，不要绕到 `c.Writer()` 手写。
- **stdlib 互操作三条路径都要在**：入用 `web.Wrap`，stdlib 中间件进洋葱用 `web.Adapt`，出用 `Engine.Handler()`。删掉任一条、或让其中一条需要手写适配器，都是回归。
- **响应写出器的能力面是一份显式的窄清单**：`Flush` / `Hijack` / `SetWriteDeadline` / `EnableFullDuplex` 四个显式实现并转发底层；`Pusher` / `FlushError` 不透出，也不提供 `Unwrap()`（那等于把底层 writer 整个交出去）。加能力是 API 决策，要一并定观测语义（见 `responseWriter.Hijack` 的 godoc）。
- **可配置处一律 functional options**（`WithSink` / `WithMaxBodyBytes` …）。
- **不允许逃生舱**：不加 `map[string]any` 式的「额外参数」，不做 vendor 特判，不留无类型属性袋子。
- **公开 API 必须有 godoc**；中文注释是本仓库常态，改到哪就沿着用同一种语言写，不要顺手翻译。
- **行尾 LF**，由 `.gitattributes` 强制。

## 在本仓库里干活的方式

- **先开 Issue，再写代码。** 任何改行为、改公开 API、改已文档化契约、改仓库结构的事都先开票（五段：做什么 / 不做什么 / 为什么 / 设计理念 / 验收标准）。错字与「明显属于某既有票」的小补丁可直接提 PR。
- **不直推 `main`**；分支命名 `feat/…` `fix/…` `docs/…` `chore/…` `test/…`。PR 有效改动按 **1000 行左右**控制。
- **每条验收标准都要有可复跑的佐证**：靠测试的写出测试名；靠实测的指到存数字的那一节。
- **守卫测试必须附变异探针**——改一处生产代码或资产让它失败，跑一遍确认抓到，再从 git 还原并核对字节一致。没被看着失败过的守卫不算守卫。探针临时文件放 `_scratch/`（已 gitignore），**不要留在仓库里**。
- **文档同步面**：行为或 API 变化要在同一个 PR 里改 `README.md`（面向使用者）、`site/` 指南（同一批读者，中英同步）与 `docs/design/web-framework-design.md`（契约、决策、验收清单）。改「N 条 / N 个」这类**计数词**、或会被别处引用的**成本数字**（`+NNN ns` / `NN allocs`）时，先 grep 全部同类措辞再下结论——两类都出过「换轮时只改了一处」。
- **换默认出口 / 动请求路径的 PR，同一个 PR 里重跑 `loadtest/`**：`cd loadtest && go run ./cmd/compare -probe -passes 4 -d 8s -warmup 2s`，并把设计文档表 C 与站点性能页（中英）一起更新。当场跑不了就在 PR 描述里写明「这一轮的负载数字不代表现状」——`SlogSink` → `ConsoleSink` 那次就是没人重跑，数字安静地错了一轮（#68 / #73）。
- **合并纪律**：`main` 开了 `required_conversation_resolution` + status checks `strict` + `enforce_admins`。PR 落后 `main` 要 `update-branch`，然后**按新的 head SHA 核对 check-runs**（`gh api repos/<slug>/commits/<sha>/check-runs`）再合——`gh pr checks` 可能返回更新前那一轮的结果。未 resolve 的 review 线程会挡合并（`mergeable_state` 会显示 blocked，别误判成冲突）。
- **合并动作由维护者做**。agent 的终点是「分支推送 + PR 开好 + CI 绿 + 自审结论 + 回报」。
- **外部意见（含其他 AI 给的 review）必须逐条探针实测后再采纳**，不要照抄结论。
- **`bench/gin-compare` 分支要保留**（历史对比数据），别清理；但**采集工程的现状在 `loadtest/`**——分支上是它挪进仓库之前的样子，别从那上面取数。

## 本机环境坑（Windows）

- **PowerShell，不是 bash。** 不要用 `cmd` / `dir` / `type` / `del`；用 `Get-ChildItem` / `Get-Content`。
- **外部命令的 `-flag=value` 先落变量**再传，避免被 PS 的参数绑定改写。
- **`gh` 发非 ASCII 内容走 JSON payload**：`write` 工具直出 `.json` 文件 → `gh api --input <file>`。不要经过 PS 管道，也不要塞进 argv。
- **不要用 `Get-Content | … | Set-Content` 改文件内容**（PS 5.1 默认 ANSI 会静默损坏 UTF-8）；用 `read`/`write`/`edit` 工具，或写 Python 脚本（`encoding='utf-8'`）。
- **删文件会被安全策略拦**：用移走（`_scratch/`）或移到备份位置，不要 `Remove-Item`。
- **基线对照用 `git worktree add --detach <dir> <base>`**，两边跑同一组输入再逐字节 diff。
- **Python 改文件时注意行尾**：CRLF 变 LF 要**两个条件同时成立**——读侧是文本模式（`read_text` / `open('r')`，会把 `\r\n` 规整成 `\n`）**且**写侧不回译（`write_bytes` / `open('wb')`，或 `open('w', newline='')`）。实测 6 种组合里只有这 3 种会变；「文本读 + 文本写」会被写侧把 `\n` 回译成 `\r\n` 而往返不变。要绝对稳妥就两边都用二进制。变了的后果是 `git status` 假报 modified（提交的 blob 不受影响）——用 `git checkout -- <file>` 还原。
