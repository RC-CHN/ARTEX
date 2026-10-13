package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
	"github.com/RC-CHN/ARTEX/runnernode"
)

// A shell may print binary bytes. The broker checkpoint is an opaque JSON
// document, not searchable application data: changing its strings also changes
// tool-call identities and results used for idempotent redelivery.
func TestRunnerPGStatePreservesBinaryOutput(t *testing.T) {
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("DELETE FROM runner_state"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM runner_state") })
	store := runnerPGStore{m.pg.DB}
	body := "head\x00\x01\x08\xfftail\n中文 😀 literal \\u0000"
	raw, err := json.Marshal(map[string]any{
		"output": body,
		"nested": map[string]any{"notification": "finished\x00", "number": json.RawMessage("9007199254740993")},
		"input":  json.RawMessage(`{"command":"printf '\\000'","literal":"\\u0000"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := uint64(0); i < 3; i++ {
		revision, err := store.Save(t.Context(), i, raw)
		if err != nil {
			t.Fatalf("binary tool output cannot be acknowledged: %v", err)
		}
		loaded, err := store.Load(t.Context())
		if err != nil || revision != i+1 || loaded.Revision != revision || !bytes.Equal(loaded.Data, raw) {
			t.Fatalf("checkpoint changed across save/load: revision=%d loaded=%d exact=%t err=%v", revision, loaded.Revision, bytes.Equal(loaded.Data, raw), err)
		}
	}
}

type binaryReplyFixture struct{ calls atomic.Int32 }

func (f *binaryReplyFixture) Start(context.Context, string, runner.RunSpec, runnerfiles.Plan, map[string]string, []string) (runnernode.Sandbox, error) {
	return f, nil
}
func (f *binaryReplyFixture) Stop() error { return nil }
func (f *binaryReplyFixture) Request(_ context.Context, req runnernode.SandboxRequest) (runnernode.SandboxReply, error) {
	if req.Method != "call" {
		return runnernode.SandboxReply{}, nil
	}
	f.calls.Add(1)
	result := tool.Text("header\x00" + strings.Repeat("\x00", 127) + "tail\\u0000")
	result.Extra = []llm.Message{llm.UserText("extra\x00message")}
	return runnernode.SandboxReply{Executed: true, Result: result}, nil
}

func TestRunnerBinaryResultsReachControllerAndDeduplicate(t *testing.T) {
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("DELETE FROM runner_state"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM runner_state") })
	store := runnerPGStore{m.pg.DB}
	hub, err := runner.New(runner.Options{Store: store, PollWait: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	mux := http.NewServeMux()
	mux.Handle("/runner/v1/", http.StripPrefix("/runner/v1", hub.Handler()))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	ctx, stop := context.WithTimeout(t.Context(), 15*time.Second)
	defer stop()
	token, _, err := hub.CreateRegistrationToken(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	node, err := runnernode.New(runnernode.Config{ServerURL: srv.URL, Name: "binary-fixture", DataDir: t.TempDir(), AllowInsecureHTTP: true, Capabilities: runner.Capabilities{Slots: 2, CPUMillis: 2000, MemoryMiB: 2048}})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &binaryReplyFixture{}
	node.Executor, node.Log = fixture, log.New(io.Discard, "", 0)
	nodeCtx, stopNode := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- node.Run(nodeCtx, token) }()
	defer func() {
		stopNode()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("node did not stop")
		}
	}()
	var wg sync.WaitGroup
	for worker := range 2 {
		wg.Go(func() {
			session, err := hub.Acquire(ctx, runner.RunSpec{ThreadID: fmt.Sprintf("binary-%d", worker), WorkingDir: "/workspace", Image: "example.invalid/fixture@sha256:" + strings.Repeat("a", 64), Tools: []runner.ToolDefinition{{Name: "fixture", Kind: "builtin"}}})
			if err != nil {
				t.Error(err)
				return
			}
			defer session.Close()
			for i := range 12 {
				tc := &tool.ToolContext{ToolUseID: fmt.Sprintf("fixture-%d", i)}
				for range 2 { // Repeat after the ACK: no second execution is allowed.
					result, err := session.Call(ctx, "fixture", json.RawMessage(`{}`), tc)
					if err != nil || strings.Count(result.Flatten(), "\x00") != 128 || len(result.Extra) != 1 || result.Extra[0].Content[0].Text != "extra\x00message" {
						t.Errorf("binary result lost: err=%v nul=%d extra=%d", err, strings.Count(result.Flatten(), "\x00"), len(result.Extra))
						return
					}
				}
			}
		})
	}
	wg.Wait()
	if fixture.calls.Load() != 24 {
		t.Errorf("commands lost or replayed: %d", fixture.calls.Load())
	}
	snapshot, err := store.Load(ctx)
	if err != nil || !json.Valid(snapshot.Data) || !bytes.Contains(snapshot.Data, []byte(`\u0000`)) {
		t.Fatalf("binary receipts not durably saved: %v", err)
	}
}
