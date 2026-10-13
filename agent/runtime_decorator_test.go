package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/permission"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
)

type decoratorRuntime struct {
	input     json.RawMessage
	name      string
	panicCall bool
}

func (r *decoratorRuntime) Call(_ context.Context, name string, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
	if r.panicCall {
		panic("remote transport panic")
	}
	r.name = name
	r.input = append(json.RawMessage(nil), in...)
	return actool.Text("remote result"), nil
}
func (*decoratorRuntime) Close() error                              { return nil }
func (*decoratorRuntime) DrainNotifications() []actool.Notification { return nil }
func (*decoratorRuntime) Handles(name string) bool                  { return name == "custom_exec" }

func TestRemoteBindingPreservesDefaultsPermissionAndPanicGuard(t *testing.T) {
	for _, name := range []string{"Bash", "custom_exec"} {
		t.Run(name, func(t *testing.T) {
			localCalls := 0
			base := actool.Build(actool.Spec{Name: name, Description: "original", Concurrent: func(json.RawMessage) bool { return true }, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
				return permission.AskUser("requires approval")
			}, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
				localCalls++
				return actool.Text("local result"), nil
			}})
			schema := map[string]any{"type": "object", "properties": map[string]any{"limit": map[string]any{"type": "integer", "default": float64(7)}}}
			original := guardPanic(DecorateTool(base, "configured", schema))
			runtime := &decoratorRuntime{}
			remote := actool.BindExecutionRuntime([]actool.CoreTool{original}, runtime)[0]
			if remote.Description() != "configured" || !reflect.DeepEqual(remote.InputSchema(), schema) || !remote.IsConcurrencySafe(nil) {
				t.Fatal("binding lost tool metadata")
			}
			if !reflect.DeepEqual(remote.CheckPermissions(context.Background(), nil, permission.Context{}), base.CheckPermissions(context.Background(), nil, permission.Context{})) {
				t.Fatal("binding changed permission check")
			}
			result, err := remote.Call(context.Background(), json.RawMessage(`{}`), nil)
			if err != nil || result.Flatten() != "remote result" || localCalls != 0 || runtime.name != name {
				t.Fatalf("remote delegation: result=%s err=%v local=%d", result.Flatten(), err, localCalls)
			}
			var input map[string]int
			if err = json.Unmarshal(runtime.input, &input); err != nil || input["limit"] != 7 {
				t.Fatalf("remote default missing: %s %v", runtime.input, err)
			}
			_, err = remote.Call(context.Background(), json.RawMessage(`{"limit":11}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(runtime.input, &input); err != nil || input["limit"] != 11 {
				t.Fatalf("explicit argument replaced: %s", runtime.input)
			}
			runtime.panicCall = true
			result, err = remote.Call(context.Background(), json.RawMessage(`{}`), nil)
			if err != nil || !result.IsError || !strings.Contains(result.Flatten(), "remote transport panic") {
				t.Fatalf("panic guard bypassed: %s %v", result.Flatten(), err)
			}
			result, err = original.Call(context.Background(), nil, nil)
			if err != nil || result.Flatten() != "local result" || localCalls != 1 {
				t.Fatal("binding mutated original local tool")
			}
		})
	}
}
