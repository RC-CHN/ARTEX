package server

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
)

type meteredRemoteRuntime struct {
	input json.RawMessage
	calls int
}

func (r *meteredRemoteRuntime) Call(_ context.Context, _ string, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
	r.calls++
	r.input = append(json.RawMessage(nil), in...)
	return actool.Text("remote"), nil
}
func (*meteredRemoteRuntime) Close() error                              { return nil }
func (*meteredRemoteRuntime) DrainNotifications() []actool.Notification { return nil }
func (*meteredRemoteRuntime) Handles(name string) bool                  { return name == "custom_exec" }

func TestRemoteToolPreservesCatalogUsageAndDefaultInjection(t *testing.T) {
	for _, name := range []string{"Bash", "custom_exec"} {
		t.Run(name, func(t *testing.T) {
			base := actool.Build(actool.Spec{Name: name, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				t.Fatal("remote catalog tool executed local implementation")
				return actool.Result{}, nil
			}})
			schema := map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "default": float64(3)}}}
			recorder := &recordingToolUsage{}
			ri := agent.RunInfo{TaskID: 42, ExplorationID: 8, IntentID: 9, SessionID: "conversation"}
			catalog := meterTool(agent.DecorateTool(base, "configured", schema), recorder, name, "worker", ri)
			runtime := &meteredRemoteRuntime{}
			bound := actool.BindExecutionRuntime([]actool.CoreTool{catalog}, runtime)[0]
			if _, err := bound.Call(context.Background(), json.RawMessage(`{}`), nil); err != nil {
				t.Fatal(err)
			}
			var input map[string]int
			if err := json.Unmarshal(runtime.input, &input); err != nil || input["limit"] != 3 || runtime.calls != 1 {
				t.Fatalf("remote defaults/calls: input=%s calls=%d err=%v", runtime.input, runtime.calls, err)
			}
			want := &db.ToolUsage{ToolKey: name, AgentKey: "worker", TaskID: 42, ExplorationID: 8, IntentID: 9, SessionID: "conversation"}
			if len(recorder.rows) != 1 || !reflect.DeepEqual(recorder.rows[0], want) {
				t.Fatalf("remote usage attribution lost: %+v", recorder.rows)
			}
		})
	}
}
