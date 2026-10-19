package tool

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// captureWriter is Capture for a process output stream. Keep the same head/tail,
// notice and complete artifact without buffering the entire command output in
// RAM first. Disk failures retain the existing best-effort truncation behavior.
// exec.Cmd serializes writes when Stdout and Stderr share this writer.
type captureWriter struct {
	tc              *ToolContext
	limit           int
	head, tail      []byte
	total, newlines int64
	file            *os.File
	spillFailed     bool
}

func newCaptureWriter(tc *ToolContext) *captureWriter {
	return &captureWriter{tc: tc, limit: maxOut(tc)}
}

func (w *captureWriter) failSpill() {
	w.spillFailed = true
	if w.file != nil {
		w.file.Close()
		os.Remove(w.file.Name())
		w.file = nil
	}
}

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.file == nil && !w.spillFailed && w.total+int64(len(p)) > int64(w.limit) && w.tc != nil && w.tc.OutputDir != "" {
		if err := os.MkdirAll(w.tc.OutputDir, 0755); err != nil {
			w.spillFailed = true
		} else {
			name := fmt.Sprintf("output-%d-%d.txt", time.Now().Unix(), spillSeq.Add(1))
			f, err := os.OpenFile(filepath.Join(w.tc.OutputDir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
			if err != nil {
				w.spillFailed = true
			} else {
				w.file = f
				if n, err := f.Write(w.head); err != nil || n != len(w.head) {
					w.failSpill()
				}
			}
		}
	}
	if w.file != nil {
		if n, err := w.file.Write(p); err != nil || n != len(p) {
			w.failSpill()
		}
	}
	if len(w.head) < w.limit {
		w.head = append(w.head, p[:min(len(p), w.limit-len(w.head))]...)
	}
	half := w.limit / 2
	if len(p) >= half {
		w.tail = append(w.tail[:0], p[len(p)-half:]...)
	} else {
		if len(w.tail)+len(p) > half {
			skip := len(w.tail) + len(p) - half
			copy(w.tail, w.tail[skip:])
			w.tail = w.tail[:len(w.tail)-skip]
		}
		w.tail = append(w.tail, p...)
	}
	w.total += int64(len(p))
	w.newlines += int64(bytes.Count(p, []byte{'\n'}))
	return len(p), nil
}

func (w *captureWriter) finish() string {
	if w.file != nil {
		if err := w.file.Close(); err == nil {
			ref := w.file.Name()
			if w.tc.WorkingDir != "" {
				if rel, e := filepath.Rel(w.tc.WorkingDir, ref); e == nil && !strings.HasPrefix(rel, "..") {
					ref = rel
				}
			}
			return string(w.head) + fmt.Sprintf("\n\n... <persisted-output>[Output too large: full %d bytes / %d lines.Full output saved to  %s </persisted-output>", w.total, w.newlines+1, ref)
		}
		w.failSpill()
	}
	if w.total <= int64(w.limit) {
		return string(w.head)
	}
	return string(w.head[:w.limit/2]) + fmt.Sprintf("\n\n... [%d characters truncated] ...\n\n", w.total-int64(w.limit)) + string(w.tail)
}

var _ io.Writer = (*captureWriter)(nil)
