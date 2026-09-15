package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 本文件是**站点守卫**（#52），与 `docs_test.go`（文档面）、`assets_test.go`（品牌资产）
// 同类：把写在票面与设计文档里的约定变成能跑的断言。
//
// 站点与 README 一样是**有意的双语副本**（根 = 中文、`/en/` = English，对齐上游 pulse），
// 两份副本必然有漂的风险。能自动化的部分是**结构与规格**，不是措辞——比措辞的守卫
// 会因为翻译腔天天误报。
//
// 受守卫的五条约定：
//
//  1. 两版**页集相同**，逐页**结构等价**（frontmatter 键、列表项数、标题数、代码块语言数、
//     引用目标集合）。漏译一页、少写一节、示例只加一边，都会在这里响。
//  2. 站内链接都能落到真实页面——构建期 VitePress 也会拦（死链即构建失败），这条让
//     Go 门禁在**不开 npm** 的情况下也能拦。
//  3. 首页字标与 `assets/banner.svg` **同规格**：字号 / 字重 / 字体栈三项逐字相等。
//     这是「同一枚字标在两处各写一遍」这类漂移的自动化拦截面。
//  4. `base` 与仓库名一致，且两语言都声明、英文版挂在 `/en/` 下（语言切换双向可达的地基）。
//  5. **对外 API 都有落点**：源码里的每个导出符号都要在站点页面里出现过——符号直接从
//     源码抽，不维护第二份清单（清单一定会过期）。
//
// 每条都给了变异探针（见各自注释）：改一处必须让对应用例红。

const (
	siteRootPath  = "site"
	siteEnPath    = "site/en"
	siteConfPath  = "site/.vitepress/config.mts"
	siteThemePath = "site/.vitepress/theme/custom.css"
)

// siteSkipDirs 是站点目录里**不是内容**的部分：依赖、构建产物与缓存、构建期生成的
// 公开资源、辅助脚本。它们不参与双语结构比对。
var siteSkipDirs = map[string]bool{
	"node_modules": true,
	".vitepress":   true,
	"public":       true,
	"scripts":      true,
}

// walkSitePages 遍历一个语言版下的 Markdown 页面，把「相对该语言根的路径 + 原文」交给 fn；
// 返回扫描到的页数。
//
// 两个调用方（结构比对、链接可达）共用同一个 walker：两边各写一遍遍历，
// 迟早会出现「一处跳过了某目录、另一处没有」这种漂移。
func walkSitePages(t *testing.T, root string, fn func(rel, src string)) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.FromSlash(root), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(filepath.FromSlash(root), p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// 根语言版要跳过 `en/` 子树——那是**另一版**，不是本版的一页。
			if rel == "en" && root == siteRootPath {
				return fs.SkipDir
			}
			if siteSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".md" {
			return nil
		}
		n++
		fn(rel, readDocFile(t, path.Join(root, rel)))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", root, err)
	}
	if n == 0 {
		t.Fatalf("%s 下没扫到任何 .md——路径或遍历逻辑不对（守卫不能因为没查所以通过）", root)
	}
	return n
}

// sitePage 是一页的结构摘要（措辞不进判据）。
type sitePage struct {
	path    string
	fmKeys  []string       // frontmatter 顶层键，保持出现顺序
	fmItems int            // frontmatter 里的 `- key:` 列表项数（hero actions / features）
	h2, h3  int            // `##` / `###` 数
	fences  map[string]int // 开围栏的语言标注 → 个数
	links   map[string]bool
}

// siteLocalePages 收集一个语言版下的页面：键是**相对该语言根**的路径（两版能一一对应
// 的前提就是键相同），值是页面结构。
func siteLocalePages(t *testing.T, root string) map[string]sitePage {
	t.Helper()
	pages := map[string]sitePage{}
	walkSitePages(t, root, func(rel, src string) {
		pages[rel] = sitePageOf(t, path.Join(root, rel), src)
	})
	return pages
}

// sitePageOf 抽出一页的结构。
//
// 语言无关的归一化只有一处：英文版的站内链接带 `/en` 前缀，比之前去掉——
// 两版的引用目标集合才能逐项对上。
func sitePageOf(t *testing.T, file, src string) sitePage {
	t.Helper()
	p := sitePage{path: file, fences: map[string]int{}, links: map[string]bool{}}
	inFence, inFront, frontDone := false, false, false
	for i, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)

		// frontmatter 只在文件开头（第一行 `---` 起，到下一根 `---` 止）。
		if trimmed == "---" && !frontDone {
			if i == 0 {
				inFront = true
				continue
			}
			if inFront {
				inFront, frontDone = false, true
				continue
			}
		}
		if inFront {
			if m := frontKeyRe.FindStringSubmatch(line); m != nil {
				p.fmKeys = append(p.fmKeys, m[1])
			}
			if frontItemRe.MatchString(line) {
				p.fmItems++
			}
			continue
		}

		switch {
		case strings.HasPrefix(trimmed, "```"):
			if !inFence {
				p.fences[strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))]++
			}
			inFence = !inFence
			continue
		case strings.HasPrefix(line, "## "):
			p.h2++
		case strings.HasPrefix(line, "### "):
			p.h3++
		}
	}
	for _, target := range siteLinkTargets(src) {
		p.links[normalizeSiteTarget(target)] = true
	}
	return p
}

var (
	// frontKeyRe 匹配 frontmatter 的顶层键（`layout: home`）。
	frontKeyRe = regexp.MustCompile(`^([a-z][a-z0-9_-]*):`)
	// frontItemRe 匹配 frontmatter 里的列表项（`- title: …` / `- theme: brand`）。
	frontItemRe = regexp.MustCompile(`^\s*-\s+[a-z]`)
	// confLinkRe 匹配 config.mts 里的 `link: '…'`。
	confLinkRe = regexp.MustCompile(`link: '([^']+)'`)
	// frontLinkRe 匹配页面 frontmatter 里 YAML 写法的 `link:`（首页 hero 按钮）。
	frontLinkRe = regexp.MustCompile(`^\s*link:\s*(\S+)\s*$`)
)

// normalizeSiteTarget 把英文版的 `/en/…` 前缀去掉，让两版的引用目标可比：
// `/en/guide/x` 与中文版的 `/guide/x` 是同一页。去掉前缀后**要补回前导斜杠**，
// 否则 `/en/guide/x` 会变成相对路径形状的 `guide/x`，与中文版逐字对不上。
func normalizeSiteTarget(target string) string {
	if target == "/en" {
		return "/"
	}
	if rest, ok := strings.CutPrefix(target, "/en/"); ok {
		return "/" + rest
	}
	return target
}

// TestSiteBilingualPagesMatch 守卫两版**页集相同 + 逐页结构等价**。
//
// 变异探针（任选其一，本用例必须红）：
//   - 在 `site/guide/` 下加一页而 `site/en/guide/` 不加（或反之）
//   - 删掉任一版某页的一节 / 一个代码块
//   - 只在某一版的 frontmatter 里加一个 feature
//   - 只给某一版加一条链接目标
func TestSiteBilingualPagesMatch(t *testing.T) {
	zh := siteLocalePages(t, siteRootPath)
	en := siteLocalePages(t, siteEnPath)

	var missing, extra []string
	for rel := range zh {
		if _, ok := en[rel]; !ok {
			missing = append(missing, rel)
		}
	}
	for rel := range en {
		if _, ok := zh[rel]; !ok {
			extra = append(extra, rel)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("中文版有而英文版没有的页：%v（两版必须一一对应）", missing)
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("英文版有而中文版没有的页：%v", extra)
	}
	if t.Failed() {
		return
	}

	for rel, a := range zh {
		b := en[rel]
		if strings.Join(a.fmKeys, ",") != strings.Join(b.fmKeys, ",") {
			t.Errorf("%s: frontmatter 顶层键不一致\n  %s: %v\n  %s: %v",
				rel, a.path, a.fmKeys, b.path, b.fmKeys)
		}
		if a.fmItems != b.fmItems {
			t.Errorf("%s: frontmatter 列表项数不一致（%s=%d，%s=%d）——hero 按钮 / features 漏了一个？",
				rel, a.path, a.fmItems, b.path, b.fmItems)
		}
		if a.h2 != b.h2 {
			t.Errorf("%s: `##` 数量不一致（%s=%d，%s=%d）", rel, a.path, a.h2, b.path, b.h2)
		}
		if a.h3 != b.h3 {
			t.Errorf("%s: `###` 数量不一致（%s=%d，%s=%d）", rel, a.path, a.h3, b.path, b.h3)
		}
		for lang, n := range a.fences {
			if b.fences[lang] != n {
				t.Errorf("%s: ```%s 代码块数量不一致（%s=%d，%s=%d）",
					rel, lang, a.path, n, b.path, b.fences[lang])
			}
		}
		for lang, n := range b.fences {
			if _, ok := a.fences[lang]; !ok && n > 0 {
				t.Errorf("%s: ```%s 只出现在 %s（%d 个），%s 里没有", rel, lang, b.path, n, a.path)
			}
		}
		for target := range a.links {
			if !b.links[target] {
				t.Errorf("%s: %s 引用 %s，而 %s 没有", rel, a.path, target, b.path)
			}
		}
		for target := range b.links {
			if !a.links[target] {
				t.Errorf("%s: %s 引用 %s，而 %s 没有", rel, b.path, target, a.path)
			}
		}
	}

	var pages, links int
	for _, p := range zh {
		pages++
		links += len(p.links)
	}
	if pages < 3 || links < 4 {
		t.Fatalf("扫到的页数/引用数太少（%d 页 / %d 个引用）——守卫可能失效，不是通过", pages, links)
	}
	t.Logf("两版各 %d 页、结构等价；中文版引用目标 %d 个", pages, links)
}

// resolveSiteTarget 把一条站内链接落到仓库里的真实文件。
//
// 带扩展名且不是 `.md` 的目标是 `public/` 下的资源（由 `assets/` 在构建期生成，
// 仓库里没有它）——不在本守卫的判据面内，构建期 VitePress 会拦。
func resolveSiteTarget(t *testing.T, page, target string) (string, bool) {
	t.Helper()
	if ext := path.Ext(target); ext != "" && ext != ".md" {
		return "", true
	}
	base := siteRootPath
	if !strings.HasPrefix(target, "/") {
		base = path.Dir(page) // 相对链接：相对当前页所在目录
	}
	joined := path.Clean(path.Join(base, strings.TrimSuffix(target, ".md")))
	for _, c := range []string{joined + ".md", path.Join(joined, "index.md")} {
		if st, err := os.Stat(filepath.FromSlash(c)); err == nil && !st.IsDir() {
			return c, true
		}
	}
	return "", false
}

// TestSiteInternalLinksResolve 守卫「站内链接都落到真实页面」。
//
// 判据面 = 页面正文的 Markdown / HTML 链接、页面 frontmatter 的 `link:`（hero 按钮）、
// 以及 `config.mts` 里 nav / sidebar 的 `link: '…'`。
// 两条：目标文件存在、且**指向本语言版**（英文版不该链到中文页，反之亦然——
// 那会让语言切换看起来像跳错站）。
//
// 变异探针：把任一页里的一条站内链接改成不存在的路径（如 `/guide/nope`）、
// 把英文版的正文链接或 hero 按钮写成中文页路径（`/guide/…`）、或把 config 的 nav
// 指向不存在的页，本用例必须红——三处都要能红，漏一处就是判据面缺一块。
func TestSiteInternalLinksResolve(t *testing.T) {
	var checked int
	for _, root := range []string{siteRootPath, siteEnPath} {
		walkSitePages(t, root, func(rel, src string) {
			page := path.Join(root, rel)
			for _, target := range siteLinkTargets(src) {
				if !strings.HasPrefix(target, "/") {
					continue // 外部 URL 与相对链接另有判据（相对链接由结构比对守着）
				}
				checked++
				file, ok := resolveSiteTarget(t, page, target)
				if !ok {
					t.Errorf("%s 里的链接 %s 落不到任何页面", page, target)
					continue
				}
				// 语言自洽只管**页面**：`public/` 下的资源（logo / favicon）只有一份，
				// 没有 /en 版本，拿语言前缀要求它是错的。
				if !strings.HasSuffix(file, ".md") {
					continue
				}
				if root == siteEnPath && !strings.HasPrefix(target, "/en/") {
					t.Errorf("%s 里的链接 %s 指向中文版——英文版应链到 /en/…", page, target)
				}
				if root == siteRootPath && strings.HasPrefix(target, "/en/") {
					t.Errorf("%s 里的链接 %s 指向英文版——中文版应链到根路径", page, target)
				}
			}
		})
	}

	// config.mts 的 nav / sidebar 也是站内链接，一并核。
	conf := readDocFile(t, siteConfPath)
	for _, m := range confLinkRe.FindAllStringSubmatch(conf, -1) {
		if !strings.HasPrefix(m[1], "/") {
			continue
		}
		checked++
		file, ok := resolveSiteTarget(t, siteConfPath, m[1])
		if !ok {
			t.Errorf("%s 里的 link: %q 落不到任何页面", siteConfPath, m[1])
		} else if !strings.HasSuffix(file, ".md") {
			t.Errorf("%s 里的 link: %q 指向 %s，期望一个页面", siteConfPath, m[1], file)
		}
	}

	if checked < 6 {
		t.Fatalf("只核了 %d 条站内链接——守卫可能失效，不是通过", checked)
	}
	t.Logf("站内链接 %d 条全部可达", checked)
}

// siteLinkTargets 抽出一页里**所有可达的链接目标**，两个来源：
//
//  1. 正文的 Markdown 链接与 HTML 的 `href` / `src`（代码围栏内不算——那是示例文本）
//  2. frontmatter 的 `link:`——**首页 hero 按钮就在这儿**，它是 YAML 不是 Markdown，
//     只扫正文的正则看不到它。早先的版本漏了这一来源，探针因此报 MISSED
//     （把英文版 hero 的 `/en/guide/x` 改成 `/guide/x` 无人察觉）。
//
// **外部 URL 也收**：两版对同一条外部引用（设计文档 / README）必须一致，只收内链的话
// 「某一版把设计文档链接改旧了」会溜过去——与 README 守卫收绝对 URL 是同一条理由。
// 锚点不收（两版标题本来就不同）；需要只看内链的调用方自己按前缀过滤。
func siteLinkTargets(src string) []string {
	var out []string
	inFence := false
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := frontLinkRe.FindStringSubmatch(line); m != nil {
			if target := strings.Trim(m[1], `"'`); !strings.HasPrefix(target, "#") {
				out = append(out, target)
			}
		}
		for _, re := range []*regexp.Regexp{linkRe, htmlLinkRe} {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				if target := m[1]; !strings.HasPrefix(target, "#") {
					out = append(out, target)
				}
			}
		}
	}
	return out
}

// TestSiteWordmarkMatchesBanner 钉住「首页字标与 assets/banner.svg 同规格」。
//
// 字标落在首页落地页的自定义样式里（`.pw-wordmark`，见 `custom.css` 的落地页一节），
// 不再走默认主题的 hero——所以这里比对的是那份自定义规则。
//
// 比的是三项**规格**，不是渲染结果：字体栈、字号、字重逐字相等。
//
// `textLength="232"` 这条在 CSS 里没有对应属性，等价约束是**不调字距**——banner 钉定长的
// 目的是跨平台字体（Segoe UI / SF / Helvetica）既不溢出也不留大片空白；同一栈同一字号下
// 实测排版宽度 231px（headless Chrome、DSF=1，像素尺用 232px 色块自校），与钉子的 232 同值。
// 所以这里断言 hero 名字没有字距调节；真要调字距，该做的是连 banner 与设计文档口径一起改，
// 而不是让两处悄悄分叉。
//
// 变异探针：把 `custom.css` 里的字标字号改成 48px（或字体栈换成别的），
// 或把 `assets/banner.svg` 的 font-size 改成 44，本用例必须红。
func TestSiteWordmarkMatchesBanner(t *testing.T) {
	banner := readBrandSVG(t, bannerPath)
	if len(banner.texts) != 1 {
		t.Fatalf("%s 期望 1 个 <text>，实际 %d 个", bannerPath, len(banner.texts))
	}
	attrs := banner.texts[0].attrs

	decl := heroNameRule(t, readDocFile(t, siteThemePath))
	for _, c := range []struct{ prop, want string }{
		{"font-family", attrs["font-family"]},
		{"font-size", attrs["font-size"] + "px"},
		{"font-weight", attrs["font-weight"]},
	} {
		got, ok := decl[c.prop]
		if !ok {
			t.Errorf("%s 的首页字标规则里没有 %s——口径是「与 %s 同规格」", siteThemePath, c.prop, bannerPath)
			continue
		}
		if got != c.want {
			t.Errorf("首页字标 %s = %q，而 %s 的 <text> 是 %q——两处必须同规格",
				c.prop, got, bannerPath, c.want)
		}
	}
	if ls, ok := decl["letter-spacing"]; ok && ls != "normal" {
		t.Errorf("首页字标 letter-spacing = %q——banner 的 textLength 是在**不调字距**的前提下钉定长，CSS 侧应保持 normal", ls)
	}
	t.Logf("字标规格与 banner 一致：%s / %s / %s", decl["font-family"], decl["font-size"], decl["font-weight"])
}

// wordmarkRuleRe 匹配 `.pw-wordmark` 那条规则本体。
//
// 必须在**行首**匹配选择器再吃 `{…}`：直接找 `.pw-wordmark` 会先命中注释里提到它的
// 那句（落地页一节的注释里就写着 `.pw-wordmark`），于是解析出的是注释与选择器之间的
// 文字，三个属性一个都读不到——探针第一次就是这么红的。
var wordmarkRuleRe = regexp.MustCompile(`(?m)^\.pw-wordmark\s*\{([^}]*)\}`)

// heroNameRule 从主题 CSS 里抽出首页字标的声明块（`.pw-wordmark`）。
func heroNameRule(t *testing.T, css string) map[string]string {
	t.Helper()
	m := wordmarkRuleRe.FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("%s 里找不到 `.pw-wordmark { … }` 规则——首页字标的口径没落地？", siteThemePath)
	}
	decl := map[string]string{}
	for _, item := range strings.Split(m[1], ";") {
		k, v, ok := strings.Cut(item, ":")
		if !ok {
			continue
		}
		decl[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if len(decl) == 0 {
		t.Fatalf("%s 里 `.pw-wordmark` 规则是空的", siteThemePath)
	}
	return decl
}

// TestSiteBaseMatchesRepoName 钉住站点部署口径：`base` 取仓库名（GitHub Pages 项目页的
// 路径就是仓库名），两语言都声明，且英文版挂在 `/en/` 下。
//
// 变异探针：把 `base` 改成 `/pulse/`，或删掉英文 locale 块里的 `link: '/en/'`，
// 本用例必须红。
func TestSiteBaseMatchesRepoName(t *testing.T) {
	var repoName string
	for _, line := range strings.Split(readDocFile(t, "go.mod"), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			repoName = path.Base(strings.TrimSpace(rest))
			break
		}
	}
	if repoName == "" {
		t.Fatalf("go.mod 里没找到 module 行")
	}

	conf := readDocFile(t, siteConfPath)
	if want := "base: '/" + repoName + "/'"; !strings.Contains(conf, want) {
		t.Errorf("%s 里没有 %q——Pages 是项目页，base 必须与仓库名一致", siteConfPath, want)
	}
	for _, want := range []string{
		"root: {",       // 默认语言 = 中文
		"en: {",         // 英文版
		"lang: 'zh-CN'", // 语言标注：中文
		"lang: 'en'",    // 语言标注：英文
		"link: '/en/'",  // 语言切换入口（英文版挂在 /en/ 下）
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("%s 里没有 %q——语言结构（根 = 中文、/en/ = English）没落全", siteConfPath, want)
		}
	}
}

// siteExportedAPI 解析仓库根目录的 Go 源文件（跳过 _test.go），抽出**对外符号名**：
// 导出函数、导出类型、导出方法、导出常量与变量。方法与类型同名时按名字去重。
func siteExportedAPI(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("解析包目录: %v", err)
	}
	// 先收导出**类型**名：方法只在这个集合的接收者上才算对外 API——
	// `responseWriter` 这类非导出类型上的 `WriteHeader` 不是公开面，收进来会让守卫
	// 要求文档去提一个用户根本写不出的符号。
	exportedTypes := map[string]bool{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				for _, spec := range gd.Specs {
					if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.IsExported() {
						exportedTypes[ts.Name.Name] = true
					}
				}
			}
		}
	}

	names := map[string]bool{}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if !d.Name.IsExported() {
						break
					}
					if d.Recv != nil {
						if recv := receiverTypeName(d); !exportedTypes[recv] {
							break // 非导出类型上的方法：不是公开面
						}
					}
					names[d.Name.Name] = true
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if s.Name.IsExported() {
								names[s.Name.Name] = true
							}
						case *ast.ValueSpec:
							for _, n := range s.Names {
								if n.IsExported() {
									names[n.Name] = true
								}
							}
						}
					}
				}
			}
		}
	}
	out := make([]string, 0, len(names))
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// siteMarkdown 返回 site/ 下所有页面的正文（键是相对仓库根的路径）。
func siteMarkdown(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(filepath.FromSlash(siteRootPath), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if siteSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) == ".md" {
			out[filepath.ToSlash(p)] = readDocFile(t, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", siteRootPath, err)
	}
	return out
}

// TestSiteCoversExportedAPIs 守卫「对外提供的 API 都在站点上出现过」。
//
// 起因是维护者对指南的评价：只有「快速开始 + 观测」两页，API 面基本没覆盖
// （原话「只要是对外提供的 API 都至少要提到吧」）。这条把那个要求变成机器判据：
// 从**源码**抽导出符号（不看任何清单，避免清单自己过期），逐个要求在 `site/` 的页面里
// 出现一次——出现方式不限（标题、正文、代码示例都算），但必须真的提到。
//
// 长名按词边界比对；**两个字符以内的短名**（`H`、`Get`、`Set`、`JSON` 这类）另加判据：
// 必须出现在代码语境里（`web.H` / “ `H` “ / `H{`），否则「H」这种字母在中文正文里
// 随便就能撞上，守卫会变成永远绿灯。
//
// 变异探针：删掉任一页里对某个符号的唯一一处提及（如 `WithTrustedTraceHeader`），
// 或把源码里的一个导出符号改名而文档不改，本用例必须红。
func TestSiteCoversExportedAPIs(t *testing.T) {
	api := siteExportedAPI(t)
	if len(api) < 50 {
		t.Fatalf("只抽到 %d 个导出符号——解析逻辑不对（实测 80+），守卫不能因为没查所以通过", len(api))
	}
	pages := siteMarkdown(t)
	if len(pages) < 6 {
		t.Fatalf("只扫到 %d 个站点页面——路径不对（遍历坏了会返回 0）", len(pages))
	}

	var missing []string
	for _, name := range api {
		pat := `\b` + regexp.QuoteMeta(name) + `\b`
		if len([]rune(name)) <= 2 {
			pat = `(?:web\.|\x60)` + regexp.QuoteMeta(name) + `(?:\x60|[{(]|\b)`
		}
		re := regexp.MustCompile(pat)
		found := false
		for _, src := range pages {
			if re.MatchString(src) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("这些导出符号在 site/ 的页面里一次都没出现（%d 个）：%s",
			len(missing), strings.Join(missing, " / "))
	}
	t.Logf("导出符号 %d 个，站点页面 %d 个，覆盖 %d/%d",
		len(api), len(pages), len(api)-len(missing), len(api))
}

// receiverTypeName 取方法接收者的类型名（`*Ctx` / `Ctx` / `Detached` 都归到类型名）。
func receiverTypeName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return ""
	}
	switch t := d.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}
