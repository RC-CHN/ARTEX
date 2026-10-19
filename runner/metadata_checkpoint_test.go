package runner

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/tool"
)

// Test store reconstructs snapshots so load/restart and rollback are checked,
// not just the decision to take the fast path. Production updates SQL fragments.
type metadataTestStore struct {
	MemoryStore
	full, metadata int
	fail           error
	block          chan struct{}
}

func (s *metadataTestStore) Save(ctx context.Context, rev uint64, raw json.RawMessage) (uint64, error) {
	s.full++
	if s.fail != nil {
		return 0, s.fail
	}
	return s.MemoryStore.Save(ctx, rev, raw)
}

func (s *metadataTestStore) SaveMetadata(ctx context.Context, rev uint64, parts map[string]json.RawMessage, _ []SessionStatus) (uint64, error) {
	s.metadata++
	if s.block != nil {
		close(s.block)
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if s.fail != nil {
		return 0, s.fail
	}
	snap, err := s.Load(ctx)
	if err != nil {
		return 0, err
	}
	if snap.Revision != rev {
		return 0, ErrStoreConflict
	}
	state, err := decodeState(snap.Data)
	if err != nil {
		return 0, err
	}
	for key, raw := range parts {
		var path []string
		if err := json.Unmarshal([]byte(strings.TrimSuffix(key, "/0")), &path); err != nil {
			return 0, err
		}
		if path[0] == "nodes" {
			err = json.Unmarshal(raw, state.Nodes[path[1]])
		} else {
			err = json.Unmarshal(raw, state.Sessions[path[1]])
		}
		if err != nil {
			return 0, err
		}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return 0, err
	}
	return s.MemoryStore.Save(ctx, rev, raw)
}

func TestLeaseMetadataMatchesFullState(t *testing.T) {
	s := &metadataTestStore{}
	c := &testClock{now: time.Now()}
	h := newTestHub(t, s, c)
	n := registerNode(t, h, defaultCapabilities())
	openSession(t, n, spec("metadata"))
	h.mu.Lock()
	full := s.full
	h.mu.Unlock()
	c.Advance(2 * time.Second)
	n.poll(t)
	h.mu.Lock()
	fullAfter, metadataAfter := s.full, s.metadata
	wantRaw, _ := json.Marshal(h.state)
	lease, seen := h.sessionStatusesLocked()[0].LeaseUntil, h.state.Nodes[n.id].LastSeen
	h.mu.Unlock()
	if fullAfter != full || metadataAfter != 1 {
		t.Fatalf("full=%d metadata=%d", fullAfter, metadataAfter)
	}
	snap, err := s.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeState(snap.Data)
	want, _ := decodeState(wantRaw)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata checkpoint lost state: %v", err)
	}
	// Both metadata and subsequent full-save failures must restore the latest
	// committed lease, never the older full-checkpoint baseline.
	h.mu.Lock()
	s.fail = errors.New("synthetic disk failure")
	h.mu.Unlock()
	c.Advance(2 * time.Second)
	if _, _, err = h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id); err == nil {
		t.Fatal("accepted failed renewal")
	}
	h.mu.Lock()
	rolledLease, rolledSeen := h.sessionStatusesLocked()[0].LeaseUntil, h.state.Nodes[n.id].LastSeen
	h.mu.Unlock()
	if !rolledLease.Equal(lease) || !rolledSeen.Equal(seen) {
		t.Fatal("metadata rollback lost committed overlay")
	}
	h.mu.Lock()
	h.state.Nodes[n.id].Name = "uncommitted"
	err = h.saveLocked()
	name, rolledLease := h.state.Nodes[n.id].Name, h.sessionStatusesLocked()[0].LeaseUntil
	s.fail = nil
	h.mu.Unlock()
	if err == nil || name == "uncommitted" || !rolledLease.Equal(lease) {
		t.Fatal("full rollback lost committed overlay")
	}
	n.poll(t)
	h.mu.Lock()
	err = h.saveLocked()
	overlay := h.metadataOverlay
	h.mu.Unlock()
	if err != nil || overlay {
		t.Fatal("full save did not incorporate overlay", err)
	}
}

func TestLeaseMetadataCancellationAndFencing(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "conflict"}[conflict], func(t *testing.T) {
			s := &metadataTestStore{}
			c := &testClock{now: time.Now()}
			h := newTestHub(t, s, c)
			n := registerNode(t, h, defaultCapabilities())
			openSession(t, n, spec("metadata"))
			lease := h.SessionStatuses()[0].LeaseUntil
			c.Advance(2 * time.Second)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h.mu.Lock()
			if conflict {
				s.fail = ErrStoreConflict
			} else {
				s.block = make(chan struct{})
			}
			h.mu.Unlock()
			done := make(chan error, 1)
			go func() {
				_, _, err := h.pollContext(ctx, PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id, true)
				done <- err
			}()
			want := ErrStoreConflict
			if !conflict {
				await(t, s.block)
				cancel()
				want = context.Canceled
			}
			if err := await(t, done); !errors.Is(err, want) {
				t.Fatal(err)
			}
			if !conflict && !h.SessionStatuses()[0].LeaseUntil.Equal(lease) {
				t.Fatal("published canceled renewal")
			}
			h.mu.Lock()
			s.block = nil
			s.fail = nil
			h.mu.Unlock()
			_, _, err := h.poll(PollRequest{NodeID: n.id, BootID: n.boot, Protocol: ProtocolVersion}, n.id)
			if conflict && err == nil {
				t.Fatal("conflicted controller remained usable")
			}
			if !conflict && err != nil {
				t.Fatal("failed recovery", err)
			}
		})
	}
}

func BenchmarkLeaseMetadata(b *testing.B) {
	for _, size := range []int{0, 37 << 20} {
		b.Run(map[int]string{0: "empty", 37 << 20: "37MiB-history"}[size], func(b *testing.B) {
			now := time.Now()
			h := &Hub{state: persistedState{Nodes: map[string]*nodeRecord{"n": {ID: "n", LastSeen: now}}, Sessions: map[string]*sessionRecord{"s": {ID: "s", State: "active", LeaseUntil: now, Commands: map[string]*commandRecord{"done": {Result: &ResultRequest{Result: tool.Text(strings.Repeat("x", size))}}}}}}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				parts, err := h.leaseMetadataLocked()
				if err != nil || len(parts) != 2 {
					b.Fatal(err)
				}
			}
		})
	}
}
