package server

import (
	"context"
	"encoding/json"

	"github.com/RC-CHN/ARTEX/db"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
)

func (s *Server) recordExportTools() []actool.CoreTool {
	return []actool.CoreTool{wrTool("export_task_records", "异步导出当前任务的实际留存记录、完整工具参数/返回、会话、任务文件、关联资源版本和绑定的流量证据。返回排队回执，你可继续其他工作，状态用 status/list 查询；无需轮询等待。完成后供用户下载ZIP，包含JSONL、Markdown会话、离线HTML索引、SHA-256和缺失清单。默认原始内部副本；redacted=true仅过滤常见凭据字段并排除二进制，不能视为已审查可外发。不会暂停、归档或删除原始任务。单包20 GiB/20万文件，完成副本保留7天。", objSchema(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"create", "status", "list", "cancel", "retry"}}, "id": map[string]any{"type": "integer"}, "before": map[string]any{"type": "integer"}, "request_id": strParam("create必填，回执丢失用相同值和参数重试"), "redacted": map[string]any{"type": "boolean"}}, "action"), func(ctx context.Context, raw json.RawMessage) (actool.Result, error) {
		a, err := resourceActor(ctx)
		if err != nil {
			return resourceToolError(err)
		}
		var in struct {
			Action     string
			ID, Before int64
			RequestID  string `json:"request_id"`
			Redacted   bool
		}
		if json.Unmarshal(raw, &in) != nil {
			return actool.Fail("invalid_input", "导出参数不是有效 JSON。", "使用 action=create/status/list/cancel/retry。"), nil
		}
		switch in.Action {
		case "create":
			job, err := s.m.pg.QueueRecordExport(ctx, db.RecordExportRequest{TaskIDs: []int64{a.TaskID}, RequestID: in.RequestID, Redacted: in.Redacted}, a.TaskID)
			if err != nil {
				return resourceToolError(err)
			}
			return exportJobResult(job)
		case "list":
			jobs, err := s.m.pg.RecordExports(ctx, a.TaskID, in.Before)
			if err != nil {
				return resourceToolError(err)
			}
			visible := []db.RecordExport{}
			next := int64(0)
			if len(jobs) == 50 {
				next = jobs[len(jobs)-1].ID
			}
			for _, job := range jobs {
				if len(job.TaskIDs) == 1 && job.TaskIDs[0] == a.TaskID {
					visible = append(visible, job)
				}
			}
			return jsonResult(map[string]any{"items": visible, "next_before": next})
		case "status", "cancel", "retry":
			if in.ID <= 0 {
				return actool.Fail("invalid_input", "需要导出 id。", "使用 create 回执的 id，或先 list 查询。"), nil
			}
			job, err := s.m.pg.RecordExport(ctx, in.ID)
			if err != nil {
				return resourceToolError(err)
			}
			if len(job.TaskIDs) != 1 || job.TaskIDs[0] != a.TaskID {
				return actool.Fail("export_scope_denied", "该导出不属于当前任务的单任务范围。", "只使用当前任务 create/list 返回的 id。"), nil
			}
			if in.Action != "status" {
				job, err = s.m.pg.ChangeRecordExport(ctx, in.ID, in.Action)
				if err != nil {
					return resourceToolError(err)
				}
			}
			return exportJobResult(job)
		default:
			return actool.Fail("invalid_action", "未知导出操作。", "action 使用 create/status/list/cancel/retry。"), nil
		}
	})}
}
func exportJobResult(job db.RecordExport) (actool.Result, error) {
	next := "已进入导出队列；继续其他工作，稍后查询 status。"
	if job.State == "ready" {
		next = "资料包已完成。用户可在任务的记录与收尾页面下载；先检查 manifest.json 的缺失范围，再用于报告。"
	}
	if job.State == "failed" || job.State == "canceled" {
		next = "检查 error；确认原因处理后使用 retry 生成新快照，原始记录未改动。"
	}
	if job.State == "deleted" || job.State == "deleting" {
		next = "导出副本已删除或正在删除，原任务记录保留；需要新资料包时使用新的 request_id 创建。"
	}
	return jsonResult(map[string]any{"export": job, "original_task_changed": false, "next_action": next})
}
