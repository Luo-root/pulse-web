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

// designDocPath 复用 assets_test.go 的声明（同一份设计文档，同一个包）。
const criteriaHead = "## 验收标准"

var (
	// criterionRe 匹配验收条目 `- [x] **名字**：…`，捕获开头的加粗名字。
	criterionRe = regexp.MustCompile(`^- \[[ x]\] \*\*(.+?)\*\*`)
	// namedRefRe 匹配按条目名的引用：设计验收标准『性能回归』条。
	namedRefRe = regexp.MustCompile(`设计验收标准『([^』]+)』`)
	// numericRefRe 匹配按序号的引用（本仓库已禁用这种写法）。
	numericRefRe = regexp.MustCompile(`设计验收标准第\s*\d+\s*条`)
	// statusCountRe 匹配 README 状态行的计数：**v1 功能面已实现**（13/13）。
	statusCountRe = regexp.MustCompile(`（(\d+)/(\d+)）`)
)

// designCriterionNames 抽出「验收标准」节里的条目名，保持文档顺序。
func designCriterionNames(t *testing.T) []string {
	t.Helper()
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

// repoDocFiles 返回守卫要扫的文本文件：仓库内的 .go 与 .md。
// 跳过 `.git`、`_scratch`（gitignore 的草稿区）与**本文件自身**——
// 本文件里写着引用形态（正则字面量 + 文档注释里的示例），扫它必然自命中，
// 那是守卫自己的写法，不是对条目的引用。这是唯一的豁免。
func repoDocFiles(t *testing.T) []string {
	t.Helper()
	self := guardSelfPath()
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
		case ".go", ".md":
			if abs, err := filepath.Abs(path); err == nil && abs == self {
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
		t.Fatal("没扫到任何 .go / .md——工作目录或遍历逻辑不对")
	}
	return out
}

// guardSelfPath 返回本文件的**绝对**路径。用调用点定位而不是写死文件名：
// 文件改名后守卫仍然正确（写死会让改名变成一次「看起来像引用坏了」的假失败）。
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
//  3. 全树不再出现 `设计验收标准第 N 条` 的序号写法（反向断言，防回退）
//
// 变异探针：改掉任一处的名字（或往任意文件写一条序号引用），本用例必须红。
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

// TestREADMEStatusCountMatchesDesignDoc 把 README 的 `（N/N）` 与验收清单实际条数绑定。
//
// 这正是 #47 证据里那次漂移的直接守卫：清单加一条而 README 没跟，本用例红。
// 语言版本按存在与否逐个纳入——英文版与中文版都过，谁先漏谁红。
//
// 变异探针：把 README 的 `（13/13）` 改成 `（12/12）`，或往清单里加一条，本用例必须红。
func TestREADMEStatusCountMatchesDesignDoc(t *testing.T) {
	want := len(designCriterionNames(t))
	if want == 0 {
		t.Fatal("抽不到条目，无法核对计数")
	}

	readmeSeen := 0
	for _, path := range []string{"README.md", "README_zh.md"} {
		b, err := os.ReadFile(filepath.FromSlash(path))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("读 %s: %v", path, err)
		}
		readmeSeen++
		m := statusCountRe.FindStringSubmatch(string(b))
		if m == nil {
			t.Errorf("%s 里找不到 `（N/N）` 形态的状态计数", path)
			continue
		}
		if m[1] != m[2] {
			t.Errorf("%s 的 `（%s/%s）` 前后不一致", path, m[1], m[2])
		}
		if m[2] != strconv.Itoa(want) {
			t.Errorf("%s 的计数是 `%s/%s`，而 %s 的验收清单实际有 %d 条",
				path, m[1], m[2], designDocPath, want)
		}
	}
	if readmeSeen == 0 {
		t.Fatal("README.md 与 README_zh.md 都不存在——状态计数没有落点")
	}
}
