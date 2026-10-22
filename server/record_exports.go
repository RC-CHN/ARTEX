package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/runtimeobs"
	"github.com/RC-CHN/ARTEX/recordexport"
	"github.com/RC-CHN/ARTEX/resourcelib"
)

func (s *Server) recordExportRoot() string { return filepath.Join(s.m.dir, "exports", "records") }

// Optional operator-selected scratch storage. The original export root remains
// the default and completed ZIPs always live there. The existing byte cap applies.
func (s *Server) recordExportScratch() string {
	if p := strings.TrimSpace(os.Getenv("ARTEX_RECORD_EXPORT_SCRATCH")); p != "" && filepath.IsAbs(p) {
		return filepath.Join(p, "artex-record-export")
	}
	return s.recordExportRoot()
}
func (s *Server) recordExportPath(job db.RecordExport) string {
	return filepath.Join(s.recordExportRoot(), fmt.Sprintf("%d-%s.zip", job.ID, job.Owner))
}
func (s *Server) recordExportLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastGC := time.Time{}
	for {
		if s.ctx.Err() != nil {
			return
		}
		if time.Since(lastGC) > time.Hour {
			s.gcRecordExports()
			lastGC = time.Now()
		}
		job, err := s.m.pg.ClaimRecordExport(s.ctx, rand.Text())
		if err == nil {
			if err = s.buildRecordExport(s.ctx, job); err != nil {
				// Log identifiers and error class only; task text and filenames stay in
				// the authenticated export manifest, not operational logs.
				message := "导出未完成：源记录不可读取或存储/数据库操作失败。原始任务未改动；请重试。"
				if errors.Is(err, recordexport.ErrCapacity) {
					message = "导出达到20 GiB或20万个文件上限。减少单次任务选择，原始记录未改动。"
				}
				if errors.Is(err, context.Canceled) {
					message = "导出已中断；可重试生成新的快照。"
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.m.pg.FailRecordExport(ctx, job.ID, job.Owner, message)
				cancel()
				log.Printf("[record-export] id=%d attempt=%d failed canceled=%t capacity=%t", job.ID, job.Attempt, errors.Is(err, context.Canceled), errors.Is(err, recordexport.ErrCapacity))
			}
			continue
		} else if !errors.Is(err, sql.ErrNoRows) && s.ctx.Err() == nil {
			log.Printf("[record-export] queue unavailable")
		}
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) buildRecordExport(parent context.Context, job db.RecordExport) (err error) {
	start := time.Now()
	defer func() { runtimeobs.Record(runtimeobs.ExportBuild, start, err != nil) }()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// Admission protects originals against concurrent cold archive/deletion,
	// while workers may continue and races are reported in the manifest.
	for _, id := range job.TaskIDs {
		key := strconv.FormatInt(id, 10)
		if s.engine != nil {
			if !s.engine.beginTaskOperation(key) {
				return fmt.Errorf("task operation unavailable")
			}
			defer s.engine.decInflight(key)
		}
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				heartbeatCtx, heartbeatCancel := context.WithTimeout(ctx, 5*time.Second)
				e := s.m.pg.HeartbeatRecordExport(heartbeatCtx, job.ID, job.Owner)
				heartbeatCancel()
				if e != nil {
					cancel()
					return
				}
			}
		}
	}()
	if err = os.MkdirAll(s.recordExportRoot(), 0700); err != nil {
		return err
	}
	if err = os.MkdirAll(s.recordExportScratch(), 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(s.recordExportScratch(), fmt.Sprintf(".%d-%s-", job.ID, job.Owner))
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	zipPath := s.recordExportPath(job)
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(zipPath)
		}
	}()
	b, err := recordexport.New(ctx, stage, job.TaskIDs, job.Redacted, db.MaxRecordExportBytes)
	if err != nil {
		return err
	}
	progress := func(phase string) error {
		return s.m.pg.ProgressRecordExport(ctx, job.ID, job.Owner, phase, int64(len(b.Manifest.Files)), b.Bytes)
	}
	snapshots, err := s.m.pg.SnapshotTaskRecords(ctx, job.TaskIDs, func(taskID int64, table string) (io.WriteCloser, error) {
		w, e := b.Open(fmt.Sprintf("tasks/%d/database/%s.jsonl", taskID, table))
		if e != nil {
			return nil, e
		}
		if job.Redacted {
			w = recordexport.RedactJSON(w)
		}
		return &exportProgressWriter{WriteCloser: w, after: func() error { return progress("database") }}, nil
	})
	if err != nil {
		return err
	}
	b.Manifest.Cutoff = snapshots[0].CreatedAt
	for _, snapshot := range snapshots {
		prefix := fmt.Sprintf("tasks/%d", snapshot.TaskID)
		if err = b.JSON(prefix+"/snapshot.json", snapshot); err != nil {
			return err
		}
		if err = exportHandoff(b, prefix); err != nil {
			return err
		}
		if err = progress("files"); err != nil {
			return err
		}
		if err = b.Tree(filepath.Join(s.m.dir, "tasks", fmt.Sprint(snapshot.TaskID)), prefix+"/workspace", snapshot.CreatedAt); err != nil {
			return err
		}
		if err = s.exportTranscripts(b, snapshot); err != nil {
			return err
		}
		if snapshot.DataCounts["llm_records"] == 0 {
			b.Omit(prefix+"/database/llm_records.jsonl", "no_raw_model_records_retained; recording_may_have_been_disabled")
		}
		if err = readExportRows(filepath.Join(stage, prefix, "database", "owned_resource_versions.jsonl"), func(raw json.RawMessage) error {
			var v resourcelib.Version
			if e := json.Unmarshal(raw, &v); e != nil {
				return e
			}
			return b.Tree(s.resourceLibrary().VersionPath(v), fmt.Sprintf("%s/resources/%d/version-%d", prefix, v.ResourceID, v.ID), time.Time{})
		}); err != nil {
			return err
		}
		if err = readExportRows(filepath.Join(stage, prefix, "database", "traffic_evidence_snapshots.jsonl"), func(raw json.RawMessage) error {
			var v db.TrafficEvidenceSnapshot
			if e := json.Unmarshal(raw, &v); e != nil {
				return e
			}
			return s.exportEvidence(b, prefix, v)
		}); err != nil {
			return err
		}
		b.Omit(prefix+"/traffic/legacy-unattributed", "legacy_traffic_has_host_but_no_task_attribution; only_explicitly_bound_evidence_included")
	}
	if err = progress("index"); err != nil {
		return err
	}
	if err = b.Index(); err != nil {
		return err
	}
	if err = progress("package"); err != nil {
		return err
	}
	checksum, size, err := b.Finish(zipPath)
	if err != nil {
		return err
	}
	if size > db.MaxRecordExportBytes {
		return recordexport.ErrCapacity
	}
	if err = s.m.pg.FinishRecordExport(ctx, job.ID, job.Owner, checksum, b.Manifest.Cutoff, int64(len(b.Manifest.Files)), size); err != nil {
		return err
	}
	committed = true
	return nil
}

type exportProgressWriter struct {
	io.WriteCloser
	after func() error
}

func (w *exportProgressWriter) Close() error {
	if err := w.WriteCloser.Close(); err != nil {
		return err
	}
	return w.after()
}
func readExportRows(file string, consume func(json.RawMessage) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	for {
		var raw json.RawMessage
		if err = decoder.Decode(&raw); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if err = consume(raw); err != nil {
			return err
		}
	}
}
func (s *Server) exportTranscripts(b *recordexport.Builder, snapshot *db.TaskArchiveSnapshot) error {
	dir := filepath.Join(s.m.dir, "transcripts")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		b.Omit(fmt.Sprintf("tasks/%d/transcripts", snapshot.TaskID), "no_transcripts_retained")
		return nil
	}
	if err != nil {
		return err
	}
	prefix := fmt.Sprintf("exp%d-", snapshot.ExplorationID)
	found := false
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			b.Omit(e.Name(), "transcript_symlink_not_followed")
			continue
		}
		source := filepath.Join(dir, e.Name())
		walk := func(p string, item fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if item.IsDir() {
				return nil
			}
			if item.Type()&os.ModeSymlink != 0 {
				b.Omit(item.Name(), "transcript_symlink_not_followed")
				return nil
			}
			if !strings.HasSuffix(item.Name(), ".jsonl") {
				return nil
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			dest := fmt.Sprintf("tasks/%d/transcripts/%s", snapshot.TaskID, strings.TrimSuffix(filepath.ToSlash(rel), ".jsonl"))
			found = true
			return b.Transcript(p, dest)
		}
		if err = filepath.WalkDir(source, walk); err != nil {
			return err
		}
	}
	if !found {
		b.Omit(fmt.Sprintf("tasks/%d/transcripts", snapshot.TaskID), "no_transcripts_retained")
	}
	return nil
}
func (s *Server) exportEvidence(b *recordexport.Builder, prefix string, v db.TrafficEvidenceSnapshot) error {
	for _, side := range []string{"request", "response"} {
		name := fmt.Sprintf("%s/evidence/%s/%s.http", prefix, v.ID, side)
		file, length, err := s.evidenceStore().OpenBody(v, side)
		if err != nil {
			b.Omit(name, "captured_body_missing")
			continue
		}
		head := v.ReqHead
		if side == "response" {
			head = v.RespHead
		}
		if b.Manifest.Redacted {
			if length > 8<<20 {
				file.Close()
				b.Omit(name, "redacted_copy_excludes_large_body")
				continue
			}
			data, e := io.ReadAll(io.LimitReader(file, 8<<20+1))
			file.Close()
			if e != nil {
				return e
			}
			if !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
				b.Omit(name, "redacted_copy_excludes_binary_body")
				continue
			}
			filtered := append(recordexport.Redact([]byte(strings.TrimRight(head, "\r\n"))), []byte("\r\n\r\n")...)
			filtered = append(filtered, recordexport.Redact(data)...)
			if err = b.Write(name, filtered); err != nil {
				return err
			}
			continue
		}
		w, err := b.Open(name)
		if err != nil {
			file.Close()
			return err
		}
		_, err = io.WriteString(w, strings.TrimRight(head, "\r\n")+"\r\n\r\n")
		if err == nil {
			_, err = io.Copy(w, file)
		}
		err = errors.Join(err, file.Close(), w.Close())
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) gcRecordExports() {
	rows, err := s.m.pg.QueryContext(s.ctx, `UPDATE task_record_exports SET state='deleting',phase='deleting' WHERE state='deleting' OR (state IN ('ready','failed','canceled') AND expires_at<now()) RETURNING id,owner`)
	if err != nil {
		return
	}
	var jobs []db.RecordExport
	for rows.Next() {
		var v db.RecordExport
		if rows.Scan(&v.ID, &v.Owner) == nil {
			jobs = append(jobs, v)
		}
	}
	rows.Close()
	for _, job := range jobs {
		_ = s.deleteRecordExportCopy(s.ctx, job)
	}
	s.gcOrphanRecordCopies()
}

var recordCopyName = regexp.MustCompile(`^\.?([0-9]+)-([A-Za-z0-9]+)(?:\.zip|-[A-Za-z0-9]+)$`)

// Cleanup only files/directories generated by this exporter, after a grace
// period and a successful ownership read. Original task trees are elsewhere.
func (s *Server) gcOrphanRecordCopies() {
	rows, err := s.m.pg.QueryContext(s.ctx, `SELECT id,owner FROM task_record_exports WHERE state='ready' OR (state='running' AND lease_until>=now())`)
	if err != nil {
		return
	}
	keep := map[string]bool{}
	for rows.Next() {
		var id int64
		var owner string
		if err = rows.Scan(&id, &owner); err != nil {
			rows.Close()
			return
		}
		keep[fmt.Sprintf("%d-%s", id, owner)] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	roots := []string{s.recordExportRoot()}
	if scratch := s.recordExportScratch(); scratch != roots[0] {
		roots = append(roots, scratch)
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			match := recordCopyName.FindStringSubmatch(entry.Name())
			if match == nil || keep[match[1]+"-"+match[2]] {
				continue
			}
			info, err := entry.Info()
			if err != nil || time.Since(info.ModTime()) < 10*time.Minute {
				continue
			}
			_ = os.RemoveAll(filepath.Join(root, entry.Name()))
		}
	}
}
func (s *Server) deleteRecordExportCopy(ctx context.Context, job db.RecordExport) error {
	if job.Owner != "" {
		if err := os.Remove(s.recordExportPath(job)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_, err := s.m.pg.ExecContext(ctx, `UPDATE task_record_exports SET state='deleted',phase='deleted',bytes=0,updated_at=now() WHERE id=$1 AND state='deleting'`, job.ID)
	return err
}
func (s *Server) recordExportsAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		rows, err := s.m.pg.RecordExports(r.Context(), resourceInt(r, "task_id"), resourceInt(r, "before"))
		if err != nil {
			resourceErr(w, err)
			return
		}
		writeJSON(w, 200, rows)
		return
	}
	var in db.RecordExportRequest
	if !resourceDecode(w, r, &in) {
		return
	}
	job, err := s.m.pg.QueueRecordExport(r.Context(), in, 0)
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 202, job)
}
func (s *Server) recordExportAPI(w http.ResponseWriter, r *http.Request) {
	id := resourceID(r)
	if r.Method == http.MethodGet {
		job, err := s.m.pg.RecordExport(r.Context(), id)
		if err != nil {
			resourceErr(w, err)
			return
		}
		writeJSON(w, 200, job)
		return
	}
	action := r.PathValue("action")
	if r.Method == http.MethodDelete {
		action = "delete"
	}
	job, err := s.m.pg.ChangeRecordExport(r.Context(), id, action)
	if err == nil && action == "delete" {
		err = s.deleteRecordExportCopy(r.Context(), job)
		if err == nil {
			job.State = "deleted"
			job.Bytes = 0
		}
	}
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 200, job)
}
func (s *Server) downloadRecordExport(w http.ResponseWriter, r *http.Request) {
	job, err := s.m.pg.RecordExport(r.Context(), resourceID(r))
	if err != nil {
		resourceErr(w, err)
		return
	}
	if job.State != "ready" {
		writeErr(w, 409, "导出尚未完成或已被删除。")
		return
	}
	f, err := os.Open(s.recordExportPath(job))
	if err != nil {
		writeErr(w, 410, "导出副本不可用，请重建资料包；原始任务记录未改动。")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="artex-records-%d.zip"`, job.ID))
	w.Header().Set("ETag", `"`+job.SHA256+`"`)
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, fmt.Sprintf("artex-records-%d.zip", job.ID), job.UpdatedAt, f)
}
