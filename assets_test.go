package web

import (
	"encoding/xml"
	"io"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 本文件不是框架逻辑，是品牌资产的验收守卫（#36 验收第 4 条：不靠人工比对）。
//
// 设计文档「品牌标识」一节的参数表是**给人读的口径**，assets/logo.svg 是**事实源**。
// 这里把两边都读出来逐项比：改了图忘了改表（或反过来）时这条测试先响，
// 而不是等人下次翻文档才发现表是旧的。修法永远同一条——以 SVG 为准回改文档表。

const (
	designDocPath = "docs/design/web-framework-design.md"
	logoAssetPath = "assets/logo.svg"
	bannerPath    = "assets/banner.svg"
	faviconPath   = "assets/favicon.svg"
)

// svgBar 是一根柱（<rect>）的几何。
type svgBar struct{ X, Y, W, H, RX float64 }

// svgContent 是一次解析的结果。
//
// grouped / ungrouped 的分法是为了 README 的 banner：它把 mark 内联在一个 <g> 里
// （与 logo.svg 同一份几何），短横条则是 <g> 之外的独立 <rect>——两者必须分得开。
type svgContent struct {
	bars      []svgBar  // 全部 <rect>
	grouped   []svgBar  // 位于 <g> 内的 <rect>
	ungrouped []svgBar  // 不在任何 <g> 里的 <rect>
	texts     []svgText // <text> 元素（内容 + 原始属性）
	elems     []string  // 出现过的元素名（用来断言「图上没有别的形状」）
	style     string    // <style> 文本
}

// svgText 是 <text> 元素：内容与原始属性（textLength / font-family 这类要看原值）。
type svgText struct {
	content string
	attrs   map[string]string
}

// readBrandSVG 解出 SVG 的柱、文字元素、元素名序列与 <style> 文本。
// 走 token 流而不是反序列化进结构体：既要 rect 的几何，也要能断言「图上没有别的形状」，
// 还要知道每个 rect 在不在 <g> 里。
func readBrandSVG(t *testing.T, path string) svgContent {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	var out svgContent
	var cur svgText
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	inStyle, inText, depth := false, false, 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("%s 不是合法 XML: %v", path, err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			out.elems = append(out.elems, el.Name.Local)
			switch el.Name.Local {
			case "g":
				depth++
			case "rect":
				bar := svgBar{
					X:  svgAttr(t, path, el, "x"),
					Y:  svgAttr(t, path, el, "y"),
					W:  svgAttr(t, path, el, "width"),
					H:  svgAttr(t, path, el, "height"),
					RX: svgAttr(t, path, el, "rx"),
				}
				out.bars = append(out.bars, bar)
				if depth > 0 {
					out.grouped = append(out.grouped, bar)
				} else {
					out.ungrouped = append(out.ungrouped, bar)
				}
			case "style":
				inStyle = true
			case "text":
				inText = true
				cur = svgText{attrs: map[string]string{}}
				for _, a := range el.Attr {
					cur.attrs[a.Name.Local] = a.Value
				}
			}
		case xml.EndElement:
			switch el.Name.Local {
			case "g":
				depth--
			case "style":
				inStyle = false
			case "text":
				inText = false
				out.texts = append(out.texts, cur)
			}
		case xml.CharData:
			if inStyle {
				out.style += string(el)
			}
			if inText {
				cur.content += string(el)
			}
		}
	}
	if len(out.bars) == 0 {
		t.Fatalf("%s 里一根柱都没有", path)
	}
	return out
}

func svgAttr(t *testing.T, path string, el xml.StartElement, name string) float64 {
	t.Helper()
	for _, a := range el.Attr {
		if a.Name.Local != name {
			continue
		}
		v, err := strconv.ParseFloat(a.Value, 64)
		if err != nil {
			t.Fatalf("%s <%s %s=%q>: %v", path, el.Name.Local, name, a.Value, err)
		}
		return v
	}
	t.Fatalf("%s <%s> 缺 %s 属性", path, el.Name.Local, name)
	return 0
}

// baseline 取「出现次数最多的端点」——柱阵的基线是每根柱共用的那一端
// （向上的柱共享底端、向下的柱共享顶端，所以它在全部端点里出现次数最多）。
func baseline(t *testing.T, bars []svgBar) float64 {
	t.Helper()
	count := map[float64]int{}
	for _, b := range bars {
		count[b.Y]++
		count[b.Y+b.H]++
	}
	best, bestN := 0.0, -1
	for v, n := range count {
		if n > bestN || (n == bestN && v < best) {
			best, bestN = v, n
		}
	}
	return best
}

// amplitudes 把每根柱翻成振幅（向上为正）：设计里柱要么从基线往上长，要么从基线往下长，
// 没有第三种形态——出现第三种就说明图被改坏了，直接失败而不是猜。
func amplitudes(t *testing.T, bars []svgBar, base float64) []float64 {
	t.Helper()
	out := make([]float64, 0, len(bars))
	for i, b := range bars {
		switch {
		case near(b.Y+b.H, base):
			out = append(out, base-b.Y)
		case near(b.Y, base):
			out = append(out, -(b.Y + b.H - base))
		default:
			t.Fatalf("第 %d 根柱（y=%g h=%g）两端都不在基线 %g 上", i+1, b.Y, b.H, base)
		}
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func nearAll(vals []float64, want float64) bool {
	for _, v := range vals {
		if !near(v, want) {
			return false
		}
	}
	return true
}

// docTableCell 取设计文档里「| 参数 | 值 |」表的取值单元格（按行首关键词匹配）。
func docTableCell(t *testing.T, row string) string {
	t.Helper()
	raw, err := os.ReadFile(designDocPath)
	if err != nil {
		t.Fatalf("读 %s: %v", designDocPath, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		if label := strings.Trim(strings.TrimSpace(cells[1]), "*"); strings.HasPrefix(label, row) {
			return strings.TrimSpace(cells[2])
		}
	}
	t.Fatalf("%s 里找不到「%s」这一行——口径表被动过？", designDocPath, row)
	return ""
}

// numRe 抓取单元格里的显式数字（含符号）。文档里的负号是 U+2212，先归一化再解析。
var numRe = regexp.MustCompile(`[-+]?\d+(?:\.\d+)?`)

func docNumbers(t *testing.T, row string) []float64 {
	t.Helper()
	cell := strings.NewReplacer("\u2212", "-", "–", "-", "—", "-").Replace(docTableCell(t, row))
	matches := numRe.FindAllString(cell, -1)
	out := make([]float64, 0, len(matches))
	for _, m := range matches {
		v, err := strconv.ParseFloat(m, 64)
		if err != nil {
			t.Fatalf("「%s」行的数字 %q 解析失败: %v", row, m, err)
		}
		out = append(out, v)
	}
	return out
}

func equalFloats(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !near(a[i], b[i]) {
			return false
		}
	}
	return true
}

// TestLogoMarkMatchesDesignDoc 是 #36 验收第 4 条：文档参数表 ↔ assets/logo.svg 逐项一致。
func TestLogoMarkMatchesDesignDoc(t *testing.T) {
	logo := readBrandSVG(t, logoAssetPath)
	bars, elems := logo.bars, logo.elems

	// 形态：只有 <rect>，没有峰值黑点（circle）也没有折线（path / polyline）。
	for _, name := range elems {
		switch name {
		case "svg", "title", "style", "rect":
		default:
			t.Errorf("%s 出现了 <rect> 之外的元素 <%s>——设计口径是「纯柱阵 + 无峰值黑点」", logoAssetPath, name)
		}
	}

	base := baseline(t, bars)
	amps := amplitudes(t, bars, base)

	widths := make([]float64, 0, len(bars))
	radii := make([]float64, 0, len(bars))
	gaps := make([]float64, 0, len(bars)-1)
	for i, b := range bars {
		widths = append(widths, b.W)
		radii = append(radii, b.RX)
		if i > 0 {
			gaps = append(gaps, b.X-(bars[i-1].X+bars[i-1].W))
		}
	}
	if !nearAll(widths, widths[0]) {
		t.Errorf("%s 柱宽不一致：%v——文档表只有一个「柱宽」，图必须等宽", logoAssetPath, widths)
	}
	if !nearAll(radii, radii[0]) {
		t.Errorf("%s 圆角不一致：%v", logoAssetPath, radii)
	}
	if !nearAll(gaps, gaps[0]) {
		t.Errorf("%s 间距不一致：%v——文档表只有一个「间距」，图必须等距", logoAssetPath, gaps)
	}

	lo, hi := amps[0], amps[0]
	for _, a := range amps {
		lo, hi = math.Min(lo, a), math.Max(hi, a)
	}

	for _, c := range []struct {
		row  string
		want []float64
	}{
		{"基线", []float64{base}},
		{"柱数 / 柱宽 / 间距", []float64{float64(len(bars)), widths[0], gaps[0]}},
		{"振幅序列", amps},
		{"落差", []float64{hi - lo, hi, lo}}, // 落差 40px（峰 +23 → 谷 −17）
		{"圆角", []float64{radii[0]}},
	} {
		got := docNumbers(t, c.row)
		if !equalFloats(got, c.want) {
			t.Errorf("「%s」行与 %s 的实际坐标不一致：\n  文档 %v\n  图   %v\n以图为准回改文档表。",
				c.row, logoAssetPath, got, c.want)
		}
	}
}

// TestFaviconIsSimplifiedMark 钉住 favicon 的简化规则：柱更少更粗，但峰谷两个关键柱必须在。
func TestFaviconIsSimplifiedMark(t *testing.T) {
	logo := readBrandSVG(t, logoAssetPath)
	logoBars, logoStyle := logo.bars, logo.style
	logoAmps := amplitudes(t, logoBars, baseline(t, logoBars))
	logoLo, logoHi := logoAmps[0], logoAmps[0]
	for _, a := range logoAmps {
		logoLo, logoHi = math.Min(logoLo, a), math.Max(logoHi, a)
	}

	fav := readBrandSVG(t, faviconPath)
	bars, elems, style := fav.bars, fav.elems, fav.style
	for _, name := range elems {
		switch name {
		case "svg", "title", "style", "rect":
		default:
			t.Errorf("%s 出现了 <rect> 之外的元素 <%s>", faviconPath, name)
		}
	}
	if len(bars) != 5 {
		t.Errorf("%s 柱数 = %d，简化规则是 5 根（16px 下 9 根细柱会糊）", faviconPath, len(bars))
	}
	for i, b := range bars {
		if !near(b.W, 6) {
			t.Errorf("%s 第 %d 根柱宽 = %g，简化规则是 6", faviconPath, i+1, b.W)
		}
	}

	amps := amplitudes(t, bars, baseline(t, bars))
	lo, hi := amps[0], amps[0]
	for _, a := range amps {
		lo, hi = math.Min(lo, a), math.Max(hi, a)
	}
	if !near(hi, logoHi) || !near(lo, logoLo) {
		t.Errorf("favicon 的峰谷（%g / %g）与 logo（%g / %g）不一致——简化要保留这两个关键柱",
			hi, lo, logoHi, logoLo)
	}

	// 明暗两档 + currentColor：favicon 作为独立资源渲染，继承不到宿主 CSS。
	assertThemeColors(t, faviconPath, style)
	assertThemeColors(t, logoAssetPath, logoStyle)
}

// assertThemeColors 钉住颜色口径：柱走 currentColor（宿主可以覆盖），
// 同时内嵌明暗两档中性色——独立当 <img> 渲染时没有宿主 color 可继承。
func assertThemeColors(t *testing.T, path, style string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	if !strings.Contains(strings.ToLower(string(raw)), `fill="currentcolor"`) {
		t.Errorf(`%s 的柱没有 fill="currentColor"——内联使用时就跟不上宿主的 color 了`, path)
	}
	if !strings.Contains(style, "prefers-color-scheme: dark") {
		t.Errorf("%s 的 <style> 里没有 prefers-color-scheme: dark——深色环境下不会换色", path)
	}
	lower := strings.ToLower(style)
	for _, c := range []string{"#17181a", "#f4f3f0"} {
		if !strings.Contains(lower, c) {
			t.Errorf("%s 的 <style> 里没有明暗两档中性色之一的 %q", path, c)
		}
	}
}

// TestBrandAssetPathsResolve 防的是「README 里的相对路径写错」——GitHub 上表现就是图裂。
func TestBrandAssetPathsResolve(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("读 README.md: %v", err)
	}
	m := regexp.MustCompile(`<img[^>]*src="([^"]+)"`).FindStringSubmatch(string(readme))
	if m == nil {
		t.Fatalf("README.md 里没有 <img src=\"...\">——顶部锁定被删了？")
	}
	if m[1] != bannerPath {
		t.Errorf("README 引的是 %q，顶部锁定是 %q（同步面口径见设计文档「品牌标识」）", m[1], bannerPath)
	}

	doc, err := os.ReadFile(designDocPath)
	if err != nil {
		t.Fatalf("读 %s: %v", designDocPath, err)
	}
	for _, p := range []string{logoAssetPath, bannerPath, faviconPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("资产缺失 %s: %v", p, err)
		}
		if !strings.Contains(string(doc), p) {
			t.Errorf("设计文档没提到 %s——同步面口径漏了这一处", p)
		}
	}
}

// numOf 解析属性里的数字（<text> 没有几何，只能从原始属性读）。
func numOf(t *testing.T, raw string) float64 {
	t.Helper()
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		t.Fatalf("解析数字 %q: %v", raw, err)
	}
	return v
}

// TestBannerMarkMatchesLogo 钉住 README 顶部锁定里的 mark 就是 assets/logo.svg 那一份几何——
// 「同一枚 mark 在两处各画一遍」是品牌资产最典型的漂移方式。
func TestBannerMarkMatchesLogo(t *testing.T) {
	logo := readBrandSVG(t, logoAssetPath)
	banner := readBrandSVG(t, bannerPath)

	if len(banner.grouped) != len(logo.bars) {
		t.Fatalf("%s 的 <g> 里有 %d 根柱，%s 有 %d 根——banner 的 mark 必须与 logo 同源",
			bannerPath, len(banner.grouped), logoAssetPath, len(logo.bars))
	}
	for i := range logo.bars {
		if banner.grouped[i] != logo.bars[i] {
			t.Errorf("banner 第 %d 根柱 %+v ≠ logo 的 %+v——banner 里的 mark 必须逐项同坐标",
				i+1, banner.grouped[i], logo.bars[i])
		}
	}

	// <g> 之外不该有 <rect>：字标整段是 <text>，连字符用字体自带字形。
	if len(banner.ungrouped) != 0 {
		t.Errorf("%s 的 <g> 之外有 %d 个 <rect>，期望 0 个（字标是纯文字）",
			bannerPath, len(banner.ungrouped))
	}
	assertThemeColors(t, bannerPath, banner.style)
}

// TestBannerWordmarkStructure 钉住字标口径：与 pulse 同一套 sans 栈、大写字面、
// 文字定长（textLength 只调字距、不变形字形），且起点在 mark 右侧。
func TestBannerWordmarkStructure(t *testing.T) {
	banner := readBrandSVG(t, bannerPath)
	if len(banner.texts) != 1 {
		t.Fatalf("%s 期望 1 个 <text>（Pulse-Web），实际 %d 个", bannerPath, len(banner.texts))
	}
	txt := banner.texts[0]
	if got := strings.TrimSpace(txt.content); got != "Pulse-Web" {
		t.Errorf("字标内容 = %q，期望 %q（大写首字母 + 字体自带连字符）", got, "Pulse-Web")
	}
	if txt.attrs["textLength"] == "" {
		t.Errorf("字标没有 textLength——sans 宽度随平台字体变，不定长就得为最宽的那家留空白")
	} else if txt.attrs["lengthAdjust"] != "spacing" {
		t.Errorf("lengthAdjust = %q，期望 spacing（只调字距，不变形字形）", txt.attrs["lengthAdjust"])
	}
	if !strings.Contains(txt.attrs["font-family"], "Segoe UI") {
		t.Errorf("字标 font-family = %q——应与 pulse 用同一套 sans 栈", txt.attrs["font-family"])
	}
	if txt.attrs["font-weight"] != "700" {
		t.Errorf("字标 font-weight = %q，期望 700", txt.attrs["font-weight"])
	}

	// mark 墨迹右边界 = 20 + (39.3+3)*1.3 ≈ 75；字标必须落在它右侧（同一行锁定不重叠）。
	if x := numOf(t, txt.attrs["x"]); x < 76 {
		t.Errorf("字标 x = %g，压到 mark 墨迹范围（右边界 ≈75）上了", x)
	}
}
