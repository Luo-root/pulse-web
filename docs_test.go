package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// 本文件是**文档面守卫**，与 `assets_test.go`（品牌资产守卫）同类：
// 把「写在文档里的约定」变成能跑的断言。
//
// 起因（#47）：设计文档的验收清单从 10 条长到 13 条，期间按**序号**引用它的
// 4 处里有 3 处静默漂了位（第 11→8 / 第 7→12 / 第 8→13）。序号不是标识而是位置，
// 插队即失效。改成引用**条目名**之后，名字对不对得上可以被机器检查。

// criteriaHead 是验收清单节的标题。清单是设计文档的最后一节，切到这里之后整节都算清单。
const criteriaHead = "## 验收标准"

var (
	// criterionRe 匹配验收条目 `- [x] **名字**：…`，捕获开头的加粗名字。
	criterionRe = regexp.MustCompile(`^- \[[ x]\] \*\*(.+?)\*\*`)
	// namedRefRe 匹配按条目名的引用：设计验收标准『性能回归』条。
	namedRefRe = regexp.MustCompile(`设计验收标准『([^』]+)』`)
	// numericRefRe 匹配按序号的引用（本仓库已禁用这种写法）。
	//
	// 两种**历史写法**也在射程内，因为它们正是最可能被抄回来的形态：设计文档当初写的是
	// `验收标准第 12 条`（不带「设计」），`context_test.go`（#83 之前叫 `stream_test.go`）
	// 当初写的是 `v1 功能面第 12 条`。
	// 只认 `设计验收标准第 N 条` 的话，这两句抄回去 CI 不会响——反向断言就白设了。
	//
	// 不误伤的依据：全仓其余「第 N 条」都不是这两个形状（`assets_test.go` 是
	// `#36 验收第 4 条`，缺「标准」；设计文档是 `12-factor 第 11 条`）。
	numericRefRe = regexp.MustCompile(`(?:设计)?验收标准第\s*\d+\s*条|v1 功能面第\s*\d+\s*条`)
	// linkRe 匹配 Markdown 链接/图片的目标（去掉锚点）：[文本](目标)。
	linkRe = regexp.MustCompile(`\[[^\]]*\]\(([^)#]+?)(?:#[^)]*)?\)`)
	// htmlLinkRe 匹配 HTML 里的引用目标：<a href="…"> / <img src="…">。
	//
	// 顶部 banner 与徽章行都是 HTML，Markdown 的 linkRe 看不见它们——不单收一份，
	// 两版的徽章走样（少一个、换一个目标）就成了双语守卫的盲区。
	htmlLinkRe = regexp.MustCompile(`(?:href|src)="([^"]+)"`)
)

// readmePaths 是双语 README：英文主版在前，中文版在后。
var readmePaths = []string{"README.md", "README_zh.md"}

// siteOrigin 是 README 里站点链接的绝对前缀（站内用相对路径，README 里跨仓库只能用绝对 URL）。
const siteOrigin = "https://luo-root.github.io/pulse-web"

// normalizeREADMEtarget 让两版 README 的**站点深链**可比：英文版指向 `/en/…`、中文版指向
// `/…`，指的是同一页（站点语言布局：根 = 中文、`/en/` = English）。口径与站点守卫的
// normalizeSiteTarget 相同，区别只在目标形态是绝对 URL。
//
// 只归一化站点自己的域：外部引用（徽章、上游仓库、规范原文）一律原样比，两版必须逐字一致。
func normalizeREADMEtarget(target string) string {
	rest, ok := strings.CutPrefix(target, siteOrigin+"/en")
	if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
		// 不是本站链接，或只是同前缀的另一个路径（`/pulse-web/english`）——不折。
		return target
	}
	return siteOrigin + "/" + strings.TrimPrefix(rest, "/")
}

// designCriterionNames 抽出「验收标准」节里的条目名，保持文档顺序。
func designCriterionNames(t *testing.T) []string {
	t.Helper()
	// designDocPath 声明在 assets_test.go：同一份设计文档，同一个包。
	doc := readDocFile(t, designDocPath)
	_, after, ok := strings.Cut(doc, criteriaHead)
	if !ok {
		t.Fatalf("%s 里找不到 %q 节", designDocPath, criteriaHead)
	}
	var names []string
	for _, line := range strings.Split(after, "\n") {
		if m := criterionRe.FindStringSubmatch(line); m != nil {
			names = append(names, m[1])
		}
	}
	return names
}

// readDocFile 读仓库内的文本文件（相对包目录）。
func readDocFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return string(b)
}

// repoDocFiles 返回守卫要扫的文本文件：仓库内的 .go / .md / .yml。
// 跳过 `.git`、`_scratch`（gitignore 的草稿区）与**本文件自身**——
// 本文件里写着引用形态（正则字面量 + 文档注释里的示例），扫它必然自命中，
// 那是守卫自己的写法，不是对条目的引用。这是唯一的豁免。
//
// `.yml` 也在扫描面内：`.github/ISSUE_TEMPLATE/*.yml` 是最可能被写进序号引用的地方
// （「对应验收标准第 N 条」这种话很容易落在 issue 表单里）。
func repoDocFiles(t *testing.T) []string {
	t.Helper()
	self := filepath.Base(guardSelfPath())
	var out []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != "." && (name == ".git" || strings.HasPrefix(name, "_")) {
				return fs.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".go", ".md", ".yml":
			if filepath.Base(path) == self {
				return nil // 本文件自身
			}
			out = append(out, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历仓库: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("没扫到任何 .go / .md / .yml——工作目录或遍历逻辑不对")
	}
	return out
}

// guardSelfPath 返回本文件的路径。用调用点定位而不是写死文件名：
// 文件改名后守卫仍然正确（写死会让改名变成一次「看起来像引用坏了」的假失败）。
//
// 调用方比较时只取 **basename**：`runtime.Caller` 给的是编译期路径，
// `go test -trimpath` 会把它裁成模块路径（`github.com/Luo-root/pulse-web/docs_test.go`），
// 与遍历到的相对路径的绝对形式对不上——那时自排除失效，本文件里的正则源码与示例会被
// 当成真引用，报错还指向守卫自己。本包只有一个 docs_test.go，按名字比就够。
func guardSelfPath() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "\x00" // 取不到就返回一个不可能匹配的哨兵值
	}
	if abs, err := filepath.Abs(file); err == nil {
		return abs
	}
	return file
}

// TestDesignCriterionNamesAreUsedInReferences 守卫「引用验收条目用名字、不用序号」：
//
//  1. 名字唯一（重名会让引用有歧义）
//  2. 每条 `设计验收标准『X』` 引用里的 X 都真实存在
//  3. 全树不再出现按**序号**引用的写法（反向断言，防回退）——覆盖三种历史形态：
//     `验收标准第 N 条`、`设计验收标准第 N 条`、`v1 功能面第 N 条`；`.yml`（issue
//     表单）也在扫描面内。
//
// 变异探针：改掉任一处的名字，或往任意文件（含 `.yml`）写一条任一形态的序号引用，本用例必须红。
func TestDesignCriterionNamesAreUsedInReferences(t *testing.T) {
	names := designCriterionNames(t)
	if len(names) == 0 {
		t.Fatalf("%s 的 %q 节里一条 `- [x] **名字**` 都没抽到——格式变了？",
			designDocPath, criteriaHead)
	}

	known := make(map[string]bool, len(names))
	for _, n := range names {
		if known[n] {
			t.Errorf("条目名重复：%q——引用会变得有歧义", n)
		}
		if strings.ContainsAny(n, "`*") {
			t.Errorf("条目名 %q 带 Markdown 标记，引用时无法逐字对上", n)
		}
		known[n] = true
	}

	var refs int
	for _, path := range repoDocFiles(t) {
		src := readDocFile(t, path)
		if bad := numericRefRe.FindString(src); bad != "" {
			t.Errorf("%s 里还有序号引用 %q——改用条目名（序号会被后续插入挤走）", path, bad)
		}
		for _, m := range namedRefRe.FindAllStringSubmatch(src, -1) {
			refs++
			if !known[m[1]] {
				t.Errorf("%s 引用了不存在的条目名『%s』（现有：%s）",
					path, m[1], strings.Join(names, " / "))
			}
		}
	}

	// 扫不到引用说明扫描范围或正则坏了——守卫不能「因为没查所以通过」。
	if refs == 0 {
		t.Fatalf("一处条目名引用都没扫到——守卫失效（路径或正则不对），不是通过；自身路径=%s", guardSelfPath())
	}
	t.Logf("条目名 %d 个，已校验 %d 处引用（跳过自身 %s）", len(names), refs, guardSelfPath())
}

// TestREADMEBilingualStructureMatches 守卫双语 README 的**结构等价**。
//
// 双语是本仓库「同一结论只在一个文件里展开」原则的**有意例外**——两份副本必然有
// 漂的风险。能自动化的部分是结构而不是措辞，所以这里比的是：
//
//  1. `##` / `###` 数量一致（章节骨架相同）
//  2. 代码围栏数量一致，且**各语言标注的数量**逐一相同（示例一一对应，不是「反正都是 8 个块」）
//  3. 引用目标集合一致——Markdown 链接 + HTML 的 `href` / `src`，**绝对 URL 也算**
//     （徽章行就是靠这一点比对：少一个徽章、两版徽章不同，都会被这一条抓住）；
//     站点深链先按语言归一化（`/en/guide/x` ≡ `/guide/x`，同一页的两种写法），
//     锚点与语言切换链接除外
//  4. 两版互相链接（切换入口双向可达）
//
// 措辞各语言自己地道，不逐句比——那样的守卫会因为翻译腔而天天误报。
//
// 变异探针：删掉任一版的一节、让某一版少一个代码块、断掉语言切换链接、只给一版加一个
// 新的链接或徽章目标、把某一版的站点深链换到另一页，本用例必须红。
func TestREADMEBilingualStructureMatches(t *testing.T) {
	type shape struct {
		path     string
		h2, h3   int
		fences   map[string]int
		links    map[string]bool
		switcher bool
	}

	shapes := make([]shape, 0, len(readmePaths))
	for i, path := range readmePaths {
		b, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			t.Fatalf("读 %s: %v（双语守卫要求两版都在）", path, err)
		}
		src := string(b)
		s := shape{path: path, fences: map[string]int{}, links: map[string]bool{}}

		inFence := false
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(trimmed, "```"):
				if !inFence {
					lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
					s.fences[lang]++ // 只数开围栏，语言标注取开围栏那一行的
				}
				inFence = !inFence
				continue
			case strings.HasPrefix(line, "## "):
				s.h2++
			case strings.HasPrefix(line, "### "):
				s.h3++
			}
			for _, re := range []*regexp.Regexp{linkRe, htmlLinkRe} {
				for _, m := range re.FindAllStringSubmatch(line, -1) {
					target := m[1]
					// 锚点不参与比较：两种语言的标题本来就不同（#install / #安装）。
					if strings.HasPrefix(target, "#") {
						continue
					}
					// 语言切换链接指向另一版，不算「内容引用」
					if target == readmePaths[1-i] {
						s.switcher = true
						continue
					}
					// 绝对 URL 也收：徽章行（href + shields 图片地址）就是靠它比对——
					// 只收相对路径的话，少一个徽章、两版徽章不一样，守卫都看不见。
					s.links[normalizeREADMEtarget(target)] = true
				}
			}
		}
		shapes = append(shapes, s)
	}

	a, b := shapes[0], shapes[1]
	if a.h2 != b.h2 {
		t.Errorf("`##` 数量不一致：%s=%d，%s=%d", a.path, a.h2, b.path, b.h2)
	}
	if a.h3 != b.h3 {
		t.Errorf("`###` 数量不一致：%s=%d，%s=%d", a.path, a.h3, b.path, b.h3)
	}
	for lang, n := range a.fences {
		if b.fences[lang] != n {
			t.Errorf("```%s 代码块数量不一致：%s=%d，%s=%d", lang, a.path, n, b.path, b.fences[lang])
		}
	}
	for lang, n := range b.fences {
		if _, ok := a.fences[lang]; !ok && n > 0 {
			t.Errorf("```%s 只出现在 %s（%d 个），%s 里没有", lang, b.path, n, a.path)
		}
	}
	for target := range a.links {
		if !b.links[target] {
			t.Errorf("%s 引用 %s，而 %s 没有", a.path, target, b.path)
		}
	}
	for target := range b.links {
		if !a.links[target] {
			t.Errorf("%s 引用 %s，而 %s 没有", b.path, target, a.path)
		}
	}
	if !a.switcher || !b.switcher {
		t.Errorf("两版必须互相链接（语言切换）：%s switcher=%v，%s switcher=%v",
			a.path, a.switcher, b.path, b.switcher)
	}
	if t.Failed() {
		return
	}
	t.Logf("%s: %d 个 `##` / %d 个 `###` / 代码块 %v / 引用目标 %d 个；两版结构与引用目标一致",
		a.path, a.h2, a.h3, a.fences, len(a.links))
}

// TestREADMEtargetNormalization 钉住双语守卫里唯一的归一化规则：只在站点自己的域上把
// `/en/…` 折成 `/…`，其余原样比。
//
// 不能只靠 TestREADMEBilingualStructureMatches 顺带覆盖：归一化**折宽了**会让那个守卫
// 静默失去分辨力（把两版指向不同页的深链看成一致），而它自己仍然全绿。
//
// 变异探针：把 `siteOrigin+"/en"` 改成 `siteOrigin`（边界外的也折），或去掉 `/en` 之后的
// 分隔符判断（`/pulse-web/english` 被折成 `/pulse-web/lish`），本用例必须红。
func TestREADMEtargetNormalization(t *testing.T) {
	const origin = "https://luo-root.github.io/pulse-web"
	cases := []struct{ in, want string }{
		{origin + "/en/guide/testing", origin + "/guide/testing"},
		{origin + "/en", origin + "/"},
		{origin + "/guide/testing", origin + "/guide/testing"},
		{origin + "/", origin + "/"},
		{origin + "/english", origin + "/english"},
		{"https://github.com/Luo-root/pulse", "https://github.com/Luo-root/pulse"},
	}
	for _, c := range cases {
		if got := normalizeREADMEtarget(c.in); got != c.want {
			t.Errorf("normalizeREADMEtarget(%q) = %q，想要 %q", c.in, got, c.want)
		}
	}
}

// ---- #93：门禁清单与 ci.yml 的计数守卫 ----

// ciStepRe 匹配 workflow 里的一个 step：`      - name: Build`。
var ciStepRe = regexp.MustCompile(`(?m)^\s+- name:\s*\S`)

// bashFenceRe 抽出 ```bash 代码块的内容（跨行、非贪婪）。
var bashFenceRe = regexp.MustCompile("(?s)```bash\n(.*?)```")

// TestGateListsMatchCIWorkflow 守卫「贡献者向的门禁清单不会与 CI 悄悄脱节」。
//
// 起因（#93）：`ci.yml` 从 6 步长到 9 步（先加 `loadtest` / `otel`，随后 `interop`），
// 而 `CONTRIBUTING.md` 中英两份与 PR 模板停在 6 条没动——**恰好漏掉那几个嵌套 module**，
// 而它们正是「没人跑就静默过期」的一类（#73：性能工程曾长期量一个已经不存在的默认，
// 不报错、不告警）。清单靠人记得同步，就等于没有同步；这里把条数变成断言。
//
// 判据四条，**全部以 `ci.yml` 的 `- name:` 条数为准**（不拿文档自己数的数当基准）：
//
//  1. `AGENTS.md` 的门禁 bash 块：命令条数 == step 数
//  2. `CONTRIBUTING.md` 的门禁 bash 块：中英各一块，各自 == step 数
//  3. `.github/PULL_REQUEST_TEMPLATE.md` 的 Testing 清单：条目数 == step 数
//  4. 三处引出清单的话里**写明**的条数词与 step 数一致（「九条」/「nine commands」）
//
// 第 4 条是这条守卫的重点：只比条数的话，清单补齐而那句话没改，读者仍会按旧数字判断
// 「这些就是全部门禁」——那正是 #93 描述的那种误判。
//
// 变异探针（三个方向都实测过，全部让本用例变红）：给 `ci.yml` 加一条 `- name:` 而清单
// 不动；从 PR 模板删掉一条；把 `AGENTS.md` 的「九条」改成「六条」。
func TestGateListsMatchCIWorkflow(t *testing.T) {
	want := len(ciStepRe.FindAllString(readRepoFile(t, ".github/workflows/ci.yml"), -1))
	if want == 0 {
		t.Fatal("没从 ci.yml 里数到任何 step——守卫失效，不是通过")
	}
	// 数词表是有限的（到二十）：`ci.yml` 长过这个数而没人更新词表时，先红而不是静默跳过。
	if _, ok := gateCountWord(gateNumeralZH, want); !ok {
		t.Fatalf("ci.yml 有 %d 个 step，但数词表（gateNumeralZH / gateNumeralEN）里没有对应写法——"+
			"加/删 CI step 时请一并更新词表、三处清单与那三句话", want)
	}

	// 1 + 2：门禁 bash 块按**内容**认（含 `go build ./...`），不按位置——文档里还有别的 bash 片段。
	for _, c := range []struct {
		file string
		ndoc int // 期望找到几块：CONTRIBUTING 中英各一块
	}{{"AGENTS.md", 1}, {"CONTRIBUTING.md", 2}} {
		src := readRepoFile(t, c.file)
		found := 0
		for _, m := range bashFenceRe.FindAllStringSubmatch(src, -1) {
			if !strings.Contains(m[1], "go build ./...") {
				continue
			}
			found++
			if got := countGateLines(m[1]); got != want {
				t.Errorf("%s 的门禁清单 %d 条命令，ci.yml 有 %d 个 step（多半漏了嵌套 module）", c.file, got, want)
			}
		}
		if found != c.ndoc {
			t.Errorf("%s 里找到 %d 个门禁 bash 块，期望 %d 个", c.file, found, c.ndoc)
		}
	}

	// 3：PR 模板的自检清单。
	tpl := readRepoFile(t, ".github/PULL_REQUEST_TEMPLATE.md")
	if got := strings.Count(testingSection(t, tpl), "- [ ]"); got != want {
		t.Errorf("PR 模板 Testing 清单 %d 条，ci.yml 有 %d 个 step", got, want)
	}

	// 4：引出清单那句话里写明的条数词。
	//
	// 只查「期望的数词出现过」不够：同一句里往往还有第二个数字（`下面九条就是全部 CI 门禁
	// （… 的九个 step）`），把前半段的「九条」改成「六条」时它照样通过。所以改成**逐个数词
	// 核**：这句话里凡是以「N 条 / N 个 step / N commands / N steps」形态出现的数字，每一个
	// 都必须等于当前条数（探针：把 AGENTS.md 的「九条」改成「六条」，本用例必须红）。
	//
	// 只看「能解析成数字」的那些：英文那句里 `one per step of …` 的 `per` 也会被正则捞到，
	// 它不是数词，跳过。
	zhWord, _ := gateCountWord(gateNumeralZH, want)
	enWord, _ := gateCountWord(gateNumeralEN, want)
	for _, c := range []struct {
		file, anchor string
		re           *regexp.Regexp
		digits       map[string]int
	}{
		{"AGENTS.md", "就是全部 CI 门禁", gateNumReZH, gateNumeralZH},
		{"CONTRIBUTING.md", "就是全部门禁", gateNumReZH, gateNumeralZH},
		{"CONTRIBUTING.md", "the whole gate", gateNumReEN, gateNumeralEN},
	} {
		line := lineContaining(t, readRepoFile(t, c.file), c.anchor)
		seen := 0
		for _, m := range c.re.FindAllStringSubmatch(line, -1) {
			got, ok := gateNumeralValue(m[1], c.digits)
			if !ok {
				continue
			}
			seen++
			if got != want {
				t.Errorf("%s 里那句话把条数写成 %q（ci.yml 是 %d 个 step）：\n  %s",
					c.file, strings.TrimSpace(m[1]), want, strings.TrimSpace(line))
			}
		}
		if seen == 0 {
			t.Errorf("%s 里那句话没写明条数（该写 %q 或 %q commands）：\n  %s",
				c.file, zhWord+"条", enWord, strings.TrimSpace(line))
		}
	}
}

// gateNumReZH / gateNumReEN 抓「引出清单那句话」里的条数词。
var (
	gateNumReZH = regexp.MustCompile(`([0-9]+|[一二三四五六七八九十]+)\s*(?:条|个 step)`)
	gateNumReEN = regexp.MustCompile(`([0-9]+|[a-z]+)\s+(?:commands?|steps?)\b`)
)

// gateNumeralZH / gateNumeralEN 把数词解析成数字（只到二十——CI 的 step 不可能更多；
// 真超过二十时用例会在开头就报「数词表不够」，不会静默放过）。
var (
	gateNumeralZH = map[string]int{"一": 1, "二": 2, "三": 3, "四": 4, "五": 5,
		"六": 6, "七": 7, "八": 8, "九": 9, "十": 10,
		"十一": 11, "十二": 12, "十三": 13, "十四": 14, "十五": 15,
		"十六": 16, "十七": 17, "十八": 18, "十九": 19, "二十": 20}
	gateNumeralEN = map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
		"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
		"eleven": 11, "twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15,
		"sixteen": 16, "seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20}
)

// gateNumeralValue 把一次正则捕获解析成数字：阿拉伯数字直接解析，数词查表，两者都不是
// 就返回 ok=false（调用方跳过——`per` 这类词就是这么被放过的）。
func gateNumeralValue(tok string, words map[string]int) (int, bool) {
	tok = strings.TrimSpace(tok)
	if n, err := strconv.Atoi(tok); err == nil {
		return n, true
	}
	n, ok := words[tok]
	return n, ok
}

// gateCountWord 反查某条数在词表里的写法。
func gateCountWord(words map[string]int, want int) (string, bool) {
	for k, v := range words {
		if v == want {
			return k, true
		}
	}
	return "", false
}

// testingSection 切出 PR 模板里「## 测试 / Testing」到下一个二级标题之间的内容。
func testingSection(t *testing.T, src string) string {
	t.Helper()
	_, rest, ok := strings.Cut(src, "## 测试 / Testing")
	if !ok {
		t.Fatal("PR 模板里没有「## 测试 / Testing」段——标题被改了？")
	}
	sec, _, _ := strings.Cut(rest, "\n## ")
	return sec
}

// readRepoFile 读仓库里的文本文件，并归一化行尾：下面两条判据都按行切分，
// 不想让 CRLF 把一行数成两行。
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(rel))
	if err != nil {
		t.Fatalf("读 %s: %v", rel, err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// countGateLines 数代码块里的**门禁行**：非空、且不是整行注释（`# …`）。
//
// 整行注释不算——把一条门禁注释掉意味着它不再被要求跑，那正是这条守卫要拦的
// （探针：把 CONTRIBUTING 英文区的一条 `(cd …)` 行前面加 `#` 注释掉，本用例必须红）。
func countGateLines(block string) int {
	n := 0
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		n++
	}
	return n
}

// lineContaining 返回第一行含该锚点的行；找不到就直接失败——锚点被改写时给出明确信号，
// 而不是让第 4 条判据静默通过。
func lineContaining(t *testing.T, src, anchor string) string {
	t.Helper()
	for _, line := range strings.Split(src, "\n") {
		if strings.Contains(line, anchor) {
			return line
		}
	}
	t.Fatalf("没找到含 %q 的行——引出清单的那句话被改了？", anchor)
	return ""
}
