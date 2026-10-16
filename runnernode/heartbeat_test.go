package runnernode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

func TestDelayedPollNeverShortensRenewedLease(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	defer n.stopAll()
	now := time.Now()
	server := now.Add(2 * time.Hour)
	original := runner.Lease{SessionID: "s", AttemptID: "a", Epoch: "epoch", Until: now.Add(5 * time.Second)}
	n.leases["s"] = original
	// A successful 24s poll returning a 22s TTL used to move the expiry into the
	// past. The node must preserve its previously established conservative bound.
	response := runner.PollResponse{Epoch: "epoch", ServerTime: server, Leases: []runner.Lease{{SessionID: "s", AttemptID: "a", Epoch: "epoch", Until: server.Add(22 * time.Second)}}}
	n.applyPoll(t.Context(), response, now.Add(-24*time.Second))
	if !n.leases["s"].Until.Equal(original.Until) {
		t.Fatalf("lease regressed: %v", n.leases["s"].Until)
	}
	response.ServerTime = server.Add(time.Second)
	response.Leases[0].Until = response.ServerTime.Add(30 * time.Second)
	n.applyPoll(t.Context(), response, now)
	if !n.leases["s"].Until.Equal(now.Add(30 * time.Second)) {
		t.Fatal("fresh heartbeat did not extend lease")
	}
	n.applyPoll(t.Context(), runner.PollResponse{Epoch: "epoch", ServerTime: server}, now.Add(-time.Second))
	if len(n.leases) != 1 {
		t.Fatal("stale response revoked newer lease")
	}
	n.applyPoll(t.Context(), runner.PollResponse{Epoch: "epoch", ServerTime: server.Add(2 * time.Second)}, now.Add(time.Second))
	if n.sessions["s"].ctx.Err() == nil {
		t.Fatal("actual revocation did not stop session")
	}
}
func TestLeaseAttemptEpochAndInvalidTTLNeverCarryOver(t *testing.T) {
	for _, mode := range []string{"attempt", "epoch", "expired", "too_long"} {
		t.Run(mode, func(t *testing.T) {
			n, _ := testNode(t)
			openNode(t, n)
			defer n.stopAll()
			now := time.Now()
			p := runner.PollResponse{Epoch: "epoch", ServerTime: now, Leases: []runner.Lease{{SessionID: "s", AttemptID: "a", Epoch: "epoch", Until: now.Add(30 * time.Second)}}}
			switch mode {
			case "attempt":
				p.Leases[0].AttemptID = "new"
			case "epoch":
				p.Epoch = "new"
				p.Leases[0].Epoch = "new"
			case "expired":
				p.Leases[0].Until = now
			case "too_long":
				p.Leases[0].Until = now.Add(3 * time.Minute)
			}
			n.applyPoll(t.Context(), p, now)
			if n.sessions["s"].ctx.Err() == nil {
				t.Fatal("unsafe lease preserved session")
			}
		})
	}
}
func TestStoppedReceiptSurvivesCollectorUntilHeartbeatAck(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	n.heartbeatEnabled.Store(true)
	s := n.sessions["s"]
	s.mu.Lock()
	s.stopReason = "lease_expired"
	s.mu.Unlock()
	if err := n.abort(s); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	l := n.leases["s"]
	l.Until = now.Add(-time.Second)
	n.leases["s"] = l
	n.collectRetired(now)
	if n.sessions["s"] == nil {
		t.Fatal("stop report collected before heartbeat")
	}
	n.applyPoll(t.Context(), runner.PollResponse{Epoch: "epoch", ServerTime: now}, now)
	n.collectRetired(now)
	if n.sessions["s"] != nil {
		t.Fatal("acknowledged stop receipt leaked")
	}
}
func TestHeartbeatRenewsWhileCommandPollIsBlocked(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	defer n.stopAll()
	n.heartbeatEnabled.Store(true)
	received := make(chan runner.HeartbeatRequest, 1)
	polling := make(chan struct{})
	releasePoll := make(chan struct{})
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			var req runner.HeartbeatRequest
			if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
				t.Error(e)
			}
			received <- req
			now := time.Now()
			json.NewEncoder(w).Encode(runner.PollResponse{Epoch: "epoch", ServerTime: now, Leases: []runner.Lease{{SessionID: "s", AttemptID: "a", Epoch: "epoch", Until: now.Add(30 * time.Second)}}})
			return
		}
		close(polling)
		select {
		case <-r.Context().Done():
		case <-releasePoll:
		}
	}))
	defer hs.Close()
	n.Config.ServerURL = hs.URL
	n.HTTP = hs.Client()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		var response runner.PollResponse
		_ = n.request(ctx, "poll", runner.PollRequest{}, &response)
	}()
	select {
	case <-polling:
	case <-time.After(time.Second):
		t.Fatal("command poll did not start")
	}
	defer close(releasePoll)
	done := make(chan struct{})
	go func() { n.heartbeats(ctx); close(done) }()
	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat waited for command poll")
	}
	cancel()
	<-done
	<-pollDone
}
func TestConcurrentPollHeartbeatAndWatchdog(t *testing.T) {
	n, _ := testNode(t)
	openNode(t, n)
	defer n.stopAll()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { n.watchdog(ctx); close(done) }()
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Go(func() {
			now := time.Now()
			p := runner.PollResponse{Epoch: "epoch", ServerTime: now.Add(time.Duration(i) * time.Millisecond), Leases: []runner.Lease{{SessionID: "s", AttemptID: "a", Epoch: "epoch", Until: now.Add(time.Minute)}}}
			n.applyPoll(ctx, p, now)
		})
	}
	wg.Wait()
	cancel()
	<-done
	if n.sessions["s"].ctx.Err() != nil {
		t.Fatal("live session cancelled during concurrent renewal")
	}
}
