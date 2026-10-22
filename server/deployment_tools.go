package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/RC-CHN/ARTEX/db"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/resourcelib"
)

func deploymentSchema() map[string]any {
	integer := map[string]any{"type": "integer"}
	fields := map[string]any{"id": integer, "expected_revision": integer, "request_id": strParam("稳定操作标识；超时重试沿用"), "resource_id": integer}
	for k, d := range map[string]string{"name": "独立部署名称", "kind": "部署类型，例如 service/process/file/account/proxy", "location": "部署位置，不含凭据", "identifier": "准确的服务名、进程标识或文件路径，不使用泛匹配", "cleanup": "获准撤除的具体方法及验证方式，仅记录不会自动执行", "verification": "实际验证结果或明确保留依据；禁止编造成功", "note": "说明，不含凭据"} {
		fields[k] = strParam(d)
	}
	fields["state"] = map[string]any{"type": "string", "enum": []string{"active", "removing", "removed", "retained", "unknown", "failed"}}
	return map[string]any{"type": "object", "properties": fields, "required": []string{"request_id"}}
}
func (s *Server) deploymentTools() []actool.CoreTool {
	integer := map[string]any{"type": "integer"}
	return []actool.CoreTool{
		wrTool("manage_deployments", "登记或更新当前任务的独立部署与清理清单；该工具只记录，不执行远端命令。list/get 读取，save 创建或更新；save 可用 items 批量提交(1–25项，各自提交)。创建填写 name/kind/location/identifier/cleanup；更新传 id/expected_revision，只给需修改字段。每项必须有 request_id。removed 要提供本次实际 verification；无法确认执行结果用 unknown，不要自动重放撤除命令。retained 要注明保留依据。冲突后重新读取合并。", objSchema(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"list", "get", "save"}}, "id": integer, "before": integer, "state": strParam("列表状态筛选，省略全部"), "item": deploymentSchema(), "items": map[string]any{"type": "array", "minItems": 1, "maxItems": 25, "items": deploymentSchema()}}, "action"), s.manageDeployments),
		roTool("check_task_handoff", "检查当前任务的部署清理、未完成 Worker 和待处理主 Agent 问答，返回数量与下一步操作；只核对已登记和已记录的内容，不会证明所有外部部署都已登记或撤除。", objSchema(map[string]any{}), func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
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
func (s *Server) manageDeployments(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
	a, err := resourceActor(ctx)
	if err != nil {
		return resourceToolError(err)
	}
	var in struct {
		Action     string
		ID, Before int64
		State      string
		Item       *db.DeploymentChange
		Items      []db.DeploymentChange
	}
	if json.Unmarshal(raw, &in) != nil {
		return actool.Fail("invalid_input", "部署参数不是有效 JSON。", "使用 action=list/get/save。"), nil
	}
	switch in.Action {
	case "list":
		items, err := s.m.pg.ListDeployments(ctx, a.TaskID, in.Before, in.State)
		if err != nil {
			return resourceToolError(err)
		}
		next := int64(0)
		if len(items) == 100 {
			next = items[len(items)-1].ID
		}
		return jsonResult(map[string]any{"items": items, "next_before": next, "task_id": a.TaskID})
	case "get":
		v, err := s.m.pg.Deployment(ctx, a.TaskID, in.ID)
		if err != nil {
			return resourceToolError(err)
		}
		return jsonResult(v)
	case "save":
		if in.Item != nil {
			if len(in.Items) > 0 {
				return actool.Fail("invalid_input", "item 和 items 不能同时提供。", "单项用 item，批量用 items。"), nil
			}
			v, err := s.m.pg.ChangeDeployment(ctx, a.TaskID, *in.Item, a)
			if err != nil {
				return resourceToolError(err)
			}
			return jsonResult(v)
		}
		if len(in.Items) == 0 || len(in.Items) > 25 {
			return actool.Fail("invalid_input", "save 需要 item 或1–25项 items。", "创建至少填写 request_id/name/kind/location/identifier/cleanup。"), nil
		}
		results := []any{}
		failed := 0
		for i, item := range in.Items {
			if err = ctx.Err(); err != nil {
				return actool.Result{}, err
			}
			v, e := s.m.pg.ChangeDeployment(ctx, a.TaskID, item, a)
			entry := map[string]any{"index": i, "request_id": item.RequestID, "ok": e == nil}
			if e == nil {
				entry["result"] = v
			} else {
				failed++
				r, e := resourceToolError(e)
				if e != nil {
					return r, e
				}
				var body any
				_ = json.Unmarshal([]byte(r.Flatten()), &body)
				entry["error"] = body
			}
			results = append(results, entry)
		}
		return jsonResult(map[string]any{"items": results, "atomic_batch": false, "succeeded": len(results) - failed, "failed": failed, "next_action": "逐项检查 ok；只修正失败项，已成功项不用重复提交。回执未确认时使用原 request_id 和原参数重试。"})
	default:
		return actool.Fail("invalid_action", "未知部署操作。", "action 必须为 list、get 或 save。"), nil
	}
}

func (s *Server) taskDeployments(w http.ResponseWriter, r *http.Request) {
	taskID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if r.Method == http.MethodGet {
		before, _ := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
		v, err := s.m.pg.ListDeployments(r.Context(), taskID, before, r.URL.Query().Get("state"))
		if err != nil {
			resourceErr(w, err)
			return
		}
		writeJSON(w, 200, v)
		return
	}
	var in db.DeploymentChange
	if !resourceDecode(w, r, &in) {
		return
	}
	v, err := s.m.pg.ChangeDeployment(r.Context(), taskID, in, resourcelib.Actor{})
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 200, v)
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
