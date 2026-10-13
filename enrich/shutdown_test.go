package enrich

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCloseCancelsActiveHTTPProbe(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer target.Close()
	e := New(nil, nil, 1)
	defer e.Close()
	e.ProbeSite(1, target.URL)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not start")
	}
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the active HTTP probe")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("probe request remained alive after Close")
	}
	e.ProbeSite(2, target.URL)
	if len(e.jobs) != 0 {
		t.Fatal("probe queued after Close")
	}
}
