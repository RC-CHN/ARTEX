package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

func TestRunnerPGMetadataTransaction(t *testing.T) {
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("DELETE FROM runner_state; DELETE FROM runner_diagnostics"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM runner_state; DELETE FROM runner_diagnostics") })
	s := runnerPGStore{m.pg.DB}
	raw := syntheticCheckpoint(strings.Repeat("history", 1<<20), 1)
	if _, err := s.Save(t.Context(), 0, raw); err != nil {
		t.Fatal(err)
	}
	versions := func() map[string]string {
		r, err := m.pg.Query("SELECT part_key,xmin::text FROM runner_state_parts")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		out := map[string]string{}
		for r.Next() {
			var k, v string
			if err = r.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			out[k] = v
		}
		if err = r.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := versions()
	parts := map[string]json.RawMessage{`["nodes","node"]/0`: json.RawMessage(`{"last_seen":2}`), `["sessions","session"]/0`: json.RawMessage(`{"lease_until":2}`)}
	rev, err := s.SaveMetadata(t.Context(), 1, parts, []runner.SessionStatus{})
	if err != nil || rev != 2 {
		t.Fatal(rev, err)
	}
	for k, v := range versions() {
		if _, changed := parts[k]; changed != (before[k] != v) {
			t.Fatalf("unexpected fragment rewrite %s", k)
		}
	}
	snap, err := s.Load(t.Context())
	if err != nil || snap.Revision != 2 || !bytes.Equal(snap.Data, syntheticCheckpoint(strings.Repeat("history", 1<<20), 2)) {
		t.Fatal("metadata load lost history", err)
	}
	var diagnosticRev int
	if err = m.pg.QueryRow("SELECT revision FROM runner_diagnostics WHERE id=1").Scan(&diagnosticRev); err != nil || diagnosticRev != 2 {
		t.Fatal("diagnostics not atomic", err)
	}
	stable := versions()
	bad := map[string]json.RawMessage{`["nodes","node"]/0`: json.RawMessage(`{"last_seen":3}`), `["nodes","absent"]/0`: json.RawMessage(`{}`)}
	if _, err = s.SaveMetadata(t.Context(), 2, bad, nil); err == nil {
		t.Fatal("accepted missing metadata")
	}
	if !reflect.DeepEqual(stable, versions()) {
		t.Fatal("partial metadata commit")
	}
	if _, err = s.SaveMetadata(t.Context(), 1, parts, nil); !errors.Is(err, runner.ErrStoreConflict) {
		t.Fatal("CAS not enforced", err)
	}
	// A held checkpoint transaction must honor the caller's deadline and leave
	// both the revision and all rows unchanged after cancellation.
	tx, err := m.pg.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("SELECT revision FROM runner_state WHERE id=1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err = s.SaveMetadata(ctx, 2, parts, nil); err == nil {
		t.Fatal("ignored blocked transaction deadline")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stable, versions()) {
		t.Fatal("canceled transaction changed fragments")
	}
	snap, err = s.Load(t.Context())
	if err != nil || snap.Revision != 2 {
		t.Fatal("failed update advanced revision", err)
	}
	// Normal full saves must diff against the metadata transaction, too.
	if _, err = s.Save(t.Context(), 2, raw); err != nil {
		t.Fatal(err)
	}
	snap, err = s.Load(t.Context())
	if err != nil || !bytes.Equal(snap.Data, raw) {
		t.Fatal("full save after metadata lost state", err)
	}
}
