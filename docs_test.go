package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	// `验收标准第 12 条`（不带「设计」），`stream_test.go` 当初写的是 `v1 功能面第 12 条`。
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
//     锚点与语言切换链接除外
//  4. 两版互相链接（切换入口双向可达）
//
// 措辞各语言自己地道，不逐句比——那样的守卫会因为翻译腔而天天误报。
//
// 变异探针：删掉任一版的一节、让某一版少一个代码块、断掉语言切换链接、只给一版加一个
// 新的链接或徽章目标，本用例必须红。
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
					s.links[target] = true
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
