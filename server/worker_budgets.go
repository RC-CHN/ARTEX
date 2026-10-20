package server

import (
	"net/http"
	"strconv"

	"github.com/RC-CHN/ARTEX/db"
)

func (s *Server) workerBudgets(w http.ResponseWriter, r *http.Request) {
	taskID, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if r.Method == http.MethodGet {
		after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		rows, err := s.m.pg.ListWorkerBudgets(r.Context(), taskID, after)
		if err != nil {
			resourceErr(w, err)
			return
		}
		writeJSON(w, 200, rows)
		return
	}
	var in db.WorkerBudgetGrant
	if !resourceDecode(w, r, &in) {
		return
	}
	out, err := s.m.pg.GrantWorkerBudget(r.Context(), taskID, in, "human")
	if err != nil {
		resourceErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"budget": out, "automatic_resume": false})
}
