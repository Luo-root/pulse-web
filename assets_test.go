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
	faviconPath   = "assets/favicon.svg"
)

// svgBar 是一根柱（<rect>）的几何。
type svgBar struct{ X, Y, W, H, RX float64 }

// readBrandSVG 解出 SVG 的柱、元素名序列与 <style> 文本。
// 走 token 流而不是反序列化进结构体：既要 rect 的几何，也要能断言「图上没有别的形状」。
func readBrandSVG(t *testing.T, path string) (bars []svgBar, elems []string, style string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	inStyle := false
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
			elems = append(elems, el.Name.Local)
			switch el.Name.Local {
			case "rect":
				bars = append(bars, svgBar{
					X:  svgAttr(t, path, el, "x"),
					Y:  svgAttr(t, path, el, "y"),
					W:  svgAttr(t, path, el, "width"),
					H:  svgAttr(t, path, el, "height"),
					RX: svgAttr(t, path, el, "rx"),
				})
			case "style":
				inStyle = true
			}
		case xml.EndElement:
			if el.Name.Local == "style" {
				inStyle = false
			}
		case xml.CharData:
			if inStyle {
				style += string(el)
			}
		}
	}
	if len(bars) == 0 {
		t.Fatalf("%s 里一根柱都没有", path)
	}
	return bars, elems, style
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
	bars, elems, _ := readBrandSVG(t, logoAssetPath)

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
	logoBars, _, logoStyle := readBrandSVG(t, logoAssetPath)
	logoAmps := amplitudes(t, logoBars, baseline(t, logoBars))
	logoLo, logoHi := logoAmps[0], logoAmps[0]
	for _, a := range logoAmps {
		logoLo, logoHi = math.Min(logoLo, a), math.Max(logoHi, a)
	}

	bars, elems, style := readBrandSVG(t, faviconPath)
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
		t.Fatalf("README.md 里没有 <img src=\"...\">——顶部 mark 被删了？")
	}
	if m[1] != logoAssetPath {
		t.Errorf("README 引的是 %q，资产事实源是 %q（同步面口径见设计文档「品牌标识」）", m[1], logoAssetPath)
	}

	doc, err := os.ReadFile(designDocPath)
	if err != nil {
		t.Fatalf("读 %s: %v", designDocPath, err)
	}
	for _, p := range []string{logoAssetPath, faviconPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("资产缺失 %s: %v", p, err)
		}
		if !strings.Contains(string(doc), p) {
			t.Errorf("设计文档没提到 %s——同步面口径漏了这一处", p)
		}
	}
}
