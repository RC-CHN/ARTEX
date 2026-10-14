package runnerfiles

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Authorize func(*http.Request, string, bool) (Plan, error)
type CommitGuard func(*http.Request, string, func() error) error

// Handler is mounted outside the browser JWT middleware: Authorize validates a
// runner credential and binds every operation to its current execution attempt.
func Handler(store *Store, authorize Authorize, commit CommitGuard) http.Handler {
	mux := http.NewServeMux()
	resolve := func(w http.ResponseWriter, r *http.Request, write bool) (Mount, bool) {
		plan, e := authorize(r, r.URL.Query().Get("session"), write)
		if e != nil {
			http.Error(w, "execution is not authorized", http.StatusForbidden)
			return Mount{}, false
		}
		if plan.Mode == SharedMode {
			http.Error(w, "shared mounts require direct storage access; snapshot transfer is disabled", http.StatusForbidden)
			return Mount{}, false
		}
		for _, m := range plan.Mounts {
			if m.Name == r.URL.Query().Get("mount") {
				return m, true
			}
		}
		http.Error(w, "unknown mount", http.StatusForbidden)
		return Mount{}, false
	}
	mux.HandleFunc("GET /manifest", func(w http.ResponseWriter, r *http.Request) {
		m, ok := resolve(w, r, false)
		if !ok {
			return
		}
		entries, e := store.Manifest(r.Context(), m)
		if e != nil {
			fileError(w, e)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entries)
	})
	mux.HandleFunc("GET /blob", func(w http.ResponseWriter, r *http.Request) {
		m, ok := resolve(w, r, false)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, e := store.ReadContext(r.Context(), m, r.URL.Query().Get("path"), r.URL.Query().Get("digest"), w)
		if e != nil {
			fileError(w, e)
		}
	})
	mux.HandleFunc("PUT /blob", func(w http.ResponseWriter, r *http.Request) {
		m, ok := resolve(w, r, true)
		if !ok {
			return
		}
		mode, e := strconv.ParseUint(r.Header.Get("X-File-Mode"), 8, 32)
		if e != nil {
			http.Error(w, "invalid file mode", 400)
			return
		}
		session := r.URL.Query().Get("session")
		guard := func(f func() error) error {
			if commit != nil {
				return commit(r, session, f)
			}
			if _, e := authorize(r, session, true); e != nil {
				return e
			}
			return f()
		}
		entry, e := store.Put(r.Context(), m, r.URL.Query().Get("path"), strings.Trim(r.Header.Get("If-Match"), "\""), r.Header.Get("X-Content-SHA256"), uint32(mode), session, r.Header.Get("X-File-Lease"), r.Body, guard)
		if e != nil {
			fileError(w, e)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entry)
	})
	mux.HandleFunc("POST /lock", func(w http.ResponseWriter, r *http.Request) {
		m, ok := resolve(w, r, true)
		if !ok {
			return
		}
		token, e := store.Lease(m, r.URL.Query().Get("path"), r.URL.Query().Get("session"), time.Minute)
		if e != nil {
			fileError(w, e)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
	})
	mux.HandleFunc("DELETE /lock", func(w http.ResponseWriter, r *http.Request) {
		m, ok := resolve(w, r, true)
		if !ok {
			return
		}
		if e := store.Unlock(m, r.URL.Query().Get("path"), r.URL.Query().Get("session"), r.Header.Get("X-File-Lease")); e != nil {
			fileError(w, e)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
func fileError(w http.ResponseWriter, e error) {
	if errors.Is(e, ErrSourceBusy) {
		w.Header().Set("X-ARTEX-File-Error", "source-busy")
	}
	status := http.StatusInternalServerError
	switch {
	case errors.Is(e, ErrConflict):
		status = 409
	case errors.Is(e, ErrLocked):
		status = 423
	case errors.Is(e, ErrPath):
		status = 403
	case errors.Is(e, ErrDigest):
		status = 422
	case errors.Is(e, ErrLimit):
		status = 413
	case errors.Is(e, fs.ErrNotExist):
		status = 404
	}
	http.Error(w, e.Error(), status)
}
