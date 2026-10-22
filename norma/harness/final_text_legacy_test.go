package harness

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

func TestFinalTextLegacyPromptFilteringIsExactAndPreservesCurrentInput(t *testing.T) {
	blank := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "fixture"}}}
	answer := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("fixture answer")}}
	legacy := llm.UserText(legacyFinalTextOnlyPrompt)
	for _, tc := range []struct {
		name string
		msgs []llm.Message
		want []llm.Message
	}{
		{"old_recovery", []llm.Message{blank, legacy, answer, llm.UserText("next")}, []llm.Message{blank, answer, llm.UserText("next")}},
		{"failed_old_recovery", []llm.Message{blank, legacy, llm.UserText("next")}, []llm.Message{blank, llm.UserText("next")}},
		{"current_user_input", []llm.Message{blank, legacy}, []llm.Message{blank, legacy}},
		{"after_text", []llm.Message{answer, legacy, answer}, []llm.Message{answer, legacy, answer}},
		{"user_quotation", []llm.Message{blank, llm.UserText("quote: " + legacyFinalTextOnlyPrompt), answer}, []llm.Message{blank, llm.UserText("quote: " + legacyFinalTextOnlyPrompt), answer}},
		{"assistant_quotation", []llm.Message{blank, {Role: llm.RoleAssistant, Content: legacy.Content}, answer}, []llm.Message{blank, {Role: llm.RoleAssistant, Content: legacy.Content}, answer}},
		{"standalone", []llm.Message{legacy, answer}, []llm.Message{legacy, answer}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, _ := json.Marshal(tc.msgs)
			if got := withoutLegacyFinalTextPrompt(tc.msgs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%+v want=%+v", got, tc.want)
			}
			after, _ := json.Marshal(tc.msgs)
			if string(before) != string(after) {
				t.Fatal("raw input history changed")
			}
		})
	}
}
