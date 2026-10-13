package runnerfiles

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPullRetriesSourceChangedAfterManifest(t *testing.T) {
	store, mount := fixture(t)
	writeFixture(t, mount.Source, "result", "old")
	client := fileServer(t, store, mount, nil)
	transport := client.HTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	changed := false
	client.HTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if strings.HasSuffix(request.URL.Path, "/blob") && !changed {
			changed = true
			writeFixture(t, mount.Source, "result", "new published snapshot")
		}
		return transport.RoundTrip(request)
	})
	local := t.TempDir()
	syncer := &Syncer{Client: client, Session: "allowed", Plan: Plan{Mounts: []Mount{mount}}, Roots: map[string]string{mount.Name: local}}
	if err := syncer.Pull(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertContents(t, local, "result", "new published snapshot")
}

func TestCheckpointDefersMovingSourceButPublishesOtherFiles(t *testing.T) {
	store, mount := fixture(t)
	client := fileServer(t, store, mount, nil)
	transport := client.HTTP.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	busy := true
	client.HTTP.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == "PUT" && request.URL.Query().Get("path") == "busy" && busy {
			return &http.Response{StatusCode: 409, Header: http.Header{"X-Artex-File-Error": []string{"source-busy"}}, Body: io.NopCloser(strings.NewReader("source changed during snapshot"))}, nil
		}
		return transport.RoundTrip(request)
	})
	local := t.TempDir()
	writeFixture(t, local, "busy", "still being generated")
	writeFixture(t, local, "stable", "finished output")
	syncer := &Syncer{Client: client, Session: "allowed", Plan: Plan{Mounts: []Mount{mount}}, Roots: map[string]string{mount.Name: local}}
	if err := syncer.Checkpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertContents(t, mount.Source, "stable", "finished output")
	if syncer.versions[mount.Name]["busy"] != "" {
		t.Fatal("deferred file was marked published")
	}
	if err := syncer.Push(t.Context()); !errors.Is(err, ErrSourceBusy) {
		t.Fatalf("final publication incorrectly succeeded: %v", err)
	}
	busy = false
	if err := syncer.Push(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertContents(t, mount.Source, "busy", "still being generated")
	// Genuine competing publication remains a hard conflict, including checkpoints.
	writeFixture(t, local, "stable", "our replacement")
	writeFixture(t, mount.Source, "stable", "someone else's result")
	if err := syncer.Checkpoint(t.Context()); !errors.Is(err, ErrConflict) || errors.Is(err, ErrSourceBusy) {
		t.Fatalf("lost conflict classification: %v", err)
	}
	assertContents(t, mount.Source, "stable", "someone else's result")
}

func TestUploadChangedBeforeSnapshotIsRetryableWithoutSending(t *testing.T) {
	local := t.TempDir()
	writeFixture(t, local, "result", "new bytes")
	sent := false
	client := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { sent = true; return response(""), nil })}}
	err := client.Upload(t.Context(), "allowed", "data", local, entry("result", "old"), Missing)
	if !errors.Is(err, ErrSourceBusy) || sent {
		t.Fatalf("unsafe upload: %v sent=%v", err, sent)
	}
}

func TestSourceBusyRetryIsBoundedAndCancellable(t *testing.T) {
	calls := 0
	if err := retrySourceBusy(t.Context(), func() error { calls++; return ErrSourceBusy }); !errors.Is(err, ErrSourceBusy) || calls != 5 {
		t.Fatalf("unbounded retry: %v calls=%d", err, calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	calls = 0
	err := retrySourceBusy(ctx, func() error { calls++; cancel(); return ErrSourceBusy })
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatal("retry ignored cancellation", err, calls)
	}
	calls = 0
	err = retrySourceBusy(t.Context(), func() error { calls++; return ErrConflict })
	if err != ErrConflict || calls != 1 {
		t.Fatal("retried a competing write", err, calls)
	}
}
