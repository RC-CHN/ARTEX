package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/tool"
)

func TestOpenAIErrorFrameCannotDispatchPartialOrReplayCompletedTool(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%v", partial), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var calls, effects atomic.Int32
			ran := make(chan struct{}, 1)
			probe := tool.Build(tool.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage, *tool.ToolContext) (tool.Result, error) {
				effects.Add(1)
				select {
				case ran <- struct{}{}:
				default:
				}
				return tool.Text("synthetic effect complete"), nil
			}})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				args := "{}"
				if partial {
					args = `{"partial":`
				}
				encodedArgs, _ := json.Marshal(args)
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"probe-1\",\"function\":{\"name\":\"probe\",\"arguments\":%s}}]}}]}\n\n", encodedArgs)
				if !partial {
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"synthetic tool boundary\"}}]}\n\n")
					w.(http.Flusher).Flush()
					select {
					case <-ran:
					case <-r.Context().Done():
						return
					}
				}
				fmt.Fprint(w, "data: {\"error\":{\"message\":\"synthetic upstream failure\",\"code\":\"insufficient_quota\"},\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":5}}\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()
			provider, err := llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, MaxRetries: -1, EmptyResponseRetries: 3, EmptyResponseInterval: time.Nanosecond})
			if err != nil {
				t.Fatal(err)
			}
			var terminal *harness.Terminal
			toolUses := 0
			for ev, err := range harness.Query(ctx, harness.QueryInput{Tools: tool.NewRegistry(probe), PermissionMode: permission.ModeBypass, RequireFinalText: true}, harness.QueryDeps{CallModel: provider.Stream}) {
				if err != nil {
					t.Fatal(err)
				}
				if ev.ToolUse != nil {
					toolUses++
				}
				if ev.Terminal != nil {
					terminal = ev.Terminal
				}
			}
			wantEffects := int32(1)
			if partial {
				wantEffects = 0
			}
			if terminal == nil || terminal.Reason != harness.ReasonIncompleteResponse || terminal.SummaryRecoveryAttempted || calls.Load() != 1 || effects.Load() != wantEffects || toolUses != int(wantEffects) {
				t.Fatalf("terminal=%+v calls=%d effects=%d toolUses=%d", terminal, calls.Load(), effects.Load(), toolUses)
			}
			var incomplete *llm.IncompleteResponseError
			if !errors.As(terminal.Err, &incomplete) || incomplete.Reason != llm.IncompleteUpstreamError || terminal.Usage.InputTokens != 17 || terminal.Usage.OutputTokens != 5 {
				t.Fatalf("lost protocol failure or usage: %+v", terminal)
			}
			if partial && terminal.InterruptedToolInput == nil {
				t.Fatal("lost unsubmitted tool input")
			}
		})
	}
}
