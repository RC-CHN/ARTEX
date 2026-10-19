package runner

import (
	"context"
	"encoding/json"
	"errors"
)

// Outer fields hide large embedded fields from JSON encoding entirely. This
// mirrors the full store's fragment layout without walking command receipts.
type leaseSessionMetadata struct {
	*sessionRecord
	Spec            *struct{} `json:"spec,omitempty"`
	Commands        *struct{} `json:"commands,omitempty"`
	CallIDs         *struct{} `json:"call_ids,omitempty"`
	MCPTools        *struct{} `json:"mcp_tools,omitempty"`
	Notifications   *struct{} `json:"notifications,omitempty"`
	NotificationIDs *struct{} `json:"notification_ids,omitempty"`
}

func (h *Hub) leaseMetadataLocked() (map[string]json.RawMessage, error) {
	parts := map[string]json.RawMessage{}
	add := func(path []string, value any) error {
		key, _ := json.Marshal(path)
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		// Leave ample room below a full fragment. Only timestamps changed since
		// the last full save, so its metadata also fit in a single fragment.
		if len(raw) > 512<<10 {
			return errors.New("runner lease metadata exceeds fragment limit")
		}
		parts[string(key)+"/0"] = raw
		return nil
	}
	for id, n := range h.state.Nodes {
		if !n.LastSeen.Equal(h.durableNodeSeen[id]) {
			if err := add([]string{"nodes", id}, n); err != nil {
				return nil, err
			}
		}
	}
	for id, s := range h.state.Sessions {
		if s.live() && !s.LeaseUntil.Equal(h.durableLeases[id]) {
			if err := add([]string{"sessions", id}, leaseSessionMetadata{sessionRecord: s}); err != nil {
				return nil, err
			}
		}
	}
	return parts, nil
}

func (h *Hub) saveLeaseMetadataLocked(parent context.Context) (err error) {
	store, ok := h.opts.Store.(MetadataStore)
	if !ok {
		return h.saveContextLocked(parent)
	}
	op := h.observe("checkpoint_metadata", false)
	size := 0
	defer op.finishDeferred(&err, &size)
	ctx, cancel := context.WithTimeout(parent, h.checkpointTimeoutLocked())
	defer cancel()
	parts, err := h.leaseMetadataLocked()
	if err != nil {
		return h.saveContextLocked(parent)
	}
	for _, raw := range parts {
		size += len(raw)
	}
	rev, e := store.SaveMetadata(ctx, h.revision, parts, h.sessionStatusesLocked())
	err = storeError(e)
	if err != nil {
		if restoreErr := h.restoreLocked(); restoreErr != nil {
			h.closeLocked("rollback_failed", restoreErr, false)
		}
		if errors.Is(err, ErrStoreConflict) {
			h.closeLocked("store_conflict", err, false)
		}
		h.signalLocked()
		return err
	}
	h.revision = rev
	h.metadataOverlay = true
	h.rememberLeaseMetadataLocked()
	h.signalLocked()
	return nil
}
