package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func assertOpenAIToolOrder(t *testing.T, msgs []oaMessage) {
	t.Helper()
	pending := map[string]bool{}
	for i, m := range msgs {
		if m.Role == "tool" {
			if m.ToolCallID == "" || !pending[m.ToolCallID] {
				t.Fatalf("message %d has an unexpected tool result %q", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
			continue
		}
		if len(pending) > 0 {
			t.Fatalf("message %d interrupts unanswered tool calls %v", i, pending)
		}
		for _, c := range m.ToolCalls {
			if c.ID == "" || pending[c.ID] {
				t.Fatalf("invalid or duplicate call id %q", c.ID)
			}
			pending[c.ID] = true
		}
	}
	if len(pending) > 0 {
		t.Fatalf("unanswered tail: %v", pending)
	}
}

func TestSanitizeRequiresResultsInTheSameExchange(t *testing.T) {
	call := oaMessage{Role: "assistant", ToolCalls: []oaToolCall{oaCall("a")}}
	result := oaMessage{Role: "tool", ToolCallID: "a", Content: "original result"}
	for _, tc := range []struct {
		name    string
		msgs    []oaMessage
		results int
	}{
		{"summary between call and result", []oaMessage{call, {Role: "user", Content: "summary"}, result}, 0},
		{"result precedes call", []oaMessage{result, call}, 0},
		{"assistant interrupts", []oaMessage{call, {Role: "assistant", Content: "interrupted"}, result}, 0},
		{"old result cannot answer reused id", []oaMessage{call, result, call}, 1},
		{"duplicate result", []oaMessage{call, result, result}, 1},
		{"duplicate call", []oaMessage{{Role: "assistant", ToolCalls: []oaToolCall{oaCall("a"), oaCall("a")}}, result}, 1},
		{"empty ids", []oaMessage{{Role: "assistant", ToolCalls: []oaToolCall{oaCall("")}}, {Role: "tool"}}, 0},
		{"parallel reverse completion", []oaMessage{{Role: "assistant", ToolCalls: []oaToolCall{oaCall("a"), oaCall("b")}}, {Role: "tool", ToolCallID: "b", Content: "second"}, result}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeOpenAIMessages(tc.msgs, true)
			assertOpenAIToolOrder(t, got)
			if countRole(got, "tool") != tc.results {
				t.Fatalf("result count: %d, want %d", countRole(got, "tool"), tc.results)
			}
			if again := sanitizeOpenAIMessages(got, true); !reflect.DeepEqual(got, again) {
				t.Fatal("send guard is not idempotent")
			}
		})
	}
}

func TestBuildBodyPreservesEmptyToolResultContent(t *testing.T) {
	p := &openaiProvider{cfg: Config{Model: "fixture"}}
	raw, err := p.buildBody(CompletionRequest{Messages: []Message{toolUseMsg("empty"), {Role: RoleUser, Content: []ContentBlock{{Type: BlockToolResult, ToolUseID: "empty"}}}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	for _, m := range body.Messages {
		if m["role"] == "tool" {
			if v, ok := m["content"]; !ok || v != "" {
				t.Fatal("empty tool result must include an empty content field")
			}
			return
		}
	}
	t.Fatal("empty result was discarded")
}

func oaCall(id string) oaToolCall {
	tc := oaToolCall{ID: id, Type: "function"}
	tc.Function.Name = "Read"
	tc.Function.Arguments = "{}"
	return tc
}

func countRole(msgs []oaMessage, role string) int {
	n := 0
	for _, m := range msgs {
		if m.Role == role {
			n++
		}
	}
	return n
}

// An assistant message carrying a tool_call with no matching role:tool response
// is what a half-covered compaction leaves behind; the unanswered call must go
// so the provider does not 400 with "tool_calls must be followed by tool
// messages".
func TestSanitizeDropsUnansweredToolCall(t *testing.T) {
	msgs := []oaMessage{
		{Role: "assistant", ToolCalls: []oaToolCall{oaCall("a"), oaCall("b")}},
		{Role: "tool", ToolCallID: "a", Content: "ok"},
	}
	got := sanitizeOpenAIMessages(msgs, false)
	if len(got[0].ToolCalls) != 1 || got[0].ToolCalls[0].ID != "a" {
		t.Fatalf("assistant tool_calls = %+v, want only the answered call a", got[0].ToolCalls)
	}
	if countRole(got, "tool") != 1 {
		t.Fatalf("tool messages = %d, want 1", countRole(got, "tool"))
	}
}

// A role:tool message whose call is gone (its assistant turn was compressed
// away) is an orphan a strict provider also rejects.
func TestSanitizeDropsOrphanToolResult(t *testing.T) {
	msgs := []oaMessage{
		{Role: "assistant", Content: "hi"},
		{Role: "tool", ToolCallID: "ghost", Content: "stale"},
	}
	got := sanitizeOpenAIMessages(msgs, false)
	if countRole(got, "tool") != 0 {
		t.Fatalf("orphan tool result survived: %+v", got)
	}
}

// In thinking mode an assistant turn replaying tool_calls must carry
// reasoning_content; when it was lost upstream the guard fills a placeholder
// rather than let the request fail.
func TestSanitizeFillsReasoningInThinkingMode(t *testing.T) {
	msgs := []oaMessage{
		{Role: "assistant", ToolCalls: []oaToolCall{oaCall("a")}},
		{Role: "tool", ToolCallID: "a", Content: "ok"},
	}
	got := sanitizeOpenAIMessages(msgs, true)
	if got[0].ReasoningContent == "" {
		t.Fatalf("thinking-mode assistant with tool_calls must carry reasoning_content")
	}
}

// The placeholder is a thinking-mode-only concession: without thinking, a
// content-less tool_calls turn is valid and must be left as it is.
func TestSanitizeNoReasoningFillWhenNotThinking(t *testing.T) {
	msgs := []oaMessage{
		{Role: "assistant", ToolCalls: []oaToolCall{oaCall("a")}},
		{Role: "tool", ToolCallID: "a", Content: "ok"},
	}
	got := sanitizeOpenAIMessages(msgs, false)
	if got[0].ReasoningContent != "" {
		t.Fatalf("reasoning_content = %q, want empty when not in thinking mode", got[0].ReasoningContent)
	}
}

// An assistant message emptied by dropping its only (unanswered) tool_call would
// serialize to a bare {"role":"assistant"} and 400; it must gain placeholder
// content instead.
func TestSanitizeEmptiedAssistantGainsContent(t *testing.T) {
	msgs := []oaMessage{
		{Role: "assistant", ToolCalls: []oaToolCall{oaCall("a")}}, // no tool response
	}
	got := sanitizeOpenAIMessages(msgs, false)
	if len(got[0].ToolCalls) != 0 {
		t.Fatalf("unanswered call survived: %+v", got[0].ToolCalls)
	}
	if got[0].Content == "" {
		t.Fatalf("emptied assistant has no content — would serialize to a bare assistant message")
	}
}

// A well-formed transcript must pass through untouched: pairs intact, real
// reasoning_content preserved, nothing added or removed.
func TestSanitizeLeavesValidTranscriptIntact(t *testing.T) {
	msgs := []oaMessage{
		{Role: "user", Content: "task"},
		{Role: "assistant", ReasoningContent: "real thinking", ToolCalls: []oaToolCall{oaCall("a")}},
		{Role: "tool", ToolCallID: "a", Content: "ok"},
		{Role: "assistant", Content: "done"},
	}
	got := sanitizeOpenAIMessages(msgs, true)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4 (nothing dropped): %+v", len(got), got)
	}
	if got[1].ReasoningContent != "real thinking" {
		t.Fatalf("real reasoning_content was clobbered: %q", got[1].ReasoningContent)
	}
	if len(got[1].ToolCalls) != 1 {
		t.Fatalf("valid tool_call was dropped: %+v", got[1].ToolCalls)
	}
}

// End-to-end: the two failures from issue #174 in one transcript — an unanswered
// tool_call and a thinking turn that lost its reasoning — both repaired by the
// guard reached through buildBody.
func TestBuildBodyAppliesSendGuard(t *testing.T) {
	p := &openaiProvider{cfg: Config{Model: "glm-4.6", ThinkingType: "enabled"}}
	req := CompletionRequest{
		Messages: []Message{
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: BlockToolUse, ID: "x", Name: "Read", Input: []byte(`{}`)},
			}},
			// No tool_result for "x" — the half-covered case.
			UserText("continue"),
		},
	}
	raw, err := p.buildBody(req, false)
	if err != nil {
		t.Fatalf("buildBody: %v", err)
	}
	s := string(raw)
	// The unanswered tool_call must not reach the wire.
	if strings.Contains(s, `"tool_calls"`) {
		t.Fatalf("unanswered tool_call was sent: %s", s)
	}
}
