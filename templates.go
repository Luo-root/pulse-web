package web

import (
	"bytes"
	"errors"
	"fmt"
	"html/template"
	"path/filepath"
	"sync"
)

// H 是模板数据的便利写法（等价 map[string]any）；也可直接传任意 struct。
type H = map[string]any

// TemplateConfig 配置 HTML 模板（薄封装 stdlib html/template，不自实现模板引擎）。
type TemplateConfig struct {
	Root      string // 模板根目录
	Pattern   string // glob 模式，默认 "*.html"
	DevReload bool   // true = 每请求重新解析（每请求 ParseGlob + 写锁）；仅供开发
}

// WithTemplates 启用 HTML 模板。生产模式启动时解析一次并缓存；
// 解析失败在装配期 panic（对齐「装配期暴露错误」）。
func WithTemplates(cfg TemplateConfig) Option {
	return func(c *config) { c.templates = &cfg }
}

type templateSet struct {
	cfg TemplateConfig

	mu   sync.RWMutex
	tmpl *template.Template
}

func newTemplateSet(cfg TemplateConfig) (*templateSet, error) {
	if cfg.Root == "" {
		return nil, errors.New("web: TemplateConfig.Root is required")
	}
	if cfg.Pattern == "" {
		cfg.Pattern = "*.html"
	}
	ts := &templateSet{cfg: cfg}
	if !cfg.DevReload {
		if err := ts.reload(); err != nil {
			return nil, err
		}
	}
	return ts, nil
}

func (ts *templateSet) reload() error {
	t, err := template.ParseGlob(filepath.Join(ts.cfg.Root, ts.cfg.Pattern))
	if err != nil {
		return err
	}
	ts.mu.Lock()
	ts.tmpl = t
	ts.mu.Unlock()
	return nil
}

func (ts *templateSet) get() (*template.Template, error) {
	if ts.cfg.DevReload {
		if err := ts.reload(); err != nil {
			return nil, err
		}
	}
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.tmpl, nil
}

// HTML 渲染命名模板并写出。
//
// **先渲染到内存、成功后才写响应头**：模板名写错、或执行期报错（数据缺字段、
// 方法返回错误等）都能在写头之前返回 error，交由统一错误映射成 5xx —— 否则
// `WriteHeader` 先生效，「已写响应不被覆盖」规则会把错误吞成一个 200 空页。
// 代价是模板输出不再流式（模板渲染本身即内存操作）；需要流式请直接写 Writer。
func (c *Ctx) HTML(code int, name string, data any) error {
	ts := c.engine.templates
	if ts == nil {
		return errors.New("web: templates not configured (use web.WithTemplates)")
	}
	t, err := ts.get()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		return fmt.Errorf("web: render template %q: %w", name, err)
	}
	c.w.Header().Set("Content-Type", "text/html; charset=utf-8")
	c.w.WriteHeader(code)
	_, err = c.w.Write(buf.Bytes())
	return err
}
