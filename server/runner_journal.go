package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/google/uuid"
)

const runnerJournalRecent = 500

type runnerJournalRecord struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	JournalDropped uint64 `json:"journal_dropped_total,omitempty"`
	runner.Event
}

type runnerJournalStatus struct {
	Path          string    `json:"path"`
	Pending       int       `json:"pending"`
	Dropped       uint64    `json:"dropped"`
	WriteFailures uint64    `json:"write_failures"`
	InvalidLines  uint64    `json:"invalid_lines"`
	LastErrorCode string    `json:"last_error_code,omitempty"`
	LastWriteAt   time.Time `json:"last_write_at,omitempty"`
	LastPersisted string    `json:"last_persisted_id,omitempty"`
	HistoryLoaded bool      `json:"history_loaded"`
}

// The queue never performs disk/DB I/O on a broker goroutine. Recent records are
// also kept in memory, so a broken filesystem cannot block the diagnostic API.
// Disk writes are at least once: stable record IDs allow deduplication after an
// uncertain write/fsync. Overflow and write failures are explicitly observable.
type runnerJournal struct {
	mu      sync.Mutex
	status  runnerJournalStatus
	recent  []runnerJournalRecord
	queue   chan runnerJournalRecord
	done    chan struct{}
	stop    chan struct{}
	closed  bool
	process string
	seq     uint64
	maxSize int64
	backups int
}

func newRunnerJournal(dir string) *runnerJournal {
	return startRunnerJournal(filepath.Join(dir, "diagnostics", "runner-incidents.jsonl"), 8<<20, 4, 1024)
}

func startRunnerJournal(path string, maxSize int64, backups, capacity int) *runnerJournal {
	j := &runnerJournal{status: runnerJournalStatus{Path: path}, queue: make(chan runnerJournalRecord, capacity), done: make(chan struct{}), stop: make(chan struct{}), process: uuid.NewString(), maxSize: maxSize, backups: backups}
	go j.run()
	return j
}

func (j *runnerJournal) Record(event runner.Event) {
	if j == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		j.status.Dropped++
		return
	}
	j.seq++
	record := runnerJournalRecord{ID: fmt.Sprintf("%s-%d", j.process, j.seq), Version: BuildVersion, Event: event}
	j.recent = append(j.recent, record)
	if len(j.recent) > runnerJournalRecent {
		j.recent = append([]runnerJournalRecord(nil), j.recent[len(j.recent)-runnerJournalRecent:]...)
	}
	select {
	case j.queue <- record:
		j.status.Pending++
	default:
		j.status.Dropped++
	}
}

func (j *runnerJournal) snapshot(limit int, incident string) (runnerJournalStatus, []runnerJournalRecord) {
	if j == nil {
		return runnerJournalStatus{LastErrorCode: "not_initialized"}, []runnerJournalRecord{}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	items := make([]runnerJournalRecord, 0, limit)
	for i := len(j.recent) - 1; i >= 0 && len(items) < limit; i-- {
		v := j.recent[i]
		if incident == "" || v.IncidentID == incident {
			items = append(items, v)
		}
	}
	return j.status, items
}

func (j *runnerJournal) Close() {
	if j == nil {
		return
	}
	j.mu.Lock()
	if !j.closed {
		j.closed = true
		close(j.stop)
		close(j.queue)
	}
	j.mu.Unlock()
	select {
	case <-j.done:
	case <-time.After(5 * time.Second):
		log.Printf("runner_journal code=shutdown_flush_timeout")
	}
}

func journalFailureCode(err error) string {
	switch {
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, syscall.ENOSPC):
		return "no_space"
	default:
		return "io_error"
	}
}

func (j *runnerJournal) loadHistory() {
	var records []runnerJournalRecord
	seen := map[string]bool{}
	var invalid uint64
	for i := j.backups; i >= 0; i-- {
		path := j.status.Path
		if i > 0 {
			path += "." + strconv.Itoa(i)
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		// Read a bounded tail of each retained file, never a growing full log.
		tail := false
		if st, err := f.Stat(); err == nil && st.Size() > 1<<20 {
			_, _ = f.Seek(st.Size()-(1<<20), io.SeekStart)
			tail = true
		}
		reader := bufio.NewReader(f)
		if tail {
			_, _ = reader.ReadBytes('\n') // discard the intentionally clipped first line
		}
		scan := bufio.NewScanner(reader)
		scan.Buffer(make([]byte, 4096), 64<<10)
		for scan.Scan() {
			var r runnerJournalRecord
			if json.Unmarshal(scan.Bytes(), &r) != nil || r.ID == "" || r.Kind == "" {
				invalid++
				continue
			}
			if !seen[r.ID] {
				seen[r.ID] = true
				records = append(records, r)
			}
		}
		_ = f.Close()
	}
	j.mu.Lock()
	records = append(records, j.recent...)
	if len(records) > runnerJournalRecent {
		records = records[len(records)-runnerJournalRecent:]
	}
	j.recent = append([]runnerJournalRecord(nil), records...)
	j.status.InvalidLines, j.status.HistoryLoaded = invalid, true
	j.mu.Unlock()
}

func (j *runnerJournal) open() (*os.File, int64, error) {
	path := j.status.Path // immutable
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, 0, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, 0, err
	}
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	size := st.Size()
	// A crash may leave a partial last line. Separate it from subsequent JSON
	// records; readers skip the partial record and report invalid_lines.
	if size > 0 {
		var last [1]byte
		if _, err = f.ReadAt(last[:], size-1); err == nil && last[0] != '\n' {
			_, err = f.Write([]byte{'\n'})
			size++
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, size, nil
}

func (j *runnerJournal) rotate() error {
	for i := j.backups; i > 0; i-- {
		from := j.status.Path
		if i > 1 {
			from += "." + strconv.Itoa(i-1)
		}
		to := j.status.Path + "." + strconv.Itoa(i)
		if i == j.backups {
			if err := os.Remove(to); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (j *runnerJournal) run() {
	defer close(j.done)
	j.loadHistory()
	var file *os.File
	var size int64
	defer func() {
		if file != nil {
			_ = file.Close()
		}
	}()
	var lastReport time.Time
	for record := range j.queue {
		j.mu.Lock()
		record.JournalDropped = j.status.Dropped
		j.mu.Unlock()
		line, err := json.Marshal(record)
		if err != nil || len(line) >= 64<<10 {
			j.mu.Lock()
			j.status.Dropped++
			j.status.Pending--
			j.status.LastErrorCode = "record_too_large"
			j.mu.Unlock()
			continue
		}
		line = append(line, '\n')
		for {
			if file == nil {
				file, size, err = j.open()
			}
			if err == nil && size > 0 && size+int64(len(line)) > j.maxSize {
				err = file.Close()
				file = nil
				if err == nil {
					err = j.rotate()
				}
				if err == nil {
					file, size, err = j.open()
				}
			}
			if err == nil {
				var n int
				n, err = file.Write(line)
				size += int64(n)
				if err == nil && n != len(line) {
					err = io.ErrShortWrite
				}
				if err == nil {
					err = file.Sync()
				}
			}
			j.mu.Lock()
			if err == nil {
				j.status.Pending--
				j.status.LastWriteAt, j.status.LastPersisted, j.status.LastErrorCode = time.Now(), record.ID, ""
				j.mu.Unlock()
				break
			}
			j.status.WriteFailures++
			j.status.LastErrorCode = journalFailureCode(err)
			closed, code := j.closed, j.status.LastErrorCode
			if closed {
				j.status.Dropped += uint64(j.status.Pending)
				j.status.Pending = 0
			}
			j.mu.Unlock()
			if time.Since(lastReport) >= 30*time.Second {
				log.Printf("runner_journal code=%s", code)
				lastReport = time.Now()
			}
			if file != nil {
				_ = file.Close()
				file = nil
			}
			if closed {
				return
			}
			select {
			case <-j.stop:
			case <-time.After(time.Second):
			}
		}
	}
}

func (s *Server) runnerIncidents(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > runnerJournalRecent {
			writeErr(w, 400, "limit must be 1..500")
			return
		}
		limit = n
	}
	incident := r.URL.Query().Get("incident_id")
	if len(incident) > 64 || bytes.ContainsAny([]byte(incident), "\r\n") {
		writeErr(w, 400, "invalid incident_id")
		return
	}
	status, records := s.runnerJournal.snapshot(limit, incident)
	writeJSON(w, 200, map[string]any{"journal": status, "events": records})
}
