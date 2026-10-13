package runner

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/RC-CHN/ARTEX/norma/tool"
)

// MemoryStore is useful for isolated tests and explicitly ephemeral deployments.
// Production controllers should supply a persistent CAS implementation.
type MemoryStore struct {
	mu       sync.Mutex
	snapshot Snapshot
}

func (s *MemoryStore) Load(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{Revision: s.snapshot.Revision, Data: append(json.RawMessage(nil), s.snapshot.Data...)}, nil
}

func (s *MemoryStore) Save(ctx context.Context, expected uint64, data json.RawMessage) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Revision != expected {
		return 0, ErrStoreConflict
	}
	s.snapshot = Snapshot{Revision: expected + 1, Data: append(json.RawMessage(nil), data...)}
	return s.snapshot.Revision, nil
}

func (s *MemoryStore) Guard(ctx context.Context, expected uint64, commit func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.snapshot.Revision != expected {
		return ErrStoreConflict
	}
	return commit()
}

type persistedState struct {
	Version       int                       `json:"version"`
	Epoch         string                    `json:"epoch"`
	Registrations map[string]time.Time      `json:"registrations"`
	Nodes         map[string]*nodeRecord    `json:"nodes"`
	Sessions      map[string]*sessionRecord `json:"sessions"`
}

type nodeRecord struct {
	CPULimitMillis   int64           `json:"cpu_limit_millis,omitempty"`
	MemoryLimitMiB   int64           `json:"memory_limit_mib,omitempty"`
	ConcurrencyLimit int             `json:"concurrency_limit,omitempty"`
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	BootID           string          `json:"boot_id"`
	BootIDs          map[string]bool `json:"boot_ids"`
	CredentialHash   string          `json:"credential_hash"`
	Enabled          bool            `json:"enabled"`
	Capabilities     Capabilities    `json:"capabilities"`
	LastSeen         time.Time       `json:"last_seen"`
}

type sessionRecord struct {
	ID              string                    `json:"id"`
	AttemptID       string                    `json:"attempt_id"`
	NodeID          string                    `json:"node_id"`
	BootID          string                    `json:"boot_id"`
	Epoch           string                    `json:"epoch"`
	State           string                    `json:"state"`
	Spec            RunSpec                   `json:"spec"`
	GPUIds          []string                  `json:"gpu_ids,omitempty"`
	LeaseUntil      time.Time                 `json:"lease_until"`
	Reason          string                    `json:"reason,omitempty"`
	HoldUntilExpiry bool                      `json:"hold_until_expiry,omitempty"`
	Commands        map[string]*commandRecord `json:"commands"`
	CallIDs         map[string]string         `json:"call_ids"`
	MCPTools        []ToolDefinition          `json:"mcp_tools,omitempty"`
	Notifications   []tool.Notification       `json:"notifications,omitempty"`
	NotificationIDs map[string]bool           `json:"notification_ids,omitempty"`
	FinishedAt      time.Time                 `json:"finished_at,omitempty"`
}

type commandRecord struct {
	Command Command        `json:"command"`
	Result  *ResultRequest `json:"result,omitempty"`
}

func (s *sessionRecord) live() bool {
	return s.State == "opening" || s.State == "active" || s.State == "draining"
}
func (s *sessionRecord) reserved(now time.Time) bool {
	return s.State != "closed" && s.State != "expired" && now.Before(s.LeaseUntil)
}
