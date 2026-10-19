package server

import (
	"github.com/RC-CHN/ARTEX/runner"
	"net/http"
	"strconv"
	"strings"
)

// This endpoint deliberately does not acquire the broker's execution mutex or
// query PostgreSQL: it remains useful while checkpoints are blocked.
func (s *Server) runnerBrokerDiagnostics(w http.ResponseWriter, r *http.Request) {
	_, hub, err := s.runnerSnapshot()
	if hub == nil || err != nil {
		writeErr(w, 503, "Runner broker unavailable")
		return
	}
	writeJSON(w, 200, hub.Diagnostics())
}

func (s *Server) runnerSessionDiagnostics(w http.ResponseWriter, r *http.Request) {
	_, hub, err := s.runnerSnapshot()
	if hub == nil || err != nil {
		writeErr(w, 503, "Runner broker unavailable")
		return
	}
	intent := r.URL.Query().Get("intent_id")
	if intent != "" {
		if n, e := strconv.ParseInt(intent, 10, 64); e != nil || n < 1 {
			writeErr(w, 400, "invalid intent_id")
			return
		}
	}
	out := []runner.SessionStatus{}
	for _, v := range hub.SessionStatuses() {
		if task := r.URL.Query().Get("task_id"); task != "" && task != v.TaskID {
			continue
		}
		if intent != "" && !strings.Contains(v.ThreadID, ":intent:"+intent+":") {
			continue
		}
		out = append(out, v)
	}
	writeJSON(w, 200, map[string]any{"sessions": out})
}
