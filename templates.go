package web

import (
	"errors"
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
	DevReload bool   // true = 每次请求重新解析（开发用；不要用于生产）
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
// 注意：模板执行错误发生在响应头写出之后，此时无法再改状态码——
// 需要严格保证时请先渲染到 buffer 再写出。
func (c *Ctx) HTML(code int, name string, data any) error {
	ts := c.engine.templates
	if ts == nil {
		return errors.New("web: templates not configured (use web.WithTemplates)")
	}
	t, err := ts.get()
	if err != nil {
		return err
	}
	c.w.Header().Set("Content-Type", "text/html; charset=utf-8")
	c.w.WriteHeader(code)
	return t.ExecuteTemplate(c.w, name, data)
}
