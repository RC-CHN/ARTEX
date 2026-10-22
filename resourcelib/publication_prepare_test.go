package resourcelib

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPreparedPublicationDoesNotHoldQuotaAndBlocksSweep(t *testing.T) {
	s, a := fixture(t)
	r := resource(t, s, a)
	d := draft(t, s, r, a)
	d = upload(t, s, d, "bin/fixture", "synthetic", a)
	tx, err := s.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock_shared($1)`, resourceGCLock); err != nil {
		t.Fatal(err)
	}
	p, err := s.prepareTree(t.Context(), Version{Manifest: d.Manifest})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(p)
	// Hold the actual preparation lifetime open while another resource publishes.
	other := resource(t, s, a)
	otherDraft := draft(t, s, other, a)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = s.Publish(ctx, otherDraft.ID, otherDraft.Revision, "parallel fixture", "test", a); err != nil {
		t.Fatalf("preparation blocked unrelated publication: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err = os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Sweep(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(p, "bin/fixture")); err != nil {
		t.Fatal("GC removed in-flight publication", err)
	}
	tx.Rollback()
	if _, err = s.Sweep(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("orphan preparation not reclaimable", err)
	}
}
