package server

// Exact previous defaults only: edited descriptions, schemas, bindings and
// enablement remain operator-owned. This is catalog retirement, not data migration.
var legacyResourceDescriptions = map[string]string{
	"resource_references":    "查看资源的任务版本引用和部署记录。blocking 表示活动引用或尚未完成撤除；删除时当前任务自己的版本引用不拦截，旧文件仍保留。",
	"delete_resource":        "删除整个自有资源登记：从可用列表移除、阻止新领取，保留历史版本和证据。不会撤除远端进程或文件。其他活动任务引用或未验证撤除的部署会返回 resource_in_use。必须携带 expected_revision、reason、request_id；回执丢失时用相同参数和 request_id 重试。",
	"manage_resources_batch": "批量创建、更新或删除独立资源，最多25项；各项独立提交，失败项不回滚成功项。每项提供唯一 request_id；重试只提交未确认的原始项。返回按输入索引排列的结果。按可独立使用、更新和撤除的单位登记资源；不同部署分别登记，同一工具的说明与多架构文件可放在一个包内。任务临时部署用 manage_deployments 记录所属位置、准确标识、撤除方法和验证结果；不占用任务选中资源额度。删除资源登记不会执行远端撤除。",
	"manage_resource":        "创建或更新自有资源的登记信息。按可独立使用、更新和撤除的单位登记，不同部署分别记录；同一工具的说明与多架构文件可合并。提供 request_id 可安全重试。resource 为完整元数据：name/kind/description/tags/access_mode/enabled；更新还需 id/revision。已发布资源的 name/description/tags 从 RESOURCE.md 生成，请编辑草稿并发布；访问策略或停用服务通过前端管理。不要自动登记任务发现的第三方目标。",
	"check_task_handoff":     "检查当前任务的部署清理、未完成 Worker 和待处理主 Agent 问答，返回数量与下一步操作；只核对已登记和已记录的内容，不会证明所有外部部署都已登记或撤除。",
}

func (s *Server) retireDeploymentCatalog() error {
	tx, err := s.m.pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM tools WHERE key='manage_deployments' AND system AND kind='builtin'`); err != nil {
		return err
	}
	for _, tool := range s.resourceTools() {
		old, ok := legacyResourceDescriptions[tool.Name()]
		if !ok {
			continue
		}
		if _, err = tx.Exec(`UPDATE tools SET description=$2,updated_at=now() WHERE key=$1 AND system AND kind='builtin' AND description=$3`, tool.Name(), tool.Description(), old); err != nil {
			return err
		}
	}
	return tx.Commit()
}
