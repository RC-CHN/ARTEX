package runnernode

import (
	"context"
	"github.com/RC-CHN/ARTEX/runner"
	"time"
)

// Independent of long polls and tool results. Stopped reports are retried until
// the controller removes their leases; they never include tool inputs/output.
func (n *Node) heartbeats(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !n.heartbeatEnabled.Load() {
			continue
		}
		req := runner.HeartbeatRequest{NodeID: n.identity.NodeID, BootID: n.bootID, Protocol: runner.ProtocolVersion}
		n.mu.Lock()
		for _, s := range n.sessions {
			s.mu.Lock()
			if s.ended && len(req.Stopped) < 1024 {
				reason := s.stopReason
				if reason == "" {
					reason = "session_aborted"
				}
				req.Stopped = append(req.Stopped, runner.StoppedSession{SessionID: s.id, AttemptID: s.attempt, Epoch: s.epoch, Reason: reason})
			}
			s.mu.Unlock()
		}
		n.mu.Unlock()
		sent := time.Now()
		requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var response runner.PollResponse
		err := n.request(requestCtx, "heartbeat", req, &response)
		cancel()
		if err != nil {
			continue
		}
		n.responseMu.Lock()
		n.applyLeases(response, sent)
		n.responseMu.Unlock()
	}
}
