package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/internal/operation"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/resourcelib"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

func resourceActor(ctx context.Context) (resourcelib.Actor, error) {
	info := agent.RunInfoFrom(ctx)
	if info.TaskID <= 0 {
		return resourcelib.Actor{}, &operation.Error{Code: "task_context_required", Message: "此操作需要当前任务上下文。", NextAction: "在任务会话内调用；独立会话请使用资源库或任务界面。"}
	}
	a := resourcelib.Actor{TaskID: info.TaskID, ThreadID: fmt.Sprintf("task:%d:intent:%d", info.TaskID, info.IntentID), RunnerID: "local"}
	if rt, ok := ctx.Value(remoteExecutionKey{}).(*remoteExecution); ok {
		a.ThreadID = rt.spec.ThreadID
		a.RunnerID = rt.NodeID()
	}
	return a, nil
}
func resourceToolError(err error) (actool.Result, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return actool.Result{}, err
	}
	code, message, next := "resource_operation_failed", "资源操作未完成。", "使用相同 request_id 查询或重试；不要创建重复资源。仍失败时报告错误，不要声称已完成。"
	extra := map[string]any{}
	knownFailure := true
	var conflict *resourcelib.Conflict
	var inUse *resourcelib.InUse
	var input *resourcelib.InputError
	var fault *operation.Error
	switch {
	case errors.As(err, &fault):
		code, message, next = fault.Code, fault.Message, fault.NextAction
		if fault.CurrentRevision > 0 {
			extra["current_revision"] = fault.CurrentRevision
		}
	case errors.As(err, &inUse):
		code, message, next = "resource_in_use", "资源仍有活动引用或未完成撤除的部署。", "用 resource_references 查看引用；协调使用方或更新撤除验证后，再以最新 revision 删除。"
		extra["references"] = inUse.References
	case errors.Is(err, resourcelib.ErrConflict):
		code, message, next = "resource_conflict", "资源或草稿已被其他操作更新。", "用 resource_versions / resource_draft(get) 读取当前版本，合并变更后使用新的 request_id 提交。"
		if errors.As(err, &conflict) {
			extra["current_revision"] = conflict.CurrentRevision
			extra["current_version_id"] = conflict.CurrentVersionID
		}
	case errors.Is(err, resourcelib.ErrUnavailable):
		code, message, next = "resource_unavailable", "资源已停用、删除或当前任务没有访问权限。", "确认资源 ID 和任务授权；不要反复重试，也不要自动重建已删除资源。"
	case errors.Is(err, resourcelib.ErrLimit):
		code, message, next = "resource_limit", "资源文件、草稿或总量达到上限。", "缩小文件选择或丢弃不再使用的草稿；不要通过拆分请求绕过配额。"
	case errors.Is(err, operation.ErrKeyReuse):
		code, message, next = "request_id_conflict", err.Error(), "相同 request_id 只能重试完全相同的参数；新的操作使用新的 request_id。"
	case errors.Is(err, operation.ErrKeyRequired):
		code, message, next = "invalid_request_id", err.Error(), "提供稳定的 request_id；超时重试必须沿用原值。"
	case errors.Is(err, sql.ErrNoRows):
		code, message, next = "resource_not_found", "指定的资源、版本、草稿或文件不存在。", "重新查询列表并核对对应类型的 ID；不要把 version_id 当成 resource_id。"
	case errors.As(err, &input):
		code, message, next = "invalid_resource_input", input.Message, "按字段提示修正参数后再提交；尚未发生修改。"
	default:
		knownFailure = false
		if detail := safeResourceError(err); detail != "" {
			message = detail
			next = "检查文件已写完、路径在当前任务内且可读取，重新计算 SHA-256 后重试。"
		}
	}
	result := actool.Fail(code, message, next)
	if !knownFailure {
		extra["execution_status"] = "unknown"
		next = "结果尚未确认：先查询当前状态；支持 request_id 的操作使用相同参数和原 request_id 重试。不要盲目重复发布或声称已完成。"
	}
	if len(extra) > 0 {
		extra["code"] = code
		extra["message"] = message
		extra["next_action"] = next
		if knownFailure {
			extra["executed"] = false
		}
		raw, _ := json.Marshal(extra)
		result = actool.Errorf(string(raw))
	}
	return result, nil
}
func safeResourceError(err error) string {
	var input *resourcelib.InputError
	if errors.As(err, &input) {
		return input.Message
	}
	// Never send SQL/OS/credential errors into a model transcript.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return "文件不可用或超出当前任务目录"
	}
	if strings.Contains(err.Error(), "source changed") || strings.Contains(err.Error(), "checksum") {
		return "文件仍在写入或尚未同步：等待文件写完，重新计算 SHA-256，再导入"
	}
	return ""
}
func (s *Server) authorizeResource(ctx context.Context, id int64, a resourcelib.Actor) error {
	r, e := s.resourceLibrary().Get(ctx, id)
	if e != nil {
		return e
	}
	if !r.Enabled {
		return resourcelib.ErrUnavailable
	}
	if r.AccessMode == "global" || r.CreatorTaskID == a.TaskID {
		return nil
	}
	var exists bool
	e = s.m.pg.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_owned_resources WHERE task_id=$1 AND resource_id=$2)`, a.TaskID, id).Scan(&exists)
	if e != nil {
		return e
	}
	if !exists {
		return resourcelib.ErrUnavailable
	}
	return nil
}
func (s *Server) resourceTools() []actool.CoreTool {
	integer := map[string]any{"type": "integer"}
	text := func(d string) map[string]any { return strParam(d) }
	return append([]actool.CoreTool{
		roTool("search_resources", "搜索用户自有、跨任务复用的资源元数据。这些是自有资料/服务，不是任务发现的测试资产。", objSchema(map[string]any{"q": text("名称或描述"), "kind": text("tool/file/dataset/proxy/host/beacon"), "before": integer}), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Q, Kind string
				Before  int64
			}
			if json.Unmarshal(in, &a) != nil {
				return actool.Fail("invalid_input", "参数不是有效 JSON 或字段类型不正确。", "按当前工具 schema 提供 JSON 对象；ID 和 revision 使用整数。"), nil
			}
			v, e := s.resourceLibrary().List(ctx, a.Q, a.Kind, a.Before)
			if e != nil {
				return resourceToolError(e)
			}
			return jsonResult(v)
		}),
		roTool("resource_versions", "读取资源元数据、最近 100 个不可变版本和当前草稿。当前版本变化时，重新读取并基于最新版本建立草稿，不覆盖其他人的修改。凭据不返回。", objSchema(map[string]any{"resource_id": integer}, "resource_id"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID int64 `json:"resource_id"`
			}
			if json.Unmarshal(in, &a) != nil {
				return actool.Fail("invalid_input", "参数不是有效 JSON 或字段类型不正确。", "按当前工具 schema 提供 JSON 对象；ID 和 revision 使用整数。"), nil
			}
			actor, e := resourceActor(ctx)
			if e != nil {
				return resourceToolError(e)
			}
			if e = s.authorizeResource(ctx, a.ID, actor); e != nil {
				return resourceToolError(e)
			}
			lib := s.resourceLibrary()
			r, e := lib.Get(ctx, a.ID)
			if e != nil {
				return resourceToolError(e)
			}
			v, e := lib.Versions(ctx, a.ID)
			if e != nil {
				return resourceToolError(e)
			}
			d, e := lib.Drafts(ctx, a.ID)
			if e != nil {
				return resourceToolError(e)
			}
			versions := make([]map[string]any, 0, len(v))
			for _, version := range v {
				versions = append(versions, map[string]any{"id": version.ID, "version": version.Version, "resource_id": version.ResourceID, "metadata": version.Metadata, "size": version.Size, "file_count": len(version.Files), "endpoint_count": len(version.Endpoints), "note": version.Note, "validation": version.Validation, "created_at": version.CreatedAt})
			}
			drafts := make([]map[string]any, 0, len(d))
			for _, draft := range d {
				drafts = append(drafts, map[string]any{"id": draft.ID, "revision": draft.Revision, "base_version_id": draft.BaseVersionID, "size": draft.Size, "file_count": len(draft.Files), "updated_at": draft.UpdatedAt})
			}
			return jsonResult(map[string]any{"resource": r, "versions": versions, "drafts": drafts})
		}),
		wrTool("manage_resource", "创建或更新自有资源的登记信息。"+resourceAtomicGuidance+"提供 request_id 可安全重试。resource 为完整元数据：name/kind/description/tags/access_mode/enabled；更新还需 id/revision。已发布资源的 name/description/tags 从 RESOURCE.md 生成，请编辑草稿并发布；访问策略或停用服务通过前端管理。不要自动登记任务发现的第三方目标。", objSchema(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"create", "update"}}, "request_id": text("稳定操作标识；回执丢失时沿用相同参数重试"), "resource": map[string]any{"type": "object", "properties": map[string]any{"id": integer, "revision": integer, "name": text("资源名"), "kind": text("tool/file/dataset/proxy/host/beacon"), "description": text("用途与说明，不含凭据"), "tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "access_mode": text("global/restricted"), "enabled": map[string]any{"type": "boolean"}}, "required": []string{"name", "kind"}}}, "action", "resource"), s.manageOwnedResource),
		wrTool("resource_draft", "管理资源草稿。create 从最新版本建立；get 读取；import_file 从当前任务导入单个已完成文件，必须提供 source_path、path、sha256（在执行环境计算）；remove_file 删除草稿文件；configure 设置完整 endpoints 数组与 os/arch，可通过 credential_files 引用凭据文件，禁止把凭据写进工具参数；discard 丢弃。变更必须携带最新 revision。先用普通 Read/Edit/Bash 修改任务中的工作文件，再逐个导入；只处理明确选中的文件，不扫描任务历史。", objSchema(map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"create", "get", "import_file", "remove_file", "configure", "discard"}}, "resource_id": integer, "draft_id": integer, "base_version_id": integer, "revision": integer, "source_path": text("当前任务内文件路径"), "path": text("资源中的相对目录路径"), "sha256": text("source_path 完整文件 SHA-256；防止读取未同步或变更中的内容"), "executable": map[string]any{"type": "boolean"}, "os": text("平台"), "arch": text("架构"), "endpoints": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"name": text("端点名"), "url": text("不含凭据的 URL")}, "required": []string{"name", "url"}}}, "credential_files": map[string]any{"type": "object", "description": "端点名 → {path, sha256}；空文件表示清除凭据，省略保留", "additionalProperties": map[string]any{"type": "object", "properties": map[string]any{"path": text("凭据文件路径"), "sha256": text("文件 SHA-256")}, "required": []string{"path", "sha256"}}}}, "action"), s.manageOwnedDraft),
		wrTool("publish_resource", "将草稿发布为新的不可变版本；只发布显式导入的文件与端点。必须写 note 和 validation（实际验证方式/结果；未验证应明确写未验证）。并发冲突返回当前版本，保留草稿；读取最新版本后重新建立/合并草稿。已有任务固定版本不自动升级。", objSchema(map[string]any{"draft_id": integer, "revision": integer, "note": text("变更说明"), "validation": text("实际完成的验证，禁止声称未做的验证")}, "draft_id", "revision", "note", "validation"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID               int64 `json:"draft_id"`
				Revision         int64 `json:"revision"`
				Note, Validation string
			}
			if json.Unmarshal(in, &a) != nil {
				return actool.Fail("invalid_input", "参数不是有效 JSON 或字段类型不正确。", "按当前工具 schema 提供 JSON 对象；ID 和 revision 使用整数。"), nil
			}
			actor, e := resourceActor(ctx)
			if e != nil {
				return resourceToolError(e)
			}
			v, e := s.resourceLibrary().Publish(ctx, a.ID, a.Revision, a.Note, a.Validation, actor)
			if e != nil {
				return resourceToolError(e)
			}
			return jsonResult(v)
		}),
		wrTool("use_resource", "为当前任务领取并固定自有资源版本。首次省略 version_id 使用最新版本，此后继续原版本；受限资源需先绑定任务。显式切换需同时提供目标 version_id 和当前 expected_version_id；冲突时重新读取，禁止盲目重试。切换影响任务后续领取，已返回的旧路径仍保留，其他 Worker 在回合边界收到通知。返回文件目录、端点、凭据文件路径，不输出凭据、不自动执行程序或设置代理。已挂载版本只读；运行中新增版本提供独立任务副本。持久服务需独立于一次性 Worker 沙盒运行。", objSchema(map[string]any{"resource_id": integer, "version_id": integer, "expected_version_id": integer}, "resource_id"), s.useOwnedResource),
		wrTool("report_resource_status", "登记当前 Runner 对指定版本的可达性或使用观察，只影响这条观察记录，不停用全局服务。不要把凭据写入 detail。", objSchema(map[string]any{"resource_id": integer, "version_id": integer, "status": map[string]any{"type": "string", "enum": []string{"available", "unreachable", "untested"}}, "detail": text("实际观察结果")}, "resource_id", "version_id", "status"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				ID             int64 `json:"resource_id"`
				VersionID      int64 `json:"version_id"`
				Status, Detail string
			}
			if json.Unmarshal(in, &a) != nil {
				return actool.Fail("invalid_input", "参数不是有效 JSON 或字段类型不正确。", "按当前工具 schema 提供 JSON 对象；ID 和 revision 使用整数。"), nil
			}
			actor, e := resourceActor(ctx)
			if e != nil {
				return resourceToolError(e)
			}
			if e = s.resourceLibrary().Observe(ctx, a.ID, a.VersionID, a.Status, a.Detail, actor); e != nil {
				return resourceToolError(e)
			}
			return jsonResult(map[string]bool{"recorded": true})
		}),
	}, s.resourceLifecycleTools()...)
}
func (s *Server) manageOwnedResource(ctx context.Context, in json.RawMessage) (actool.Result, error) {
	var a struct {
		Action    string
		RequestID string `json:"request_id"`
		Resource  resourcelib.Resource
	}
	a.Resource = resourcelib.Resource{Enabled: true, AccessMode: "global"}
	if json.Unmarshal(in, &a) != nil {
		return actool.Fail("invalid_input", "参数不是有效 JSON 或字段类型不正确。", "按当前工具 schema 提供 JSON 对象；ID 和 revision 使用整数。"), nil
	}
	actor, e := resourceActor(ctx)
	if e != nil {
		return resourceToolError(e)
	}
	switch a.Action {
	case "create":
		e = s.resourceLibrary().CreateWithRequest(ctx, &a.Resource, actor, a.RequestID)
	case "update":
		e = s.resourceLibrary().UpdateWithRequest(ctx, &a.Resource, actor, a.RequestID)
	default:
		return actool.Fail("invalid_action", "未知资源操作。", "action 使用 create 或 update；删除整个资源使用 delete_resource。"), nil
	}
	if e != nil {
		return resourceToolError(e)
	}
	return jsonResult(a.Resource)
}

type resourceSource struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// The digest is computed in the execution environment by the caller. Matching it
// prevents silently importing an older controller snapshot or a changing file.
type verifiedResourceReader struct {
	f        *os.File
	h        hash.Hash
	expected string
	before   os.FileInfo
}

func (v *verifiedResourceReader) Read(p []byte) (int, error) {
	n, e := v.f.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
	}
	if e == io.EOF {
		after, err := v.f.Stat()
		if err != nil {
			return n, err
		}
		if after.Size() != v.before.Size() || !after.ModTime().Equal(v.before.ModTime()) || hex.EncodeToString(v.h.Sum(nil)) != v.expected {
			return n, errors.New("source changed or checksum mismatch")
		}
	}
	return n, e
}
func (s *Server) resourceSource(ctx context.Context, source resourceSource) (*verifiedResourceReader, error) {
	b, e := hex.DecodeString(source.SHA256)
	if e != nil || len(b) != 32 {
		return nil, errors.New("checksum required")
	}
	info := agent.RunInfoFrom(ctx)
	root := filepath.Join(s.m.dir, "tasks", strconv.FormatInt(info.TaskID, 10))
	f, e := resourcelib.OpenTaskFile(root, source.Path)
	if e != nil {
		return nil, e
	}
	st, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, e
	}
	return &verifiedResourceReader{f: f, h: sha256.New(), expected: strings.ToLower(source.SHA256), before: st}, nil
}
func (s *Server) manageOwnedDraft(ctx context.Context, in json.RawMessage) (actool.Result, error) {
	var a struct {
		Action                 string
		ID                     int64 `json:"draft_id"`
		ResourceID             int64 `json:"resource_id"`
		BaseVersionID          int64 `json:"base_version_id"`
		Revision               int64
		SourcePath             string `json:"source_path"`
		Path, SHA256, OS, Arch string
		Executable             bool
		Endpoints              []resourcelib.Endpoint
		CredentialFiles        map[string]resourceSource `json:"credential_files"`
	}
	if json.Unmarshal(in, &a) != nil {
		return actool.Fail("invalid_input", "参数不是有效 JSON 或字段类型不正确。", "按当前工具 schema 提供 JSON 对象；ID 和 revision 使用整数。"), nil
	}
	actor, e := resourceActor(ctx)
	if e != nil {
		return resourceToolError(e)
	}
	lib := s.resourceLibrary()
	var d resourcelib.Draft
	if a.Action == "create" {
		d, e = lib.NewDraft(ctx, a.ResourceID, a.BaseVersionID, actor)
	} else {
		d, e = lib.Draft(ctx, a.ID)
		if e != nil {
			return resourceToolError(e)
		}
		if e = s.authorizeResource(ctx, d.ResourceID, actor); e != nil {
			return resourceToolError(e)
		}
		switch a.Action {
		case "get":
		case "import_file":
			if strings.Contains(a.SourcePath, "resource-credentials") {
				return actool.Fail("credential_import_denied", "凭据文件不能当普通共享文件导入。", "使用 resource_draft(action=configure) 的 credential_files 登记，内容不会返回或写入公开文件。"), nil
			}
			var reader *verifiedResourceReader
			reader, e = s.resourceSource(ctx, resourceSource{a.SourcePath, a.SHA256})
			if e == nil {
				defer reader.f.Close()
				d, e = lib.Upload(ctx, a.ID, a.Revision, a.Path, a.Executable, reader, actor)
			}
		case "remove_file":
			d, e = lib.RemoveFile(ctx, a.ID, a.Revision, a.Path, actor)
		case "configure":
			secrets := map[string]string{}
			for name, source := range a.CredentialFiles {
				reader, err := s.resourceSource(ctx, source)
				if err != nil {
					return resourceToolError(err)
				}
				data, err := io.ReadAll(io.LimitReader(reader, 65537))
				reader.f.Close()
				if err != nil {
					clear(data)
					return resourceToolError(err)
				}
				if len(data) > 65536 {
					clear(data)
					return resourceToolError(resourcelib.ErrLimit)
				}
				secrets[name] = string(data)
				clear(data)
			}
			d, e = lib.Configure(ctx, a.ID, a.Revision, a.Endpoints, a.OS, a.Arch, secrets, actor)
		case "discard":
			e = lib.Discard(ctx, a.ID, a.Revision, actor)
			if e == nil {
				return jsonResult(map[string]bool{"discarded": true})
			}
		default:
			return actool.Fail("invalid_action", "未知草稿操作。", "action 使用 create/get/import/remove/configure/discard；发布使用 resource_publish。"), nil
		}
	}
	if e != nil {
		return resourceToolError(e)
	}
	return jsonResult(d)
}
func (s *Server) useOwnedResource(ctx context.Context, in json.RawMessage) (actool.Result, error) {
	var a struct {
		ID        int64  `json:"resource_id"`
		VersionID int64  `json:"version_id"`
		Expected  *int64 `json:"expected_version_id"`
	}
	if json.Unmarshal(in, &a) != nil {
		return actool.Fail("invalid_input", "参数不是有效 JSON 或字段类型不正确。", "按当前工具 schema 提供 JSON 对象；ID 和 revision 使用整数。"), nil
	}
	actor, e := resourceActor(ctx)
	if e != nil {
		return resourceToolError(e)
	}
	lib := s.resourceLibrary()
	var v resourcelib.Version
	if a.Expected != nil {
		v, e = lib.Switch(ctx, actor.TaskID, a.ID, a.VersionID, *a.Expected)
	} else {
		v, e = lib.Pin(ctx, actor.TaskID, a.ID, a.VersionID, false)
	}
	if e != nil {
		return resourceToolError(e)
	}
	root := filepath.Join(s.m.dir, "tasks", strconv.FormatInt(actor.TaskID, 10))
	path := ""
	readOnly := false
	snapshot := false
	if rt, ok := ctx.Value(remoteExecutionKey{}).(*remoteExecution); ok {
		var plan runnerfiles.Plan
		_ = json.Unmarshal(rt.spec.Files, &plan)
		snapshot = plan.Mode != runnerfiles.SharedMode
		for _, mount := range plan.Mounts {
			if mount.Name == fmt.Sprintf("resource-%d", v.ID) {
				path = mount.Target
				readOnly = true
				break
			}
		}
	}
	if path == "" {
		path, e = lib.Materialize(ctx, v, root)
		if e != nil {
			return resourceToolError(e)
		}
		if snapshot {
			path = resourcePublishedPath(root, path)
			readOnly = true
		}
	}
	secrets, e := lib.Credentials(ctx, actor.TaskID, v.ID)
	if e != nil {
		return resourceToolError(e)
	}
	credentials := map[string]string{}
	for name, value := range secrets {
		p, err := resourcelib.WriteCredential(ctx, root, v, name, []byte(value))
		if err != nil {
			return resourceToolError(err)
		}
		if snapshot {
			p = resourcePublishedPath(root, p)
		}
		credentials[name] = p
	}
	doc, e := lib.ReadDocument(ctx, v.Manifest)
	if e != nil {
		return resourceToolError(e)
	}
	return jsonResult(map[string]any{"document": doc, "resource_id": v.ResourceID, "version_id": v.ID, "version": v.Version, "path": path, "files": v.Files[:min(len(v.Files), 100)], "file_count": len(v.Files), "files_truncated": len(v.Files) > 100, "endpoints": v.Endpoints, "credential_files": credentials, "read_only": readOnly, "usage": "修改文件请先复制到当前任务可写目录，完成测试后显式导入草稿并发布。网络观察仅代表当前 Runner。"})
}
func resourcePublishedPath(root, path string) string {
	rel, _ := filepath.Rel(root, path)
	return filepath.Join(filepath.Dir(root), ".runner-published", filepath.Base(root), rel)
}
