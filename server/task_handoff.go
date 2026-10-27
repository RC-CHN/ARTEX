package server

import (
	"context"
	"encoding/json"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"net/http"
	"strconv"
)

func (s *Server) handoffTools() []actool.CoreTool {
	return []actool.CoreTool{
		roTool("check_task_handoff", "检查当前任务未完成 Worker 和待处理主 Agent 问答，返回数量与下一步操作；只核对已登记和已记录的内容，不会证明所有外部部署都已登记或撤除。", objSchema(map[string]any{}), func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
			a, err := resourceActor(ctx)
			if err != nil {
				return resourceToolError(err)
			}
			checks, err := s.m.pg.TaskHandoff(ctx, a.TaskID)
			if err != nil {
				return resourceToolError(err)
			}
			refs, err := s.m.pg.TaskHandoffReferences(ctx, a.TaskID)
			if err != nil {
				return resourceToolError(err)
			}
			budget, err := s.m.pg.TaskTokenBudget(ctx, a.TaskID, true)
			if err != nil {
				return resourceToolError(err)
			}
			return jsonResult(map[string]any{"references": refs, "task_budget": budget, "task_id": a.TaskID, "checks": checks, "scope": "recorded_state", "external_cleanup_verified": false, "next_action": "按 checks 中的具体提示处理；无检查项只代表已记录的事项无待处理项。"})
		}),
	}
}
func (s *Server) taskHandoff(w http.ResponseWriter, r *http.Request) {
	taskID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	v, err := s.m.pg.TaskHandoff(r.Context(), taskID)
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"checks": v})
}
