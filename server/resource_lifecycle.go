package server

import (
	"context"
	"encoding/json"
	"net/http"

	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/resourcelib"
)

const resourceAtomicGuidance = "按可独立使用、更新和撤除的单位登记资源；不同部署分别登记，同一工具的说明与多架构文件可放在一个包内。任务临时部署用 manage_deployments 记录所属位置、准确标识、撤除方法和验证结果；不占用任务选中资源额度。删除资源登记不会执行远端撤除。"

func (s *Server) resourceLifecycleTools() []actool.CoreTool {
	integer := map[string]any{"type": "integer"}
	return append([]actool.CoreTool{
		roTool("resource_references", "查看资源的任务版本引用和部署记录。blocking 表示活动引用或尚未完成撤除；删除时当前任务自己的版本引用不拦截，旧文件仍保留。", objSchema(map[string]any{"resource_id": integer}, "resource_id"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var in struct {
				ID int64 `json:"resource_id"`
			}
			if json.Unmarshal(raw, &in) != nil || in.ID <= 0 {
				return actool.Fail("invalid_input", "需要正整数 resource_id。", "先用 search_resources 查询资源 ID。"), nil
			}
			a, err := resourceActor(ctx)
			if err != nil {
				return resourceToolError(err)
			}
			if err = s.authorizeResource(ctx, in.ID, a); err != nil {
				return resourceToolError(err)
			}
			refs, err := s.resourceLibrary().References(ctx, in.ID)
			if err != nil {
				return resourceToolError(err)
			}
			return jsonResult(map[string]any{"resource_id": in.ID, "references": refs})
		}),
		wrTool("delete_resource", "删除整个自有资源登记：从可用列表移除、阻止新领取，保留历史版本和证据。不会撤除远端进程或文件。其他活动任务引用或未验证撤除的部署会返回 resource_in_use。必须携带 expected_revision、reason、request_id；回执丢失时用相同参数和 request_id 重试。", objSchema(map[string]any{"resource_id": integer, "expected_revision": integer, "reason": strParam("删除原因，不含凭据"), "request_id": strParam("本次操作稳定唯一标识，重试沿用")}, "resource_id", "expected_revision", "reason", "request_id"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
			var in resourcelib.DeleteRequest
			if json.Unmarshal(raw, &in) != nil {
				return actool.Fail("invalid_input", "删除参数不是有效 JSON。", "提供 resource_id、expected_revision、reason、request_id。"), nil
			}
			a, err := resourceActor(ctx)
			if err != nil {
				return resourceToolError(err)
			}
			out, err := s.resourceLibrary().Delete(ctx, in, a)
			if err != nil {
				return resourceToolError(err)
			}
			return jsonResult(out)
		}),
		wrTool("manage_resources_batch", "批量创建、更新或删除独立资源，最多25项；各项独立提交，失败项不回滚成功项。每项提供唯一 request_id；重试只提交未确认的原始项。返回按输入索引排列的结果。"+resourceAtomicGuidance, objSchema(map[string]any{"items": map[string]any{"type": "array", "maxItems": 25, "minItems": 1, "items": map[string]any{"type": "object", "properties": map[string]any{"action": map[string]any{"type": "string", "enum": []string{"create", "update", "delete"}}, "request_id": strParam("稳定操作标识"), "resource": map[string]any{"type": "object", "description": "create/update: 完整资源元数据，更新含 id/revision"}, "resource_id": integer, "expected_revision": integer, "reason": strParam("删除原因")}, "required": []string{"action", "request_id"}}}}, "items"), s.manageResourcesBatch),
	}, append(s.deploymentTools(), s.recordExportTools()...)...)
}

func (s *Server) manageResourcesBatch(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
	var in struct {
		Items []json.RawMessage `json:"items"`
	}
	if json.Unmarshal(raw, &in) != nil || len(in.Items) < 1 || len(in.Items) > 25 {
		return actool.Fail("invalid_input", "items 必须包含1–25项。", "每项提供 action、request_id 和该操作所需字段。"), nil
	}
	results := []any{}
	failed := 0
	for i, item := range in.Items {
		if err := ctx.Err(); err != nil {
			return actool.Result{}, err
		}
		var meta struct {
			Action    string
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(item, &meta)
		var result actool.Result
		var err error
		if meta.RequestID == "" {
			result = actool.Fail("invalid_request_id", "批量每项必须有 request_id。", "为每个独立操作提供稳定标识。")
		} else if meta.Action == "delete" {
			var request resourcelib.DeleteRequest
			_ = json.Unmarshal(item, &request)
			var actor resourcelib.Actor
			actor, err = resourceActor(ctx)
			if err == nil {
				var out resourcelib.DeleteResult
				out, err = s.resourceLibrary().Delete(ctx, request, actor)
				if err == nil {
					result, err = jsonResult(out)
				}
			}
			if err != nil {
				result, err = resourceToolError(err)
			}
		} else {
			result, err = s.manageOwnedResource(ctx, item)
		}
		if err != nil {
			return actool.Result{}, err
		}
		var body any
		if json.Unmarshal([]byte(result.Flatten()), &body) != nil {
			body = map[string]any{"message": result.Flatten()}
		}
		results = append(results, map[string]any{"index": i, "request_id": meta.RequestID, "ok": !result.IsError, "result": body})
		if result.IsError {
			failed++
		}
	}
	return jsonResult(map[string]any{"items": results, "atomic_batch": false, "succeeded": len(results) - failed, "failed": failed, "next_action": "逐项检查 ok/result；只修正失败项。回执未确认的项使用相同 request_id 和原参数重试。"})
}

func (s *Server) deleteOwnedResource(w http.ResponseWriter, r *http.Request) {
	var in resourcelib.DeleteRequest
	if !resourceDecode(w, r, &in) {
		return
	}
	in.ResourceID = resourceID(r)
	out, err := s.resourceLibrary().Delete(r.Context(), in, resourcelib.Actor{})
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) resourceReferences(w http.ResponseWriter, r *http.Request) {
	refs, err := s.resourceLibrary().References(r.Context(), resourceID(r))
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 200, refs)
}
