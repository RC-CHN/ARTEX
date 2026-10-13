package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

type recordingRuntime struct {
	mu       sync.Mutex
	calls    []string
	notes    []tool.Notification
	closed   int
	closeErr error
	run      func(context.Context, string, json.RawMessage, *tool.ToolContext) (tool.Result, error)
}

func (r *recordingRuntime) Call(ctx context.Context, name string, input json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
	r.mu.Lock()
	r.calls = append(r.calls, name)
	r.mu.Unlock()
	if r.run != nil {
		return r.run(ctx, name, input, tc)
	}
	return tool.Text("remote:" + name), nil
}

func (r *recordingRuntime) Handles(name string) bool { return name == "custom_exec" }

func (r *recordingRuntime) DrainNotifications() []tool.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	notes := r.notes
	r.notes = nil
	return notes
}

func (r *recordingRuntime) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed++
	return r.closeErr
}

func runtimeProbe(name string) tool.CoreTool {
	return tool.Build(tool.Spec{
		Name: name, Schema: map[string]any{"type": "object"},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
			return tool.Text("host:" + name), nil
		},
	})
}

func runtimeScript(turns ...[]llm.StreamEvent) harness.QueryDeps {
	n := 0
	return harness.QueryDeps{CallModel: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			if n >= len(turns) {
				yield(llm.StreamEvent{}, errors.New("unexpected extra model call"))
				return
			}
			events := turns[n]
			n++
			for _, event := range events {
				if !yield(event, nil) {
					return
				}
			}
		}
	}}
}

func runtimePrompt(t *testing.T, s *Session) []llm.ContentBlock {
	t.Helper()
	var results []llm.ContentBlock
	for event, err := range s.Prompt(t.Context(), "exercise execution") {
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == harness.KindToolResult {
			results = append(results, *event.ToolResult)
		}
	}
	return results
}

func TestExecutionRuntimeReplacesAutomaticAndOptInTools(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	r := &recordingRuntime{}
	base := append(tool.DefaultTools(), tool.ShellSessionTools()...)
	base = append(base, runtimeProbe("custom_exec"), runtimeProbe("record_fact"), runtimeProbe("Skill"))
	s := NewSession(Options{Runtime: r, Tools: base, EnableWebFetch: true, EnableWebSearch: true, SessionID: "remote-session"})
	t.Cleanup(func() { _ = s.Close() })
	if s.tasks != nil {
		t.Fatal("remote session allocated a local task manager")
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), "norma", "remote-session")); !os.IsNotExist(err) {
		t.Fatalf("local task directory created: %v", err)
	}
	names := []string{"Bash", "Read", "Write", "Edit", "MultiEdit", "LS", "Glob", "Grep", "WebFetch", "web_search", "TaskOutput", "TaskStop", "TaskList", "Monitor", "shell_open", "shell_send", "shell_read", "shell_close", "shell_list", "custom_exec"}
	for _, name := range names {
		ct, ok := s.registry.Get(name)
		if !ok {
			t.Fatalf("missing tool %s", name)
		}
		result, err := ct.Call(t.Context(), json.RawMessage(`{}`), &tool.ToolContext{})
		if err != nil || result.Flatten() != "remote:"+name {
			t.Fatalf("%s executed locally: %+v %v", name, result, err)
		}
	}
	for _, name := range []string{"record_fact", "Skill"} {
		ct, _ := s.registry.Get(name)
		result, err := ct.Call(t.Context(), json.RawMessage(`{}`), &tool.ToolContext{})
		if err != nil || result.Flatten() != "host:"+name {
			t.Fatalf("host tool redirected: %s %+v %v", name, result, err)
		}
	}
	// Binding must not mutate the host's base slice, reused by another session.
	ct := base[len(base)-3]
	result, _ := ct.Call(t.Context(), json.RawMessage(`{}`), nil)
	if result.Flatten() != "host:custom_exec" {
		t.Fatal("runtime mutated caller's tool slice")
	}
}

type runtimeHooks struct {
	block     bool
	pre, post int
}

func (h *runtimeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	h.pre++
	return h.block, "central guard", nil
}
func (h *runtimeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) { h.post++ }
func (*runtimeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

func TestExecutionRuntimePreservesCentralPolicy(t *testing.T) {
	for _, mode := range []string{"permissions", "hook", "allow", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			r := &recordingRuntime{}
			h := &runtimeHooks{block: mode == "hook"}
			input := `{"command":"echo remote"}`
			if mode == "malformed" {
				input = `{}`
			}
			opts := Options{Runtime: r, Tools: []tool.CoreTool{tool.NewBash()}, PermissionMode: permission.ModeBypass, Hooks: h,
				Deps: runtimeScript(toolTurn("c", "Bash", input), textTurn("done"))}
			if mode == "permissions" {
				opts.DisallowedTools = []string{"Bash"}
			}
			s := NewSession(opts)
			t.Cleanup(func() { _ = s.Close() })
			results := runtimePrompt(t, s)
			if len(results) != 1 {
				t.Fatalf("results: %+v", results)
			}
			if mode == "allow" {
				if len(r.calls) != 1 || h.pre != 1 || h.post != 1 || results[0].IsError {
					t.Fatalf("allowed call lost hooks/runtime: %+v %+v", r.calls, h)
				}
			} else if len(r.calls) != 0 || !results[0].IsError {
				t.Fatalf("rejected %s reached runtime: %v %+v", mode, r.calls, results)
			}
		})
	}
}

func TestExecutionRuntimeDeferredUnlockRemainsCentral(t *testing.T) {
	for _, unlocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "locked", true: "unlocked"}[unlocked], func(t *testing.T) {
			r := &recordingRuntime{}
			unlock := tool.NewUnlockSet()
			if unlocked {
				unlock.Add("custom_exec")
			}
			s := NewSession(Options{Runtime: r, Tools: []tool.CoreTool{runtimeProbe("custom_exec")},
				DeferredTools: []string{"custom_exec"}, UnlockSet: unlock, PermissionMode: permission.ModeBypass,
				Deps: runtimeScript(toolTurn("c", tool.ExecuteExtraToolName, `{"tool_name":"custom_exec","params":{}}`), textTurn("done"))})
			t.Cleanup(func() { _ = s.Close() })
			results := runtimePrompt(t, s)
			if len(results) != 1 || results[0].IsError == unlocked || (len(r.calls) == 1) != unlocked {
				t.Fatalf("unlock bypass: %+v calls=%v", results, r.calls)
			}
		})
	}
}

func TestExecutionRuntimeNotificationsAndRemoteOutput(t *testing.T) {
	r := &recordingRuntime{notes: []tool.Notification{{TaskID: "before-turn", Summary: "PREVIOUS_REMOTE_DONE"}}}
	r.run = func(_ context.Context, _ string, _ json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
		if tc.Tasks != nil || tc.OutputDir != "" {
			t.Error("remote call received local manager/output directory")
		}
		r.mu.Lock()
		r.notes = append(r.notes, tool.Notification{TaskID: "after-tool", Summary: "CURRENT_REMOTE_DONE"})
		r.mu.Unlock()
		return tool.Text(strings.Repeat("large remote output ", 200)), nil
	}
	deps := runtimeScript(toolTurn("c", "Bash", `{"command":"compute"}`), textTurn("done"))
	original := deps.CallModel
	var requests []string
	deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		raw, _ := json.Marshal(req.Messages)
		requests = append(requests, string(raw))
		return original(ctx, req)
	}
	spillDir := filepath.Join(t.TempDir(), "must-not-exist")
	s := NewSession(Options{Runtime: r, PermissionMode: permission.ModeBypass, ToolOutputDir: spillDir, MaxToolOutputChars: 100, Deps: deps})
	t.Cleanup(func() { _ = s.Close() })
	results := runtimePrompt(t, s)
	if len(requests) != 2 || !strings.Contains(requests[0], "PREVIOUS_REMOTE_DONE") || !strings.Contains(requests[1], "CURRENT_REMOTE_DONE") {
		t.Fatalf("lost runtime notifications: %v", requests)
	}
	if len(results) != 1 || !strings.Contains(results[0].Content[0].Text, "characters truncated]") {
		t.Fatalf("output not capped: %+v", results)
	}
	if _, err := os.Stat(spillDir); !os.IsNotExist(err) {
		t.Fatalf("SDK wrote an inaccessible local artifact: %v", err)
	}
	if len(r.DrainNotifications()) != 0 {
		t.Fatal("completion notifications were not drained")
	}
}

func TestExecutionRuntimeCancellationAndCloseOnce(t *testing.T) {
	started := make(chan struct{})
	r := &recordingRuntime{closeErr: errors.New("remote close failed")}
	r.run = func(ctx context.Context, _ string, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
		close(started)
		<-ctx.Done()
		return tool.Result{}, ctx.Err()
	}
	s := NewSession(Options{Runtime: r, PermissionMode: permission.ModeBypass, Deps: runtimeScript(toolTurn("c", "Bash", `{"command":"long compute"}`))})
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range s.Prompt(ctx, "compute") {
		}
	}()
	<-started
	cancel()
	<-done
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !errors.Is(s.Close(), r.closeErr) {
				t.Error("Close lost runtime error")
			}
		}()
	}
	wg.Wait()
	if r.closed != 1 {
		t.Fatalf("runtime closed %d times", r.closed)
	}
}

func TestExecutionRuntimeRunPropagatesCloseFailure(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint("cancelled=", cancelled), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			if cancelled {
				cancel()
			}
			wantClose := errors.New("final artifact publication failed")
			r := &recordingRuntime{closeErr: wantClose}
			text, err := Run(ctx, Options{Runtime: r, Deps: runtimeScript(textTurn("done"))}, "finish")
			if !errors.Is(err, wantClose) || cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost close/run error: %v", err)
			}
			if !cancelled && text != "done" {
				t.Fatalf("lost completed response: %q", text)
			}
			if r.closed != 1 {
				t.Fatalf("runtime closed %d times", r.closed)
			}
		})
	}
}

type uncertainExecution struct{ cause error }

func (e uncertainExecution) Error() string {
	return "runner disconnected after dispatch: " + e.cause.Error()
}
func (e uncertainExecution) Unwrap() error      { return e.cause }
func (uncertainExecution) FatalExecution() bool { return true }

func TestExecutionRuntimeFatalErrorStopsQueuedToolsAndModelRetry(t *testing.T) {
	want := errors.New("outcome unknown")
	r := &recordingRuntime{run: func(context.Context, string, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
		return tool.Result{}, fmt.Errorf("remote call: %w", uncertainExecution{want})
	}}
	modelCalls := 0
	s := NewSession(Options{Runtime: r, PermissionMode: permission.ModeBypass, NonStreaming: true,
		Deps: harness.QueryDeps{CallModelSync: func(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			modelCalls++
			return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
				{Type: llm.BlockToolUse, ID: "first", Name: "Bash", Input: json.RawMessage(`{"command":"first-side-effect"}`)},
				{Type: llm.BlockToolUse, ID: "queued", Name: "Bash", Input: json.RawMessage(`{"command":"must-not-run"}`)},
			}}, "tool_use", llm.Usage{}, nil
		}}})
	t.Cleanup(func() { _ = s.Close() })
	var terminal *harness.Terminal
	var gotErr error
	for event, err := range s.Prompt(t.Context(), "execute once") {
		if event.Terminal != nil {
			terminal = event.Terminal
			gotErr = terminal.Err
		}
		if err != nil {
			gotErr = err
		}
	}
	if terminal == nil || terminal.Reason != harness.ReasonExecutionError || !errors.Is(gotErr, want) {
		t.Fatalf("fatal error lost: terminal=%+v err=%v", terminal, gotErr)
	}
	if modelCalls != 1 || len(r.calls) != 1 {
		t.Fatalf("unsafe retry or queued execution: model=%d calls=%v", modelCalls, r.calls)
	}
	if len(s.Messages()) < 3 {
		t.Fatal("uncertain tool attempt missing from resumable transcript")
	}
}
