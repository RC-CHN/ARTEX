package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

func TestCanceledToolDoesNotWaitForSemaphore(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	l := &loop{ctx: ctx, toolCtx: ctx, sem: make(chan struct{}, 1)}
	l.sem <- struct{}{}
	e := newStreamExec(l)
	tr := &tracked{block: llm.ContentBlock{ID: "waiting"}, status: stExecuting}
	e.tools = []*tracked{tr}
	done := make(chan struct{})
	go func() { e.run(tr); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled waiter still requires semaphore")
	}
	if got := e.results()[0]; !strings.Contains(got.Content[0].Text, `"execution_status":"not_started"`) {
		t.Fatal(got)
	}
	<-l.sem
}

func TestCanceledToolReceiptIsImmutableAfterLateCompletion(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	started, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		close(started)
		<-release
		r := tool.Text("late success")
		r.Extra = []llm.Message{llm.UserText("late extra")}
		return r, nil
	}})
	l := &loop{ctx: ctx, toolCtx: ctx, sem: make(chan struct{}, 1), in: QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass}, yield: func(Event, error) bool { return true }}
	e := newStreamExec(l)
	tr := &tracked{block: llm.ContentBlock{ID: "late", Name: "probe", Input: json.RawMessage(`{}`)}, status: stExecuting}
	e.tools = []*tracked{tr}
	go func() { e.run(tr); close(returned) }()
	<-started
	cancel()
	if stopped, aborted := e.finish(); stopped || !aborted {
		t.Fatal(stopped, aborted)
	}
	before, _ := json.Marshal(e.results())
	if !strings.Contains(string(before), `unknown`) {
		t.Fatal(string(before))
	}
	close(release)
	<-returned
	after, _ := json.Marshal(e.results())
	if string(before) != string(after) || len(e.extraMessages()) != 0 || e.pending() {
		t.Fatalf("late completion changed published receipt: %s -> %s", before, after)
	}
}
