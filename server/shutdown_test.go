package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

func TestManagerCloseJoinsServerWorkBeforeClosingDatabase(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	dir := t.TempDir()
	s := New(context.Background(), m, dir, dir, dir)
	file := filepath.Join(dir, "finished")
	cancelled := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	if !s.background(func() {
		<-s.ctx.Done()
		close(cancelled)
		// Finishing agents may enqueue persistence after new executions have
		// been stopped. The manager must also wait for this final async write.
		if !s.persistence.start(func() {
			<-release
			if err := m.pg.Ping(); err != nil {
				finished <- err
				return
			}
			finished <- os.WriteFile(file, []byte("drained"), 0o600)
		}) {
			finished <- context.Canceled
		}
	}) {
		t.Fatal("background work was not admitted")
	}
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("Manager.Close did not cancel its server")
	}
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("Manager.Close returned before work drained: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Manager.Close did not join background work")
	}
	if err := <-finished; err != nil {
		t.Fatalf("shutdown closed the database before the final write: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal(err)
	}
	if s.background(func() { t.Error("work admitted after shutdown") }) {
		t.Fatal("background admission remained open")
	}
	if err := m.pg.Ping(); err == nil {
		t.Fatal("database remained open after Manager.Close")
	}
	// Repeat calls must not rerun hooks, decrement wait groups, or block.
	s.Close()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEngineCloseJoinsLoopsAndStandaloneOperations(t *testing.T) {
	e := NewEngine(nil)
	e.deleteMu.RLock()
	rt := e.registerTaskRoutines(context.Background(), "42", 1)
	e.deleteMu.RUnlock()
	runTaskRoutine(rt, func(ctx context.Context) { <-ctx.Done() })
	if !e.beginTaskOperation("42") {
		t.Fatal("operation was not admitted")
	}
	// Execution normally inherits a task runtime. Closing that runtime first
	// would replace AbortShutdown with plain context.Canceled.
	execCtx := e.execContextFor(rt.ctx, "42")
	closed := make(chan struct{})
	go func() { e.Close(); close(closed) }()
	select {
	case <-execCtx.Done():
	case <-time.After(time.Second):
		e.decInflight("42")
		t.Fatal("in-flight agent execution was not cancelled")
	}
	if context.Cause(execCtx) != agent.AbortShutdown {
		t.Fatalf("shutdown cause = %v", context.Cause(execCtx))
	}
	select {
	case <-closed:
		e.decInflight("42")
		t.Fatal("Close returned with an admitted operation still running")
	default:
	}
	e.decInflight("42")
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close failed to finish after operation drained")
	}
	if e.beginTaskOperation("43") {
		t.Fatal("operation admitted after shutdown")
	}
	if ctx := e.execContextFor(context.Background(), "43"); context.Cause(ctx) != agent.AbortShutdown {
		t.Fatalf("execution context created after shutdown: %v", context.Cause(ctx))
	}
	e.Close()
}

func TestShutdownWorkerCanResumeAfterRestart(t *testing.T) {
	for _, providerReturnsError := range []bool{false, true} {
		name := "provider_returns_nil"
		if providerReturnsError {
			name = "provider_returns_cancel_error"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			m, err := NewManager(dir, "")
			if err != nil {
				t.Skipf("postgres unavailable (%v)", err)
			}
			defer m.Close()
			task, err := m.CreateTask("shutdown recovery", "finish after restart", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			intentID, err := task.Store.AddIntent(map[string]any{"summary": "interrupt and resume"}, 1, nil, "test")
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			cause := make(chan error, 1)
			worker := agent.NewWorker(retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				close(started)
				<-ctx.Done()
				cause <- context.Cause(ctx)
				if providerReturnsError {
					return llm.Message{}, "", llm.Usage{}, ctx.Err()
				}
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("partial result")}}, "end_turn", llm.Usage{}, nil
			}}, "test", dir, nil, 10000, 1)
			worker.SetNonStreaming(func() bool { return true })
			e := NewEngine(m)
			e.deleteMu.RLock()
			rt := e.registerTaskRoutines(context.Background(), task.ID, 1)
			e.deleteMu.RUnlock()
			if !e.beginTaskOperation(task.ID) {
				t.Fatal("worker was not admitted")
			}
			runTaskRoutine(rt, func(ctx context.Context) {
				defer e.decInflight(task.ID)
				e.runWorkerStep(ctx, task, "test-worker", worker)
			})
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not reach its provider")
			}
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}
			if got := <-cause; got != agent.AbortShutdown {
				t.Fatalf("provider observed cancellation cause %v", got)
			}

			restarted, err := NewManager(dir, "")
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			restarted.LoadExisting()
			reloaded, ok := restarted.Task(task.ID)
			if !ok {
				t.Fatal("task was not restored")
			}
			defer restarted.DeleteTask(task.ID, DeleteTaskOptions{})
			intent, err := reloaded.Store.GetNode(intentID)
			if err != nil || intent == nil || intent.State != "open" {
				t.Fatalf("interrupted intent is not recoverable: %+v, error %v", intent, err)
			}
			if isTerminalStatus(reloaded.lifecycleSnapshot().Status) {
				t.Fatal("shutdown completed the task")
			}
			resumedWorker := agent.NewWorker(retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("finished after restart")}}, "end_turn", llm.Usage{}, nil
			}}, "test", dir, nil, 10000, 1)
			resumedWorker.SetNonStreaming(func() bool { return true })
			resumed := NewEngine(restarted)
			if !resumed.beginTaskOperation(task.ID) {
				t.Fatal("resumed worker was not admitted")
			}
			claimed := resumed.runWorkerStep(context.Background(), reloaded, "resumed-worker", resumedWorker)
			resumed.decInflight(task.ID)
			if !claimed {
				t.Fatal("restarted worker could not reclaim the interrupted intent")
			}
			intent, err = reloaded.Store.GetNode(intentID)
			if err != nil || intent == nil || intent.State != "done" {
				t.Fatalf("resumed intent did not finish: %+v, error %v", intent, err)
			}
		})
	}
}

func TestServerCloseFlushesLLMHealthWrites(t *testing.T) {
	for _, clear := range []bool{false, true} {
		name := "save"
		if clear {
			name = "clear"
		}
		t.Run(name, func(t *testing.T) {
			m, err := NewManager(t.TempDir(), "")
			if err != nil {
				t.Skipf("postgres unavailable (%v)", err)
			}
			defer m.Close()
			profileID, err := m.pg.SaveProfile(&db.LLMProfile{Name: t.Name(), Format: "openai", Model: "test", APIKey: "test"})
			if err != nil {
				t.Fatal(err)
			}
			defer m.pg.DeleteProfile(profileID)
			if clear {
				until := time.Now().Add(time.Minute)
				if err := m.pg.SaveLLMHealth(db.LLMHealth{ProfileID: profileID, Trips: 1, OpenUntil: &until}); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			s := New(context.Background(), m, dir, dir, dir)
			if clear {
				s.llmHealth.Pass(profileID)
			} else {
				s.llmHealth.Trip(profileID, "shutdown persistence fixture", true)
			}
			s.Close()
			var rows int
			if err := m.pg.QueryRow(`SELECT count(*) FROM llm_profile_health WHERE profile_id=$1`, profileID).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			want := 1
			if clear {
				want = 0
			}
			if rows != want {
				t.Fatalf("health rows after Close = %d, want %d", rows, want)
			}
		})
	}
}
