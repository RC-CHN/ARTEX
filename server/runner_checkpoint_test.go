package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func syntheticCheckpoint(output string, lease int) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"version": 1, "epoch": "synthetic", "registrations": map[string]any{}, "nodes": map[string]any{"node": map[string]any{"last_seen": lease}}, "sessions": map[string]any{"session": map[string]any{"lease_until": lease, "spec": map[string]any{"input": "synthetic"}, "commands": map[string]any{"command": map[string]any{"output": output, "large_number": json.RawMessage("9007199254740993")}}, "notification_ids": map[string]bool{"once": true}}}})
	return raw
}

func TestCheckpointPartsRoundTripAndIntegrity(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`{"epoch":"simple"}`), json.RawMessage(`{"nodes":null,"sessions":{},"unknown":"future"}`), syntheticCheckpoint("binary\x00\\u0000中文"+strings.Repeat("x", checkpointBatchBytes*2), 1)} {
		parts, err := splitCheckpoint(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range parts {
			if len(p.data) > checkpointBatchBytes {
				t.Fatal("unbounded chunk")
			}
		}
		got, err := joinCheckpoint(parts)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("lossy checkpoint roundtrip: %v", err)
		}
		altered := append([]checkpointPart(nil), parts...)
		altered[0].data = append(json.RawMessage{}, altered[0].data...)
		altered[0].data[0] ^= 1
		if _, err = joinCheckpoint(altered); err == nil {
			t.Fatal("digest mismatch accepted")
		}
		if _, err = joinCheckpoint(append(parts, parts[0])); err == nil {
			t.Fatal("duplicate accepted")
		}
	}
	parts, _ := splitCheckpoint(syntheticCheckpoint(strings.Repeat("x", checkpointBatchBytes*3), 1))
	for i, p := range parts {
		if strings.HasSuffix(p.key, "/1") {
			if _, err := joinCheckpoint(append(append([]checkpointPart{}, parts[:i]...), parts[i+1:]...)); err == nil {
				t.Fatal("missing middle chunk accepted")
			}
			break
		}
	}
	bad := []checkpointPart{{key: "[]/0", data: json.RawMessage(`{}`), hash: sha256.Sum256([]byte(`{}`))}, {key: `["sessions","missing"]/0`, data: json.RawMessage(`{}`), hash: sha256.Sum256([]byte(`{}`))}}
	if _, err := joinCheckpoint(bad); err == nil {
		t.Fatal("orphan accepted")
	}
}

func TestRunnerPGCheckpointDeltaMigrationAndDowngrade(t *testing.T) {
	m := runnerTestManager(t)
	_, err := m.pg.Exec("DELETE FROM runner_state")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM runner_state; DELETE FROM runner_diagnostics") })
	store := runnerPGStore{m.pg.DB}
	raw := syntheticCheckpoint("binary\x00"+strings.Repeat("y", checkpointBatchBytes*3), 1)
	if _, err = m.pg.Exec("INSERT INTO runner_state(id,revision,payload) VALUES(1,7,$1)", string(raw)); err != nil {
		t.Fatal(err)
	}
	// Migration is lazy and lossless; no task payload is queried by operators.
	snap, err := store.Load(t.Context())
	if err != nil || !bytes.Equal(snap.Data, raw) || snap.Revision != 7 {
		t.Fatal("legacy load failed", err)
	}
	if _, err = store.Save(t.Context(), 7, raw); err != nil {
		t.Fatal(err)
	}
	versions := func() map[string]string {
		t.Helper()
		r, e := m.pg.Query("SELECT part_key,xmin::text FROM runner_state_parts")
		if e != nil {
			t.Fatal(e)
		}
		defer r.Close()
		out := map[string]string{}
		for r.Next() {
			var k, v string
			if e = r.Scan(&k, &v); e != nil {
				t.Fatal(e)
			}
			out[k] = v
		}
		if e = r.Err(); e != nil {
			t.Fatal(e)
		}
		return out
	}
	before := versions()
	if _, err = store.Save(t.Context(), 8, raw); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, versions()) {
		t.Fatal("unchanged checkpoint rewrote fragments")
	}
	updated := syntheticCheckpoint("binary\x00"+strings.Repeat("y", checkpointBatchBytes*3), 2)
	if _, err = store.Save(t.Context(), 9, updated); err != nil {
		t.Fatal(err)
	}
	changed := 0
	for k, v := range versions() {
		if v != before[k] {
			changed++
			if strings.Contains(k, "commands") {
				t.Fatal("lease update rewrote command history")
			}
		}
	}
	if changed != 2 {
		t.Fatalf("lease update changed %d fragments, want 2", changed)
	}
	snap, err = store.Load(t.Context())
	if err != nil || !bytes.Equal(snap.Data, updated) || snap.Revision != 10 {
		t.Fatal("delta load lost data", err)
	}
	var marker struct {
		Version int
		Format  string `json:"checkpoint_format"`
	}
	var header []byte
	if err = m.pg.QueryRow("SELECT payload FROM runner_state WHERE id=1").Scan(&header); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(header, &marker); err != nil || marker.Version != -1 || marker.Format != "parts-v1" {
		t.Fatal("old binaries would load partial state")
	}
	// A missing entire leaf must be rejected, not interpreted as absent history.
	tx, err := m.pg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec("DELETE FROM runner_state_parts WHERE part_key=(SELECT part_key FROM runner_state_parts LIMIT 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err = loadRunnerCheckpoint(t.Context(), tx); err == nil {
		t.Fatal("missing fragment was accepted")
	}
	tx.Rollback()
	// Other tests retain synthetic tasks in this package's isolated database.
	// Shadow only the busy-count tables inside this maintenance transaction.
	tx, err = m.pg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TEMP TABLE tasks ON COMMIT DROP AS SELECT * FROM tasks WITH NO DATA;
	 CREATE TEMP TABLE exploration_nodes ON COMMIT DROP AS SELECT * FROM exploration_nodes WITH NO DATA;
	 CREATE TEMP TABLE main_requests ON COMMIT DROP AS SELECT * FROM main_requests WITH NO DATA;
	 CREATE TEMP TABLE runner_diagnostics ON COMMIT DROP AS SELECT * FROM runner_diagnostics WITH NO DATA;
	 INSERT INTO tasks(status,paused) VALUES('active',false)`); err != nil {
		t.Fatal(err)
	}
	if err = restoreLegacyRunnerCheckpointTx(t.Context(), tx); err == nil {
		t.Fatal("downgrade allowed an active task")
	}
	if _, err = tx.Exec("UPDATE tasks SET paused=true"); err != nil {
		t.Fatal(err)
	}
	if err = restoreLegacyRunnerCheckpointTx(t.Context(), tx); err != nil {
		t.Fatal("offline downgrade failed", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	snap, err = store.Load(t.Context())
	if err != nil || !bytes.Equal(snap.Data, updated) || snap.Revision != 11 {
		t.Fatal("downgrade changed execution state", err)
	}
	if len(versions()) != 0 {
		t.Fatal("downgrade leaked fragments")
	}
	if _, err = store.Save(t.Context(), 11, json.RawMessage(`{"sessions":{},"nodes":{}}`)); err != nil {
		t.Fatal(err)
	}
	if len(versions()) != 3 {
		t.Fatal("removed sessions left stale fragments")
	}
}

func TestCheckpointProgressDeadline(t *testing.T) {
	ctx, p := newCheckpointProgress(t.Context(), 100*time.Millisecond)
	defer p.stop()
	for range 6 {
		time.Sleep(30 * time.Millisecond)
		p.pulse()
		if ctx.Err() != nil {
			t.Fatal("progress did not renew inactivity budget")
		}
	}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			t.Fatal(context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("stalled progress did not cancel")
	}
	parent, cancel := context.WithTimeout(t.Context(), 80*time.Millisecond)
	defer cancel()
	bounded, q := newCheckpointProgress(parent, time.Second)
	defer q.stop()
	<-bounded.Done()
	if !errors.Is(context.Cause(bounded), context.DeadlineExceeded) {
		t.Fatal("parent deadline lost")
	}
}

func TestRunnerPGCheckpointLargeHistoryDelta(t *testing.T) {
	if testing.Short() {
		t.Skip("large checkpoint regression")
	}
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("DELETE FROM runner_state"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM runner_state; DELETE FROM runner_diagnostics") })
	rng := rand.New(rand.NewPCG(13, 29))
	data := make([]byte, 37*1024*1024)
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for i := range data {
		data[i] = alphabet[rng.IntN(len(alphabet))]
	}
	raw := syntheticCheckpoint(string(data), 1)
	store := runnerPGStore{m.pg.DB}
	if _, err := store.Save(t.Context(), 0, raw); err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	rows, err := m.pg.Query("SELECT part_key,xmin::text FROM runner_state_parts")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		before[k] = v
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	updated := syntheticCheckpoint(string(data), 2)
	start := time.Now()
	if _, err = store.Save(t.Context(), 1, updated); err != nil {
		t.Fatal(err)
	}
	deltaTime := time.Since(start)
	rows, err = m.pg.Query("SELECT part_key,xmin::text,octet_length(payload) FROM runner_state_parts")
	if err != nil {
		t.Fatal(err)
	}
	changed, written := 0, 0
	for rows.Next() {
		var k, v string
		var size int
		if err = rows.Scan(&k, &v, &size); err != nil {
			t.Fatal(err)
		}
		if before[k] != v {
			changed++
			written += size
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if changed != 2 || written > 1024 {
		t.Fatalf("heartbeat rewrote history: parts=%d bytes=%d", changed, written)
	}
	start = time.Now()
	if _, err = m.pg.Exec("UPDATE runner_state SET payload=$1,revision=revision+1 WHERE id=1", string(updated)); err != nil {
		t.Fatal(err)
	}
	fullTime := time.Since(start)
	t.Logf("synthetic checkpoint bytes=%d delta_parts=%d delta_bytes=%d delta_save=%s legacy_full_write=%s (local PostgreSQL, no latency injection)", len(updated), changed, written, deltaTime, fullTime)
}

func TestRunnerPGCheckpointProgressAndAtomicFailure(t *testing.T) {
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("DELETE FROM runner_state"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = m.pg.Exec("DROP TRIGGER IF EXISTS checkpoint_delay ON runner_state_parts; DROP FUNCTION IF EXISTS checkpoint_delay(); DELETE FROM runner_state; DELETE FROM runner_diagnostics")
	})
	store := runnerPGStore{m.pg.DB}
	original := syntheticCheckpoint("original", 1)
	if _, err := store.Save(t.Context(), 0, original); err != nil {
		t.Fatal(err)
	}
	// Each batch finishes within the inactivity limit, total write exceeds it.
	if _, err := m.pg.Exec(`CREATE FUNCTION checkpoint_delay() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF octet_length(NEW.payload)>=1048576 THEN PERFORM pg_sleep(0.75); END IF; RETURN NEW; END $$; CREATE TRIGGER checkpoint_delay BEFORE INSERT OR UPDATE ON runner_state_parts FOR EACH ROW EXECUTE FUNCTION checkpoint_delay()`); err != nil {
		t.Fatal(err)
	}
	updated := syntheticCheckpoint(strings.Repeat("z", checkpointBatchBytes*8), 1)
	start := time.Now()
	if _, err := store.Save(t.Context(), 1, updated); err != nil {
		t.Fatal("progressing write was aborted", err)
	}
	if time.Since(start) < checkpointIdleTimeout {
		t.Fatal("fixture did not exceed inactivity budget")
	}
	// One completed batch followed by a stuck batch must roll back everything.
	if _, err := m.pg.Exec(`CREATE OR REPLACE FUNCTION checkpoint_delay() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.part_key LIKE '%/1' THEN PERFORM pg_sleep(2); END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	failed := syntheticCheckpoint(strings.Repeat("w", checkpointBatchBytes*8), 2)
	if _, err := store.saveDiagnostic(t.Context(), 2, failed, nil, 250*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stuck write didn't time out: %v", err)
	}
	snap, err := store.Load(t.Context())
	if err != nil || snap.Revision != 2 || !bytes.Equal(snap.Data, updated) {
		t.Fatal("partial checkpoint escaped rollback", err)
	}
	tx, err := m.pg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT revision FROM runner_state WHERE id=1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.saveDiagnostic(t.Context(), 2, updated, nil, 100*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait not bounded: %v", err)
	}
}

func TestRunnerPGCheckpointConcurrentLoad(t *testing.T) {
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("DELETE FROM runner_state"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM runner_state; DELETE FROM runner_diagnostics") })
	store := runnerPGStore{m.pg.DB}
	if _, err := store.Save(t.Context(), 0, syntheticCheckpoint("1", 1)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() {
		for i := 2; i <= 30; i++ {
			if _, err := store.Save(t.Context(), uint64(i-1), syntheticCheckpoint(fmt.Sprint(i), i)); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 30 {
		snap, err := store.Load(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var state struct {
			Nodes map[string]struct {
				LastSeen int `json:"last_seen"`
			}
			Sessions map[string]struct {
				Lease    int `json:"lease_until"`
				Commands map[string]struct{ Output string }
			}
		}
		if err = json.Unmarshal(snap.Data, &state); err != nil {
			t.Fatal(err)
		}
		s := state.Sessions["session"]
		if state.Nodes["node"].LastSeen != s.Lease || s.Commands["command"].Output != fmt.Sprint(s.Lease) || snap.Revision != uint64(s.Lease) {
			t.Fatal("mixed revisions escaped snapshot isolation")
		}
	}
	wg.Wait()
}

func BenchmarkCheckpointHeartbeatDiff(b *testing.B) {
	raw := syntheticCheckpoint(strings.Repeat("x", 37*1024*1024), 1)
	parts, _ := splitCheckpoint(raw)
	known := map[string][32]byte{}
	for _, p := range parts {
		known[p.key] = p.hash
	}
	updated := syntheticCheckpoint(strings.Repeat("x", 37*1024*1024), 2)
	b.ResetTimer()
	for range b.N {
		next, err := splitCheckpoint(updated)
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for _, p := range next {
			if known[p.key] != p.hash {
				n += len(p.data)
			}
		}
		b.ReportMetric(float64(n), "written-bytes/op")
	}
	b.ReportMetric(float64(len(updated)), "checkpoint-bytes/op")
}
