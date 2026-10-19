package server

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/tool"

	"github.com/RC-CHN/ARTEX/runner"
)

func TestCapacityRecoveryRetriesOnlyPreparationAndStopsOnSuccess(t *testing.T) {
	attempts, reports := 0, 0
	err := waitWorkerPreparation(t.Context(), time.Millisecond, func() error {
		attempts++
		if attempts < 3 {
			return fmt.Errorf("prepare worker: %w", runner.ErrCapacityUnavailable)
		}
		return nil
	}, func(_ error, n int, _ time.Duration) {
		reports++
		if n != reports {
			t.Fatal("wrong retry report")
		}
	})
	if err != nil || attempts != 3 || reports != 1 {
		t.Fatalf("recovery: %v attempts=%d reports=%d", err, attempts, reports)
	}
}
func TestCapacityWaitCancellationPreventsNextAttempt(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	attempts := 0
	start := time.Now()
	err := waitWorkerPreparation(ctx, time.Hour, func() error { attempts++; return runner.ErrCapacityUnavailable }, func(error, int, time.Duration) { cancel(errors.New("human stopped waiting")) })
	if !errors.Is(err, context.Canceled) || attempts != 1 || time.Since(start) > time.Second {
		t.Fatalf("wait not cancelled: %v attempts=%d", err, attempts)
	}
}
func TestCapacityWaitDoesNotReplayOtherFailures(t *testing.T) {
	for _, err := range []error{runner.ErrUnknownOutcome, runner.ErrFenced, runner.ErrClosed, runner.ErrStoreConflict, runner.ErrStoreUnavailable, &runner.OutcomeError{Cause: errRunnerPreparationUnavailable}, context.DeadlineExceeded, errors.New("image pull failed"), errors.New("Forbidden"), errors.New("runner_capacity_unavailable in tool output")} {
		calls := 0
		got := waitWorkerPreparation(t.Context(), time.Millisecond, func() error { calls++; return err }, nil)
		if got != err || calls != 1 {
			t.Fatalf("replayed non-capacity error: %v", err)
		}
	}
}

func TestRunnerPreparationClassificationDoesNotRetryUnknownOutcomes(t *testing.T) {
	for _, err := range []error{runner.ErrClosed, runner.ErrStoreUnavailable, runner.ErrStoreConflict} {
		if !errors.Is(runnerPreparationError(err), errRunnerPreparationUnavailable) {
			t.Fatalf("prepare error not retained for retry: %v", err)
		}
		if errors.Is(runnerPreparationError(&runner.OutcomeError{Cause: err}), errRunnerPreparationUnavailable) {
			t.Fatal("unknown outcome became retryable")
		}
	}
}

func TestClosedRunnerDoesNotClaimFrontier(t *testing.T) {
	m := runnerTestManager(t)
	task, err := m.CreateTask("synthetic closed broker", "preserve frontier", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.DeleteTask(task.ID, DeleteTaskOptions{})
	id, err := task.Store.AddIntent(map[string]any{"summary": "synthetic pending"}, 1, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := recoverableServer(t)
	e := NewEngine(m)
	defer e.Close()
	e.executionAvailable = s.runnerExecutionAvailable
	// A nil worker would panic if admission accidentally passed into execution.
	for range 32 {
		if e.runWorkerStep(t.Context(), task, "synthetic", nil) {
			t.Fatal("closed broker consumed an intent")
		}
	}
	node, err := task.Store.GetNode(id)
	if err != nil || node.State != "open" {
		t.Fatal("unavailable broker changed frontier state", err)
	}
	cfg := s.runnerCfg
	cfg.AgentModes = map[string]string{"planner": "local"}
	s.publishRunnerState(cfg, s.runners, nil)
	if s.runnerExecutionAvailable("worker") || !s.runnerExecutionAvailable("planner") {
		t.Fatal("remote outage incorrectly affected explicitly local mode")
	}
}

func TestWorkerPreparationWaitPreservesFrontierAndHonorsPause(t *testing.T) {
	for _, preparationErr := range []error{runner.ErrCapacityUnavailable, errRunnerPreparationUnavailable} {
		t.Run(preparationErr.Error(), func(t *testing.T) {
			m := runnerTestManager(t)
			task, err := m.CreateTask("synthetic capacity", "wait for environment", nil, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer m.DeleteTask(task.ID, DeleteTaskOptions{})
			first, err := task.Store.AddIntent(map[string]any{"summary": "synthetic first"}, 100, nil, "test")
			if err != nil {
				t.Fatal(err)
			}
			second, err := task.Store.AddIntent(map[string]any{"summary": "synthetic second"}, 1, nil, "test")
			if err != nil {
				t.Fatal(err)
			}
			old := agent.PrepareExecution
			defer func() { agent.PrepareExecution = old }()
			prepared := make(chan struct{}, 1)
			agent.PrepareExecution = func(ctx context.Context, _ string, _ agent.RunInfo, _ string) (context.Context, tool.ExecutionRuntime, func(), error) {
				prepared <- struct{}{}
				return ctx, nil, nil, preparationErr
			}
			var modelCalls atomic.Int32
			worker := agent.NewWorker(retestProvider{complete: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				modelCalls.Add(1)
				return llm.Message{}, "", llm.Usage{}, nil
			}}, "test", m.dir, nil, 10000, 1)
			e := NewEngine(m)
			defer e.Close()
			done := make(chan bool, 1)
			go func() { done <- e.runWorkerStep(t.Context(), task, "capacity-worker", worker) }()
			select {
			case <-prepared:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not prepare")
			}
			a, _ := task.Store.GetNode(first)
			b, _ := task.Store.GetNode(second)
			if a.State != "running" || b.State != "open" || modelCalls.Load() != 0 {
				t.Fatalf("capacity consumed frontier or called model: first=%s second=%s calls=%d", a.State, b.State, modelCalls.Load())
			}
			select {
			case <-done:
				t.Fatal("capacity wait completed intent")
			case <-time.After(30 * time.Millisecond):
			}
			e.Pause(task.ID, agent.AbortPausedByUser)
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("pause blocked behind capacity wait")
			}
			a, _ = task.Store.GetNode(first)
			b, _ = task.Store.GetNode(second)
			if a.State != "open" || b.State != "open" || modelCalls.Load() != 0 {
				t.Fatalf("pause corrupted frontier: %s %s", a.State, b.State)
			}

		})
	}
}
