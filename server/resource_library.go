package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/RC-CHN/ARTEX/internal/operation"
	"github.com/RC-CHN/ARTEX/resourcelib"
)

func (s *Server) resourceLibrary() *resourcelib.Store {
	return &resourcelib.Store{DB: s.m.pg.DB, Root: filepath.Join(s.m.dir, ".resource-library"), Key: s.jwtKey}
}
func resourceID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}
func resourceInt(r *http.Request, key string) int64 {
	v, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return v
}
func resourceErr(w http.ResponseWriter, err error) {
	code := 400
	var inUse *resourcelib.InUse
	var fault *operation.Error
	switch {
	case errors.Is(err, sql.ErrNoRows):
		code = 404
	case errors.Is(err, resourcelib.ErrUnavailable):
		code = 403
	case errors.Is(err, resourcelib.ErrConflict):
		code = 409
	case errors.As(err, &inUse), errors.Is(err, operation.ErrKeyReuse):
		code = 409
	case errors.As(err, &fault):
		if fault.CurrentRevision > 0 || fault.Code == "budget_conflict" {
			code = 409
		}
	case errors.Is(err, resourcelib.ErrLimit):
		code = 413
	}
	result, e := resourceToolError(err)
	if e != nil {
		writeErr(w, 408, "操作中断；先查询结果，再决定是否重试。")
		return
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(result.Flatten()), &body)
	body["error"] = body["message"]
	writeJSON(w, code, body)
}
func resourceDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		message := "请求不是有效 JSON 对象；请检查字段名、类型和必填项。"
		var typeErr *json.UnmarshalTypeError
		var sizeErr *http.MaxBytesError
		switch {
		case errors.As(err, &typeErr):
			message = "字段 " + typeErr.Field + " 的类型错误，应为 " + typeErr.Type.String() + "。"
		case errors.As(err, &sizeErr):
			message = "请求超过 128 KiB 上限，请分批提交。"
		case strings.HasPrefix(err.Error(), "json: unknown field "):
			message = err.Error() + "；请移除未知字段。"
		}
		writeJSON(w, 400, map[string]any{"error": message, "message": message, "code": "invalid_input", "executed": false, "next_action": "按接口定义修正参数后重新提交；本次未执行。"})
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		writeErr(w, 400, "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}
func (s *Server) registerLibraryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/resources", s.listOwnedResources)
	mux.HandleFunc("POST /api/resources", s.createOwnedResource)
	mux.HandleFunc("GET /api/resources/{id}", s.getOwnedResource)
	mux.HandleFunc("PUT /api/resources/{id}", s.updateOwnedResource)
	mux.HandleFunc("DELETE /api/resources/{id}", s.deleteOwnedResource)
	mux.HandleFunc("GET /api/resources/{id}/references", s.resourceReferences)
	mux.HandleFunc("GET /api/resources/{id}/versions", s.ownedResourceVersions)
	mux.HandleFunc("GET /api/resources/{id}/versions/{version}/download", s.downloadOwnedResource)
	mux.HandleFunc("GET /api/resources/{id}/events", s.ownedResourceEvents)
	mux.HandleFunc("POST /api/resources/{id}/observations", s.observeOwnedResource)
	mux.HandleFunc("GET /api/resources/{id}/drafts", s.ownedResourceDrafts)
	mux.HandleFunc("POST /api/resources/{id}/drafts", s.createOwnedResourceDraft)
	mux.HandleFunc("GET /api/resource-drafts/{id}", s.getOwnedResourceDraft)
	mux.HandleFunc("GET /api/resource-drafts/{id}/document", s.readOwnedDraftDocument)
	mux.HandleFunc("PUT /api/resource-drafts/{id}/document", s.writeOwnedDraftDocument)
	mux.HandleFunc("GET /api/resources/{id}/versions/{version}/document", s.readOwnedVersionDocument)
	mux.HandleFunc("PUT /api/resource-drafts/{id}/configuration", s.configureOwnedResourceDraft)
	mux.HandleFunc("POST /api/resource-drafts/{id}/files", s.uploadOwnedResourceDraft)
	mux.HandleFunc("DELETE /api/resource-drafts/{id}/files", s.removeOwnedResourceDraftFile)
	mux.HandleFunc("POST /api/resource-drafts/{id}/publish", s.publishOwnedResourceDraft)
	mux.HandleFunc("DELETE /api/resource-drafts/{id}", s.discardOwnedResourceDraft)
	mux.HandleFunc("POST /api/resources/{id}/tasks", s.grantOwnedResource)
	mux.HandleFunc("DELETE /api/resources/{id}/tasks/{task}", s.revokeOwnedResource)
	mux.HandleFunc("GET /api/tasks/{id}/resources", s.taskOwnedResources)
	mux.HandleFunc("GET /api/tasks/{id}/deployments", s.taskDeployments)
	mux.HandleFunc("POST /api/tasks/{id}/deployments", s.taskDeployments)
	mux.HandleFunc("GET /api/tasks/{id}/handoff", s.taskHandoff)
	mux.HandleFunc("GET /api/tasks/{id}/worker-budgets", s.workerBudgets)
	mux.HandleFunc("POST /api/tasks/{id}/worker-budgets", s.workerBudgets)
	mux.HandleFunc("GET /api/tasks/{id}/token-budget", s.taskTokenBudget)
	mux.HandleFunc("POST /api/tasks/{id}/token-budget", s.taskTokenBudget)
	mux.HandleFunc("GET /api/task-record-exports", s.recordExportsAPI)
	mux.HandleFunc("POST /api/task-record-exports", s.recordExportsAPI)
	mux.HandleFunc("GET /api/task-record-exports/{id}", s.recordExportAPI)
	mux.HandleFunc("POST /api/task-record-exports/{id}/{action}", s.recordExportAPI)
	mux.HandleFunc("DELETE /api/task-record-exports/{id}", s.recordExportAPI)
	mux.HandleFunc("GET /api/task-record-exports/{id}/download", s.downloadRecordExport)
}
func (s *Server) listOwnedResources(w http.ResponseWriter, r *http.Request) {
	v, e := s.resourceLibrary().List(r.Context(), r.URL.Query().Get("q"), r.URL.Query().Get("kind"), resourceInt(r, "before"))
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) getOwnedResource(w http.ResponseWriter, r *http.Request) {
	v, e := s.resourceLibrary().Get(r.Context(), resourceID(r))
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) createOwnedResource(w http.ResponseWriter, r *http.Request) {
	v := resourcelib.Resource{Enabled: true, AccessMode: "global"}
	if !resourceDecode(w, r, &v) {
		return
	}
	if e := s.resourceLibrary().Create(r.Context(), &v, resourcelib.Actor{}); e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 201, v)
}
func (s *Server) updateOwnedResource(w http.ResponseWriter, r *http.Request) {
	var v resourcelib.Resource
	if !resourceDecode(w, r, &v) {
		return
	}
	v.ID = resourceID(r)
	if e := s.resourceLibrary().Update(r.Context(), &v, resourcelib.Actor{}); e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) ownedResourceVersions(w http.ResponseWriter, r *http.Request) {
	v, e := s.resourceLibrary().Versions(r.Context(), resourceID(r))
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) ownedResourceDrafts(w http.ResponseWriter, r *http.Request) {
	v, e := s.resourceLibrary().Drafts(r.Context(), resourceID(r))
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) getOwnedResourceDraft(w http.ResponseWriter, r *http.Request) {
	v, e := s.resourceLibrary().Draft(r.Context(), resourceID(r))
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) ownedResourceEvents(w http.ResponseWriter, r *http.Request) {
	v, e := s.resourceLibrary().Events(r.Context(), resourceID(r), resourceInt(r, "before"))
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) observeOwnedResource(w http.ResponseWriter, r *http.Request) {
	var a struct {
		VersionID int64  `json:"version_id"`
		Status    string `json:"status"`
		Detail    string `json:"detail"`
	}
	if !resourceDecode(w, r, &a) {
		return
	}
	if e := s.resourceLibrary().Observe(r.Context(), resourceID(r), a.VersionID, a.Status, a.Detail, resourcelib.Actor{}); e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) createOwnedResourceDraft(w http.ResponseWriter, r *http.Request) {
	var a struct {
		BaseVersionID int64 `json:"base_version_id"`
	}
	if !resourceDecode(w, r, &a) {
		return
	}
	v, e := s.resourceLibrary().NewDraft(r.Context(), resourceID(r), a.BaseVersionID, resourcelib.Actor{})
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 201, v)
}
func (s *Server) configureOwnedResourceDraft(w http.ResponseWriter, r *http.Request) {
	var a struct {
		Revision    int64                  `json:"revision"`
		Endpoints   []resourcelib.Endpoint `json:"endpoints"`
		OS          string                 `json:"os"`
		Arch        string                 `json:"arch"`
		Credentials map[string]string      `json:"credentials"`
	}
	if !resourceDecode(w, r, &a) {
		return
	}
	v, e := s.resourceLibrary().Configure(r.Context(), resourceID(r), a.Revision, a.Endpoints, a.OS, a.Arch, a.Credentials, resourcelib.Actor{})
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) uploadOwnedResourceDraft(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, resourcelib.MaxFileBytes+1)
	v, e := s.resourceLibrary().Upload(r.Context(), resourceID(r), resourceInt(r, "revision"), r.URL.Query().Get("path"), r.URL.Query().Get("executable") == "true", r.Body, resourcelib.Actor{})
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) removeOwnedResourceDraftFile(w http.ResponseWriter, r *http.Request) {
	v, e := s.resourceLibrary().RemoveFile(r.Context(), resourceID(r), resourceInt(r, "revision"), r.URL.Query().Get("path"), resourcelib.Actor{})
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) discardOwnedResourceDraft(w http.ResponseWriter, r *http.Request) {
	if e := s.resourceLibrary().Discard(r.Context(), resourceID(r), resourceInt(r, "revision"), resourcelib.Actor{}); e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) publishOwnedResourceDraft(w http.ResponseWriter, r *http.Request) {
	var a struct {
		Revision   int64  `json:"revision"`
		Note       string `json:"note"`
		Validation string `json:"validation"`
	}
	if !resourceDecode(w, r, &a) {
		return
	}
	v, e := s.resourceLibrary().Publish(r.Context(), resourceID(r), a.Revision, a.Note, a.Validation, resourcelib.Actor{})
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 201, v)
}
func (s *Server) downloadOwnedResource(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("version"), 10, 64)
	lib := s.resourceLibrary()
	v, e := lib.Version(r.Context(), id)
	if e != nil || v.ResourceID != resourceID(r) {
		writeErr(w, 404, "版本不存在")
		return
	}
	var file *resourcelib.File
	for _, f := range v.Files {
		if f.Path == r.URL.Query().Get("path") {
			f := f
			file = &f
			break
		}
	}
	if file == nil {
		writeErr(w, 404, "文件不存在")
		return
	}
	root, e := os.OpenRoot(lib.VersionPath(v))
	if e != nil {
		writeErr(w, 404, "文件不可用")
		return
	}
	defer root.Close()
	f, e := root.Open(filepath.FromSlash(file.Path))
	if e != nil {
		writeErr(w, 404, "文件不可用")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(file.Path)}))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, file.Path, v.CreatedAt, f)
}
func (s *Server) grantOwnedResource(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TaskID    int64  `json:"task_id"`
		VersionID int64  `json:"version_id"`
		Expected  *int64 `json:"expected_version_id"`
	}
	if !resourceDecode(w, r, &in) {
		return
	}
	if in.Expected == nil {
		writeErr(w, 400, "expected_version_id required (0 for first binding)")
		return
	}
	v, err := s.resourceLibrary().Grant(r.Context(), in.TaskID, resourceID(r), in.VersionID, *in.Expected)
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) revokeOwnedResource(w http.ResponseWriter, r *http.Request) {
	taskID, _ := strconv.ParseInt(r.PathValue("task"), 10, 64)
	// Serialize with Pin on the task row. Revocation prevents future credential
	// delivery; previously downloaded files cannot be recalled.
	tx, err := s.m.pg.BeginTx(r.Context(), nil)
	if err != nil {
		resourceErr(w, err)
		return
	}
	defer tx.Rollback()
	var id int64
	if err = tx.QueryRowContext(r.Context(), `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&id); err == nil {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM task_owned_resources WHERE task_id=$1 AND resource_id=$2`, taskID, resourceID(r))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
func (s *Server) taskOwnedResources(w http.ResponseWriter, r *http.Request) {
	v, err := s.resourceLibrary().TaskVersions(r.Context(), resourceID(r))
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) readOwnedDraftDocument(w http.ResponseWriter, r *http.Request) {
	lib := s.resourceLibrary()
	d, e := lib.Draft(r.Context(), resourceID(r))
	if e != nil {
		resourceErr(w, e)
		return
	}
	content, e := lib.ReadDocument(r.Context(), d.Manifest)
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]any{"content": content, "revision": d.Revision})
}
func (s *Server) writeOwnedDraftDocument(w http.ResponseWriter, r *http.Request) {
	var a struct {
		Revision int64  `json:"revision"`
		Content  string `json:"content"`
	}
	if !resourceDecode(w, r, &a) {
		return
	}
	v, e := s.resourceLibrary().Upload(r.Context(), resourceID(r), a.Revision, resourcelib.DocumentName, false, strings.NewReader(a.Content), resourcelib.Actor{})
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, v)
}
func (s *Server) readOwnedVersionDocument(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("version"), 10, 64)
	lib := s.resourceLibrary()
	v, e := lib.Version(r.Context(), id)
	if e != nil || v.ResourceID != resourceID(r) {
		writeErr(w, 404, "版本不存在")
		return
	}
	content, e := lib.ReadDocument(r.Context(), v.Manifest)
	if e != nil {
		resourceErr(w, e)
		return
	}
	writeJSON(w, 200, map[string]string{"content": content})
}
