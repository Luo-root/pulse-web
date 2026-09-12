package web

import "net/http"

// debugSnapshot 是装配诊断端点的输出单元（只读投影，不暴露 kernel 内部结构）。
type debugSnapshot struct {
	Name       string   `json:"name"`
	State      string   `json:"state"`
	WaitingFor []string `json:"waiting_for,omitempty"`
	Err        string   `json:"err,omitempty"`
}

// Debug 挂载只读装配诊断端点：输出 `kernel.FiberSnapshots()` 的 JSON 视图。
//
// 只做**当前视图**，不另存 loader 历史动作——历史已在 Bootstrap 写入 Sink，
// 去日志里看。默认不挂载；暴露前请自行评估鉴权（它含插件名与失败原因）。
func (e *Engine) Debug(path string) {
	e.register(e.prefix+path, func(c *Ctx) error {
		snaps := e.kernel.FiberSnapshots()
		out := make([]debugSnapshot, 0, len(snaps))
		for _, s := range snaps {
			item := debugSnapshot{
				Name:       s.Name,
				State:      s.State.String(),
				WaitingFor: s.WaitingFor,
			}
			if s.Err != nil {
				item.Err = s.Err.Error()
			}
			out = append(out, item)
		}
		return c.JSON(http.StatusOK, out)
	}, nil)
}
