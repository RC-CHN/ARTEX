package noaadapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/noa"
)

func assertAdjacentExchanges(t *testing.T, msgs []llm.Message) {
	t.Helper()
	pending := map[string]bool{}
	for i, m := range msgs {
		if m.Role == llm.RoleAssistant && len(pending) > 0 {
			t.Fatalf("assistant message %d interrupts pending results %v", i, pending)
		}
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult {
				if !pending[b.ToolUseID] {
					t.Fatalf("unexpected result %q at message %d", b.ToolUseID, i)
				}
				delete(pending, b.ToolUseID)
			}
		}
		for _, b := range m.Content {
			if m.Role == llm.RoleUser && b.Type != llm.BlockToolResult && len(pending) > 0 {
				t.Fatalf("user message %d interrupts pending results %v", i, pending)
			}
			if b.Type == llm.BlockToolUse {
				pending[b.ID] = true
			}
		}
	}
	if len(pending) != 0 {
		t.Fatalf("unanswered calls: %v", pending)
	}
}

func TestReassembleDefersSummariesAcrossParallelExchanges(t *testing.T) {
	msgs := []llm.Message{
		userText("fixture"),
		assistantWith(llm.ContentBlock{Type: llm.BlockThinking, Thinking: "original reasoning", Signature: "original signature"}, llm.TextBlock("read two fixtures"), toolUse("a", "Read", `{}`), toolUse("b", "Read", `{}`)),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{toolResult("a", "first original result").Content[0], toolResult("b", "second original result").Content[0]}},
		assistantWith(llm.TextBlock("finished")),
	}
	original, _ := json.Marshal(msgs)
	for _, partial := range []bool{false, true} {
		cores, sc := Project(msgs)
		if partial {
			var kept []noa.CoreMessage
			for _, c := range cores {
				if c.ToolCallID != "b" {
					kept = append(kept, c)
				}
			}
			cores = kept
		}
		for at := 0; at <= len(cores); at++ {
			t.Run(fmt.Sprintf("partial=%t/position=%d", partial, at), func(t *testing.T) {
				view := append([]noa.CoreMessage{}, cores[:at]...)
				for n := 0; n < 2; n++ {
					view = append(view, noa.CoreMessage{ID: noa.SummaryIDPrefix + fmt.Sprint(n), Role: noa.RoleUser, ContentType: noa.CTText, Text: fmt.Sprintf("fixture summary %d", n)})
				}
				view = append(view, cores[at:]...)
				before, _ := json.Marshal(view)
				ordered := deferSynthetic(view, sc)
				if !reflect.DeepEqual(ordered, deferSynthetic(ordered, sc)) {
					t.Fatal("summary placement is not idempotent")
				}
				out := Reassemble(view, msgs, sc, ReassembleOptions{})
				assertAdjacentExchanges(t, out)
				uses, results, summaries := 0, 0, 0
				for _, m := range out {
					for _, b := range m.Content {
						switch b.Type {
						case llm.BlockToolUse:
							uses++
						case llm.BlockToolResult:
							results++
							if b.ToolUseID == "a" && b.Content[0].Text != "first original result" {
								t.Fatal("original result changed")
							}
						case llm.BlockThinking:
							if b.Thinking != "original reasoning" || b.Signature != "original signature" {
								t.Fatal("thinking changed")
							}
						case llm.BlockText:
							if b.Text == fmt.Sprintf("fixture summary %d", summaries) {
								summaries++
							}
						}
					}
				}
				want := 2
				if partial {
					want = 1
				}
				if uses != want || results != want || summaries != 2 {
					t.Fatalf("calls/results/summaries=%d/%d/%d", uses, results, summaries)
				}
				after, _ := json.Marshal(view)
				source, _ := json.Marshal(msgs)
				if string(before) != string(after) || string(original) != string(source) {
					t.Fatal("source history was mutated")
				}
			})
		}
	}
}

func TestCompressReplayKeepsSurvivingParallelToolExchange(t *testing.T) {
	sess := simSession(t, 200000)
	// Parallel tools can finish in reverse order. Compress only the completed
	// inner branch; the outer call and its real result must stay visible.
	msgs := []llm.Message{userText("fixture"), assistantWith(toolUse("a", "Read", `{}`), toolUse("b", "Read", `{}`)),
		{Role: llm.RoleUser, Content: []llm.ContentBlock{toolResult("b", strings.Repeat("compressible output. ", 1000)).Content[0], toolResult("a", "retained original").Content[0]}}}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("later-%d", i)
		msgs = append(msgs, assistantWith(toolUse(id, "Read", `{}`)), toolResult(id, "fixture output"))
	}
	sess.View(msgs)
	start := refOfCore(t, sess, msgs, func(c noa.CoreMessage) bool { return c.ToolCallID == "b" && c.ContentType == noa.CTToolCall })
	end := refOfCore(t, sess, msgs, func(c noa.CoreMessage) bool { return c.ToolCallID == "b" && c.ContentType == noa.CTToolResult })
	panel := compressRange(t, sess, "compress", start, end)
	if noa.PanelBlockCount(panel) == 0 {
		t.Fatalf("compression did not create a block: %s", panel)
	}
	msgs = append(msgs, assistantWith(toolUse("compress", noa.CompressToolName, `{}`)), toolResult("compress", panel))
	assertView := func(s *Session) {
		t.Helper()
		out := s.View(msgs)
		assertAdjacentExchanges(t, out)
		calls, results := 0, 0
		for _, m := range out {
			for _, b := range m.Content {
				if b.Type == llm.BlockToolUse && b.ID == "a" {
					calls++
				}
				if b.Type == llm.BlockToolResult && b.ToolUseID == "a" {
					results++
				}
			}
		}
		if calls != 1 || results != 1 {
			t.Fatalf("uncompressed exchange was discarded: %d calls, %d results", calls, results)
		}
		assertCompressedViewOnWire(t, out)
	}
	assertView(sess)
	restored, err := newSession(Options{ArchiveBaseDir: filepath.Dir(sess.root), SessionID: filepath.Base(sess.root), ModelContextLimit: 200000})
	if err != nil {
		t.Fatal(err)
	}
	assertView(restored)
}

// Exercise the real provider encoder too: a send guard that discards the
// surviving exchange would silence the gateway error while losing its output.
func assertCompressedViewOnWire(t *testing.T, view []llm.Message) {
	t.Helper()
	type wireMessage struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
		ToolCalls  []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	type wireRequest struct {
		Stream   bool          `json:"stream"`
		Messages []wireMessage `json:"messages"`
	}
	requests := make(chan wireRequest, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req wireRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		requests <- req
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		} else {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}
	}))
	defer srv.Close()
	p, err := llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "fixture", Model: "fixture", MaxRetries: -1, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	req := llm.CompletionRequest{Messages: view, MaxTokens: 100}
	if _, _, _, err := p.Complete(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	for _, err := range p.Stream(t.Context(), req) {
		if err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		wire := <-requests
		pending := map[string]bool{}
		retained := 0
		for _, m := range wire.Messages {
			if m.Role == "tool" {
				if !pending[m.ToolCallID] {
					t.Fatalf("unexpected wire result %s", m.ToolCallID)
				}
				delete(pending, m.ToolCallID)
				if m.ToolCallID == "a" {
					retained++
					if !strings.Contains(m.Content, "retained original") {
						t.Fatal("original output lost on wire")
					}
				}
				continue
			}
			if len(pending) > 0 {
				t.Fatalf("%s interrupts wire results %v", m.Role, pending)
			}
			for _, c := range m.ToolCalls {
				pending[c.ID] = true
			}
		}
		if retained != 1 || len(pending) > 0 {
			t.Fatalf("wire retained=%d, unanswered=%v", retained, pending)
		}
	}
}
