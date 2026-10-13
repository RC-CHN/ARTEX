package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/llm"
)

func TestRunningWorkerControlWaitsForSettlement(t *testing.T) {
	for _, tc := range []struct {
		name, action, mode, state string
		disconnect                bool
	}{
		{name: "pause", action: "pause", state: "paused"},
		{name: "cancel_soft", action: "cancel", state: db.StateIntentDeleted},
		{name: "cancel_hard", action: "cancel", mode: "hard"},
		{name: "pause_caller_disconnect", action: "pause", state: "paused", disconnect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireTestPostgres(t)
			m, err := NewManager(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			defer m.Close()
			task, err := m.CreateTask("worker control regression", "interrupt one worker", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer m.DeleteTask(task.ID, DeleteTaskOptions{})
			intentID, err := task.Store.AddNode(db.KindIntent, map[string]any{"summary": "controlled worker"}, 1, "paused", "human", nil)
			if err != nil {
				t.Fatal(err)
			}
			e := NewEngine(m)
			defer e.Close()
			s := &Server{m: m, engine: e, ctx: t.Context()}
			sibling := e.execContextFor(t.Context(), "unrelated-task")
			started, release := make(chan struct{}), make(chan struct{})
			observed := make(chan error, 1)
			var startedOnce, releaseOnce, observedOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			var calls atomic.Int32
			worker := agent.NewWorker(retestProvider{complete: func(ctx context.Context, _ llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls.Add(1)
				startedOnce.Do(func() { close(started) })
				<-ctx.Done()
				observedOnce.Do(func() { observed <- context.Cause(ctx) })
				<-release
				return llm.Message{}, "", llm.Usage{}, ctx.Err()
			}}, "test", m.dir, nil, 10000, 1)
			worker.SetNonStreaming(func() bool { return true })
			e.SetAuthoritativeAgentResolver(func(*Task) (*agent.Planner, *agent.Worker) { return nil, worker })
			if err := e.runDetachedIntent(t.Context(), task, intentID, "control-regression", "continue", "continue"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not reach its provider")
			}
			callCtx, disconnect := context.WithCancel(t.Context())
			defer disconnect()
			type result struct {
				out intentControlResult
				err error
			}
			done := make(chan result, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				out, err := s.applyIntentControl(callCtx, task, intentID, tc.action, "test requested stop", tc.mode)
				done <- result{out, err}
			}()
			defer func() {
				releaseOnce.Do(func() { close(release) })
				<-finished
			}()
			select {
			case cause := <-observed:
				want := agent.AbortWorkPausedByUser
				if tc.action == "cancel" {
					want = agent.AbortWorkCancelledByUser
				}
				if !errors.Is(cause, want) {
					t.Fatalf("worker cancellation cause=%v want=%v", cause, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("control did not cancel the provider")
			}
			select {
			case got := <-done:
				t.Fatalf("control returned before worker settlement: %+v", got)
			default:
			}
			node, err := task.Store.GetNode(intentID)
			if err != nil || node == nil || node.State != "running" {
				t.Fatalf("intent was changed or deleted while its writer was active: %+v err=%v", node, err)
			}
			if tc.disconnect {
				disconnect()
				select {
				case got := <-done:
					if !errors.Is(got.err, context.Canceled) {
						t.Fatalf("disconnected caller result: %+v", got)
					}
				case <-time.After(time.Second):
					t.Fatal("control did not release its disconnected caller")
				}
			}
			releaseOnce.Do(func() { close(release) })
			if !tc.disconnect {
				select {
				case got := <-done:
					if got.err != nil || got.out.State != tc.state {
						t.Fatalf("settled control result: %+v", got)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("control did not finish after the worker drained")
				}
			}
			e.operationsWG.Wait()
			node, err = task.Store.GetNode(intentID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mode == "hard" {
				if node != nil {
					t.Fatalf("hard cancellation retained its intent: %+v", node)
				}
			} else if node == nil || node.State != tc.state {
				t.Fatalf("cancelled worker overwrote its final state: %+v, want %q", node, tc.state)
			}
			if sibling.Err() != nil || calls.Load() != 1 {
				t.Fatalf("control cancelled another task or retried a stopped provider: sibling=%v calls=%d", sibling.Err(), calls.Load())
			}
		})
	}
}
