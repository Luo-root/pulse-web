# Contributing to Pulse-Web

Pulse-Web is an open-source Go web framework, still pre-1.0 (`v0.x`). Contributions are welcome.
The rules below are mostly about **how** work is proposed — not about who proposes it.

Both halves of this file are equivalent: English first, 中文在后. Read whichever you prefer;
if they ever disagree, the English text is the reference.

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

---

## English

### Before you start

- **Non-trivial changes start as an issue.** Anything that changes behavior, a public API, a
  documented contract or the repository layout opens an issue first — the issue is where the
  design gets settled, *before* code exists. Typo fixes and small patches that clearly belong
  to an existing issue may go straight to a pull request.
- **Keep pull requests reviewable.** Aim for roughly **~1000 changed lines** per PR. If it
  grows past that, split it into stacked PRs rather than one big drop.
- **Never push to `main`.** Work on a branch (`feat/…`, `fix/…`, `docs/…`, `chore/…`, `test/…`)
  and open a pull request.
- **Security problems do not go into the issue tracker** — see [SECURITY.md](SECURITY.md).

### What an issue must contain

Every issue — feature, design or chore — answers these, in this order:

1. **What to do** — the concrete deliverable.
2. **What *not* to do** — the explicit non-goals. This is what keeps a ticket from growing
   while it is being worked on.
3. **Why** — the problem it solves, and what goes wrong if it is not solved.
4. **Design rationale** — how it will be built, which contracts it touches, and which
   alternatives were rejected and why.
5. **Acceptance criteria** — a checklist that can be verified one item at a time.

Two kinds of issue carry extra requirements:

- **Bug report** — reproduction steps, expected vs. actual behavior, and the environment you
  ran in (OS/architecture, Go version, Pulse-Web version or commit). *A bug that cannot be
  reproduced cannot be fixed*: a report without a reproduction is likely to be closed as
  `question`.
- **Feature request** — a survey before a proposal. Look at how comparable projects (gin, chi,
  echo, stdlib) solve the same problem, then lay the options out with their trade-offs —
  **including "do nothing"** — and say which one you recommend and why. Present options; don't
  assert a conclusion the evidence doesn't carry.

### What a pull request must contain

- **The issue it serves** — write `see #N`. Use a closing keyword only if the PR really
  finishes that issue.
- **What changed** — the diff in prose.
- **What is new** — new exported API, new options, new behavior.
- **Impact / blast radius** — who is affected, whether a frozen contract moves, whether
  anything breaks.
- **How it was tested** — the exact commands and what you observed. "CI is green" is a result,
  not a test plan.
- **Review focus** — where you want a second pair of eyes, and what you are unsure about.

### Local development

Requires **Go 1.27+** — the toolchain downloads itself if it is missing. There is no Makefile
and no linter config; the nine commands below are the whole gate (one per step of
`.github/workflows/ci.yml`):

```bash
go build ./...                                            # compilation
go vet ./...                                              # static checks
"$(go env GOROOT)/bin/gofmt" -l $(git ls-files '*.go')     # format gate: non-empty output = failure
go test -race ./...                                       # the full suite (the CI test step)
go test -run TestRequestPathAllocBudget ./bench/          # allocation budget — must NOT run with -race
go test -run '^$' -bench '^$' ./bench/                    # bench compile check
(cd loadtest && go build ./... && go vet ./... && go test ./...)   # nested module — the root `./...` does not cross module boundaries
(cd otel && go build ./... && go vet ./... && go test ./...)       # the OTel adapter, same reason (it is what keeps the core dependency-free)
(cd interop && go build ./... && go vet ./... && go test ./...)    # ecosystem interop probes (websocket / CORS / chi …)
```

Three details in there are not incidental:

- **`gofmt` is called through `$(go env GOROOT)/bin/gofmt`, not through `PATH`.** A stale
  `gofmt` on `PATH` cannot parse newer syntax (generic methods, for instance) and reports
  *every* file as broken. If `gofmt` fails while `go build` passes, suspect the binary before
  the code.
- **The file set comes from `git ls-files '*.go'`, not from `gofmt -l .`.** The latter walks
  untracked directories too, and `_scratch/` (gitignored) is full of throwaway files.
- **The allocation budget must be run without `-race`.** The race detector inflates `B/op`;
  that test file is excluded from race builds via `//go:build !race` and runs as its own CI
  step.

The three `(cd … && …)` lines deserve one more sentence: a nested module is **outside** the root
module's `./...`, so nobody runs it unless it is listed here and in CI. That is exactly how the
perf-comparison harness once kept measuring a default that no longer existed, silently — see the
design doc's table C note.

On Windows PowerShell the format gate is:

```powershell
& (Join-Path (go env GOROOT) 'bin\gofmt.exe') -l (git ls-files '*.go')
```

**Never commit credentials.** `.env`, `.env.*`, `*.pem` and `*.secrets` are gitignored, and
secret scanning with push protection is enabled on this repository. `_scratch/` is the
gitignored area for local drafts, one-off probe programs and measurement output — scratch work
is fine there, but nothing that a reviewer needs to see belongs in it.

### Conventions a reviewer will check

- **Zero third-party dependencies in the core module.** The framework builds on `stdlib` plus
  exactly two upstream packages: `pulse/kernel` and `pulse/observability`. The verifiable
  check is what actually gets compiled in, not what `go.mod` mentions:
  ```bash
  go list -deps . | grep -E '^[^/]+\.[^/]+/'   # only pulse/kernel, pulse/observability, pulse-web itself
  ```
  `go list -m all` is *not* that check — it lists the whole module graph, including the
  indirect requirements of anything in it.
- **One package: `package web`, at the repository root.** No `internal/`, no `cmd/`, no
  subpackages. This is a library, not a binary, and everything published here is public API.
  (`bench/` is test-only code for the performance gate.)
- **Functional options** for anything configurable — `web.WithSink`, `web.WithMaxBodyBytes`.
- **`Ctx` must not cross goroutines.** It is bound to the request and its response writer.
  Background work takes `c.Detach()` and uses the process-level kernel handle it returns.
- **Handler signature is `func(*Ctx) error`.** Errors are returned, never written by hand:
  the error mapper turns them into a status code and the access log records the same decision.
  Wrap external errors with `web.NotFound` / `web.BadRequest`-style constructors rather than
  reaching for `c.Writer()`.
- **stdlib interop has three paths, and all three stay** — `web.Wrap` for stdlib handlers in,
  `web.Adapt` for stdlib middleware inside the onion, `Engine.Handler()` for the engine out.
  Keep all three: a change that makes any of them require a hand-rolled adapter is a regression.
- **The response writer is a deliberately narrow, explicit interface.** Exactly four capabilities
  are implemented and forwarded: `Flush`, `Hijack`, `SetWriteDeadline`, `EnableFullDuplex`.
  `Pusher` and `FlushError` are not exposed, and neither is `Unwrap()` — that would hand out the
  underlying writer wholesale, taking the two hidden ones with it. Widening or narrowing this list
  is an API decision, not a convenience: it comes with an observability contract (see
  `responseWriter.Hijack`).
- **Chinese comments and docs are the norm.** When you edit near them, write in the same
  language instead of translating them.
- **Line endings are LF**, enforced by `.gitattributes`. If your working copy predates that
  file, re-check it out once.
- **The freeze contract** (`v0.1.0`+): under 0.x SemVer a breaking change may only ride a
  *minor* release, never a patch, and every breaking change is listed at the top of the
  release notes. Frozen surfaces: the option set, the `Ctx` method set, the error model, the
  `http.request` record fields, and the shape of the default console output.
- **No escape hatches.** No `map[string]any` "extra options" parameter, no vendor-specific
  branches, no untyped attribute bags. If a capability cannot be expressed in the typed
  vocabulary, that is a design problem to solve in the issue — not a hole to punch in the API.
- **Behavior changes update the docs in the same PR** — `README.md` for anything a user reads,
  and `docs/design/web-framework-design.md` for contracts, decisions and the acceptance
  checklist.
- **Every acceptance criterion carries re-runnable evidence.** A criterion backed by a test
  names the test (`go test -run <Name> ./...`); one backed by measurement points at the
  section holding the numbers. For a guard (a test whose job is to catch drift), say which
  mutation makes it fail — a guard nobody has seen fail is a comment with a `func` in front.

### Review and merge

- **Self-review first.** Read your own diff the way a reviewer would before asking anyone else
  to.
- **With a second person available, wait for their approval.** Security-related changes always
  need a human review.
- **Merging is the maintainer's call.** A green CI is a precondition, not an approval.
- **Practical note on branch protection.** `main` requires conversation resolution, requires
  status checks to pass, and requires the branch to be up to date with `main`. So a PR that
  looks mergeable may still be blocked by an unresolved review thread, and an up-to-date branch
  needs a `update-branch` (followed by a fresh CI run on the new head commit) before merging.

### Releases

Releases are cut from `main` as `vX.Y.Z` tags with notes on GitHub. Before tagging, every
issue and pull request that belongs to that release is merged or closed — a release does not
ship with half-open tickets.

---

## 中文

Pulse-Web 是开源的 Go web 框架，目前仍在 1.0 之前（`v0.x`）。欢迎贡献。下面的规则主要约束
**事情怎么被提出来**，不约束**谁来提**。

本节与上面的英文内容等价；两者若有出入，以英文为准。

参与即表示你同意[行为准则](CODE_OF_CONDUCT.md)。

### 开工之前

- **非小改动先开 Issue。** 任何会改动行为、公开 API、已文档化的契约或仓库结构的事，都先开
  Issue——设计在 Issue 里定下来，再写代码。错字修正、以及明显属于某个既有 Issue 的小补丁，
  可以直接提 PR。
- **PR 要能被 review。** 有效改动按 **1000 行左右**控制；超过就拆成前后依赖的多个 PR，而不是
  一次性丢一大坨。
- **不要直接推 `main`。** 开分支（`feat/…`、`fix/…`、`docs/…`、`chore/…`、`test/…`）再提 PR。
- **安全问题不要开 Issue**——见 [SECURITY.md](SECURITY.md)。

### Issue 必须写清什么

任何 Issue（功能、设计、杂务）都按这个顺序回答：

1. **做什么**——具体的交付物。
2. **不做什么**——明确的非目标。这条决定了票据在执行过程中会不会膨胀。
3. **为什么**——它解决什么问题；不解决会怎样。
4. **设计理念**——打算怎么做、动了哪些契约、否掉了哪些替代方案以及为什么。
5. **验收标准**——能一条条核对的清单。

两类 Issue 有额外要求：

- **Bug 报告**——复现步骤、期望行为与实际行为、运行环境（操作系统与架构、Go 版本、Pulse-Web
  版本或 commit）。**复现不了的 bug 修不了**：没有复现步骤的报告很可能被按 `question` 关闭。
- **功能需求**——先调研再提方案。看同类项目（gin、chi、echo、stdlib）怎么解同一个问题，然后把
  各个选项连同取舍摆出来——**包括「什么都不做」**——并说明你推荐哪个、为什么。给可选方案，
  不要下证据撑不住的结论。

### PR 必须写清什么

- **关联 Issue**——写「见 #N」。除非这个 PR 真的把票干完，否则不要用自动关闭关键字。
- **改了什么**——用文字把 diff 讲清楚。
- **新增了什么**——新导出 API、新选项、新行为。
- **影响范围**——谁受影响、有没有动冻结契约、有没有破坏性。
- **怎么验证的**——具体命令 + 观察到的结果。「CI 绿了」是结果，不是测试方案。
- **Review 关注点**——希望别人重点看哪里，以及你自己没把握的地方。

### 本地开发

需要 **Go 1.27+**，工具链缺失时会自动下载。本仓库没有 Makefile、没有 linter 配置；下面这九条
就是全部门禁（与 `.github/workflows/ci.yml` 的九个 step 一一对应）：

```bash
go build ./...                                            # 编译
go vet ./...                                              # 静态检查
"$(go env GOROOT)/bin/gofmt" -l $(git ls-files '*.go')     # 格式门禁：输出非空即失败
go test -race ./...                                       # 全部测试（= CI 的 test 步骤）
go test -run TestRequestPathAllocBudget ./bench/          # 分配预算门禁——必须不带 -race
go test -run '^$' -bench '^$' ./bench/                    # bench 编译检查
(cd loadtest && go build ./... && go vet ./... && go test ./...)   # 嵌套 module——根 module 的 ./... 不过 module 边界
(cd otel && go build ./... && go vet ./... && go test ./...)       # otel 适配件同理（主模块零依赖靠它保住）
(cd interop && go build ./... && go vet ./... && go test ./...)    # 生态互操作对照（websocket / CORS / chi …）
```

其中三处不是顺手写的：

- **`gofmt` 走 `$(go env GOROOT)/bin/gofmt`，不走 `PATH`。** `PATH` 上可能挂着旧版，解析不了
  较新的语法（泛型方法一类），表现为**全量**报错。如果 `gofmt` 报错而 `go build` 通过，
  先怀疑二进制而不是代码。
- **文件集来自 `git ls-files '*.go'`，不是 `gofmt -l .`。** 后者会连未跟踪目录一起扫，而
  `_scratch/`（已 gitignore）里全是一次性文件。
- **分配预算门禁必须不带 `-race`。** race 检测器会抬高 `B/op`；那个测试文件用
  `//go:build !race` 排除在 race 构建外，由 CI 的独立步骤执行。

那三条 `(cd … && …)` 值得多一句：嵌套 module **不在**根 module 的 `./...` 里，不写进这里与
CI 就等于没人跑——性能对比工程当年就是这样静默地继续量一个已经不存在的默认（见设计文档表 C 那一段）。

Windows PowerShell 下的格式门禁是：

```powershell
& (Join-Path (go env GOROOT) 'bin\gofmt.exe') -l (git ls-files '*.go')
```

**绝不要提交凭据。** `.env`、`.env.*`、`*.pem`、`*.secrets` 都在 .gitignore 中，且本仓库已开启
secret scanning 与 push protection。`_scratch/` 是本地草稿、一次性探针程序与实测输出的
gitignore 暂存区——草稿放那里没问题，但 reviewer 需要看的东西不要留在里面。

### Review 会检查的约定

- **核心模块零第三方依赖。** 框架只建立在 `stdlib` 与两个上游包（`pulse/kernel`、
  `pulse/observability`）之上。可核实的判据是**真正编译进去的东西**，不是 `go.mod` 里列了什么：
  ```bash
  go list -deps . | grep -E '^[^/]+\.[^/]+/'   # 只应有 pulse/kernel、pulse/observability、pulse-web 自身
  ```
  `go list -m all` **不是**这个判据——它列的是整个 module graph，含其中每个模块的间接依赖。
- **单包：仓库根目录的 `package web`。** 不放 `internal/`、不放 `cmd/`、不切子包。这是库不是
  可执行程序，这里发布的每个符号都是公开 API。（`bench/` 只服务性能门禁，不发布 API。）
- **可配置处一律用 functional options**——`web.WithSink`、`web.WithMaxBodyBytes`。
- **`Ctx` 不得跨 goroutine。** 它与请求及其响应写出器绑定；后台任务用 `c.Detach()` 拿独立句柄。
- **handler 签名是 `func(*Ctx) error`。** 错误靠返回，不手写状态码：错误映射器把 error 变成
  状态码，访问日志记录同一个决定。外部错误用 `web.NotFound` / `web.BadRequest` 这类构造器包一层，
  不要绕到 `c.Writer()` 去写。
- **stdlib 互操作三条路径都要在**——进来的用 `web.Wrap`，stdlib 中间件进洋葱用 `web.Adapt`，
  出去的用 `Engine.Handler()`。删掉任一条、或让其中一条需要手写适配器，就是回归。
- **响应写出器是一份有意收窄、且显式的能力清单。** 恰好四个显式实现并转发底层：`Flush` /
  `Hijack` / `SetWriteDeadline` / `EnableFullDuplex`；`Pusher` / `FlushError` 不透出，
  `Unwrap()` 也不提供（那等于把底层 writer 整个交出去，连上面两个一起）。动它是 API 决策，
  不是顺手便利——一并要定观测语义（见 `responseWriter.Hijack` 的 godoc）。
- **中文注释与中文文档是本仓库常态**；在这些内容旁边改动时，用同一种语言写，不要顺手翻译。
- **行尾是 LF**，由 `.gitattributes` 强制。如果你的工作副本早于该文件，重新检出一次即可。
- **freeze 契约**（`v0.1.0` 起）：0.x SemVer 下 breaking 只能随 **minor** 发布，patch 内永不破坏，
  且每次 breaking 都在 Release notes 顶部显式列出。冻结面：选项集、`Ctx` 方法集、错误模型、
  `http.request` 记录字段、默认控制台输出的版式。
- **不允许逃生舱。** 不加 `map[string]any` 式的「额外参数」，不做 vendor 特判，不留无类型的属性袋子。
  表达不了就说明是设计问题，回到 Issue 里解决——不要先在 API 上开个洞。
- **行为变化在同一个 PR 里同步文档**——面向使用者的进 `README.md`，契约、决策与验收清单进
  `docs/design/web-framework-design.md`。
- **每条验收标准都附带可复跑的佐证。** 靠测试支撑的，写出测试名（`go test -run <名> ./...`）；
  靠实测支撑的，指到存数字的那一节。守卫类测试（职责是抓漂移）要说明**哪种变异会让它失败**——
  没被看着失败过的守卫，就是前面挂了个 `func` 的注释。

### Review 与合并

- **先自审。**像 reviewer 那样读一遍自己的 diff，再交给别人。
- **有第二人时必须等人审过。**安全相关的改动永远需要人工 review。
- **合并由维护者决定。**CI 绿是前置条件，不是批准。
- **分支保护的实务提醒。** `main` 要求「review 线程全部 resolve」+「status check 通过」+
  「分支与 `main` 同步」。所以看起来能合的 PR 可能仍被未 resolve 的线程卡住；而落后的分支要先
  `update-branch`（并在新的 head commit 上重跑一轮 CI）才能合。

### 发版

从 `main` 打 `vX.Y.Z` tag 并在 GitHub 上写 notes。打 tag 之前，属于这次发版的 issue 与 PR 全部
合入或关闭——不带着半开的票据发版。
