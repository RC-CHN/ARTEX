package runnernode

import (
	"os"
	"time"
)

// Keep receipts for every live attempt, including acknowledged calls that can
// be redelivered after a lost response. Once its lease is gone AND termination
// is confirmed, a late command cannot pass executeCommand's lease check.
// Never discard a pending result or unconfirmed resource reservation.
func (n *Node) collectRetired(now time.Time) {
	var emptyCaches []string
	n.mu.Lock()
	for id, s := range n.sessions {
		l, leased := n.leases[id]
		if leased && l.AttemptID == s.attempt && l.Epoch == s.epoch && now.Before(l.Until) {
			continue
		}
		select {
		case <-s.ready:
		default:
			continue
		}
		s.mu.Lock()
		// Keep a stopped receipt long enough for the independent heartbeat to
		// report a local expiry, even when its conservative local lease expired.
		pendingStop := n.heartbeatEnabled.Load() && s.stopReason != "" && leased && l.AttemptID == s.attempt && l.Epoch == s.epoch && now.Before(l.Until.Add(2*time.Minute))
		if s.stopped && !s.cleanupRunning && !pendingStop {
			delete(n.sessions, id)
			if (!s.opened && s.syncErr == nil) || s.storageCheck != nil {
				emptyCaches = append(emptyCaches, s.dir)
			}
		}
		s.mu.Unlock()
	}
	for id, record := range n.commands {
		r := record.result
		if record.queued || r == nil || n.sessions[r.SessionID] != nil {
			continue
		}
		l, leased := n.leases[r.SessionID]
		if leased && l.AttemptID == r.AttemptID && l.Epoch == r.Epoch && now.Before(l.Until) {
			continue
		}
		delete(n.commands, id)
	}
	n.mu.Unlock()
	for _, dir := range emptyCaches {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	}
}
