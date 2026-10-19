package runnernode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

type requestStatus struct {
	Calls          uint64
	Failures       uint64
	Consecutive    uint64
	LastSuccess    time.Time
	LastFailure    time.Time
	LastCode       string
	LastHTTPStatus int
	LastDurationMS int64
	LastLog        time.Time
}

type transportObserver struct {
	mu     sync.Mutex // Never acquire Node.mu while holding this mutex.
	routes map[string]requestStatus
}

func requestFailure(err error) (string, int) {
	if err == nil {
		return "ok", 0
	}
	var status *httpStatusError
	if errors.As(err, &status) {
		if status.code != "" {
			return status.code, status.status
		}
		return "http_error", status.status
	}
	if errors.Is(err, context.Canceled) {
		return "canceled", 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded", 0
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_error", 0
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return "network_timeout", 0
		}
		return "network_error", 0
	}
	var syntax *json.SyntaxError
	var types *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &types) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return "invalid_response", 0
	}
	return "request_error", 0
}

func (n *Node) observeRequest(route string, start time.Time, err error, httpStatus int) {
	// Fixed route labels prevent unbounded diagnostic cardinality.
	switch route {
	case "register", "poll", "heartbeat", "results":
	default:
		return
	}
	now := time.Now()
	code, status := requestFailure(err)
	if httpStatus != 0 {
		status = httpStatus
	}
	n.transport.mu.Lock()
	if n.transport.routes == nil {
		n.transport.routes = map[string]requestStatus{}
	}
	s := n.transport.routes[route]
	changed := err != nil && (s.LastCode != code || s.LastHTTPStatus != status)
	s.Calls++
	s.LastDurationMS = now.Sub(start).Milliseconds()
	recovered := err == nil && s.Consecutive > 0
	if err != nil {
		s.Failures++
		s.Consecutive++
		s.LastFailure = now
		s.LastCode = code
		s.LastHTTPStatus = status
	} else {
		s.LastSuccess = now
		s.Consecutive = 0
	}
	slow := now.Sub(start) >= 2*time.Second
	// Long polling intentionally waits up to 10 seconds.
	if route == "poll" {
		slow = now.Sub(start) >= 15*time.Second
	}
	emit := recovered || changed || (err != nil && s.Consecutive == 1) || ((err != nil || slow) && now.Sub(s.LastLog) >= 30*time.Second)
	if emit {
		s.LastLog = now
	}
	n.transport.routes[route] = s
	n.transport.mu.Unlock()
	if emit && n.Log != nil {
		sinceSuccess := int64(-1)
		if !s.LastSuccess.IsZero() {
			sinceSuccess = now.Sub(s.LastSuccess).Milliseconds()
		}
		n.Log.Printf("runner_transport route=%s code=%s http_status=%d duration_ms=%d consecutive_failures=%d last_success_age_ms=%d recovered=%t", route, code, status, s.LastDurationMS, s.Consecutive, sinceSuccess, recovered)
	}
}

func (n *Node) logSessionStop(s *nodeSession, reason string) {
	if n.Log == nil {
		return
	}
	n.transport.mu.Lock()
	heartbeat, poll := n.transport.routes["heartbeat"], n.transport.routes["poll"]
	n.transport.mu.Unlock()
	age := func(at time.Time) int64 {
		if at.IsZero() {
			return -1
		}
		return time.Since(at).Milliseconds()
	}
	n.Log.Printf("runner_lease event=stop session=%q reason=%s heartbeat_success_age_ms=%d heartbeat_code=%s heartbeat_http_status=%d heartbeat_failures=%d poll_success_age_ms=%d poll_code=%s poll_http_status=%d poll_failures=%d", s.id, reason, age(heartbeat.LastSuccess), heartbeat.LastCode, heartbeat.LastHTTPStatus, heartbeat.Consecutive, age(poll.LastSuccess), poll.LastCode, poll.LastHTTPStatus, poll.Consecutive)
}
