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
	err := waitWorkerCapacity(t.Context(), time.Millisecond, func() error {
		attempts++
		if attempts < 3 {
			return fmt.Errorf("prepare worker: %w", runner.ErrCapacityUnavailable)
		}
		return nil
	}, func(n int, _ time.Duration) {
		reports++
		if n != reports {
			t.Fatal("wrong retry report")
		}
	})
	if err != nil || attempts != 3 || reports != 2 {
		t.Fatalf("recovery: %v attempts=%d reports=%d", err, attempts, reports)
	}
}
func TestCapacityWaitCancellationPreventsNextAttempt(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	attempts := 0
	start := time.Now()
	err := waitWorkerCapacity(ctx, time.Hour, func() error { attempts++; return runner.ErrCapacityUnavailable }, func(int, time.Duration) { cancel(errors.New("human stopped waiting")) })
	if !errors.Is(err, context.Canceled) || attempts != 1 || time.Since(start) > time.Second {
		t.Fatalf("wait not cancelled: %v attempts=%d", err, attempts)
	}
}
func TestCapacityWaitDoesNotReplayOtherFailures(t *testing.T) {
	for _, err := range []error{runner.ErrUnknownOutcome, runner.ErrFenced, context.DeadlineExceeded, errors.New("image pull failed"), errors.New("Forbidden"), errors.New("runner_capacity_unavailable in tool output")} {
		calls := 0
		got := waitWorkerCapacity(t.Context(), time.Millisecond, func() error { calls++; return err }, nil)
		if got != err || calls != 1 {
			t.Fatalf("replayed non-capacity error: %v", err)
		}
	}
}

func TestWorkerCapacityWaitPreservesFrontierAndHonorsPause(t *testing.T) {
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
		return ctx, nil, nil, runner.ErrCapacityUnavailable
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
}
