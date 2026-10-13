package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/agentcore"
	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

type executionFixture struct {
	closed   atomic.Int32
	calls    atomic.Int32
	wantDir  string
	t        *testing.T
	closeErr error
}

func (r *executionFixture) Call(_ context.Context, name string, _ json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
	if name != "Read" || tc.WorkingDir != r.wantDir {
		r.t.Errorf("unexpected runtime call %s cwd=%s", name, tc.WorkingDir)
	}
	r.calls.Add(1)
	return tool.Text("remote artifact contents"), nil
}
func (*executionFixture) DrainNotifications() []tool.Notification { return nil }
func (r *executionFixture) Close() error                          { r.closed.Add(1); return r.closeErr }
func (*executionFixture) Handles(name string) bool                { return name == "fixture_exec" }

func saveExecutionHooks(t *testing.T) {
	t.Helper()
	prepare, augment := PrepareExecution, ToolAugment
	t.Cleanup(func() { PrepareExecution, ToolAugment = prepare, augment })
}

func TestPrepareExecutionDisabledAndFailureCleanup(t *testing.T) {
	saveExecutionHooks(t)
	PrepareExecution = nil
	ctx := WithRunInfo(t.Context(), RunInfo{SessionID: "local"})
	next, runtime, cleanup, err := prepareExecution(ctx, "chat", t.TempDir())
	if err != nil || runtime != nil || next != ctx {
		t.Fatalf("disabled hook changed local behavior: %v", err)
	}
	cleanup()
	for _, nilContext := range []bool{false, true} {
		r := &executionFixture{t: t}
		cleanups := 0
		wantErr := errors.New("runner unavailable")
		PrepareExecution = func(context.Context, string, RunInfo, string) (context.Context, tool.ExecutionRuntime, func(), error) {
			if nilContext {
				return nil, r, func() { cleanups++ }, nil
			}
			return ctx, r, func() { cleanups++ }, wantErr
		}
		_, runtime, cleanup, err = prepareExecution(ctx, "worker", "logical-cwd")
		if err == nil || runtime != nil || (!nilContext && !errors.Is(err, wantErr)) {
			t.Fatalf("preparation failed open: runtime=%v err=%v", runtime, err)
		}
		cleanup()
		if r.closed.Load() != 1 || cleanups != 1 {
			t.Fatalf("failure leaked or double closed: runtime=%d cleanup=%d", r.closed.Load(), cleanups)
		}
	}
}

func TestPrepareExecutionLeaseClosesBeforeSetupCompletes(t *testing.T) {
	saveExecutionHooks(t)
	r := &executionFixture{t: t}
	cleanups := 0
	PrepareExecution = func(ctx context.Context, _ string, _ RunInfo, _ string) (context.Context, tool.ExecutionRuntime, func(), error) {
		return ctx, r, func() { cleanups++ }, nil
	}
	_, runtime, cleanup, err := prepareExecution(t.Context(), "worker", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !runtime.(tool.RuntimeToolMatcher).Handles("fixture_exec") {
		t.Fatal("runtime lease hid custom execution tools")
	}
	// Covers an early return before constructing the SDK session.
	cleanup()
	_ = runtime.Close()
	cleanup()
	if r.closed.Load() != 1 || cleanups != 1 {
		t.Fatalf("cleanup was not exactly once: %d %d", r.closed.Load(), cleanups)
	}
}

func TestChatExecutionPreparedBeforeToolAssembly(t *testing.T) {
	saveExecutionHooks(t)
	verifyExecutionEntry(t, "assistant", RunInfo{SessionID: "runtime-chat"}, nil)
	t.Run("close_failure", func(t *testing.T) {
		verifyExecutionEntry(t, "assistant", RunInfo{SessionID: "runtime-chat"}, nil, errors.New("final artifact upload failed"))
	})
}

func TestTaskExecutionPreparedBeforeToolAssembly(t *testing.T) {
	d := testDB(t)
	t.Cleanup(func() { _ = d.Close() })
	saveExecutionHooks(t)
	for _, role := range []string{"worker", "planner", "mainagent"} {
		t.Run(role, func(t *testing.T) {
			expID, err := d.CreateExploration("execution fixture", "read the remote artifact")
			if err != nil {
				t.Fatal(err)
			}
			ts := d.Exploration(expID)
			t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM explorations WHERE id=$1`, expID) })
			info := RunInfo{TaskID: expID, ExplorationID: expID}
			if role == "worker" {
				info.IntentID, err = ts.AddNode("intent", map[string]any{"summary": "read the remote artifact"}, 0, "running", "planner", nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			verifyExecutionEntry(t, role, info, ts)
			t.Run("close_failure", func(t *testing.T) {
				verifyExecutionEntry(t, role, info, ts, errors.New("final artifact upload failed"))
			})
		})
	}
}

func verifyExecutionEntry(t *testing.T, role string, info RunInfo, ts *db.ExplorationStore, closeErrors ...error) {
	t.Helper()
	dir := t.TempDir()
	expectedDir := filepath.Join(dir, "sessions", info.SessionID)
	if info.TaskID != 0 {
		expectedDir = ensureRunDir(dir, info.TaskID, info.IntentID)
	}
	r := &executionFixture{t: t, wantDir: expectedDir}
	if len(closeErrors) > 0 {
		r.closeErr = closeErrors[0]
	}
	type preparedKey struct{}
	prepared, assembled, cleaned := 0, 0, 0
	PrepareExecution = func(ctx context.Context, key string, got RunInfo, cwd string) (context.Context, tool.ExecutionRuntime, func(), error) {
		prepared++
		if key != role || got != info || cwd != expectedDir {
			t.Errorf("wrong preparation scope: %s %+v %s", key, got, cwd)
		}
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
			t.Errorf("working directory not created before preparation: %v", err)
		}
		return context.WithValue(ctx, preparedKey{}, true), r, func() {
			cleaned++
			if r.closed.Load() != 1 {
				t.Error("host cleanup ran before runtime closed")
			}
		}, nil
	}
	ToolAugment = func(ctx context.Context, key string) ([]tool.CoreTool, DeferredInfo, func()) {
		assembled++
		if ctx.Value(preparedKey{}) != true || key != role {
			t.Error("tool assembly did not receive prepared execution context")
		}
		return nil, DeferredInfo{}, func() {}
	}
	provider := &sideAgentProvider{path: "only-on-runner.txt"}
	var err error
	var reason harness.TerminalReason
	var activities []db.Activity
	emit := func(a db.Activity) { activities = append(activities, a) }
	switch role {
	case "worker":
		w := NewWorker(provider, "fixture", dir, nil, 0, 5)
		reason, _, err = w.Execute(t.Context(), "work#1", info.TaskID, nil, ts, &db.Node{ID: info.IntentID, Payload: json.RawMessage(`{"summary":"read the remote artifact"}`)}, nil, emit, nil, nil)
	case "planner":
		p := NewPlanner(provider, "fixture", dir, nil, 0, 5)
		_, _, err = p.Plan(t.Context(), info.TaskID, nil, ts, "read the remote artifact", nil, emit)
	case "mainagent":
		m := NewMainAgent(provider, "fixture", dir, nil, 0, 5)
		_, err = m.Chat(t.Context(), info.TaskID, 0, nil, ts, "read the remote artifact", "read", emit, nil, nil, nil, nil)
	default:
		c := NewChatAgent(provider, "fixture", dir, nil, 0)
		_, err = c.Chat(t.Context(), role, info.SessionID, "read", 5, 0, false, emit)
	}
	if r.closeErr == nil && err != nil || r.closeErr != nil && !errors.Is(err, r.closeErr) {
		t.Fatalf("lost run/close outcome: got %v, close error %v", err, r.closeErr)
	}
	if r.closeErr != nil {
		if role == "worker" && reason != harness.ReasonExecutionError {
			t.Fatalf("close failure may be retried as a model failure: %s", reason)
		}
		if len(activities) == 0 || !activities[len(activities)-1].IsError || !strings.Contains(activities[len(activities)-1].Detail, r.closeErr.Error()) {
			t.Fatalf("final activity did not expose close failure: %+v", activities)
		}
	}
	if prepared != 1 || assembled != 1 || cleaned != 1 || r.closed.Load() != 1 || r.calls.Load() != 1 {
		t.Fatalf("incomplete execution lifecycle: prepare=%d assembly=%d cleanup=%d close=%d calls=%d", prepared, assembled, cleaned, r.closed.Load(), r.calls.Load())
	}
}

func TestCaptureFinalSessionPreservesCancellationAndCloseFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	wantClose := errors.New("publication unconfirmed")
	r := &executionFixture{t: t, closeErr: wantClose}
	s := agentcore.NewSession(agentcore.Options{Runtime: r, Provider: &sideAgentProvider{}, PermissionMode: permission.ModeBypass})
	_, reason, err := captureFinalSession(ctx, s, "cancelled", nil)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, wantClose) || reason != harness.ReasonExecutionError {
		t.Fatalf("lost cancellation or close failure: reason=%s error=%v", reason, err)
	}
	if r.closed.Load() != 1 {
		t.Fatalf("runtime closed %d times", r.closed.Load())
	}
}

func TestChatExecutionPreparationFailureDoesNotRunLocally(t *testing.T) {
	saveExecutionHooks(t)
	PrepareExecution = func(context.Context, string, RunInfo, string) (context.Context, tool.ExecutionRuntime, func(), error) {
		return nil, nil, nil, errors.New("no eligible runner")
	}
	ToolAugment = func(context.Context, string) ([]tool.CoreTool, DeferredInfo, func()) {
		t.Error("assembled local tools after remote preparation failed")
		return nil, DeferredInfo{}, func() {}
	}
	c := NewChatAgent(nil, "fixture", t.TempDir(), nil, 0)
	_, err := c.Chat(t.Context(), "assistant", "failed", "read", 5, 0, false, nil)
	if err == nil || !strings.Contains(err.Error(), "no eligible runner") {
		t.Fatalf("failed open: %v", err)
	}
}
