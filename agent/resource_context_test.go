package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/agentcore"
	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/resourcelib"
)

func TestResourceSnapshotAppendsOnceWithoutChangingSystem(t *testing.T) {
	snapshot := `[{"resource_id":1,"version_id":7,"name":"fixture"}]`
	ctx := WithResourceSnapshot(t.Context(), snapshot)
	input := resourceSnapshotInput(ctx, nil, "start")
	if !strings.Contains(input, snapshot) || !strings.HasSuffix(input, "start") {
		t.Fatal("missing startup snapshot")
	}
	messages := []llm.Message{llm.UserText(input), llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("working")}}}
	if got := resourceSnapshotInput(ctx, messages, "continue"); got != "continue" {
		t.Fatal("unchanged snapshot duplicated and invalidated history prefix")
	}
	updated := WithResourceSnapshot(t.Context(), `[{"resource_id":1,"version_id":8}]`)
	if got := resourceSnapshotInput(updated, messages, "continue"); !strings.Contains(got, `"version_id":8`) {
		t.Fatal("explicit binding change was not disclosed")
	}
	if got := resourceSnapshotInput(WithResourceSnapshot(t.Context(), ""), nil, "start"); got != "start" {
		t.Fatal("empty selection altered prompt")
	}
	system, boundary := deferredSystem("static instructions", DeferredInfo{})
	if len(system) != 1 || system[0] != "static instructions" || boundary != 1 {
		t.Fatal("resource snapshot changed system prefix")
	}
}

func TestResourceNoticeDeliveryResumeAndQueryFailure(t *testing.T) {
	for _, mode := range []string{"new", "resume", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			calls, notices := 0, 0
			ctx := WithResourceUpdates(t.Context(), func(context.Context) ([]resourcelib.UpdateState, error) {
				calls++
				if mode == "unavailable" {
					return nil, errors.New("database unavailable")
				}
				return []resourcelib.UpdateState{{ResourceID: 1, PinnedID: 2, LatestID: 3, Enabled: true}}, nil
			})
			provider := captureUsageProvider{stream: func(_ context.Context, yield func(llm.StreamEvent, error) bool) {
				for _, event := range []llm.StreamEvent{{Type: llm.SETextDelta, Text: "complete"}, {Type: llm.SEMessageDelta, StopReason: "end_turn"}, {Type: llm.SEMessageStop}} {
					if !yield(event, nil) {
						return
					}
				}
			}}
			session := agentcore.NewSession(agentcore.Options{Provider: provider})
			defer session.Close()
			input := "start"
			if mode == "resume" {
				input += " [resource-update:1:2:3:true]"
			}
			_, reason, err := captureFinalSession(ctx, session, input, func(a db.Activity) {
				if a.Kind == "resource_update" {
					notices++
				}
			})
			if err != nil || reason != harness.ReasonCompleted || calls != 1 {
				t.Fatalf("run=%s %v calls=%d", reason, err, calls)
			}
			if (mode == "new" && notices != 1) || (mode != "new" && notices != 0) {
				t.Fatalf("notifications=%d", notices)
			}
		})
	}
}
