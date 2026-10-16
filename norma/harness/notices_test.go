package harness

import (
	"context"
	"iter"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

func TestHostNoticeArrivingDuringModelAppendsAfterResponse(t *testing.T) {
	calls, delivered := 0, false
	ctx := WithTurnNotices(t.Context(), func(context.Context, []llm.Message) string {
		if calls == 0 || delivered {
			return ""
		}
		delivered = true
		return "RESOURCE_CHANGED"
	})
	model := func(_ context.Context, request llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		calls++
		if calls == 2 {
			if !strings.Contains(request.Messages[len(request.Messages)-1].Text(), "RESOURCE_CHANGED") {
				t.Fatal("notification did not reach next model call")
			}
		}
		return alwaysModel(textTurn("done"))(t.Context(), request)
	}
	term := drainWithCtx(t, ctx, QueryInput{Messages: []llm.Message{llm.UserText("start")}}, QueryDeps{CallModel: model})
	if calls != 2 || term.Reason != ReasonCompleted || len(term.Messages) != 4 {
		t.Fatalf("calls=%d terminal=%+v", calls, term)
	}
	if term.Messages[1].Role != llm.RoleAssistant || term.Messages[2].Role != llm.RoleUser {
		t.Fatal("notification rewrote earlier response")
	}
}
