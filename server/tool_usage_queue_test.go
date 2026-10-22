package server

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

type batchUsageFunc func(context.Context, []db.ToolUsage) error

func (f batchUsageFunc) InsertToolUsageBatch(ctx context.Context, rows []db.ToolUsage) error {
	return f(ctx, rows)
}

func TestUsageQueueBlockedDatabaseDoesNotBlockToolOrClose(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	q := newToolUsageQueue(batchUsageFunc(func(ctx context.Context, _ []db.ToolUsage) error {
		once.Do(func() { close(started) })
		<-ctx.Done()
		return ctx.Err()
	}), 1)
	defer q.close()
	q.InsertToolUsage(&db.ToolUsage{ToolKey: "fixture"})
	<-started
	called := false
	// Exercise the real decorator while its database writer is stuck.
	wrapped := meterTool(tool.Build(tool.Spec{Name: "fixture", Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		called = true
		return tool.Text("ok"), nil
	}}), q, "fixture", "worker", agent.RunInfo{})
	for range 100 {
		if _, err := wrapped.Call(t.Context(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !called || q.dropped.Load() == 0 {
		t.Fatal("queue failed to apply bounded backpressure")
	}
	start := time.Now()
	q.close()
	if time.Since(start) > 3*time.Second {
		t.Fatal("shutdown was not bounded")
	}
	if q.failed.Load() != q.accepted.Load() {
		t.Fatal(q.snapshot())
	}
}

func TestUsageQueueConcurrentCloseFlushesAcceptedRows(t *testing.T) {
	var mu sync.Mutex
	rows := []db.ToolUsage{}
	q := newToolUsageQueue(batchUsageFunc(func(_ context.Context, b []db.ToolUsage) error {
		mu.Lock()
		defer mu.Unlock()
		rows = append(rows, b...)
		return nil
	}), 1024)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				q.InsertToolUsage(&db.ToolUsage{ToolKey: "fixture", TaskID: 123})
			}
		}()
	}
	wg.Wait()
	q.close()
	q.close()
	if q.accepted.Load() != 800 || q.written.Load() != 800 || len(rows) != 800 {
		t.Fatal(q.snapshot())
	}
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); q.close(); q.InsertToolUsage(&db.ToolUsage{ToolKey: "closed"}) }()
	}
	wg.Wait()
	if q.dropped.Load() != 4 {
		t.Fatal(q.snapshot())
	}
}
