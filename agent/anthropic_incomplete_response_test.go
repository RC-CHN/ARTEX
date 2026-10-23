package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/llmrec"
	"github.com/RC-CHN/ARTEX/norma/llm"
)

func anthropicIncompleteChatFixture(t *testing.T, nonstream bool, replies []incompleteChatReply) (llm.Provider, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var requests [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read synthetic Anthropic request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		n := len(requests)
		requests = append(requests, body)
		mu.Unlock()
		if r.URL.Path != "/v1/messages" || n >= len(replies) {
			t.Errorf("unexpected Anthropic request %d to %s", n+1, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply := replies[n]
		if reply.httpStatus != 0 {
			w.WriteHeader(reply.httpStatus)
			_, _ = fmt.Fprint(w, `{"error":{"type":"api_error","message":"synthetic retry failure"}}`)
			return
		}
		usage := map[string]int{"input_tokens": 11, "output_tokens": 3, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 1}
		var blocks []any
		if reply.thinking != "" {
			blocks = append(blocks, map[string]any{"type": "thinking", "thinking": reply.thinking, "signature": "synthetic-signature"})
		}
		if reply.tool != "" {
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": reply.id, "name": reply.tool, "input": json.RawMessage(reply.arguments)})
		}
		if reply.text != "" {
			blocks = append(blocks, map[string]any{"type": "text", "text": reply.text})
		}
		if nonstream {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "synthetic-message", "type": "message", "role": "assistant", "content": blocks, "stop_reason": reply.finish, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame := func(kind string, value map[string]any) {
			value["type"] = kind
			raw, _ := json.Marshal(value)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, raw)
		}
		// The API repeats input/cache counters in message_delta; they must not
		// double count, and message_start's placeholder output is not usage.
		frame("message_start", map[string]any{"message": map[string]any{"id": "synthetic-message", "type": "message", "role": "assistant", "content": []any{}, "usage": map[string]int{"input_tokens": 11, "output_tokens": 1, "cache_read_input_tokens": 2, "cache_creation_input_tokens": 1}}})
		for index, rawBlock := range blocks {
			block := rawBlock.(map[string]any)
			kind := block["type"].(string)
			start := map[string]any{"type": kind}
			delta := map[string]any{}
			switch kind {
			case "thinking":
				start["thinking"] = ""
				delta["type"], delta["thinking"] = "thinking_delta", block["thinking"]
			case "tool_use":
				start["id"], start["name"], start["input"] = block["id"], block["name"], map[string]any{}
				delta["type"], delta["partial_json"] = "input_json_delta", reply.arguments
			case "text":
				start["text"] = ""
				delta["type"], delta["text"] = "text_delta", block["text"]
			}
			frame("content_block_start", map[string]any{"index": index, "content_block": start})
			frame("content_block_delta", map[string]any{"index": index, "delta": delta})
			if kind == "thinking" {
				frame("content_block_delta", map[string]any{"index": index, "delta": map[string]any{"type": "signature_delta", "signature": "synthetic-signature"}})
			}
			frame("content_block_stop", map[string]any{"index": index})
		}
		frame("message_delta", map[string]any{"delta": map[string]any{"stop_reason": reply.finish}, "usage": usage})
		frame("message_stop", map[string]any{})
	}))
	t.Cleanup(srv.Close)
	provider, err := llm.NewProvider(llm.Config{Format: llm.FormatAnthropic, BaseURL: srv.URL, Model: "synthetic-anthropic", ThinkingType: "enabled", MaxRetries: -1, EmptyResponseRetries: 1, EmptyResponseInterval: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	return provider, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), requests...)
	}
}

func TestAutoChatAnthropicIncompleteResponseDogfood(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	for _, nonstream := range []bool{false, true} {
		for _, scenario := range []string{"retry_then_write_read", "exhaust_after_write", "http_failure_after_write"} {
			t.Run(fmt.Sprintf("nonstream=%v/%s", nonstream, scenario), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "RESOURCE.md")
				const document = "# Synthetic Anthropic resource\n\nLocal fixture with preserved tool effects.\n"
				write := incompleteChatReply{tool: "Write", id: "anthropic-write", arguments: incompleteChatJSON(map[string]any{"file_path": path, "content": document}), finish: "tool_use"}
				thinking := incompleteChatReply{thinking: "Synthetic reasoning without final text or a tool.", finish: "end_turn"}
				replies := []incompleteChatReply{write, thinking, thinking}
				wantReason, wantText := "empty_output", ""
				retryIndex, usageAttempts, logicalCalls := 1, 3, 2
				if scenario == "retry_then_write_read" {
					replies = []incompleteChatReply{thinking, write,
						{tool: "Read", id: "anthropic-read", arguments: incompleteChatJSON(map[string]any{"file_path": path}), finish: "tool_use"},
						{text: "Synthetic Anthropic resource written and verified.", finish: "end_turn"},
					}
					wantReason, wantText = "", "Synthetic Anthropic resource written and verified."
					retryIndex, usageAttempts, logicalCalls = 0, 4, 3
				} else if scenario == "http_failure_after_write" {
					replies[2] = incompleteChatReply{httpStatus: http.StatusBadGateway}
					wantReason, usageAttempts = "retry_request_error", 2
				}
				provider, requests := anthropicIncompleteChatFixture(t, nonstream, replies)
				usageKey := "anthropic-incomplete-" + filepath.Base(filepath.Dir(dir)) + "-" + filepath.Base(dir)
				metered := llmrec.Wrap(provider, d, "synthetic-anthropic", usageKey, "enabled", "", func() bool { return false })
				chat := NewChatAgent(metered, "synthetic-anthropic", dir, nil, 0)
				chat.SetNonStreaming(func() bool { return nonstream })
				ctx, cancel := context.WithTimeout(llmrec.WithTaskID(t.Context(), usageKey), 10*time.Second)
				defer cancel()
				var records []db.Activity
				text, err := chat.Chat(ctx, "auto", "conv-synthetic-anthropic", "Write and verify the local synthetic resource.", 0, 0, false, func(a db.Activity) { records = append(records, a) })
				if wantReason == "" {
					if err != nil || text != wantText {
						t.Fatalf("successful Anthropic chat text=%q err=%v", text, err)
					}
				} else {
					var incomplete *llm.IncompleteResponseError
					if !errors.As(err, &incomplete) || string(incomplete.Reason) != wantReason || text != "" {
						t.Fatalf("Anthropic failure text=%q error=%T %v; want %s", text, err, err, wantReason)
					}
				}
				calls := requests()
				if len(calls) != len(replies) || !bytes.Equal(calls[retryIndex], calls[retryIndex+1]) {
					t.Fatalf("Anthropic retry changed request or replayed work: calls=%d want=%d", len(calls), len(replies))
				}
				for i, raw := range calls {
					var request struct {
						Tools    []struct{ Name string } `json:"tools"`
						Messages []llm.Message           `json:"messages"`
					}
					if err := json.Unmarshal(raw, &request); err != nil {
						t.Fatal(err)
					}
					names := map[string]bool{}
					for _, tool := range request.Tools {
						names[tool.Name] = true
					}
					if !names["Write"] || !names["Read"] || !names["Bash"] {
						t.Fatalf("Anthropic request %d hid tools: %v", i+1, names)
					}
					for _, forbidden := range []string{"禁止调用任何工具", "禁止继续执行工作", "上一轮没有向用户给出文字答复", "上一轮没有最终文字答复"} {
						if bytes.Contains(raw, []byte(forbidden)) {
							t.Fatalf("Anthropic request %d injected recovery restrictions", i+1)
						}
					}
					if scenario == "retry_then_write_read" && i == 3 {
						if err := mainToolReceipt(llm.CompletionRequest{Messages: request.Messages}, "anthropic-read", "Local fixture"); err != nil {
							t.Fatal(err)
						}
					}
				}
				if got, err := os.ReadFile(path); err != nil || string(got) != document {
					t.Fatalf("real Anthropic Write lost effect: %q %v", got, err)
				}
				writes := 0
				for _, rec := range records {
					if rec.Kind == "tool_result" && rec.ToolUseID == "anthropic-write" {
						writes++
						if rec.IsError {
							t.Fatalf("real Anthropic Write failed: %s", rec.Detail)
						}
					}
				}
				if writes != 1 {
					t.Fatalf("Write executed %d times", writes)
				}
				result := incompleteChatResult(t, records)
				var meta map[string]any
				if err := json.Unmarshal(result.Metadata, &meta); err != nil {
					t.Fatal(err)
				}
				wantTerminal := "completed"
				if wantReason != "" {
					wantTerminal = "incomplete_response"
				}
				if result.IsError != (wantReason != "") || meta["terminal_reason"] != wantTerminal || meta["summary_recovery_attempted"] != false {
					t.Fatalf("Anthropic terminal=%+v error=%v", meta, result.IsError)
				}
				if wantReason != "" && meta["response_failure"] != wantReason {
					t.Fatalf("Anthropic reason lost: %+v", meta)
				}
				if result.InputTokens == nil || *result.InputTokens != usageAttempts*14 || result.OutputTokens == nil || *result.OutputTokens != usageAttempts*3 || result.CacheReadTokens == nil || *result.CacheReadTokens != usageAttempts*2 || result.CacheWriteTokens == nil || *result.CacheWriteTokens != usageAttempts {
					t.Fatalf("Anthropic terminal lost or duplicated usage: %+v", result)
				}
				var rows, input, output, cacheRead, cacheWrite int
				if err := d.QueryRow(`SELECT count(*), COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0), COALESCE(sum(cache_read),0), COALESCE(sum(cache_write),0) FROM llm_usage WHERE task_id=$1`, usageKey).Scan(&rows, &input, &output, &cacheRead, &cacheWrite); err != nil {
					t.Fatal(err)
				}
				if rows != logicalCalls || input != usageAttempts*14 || output != usageAttempts*3 || cacheRead != usageAttempts*2 || cacheWrite != usageAttempts {
					t.Fatalf("Anthropic ledger rows=%d input=%d output=%d cache=%d/%d", rows, input, output, cacheRead, cacheWrite)
				}
				if strings.Contains(result.Detail, "仅文字补答") {
					t.Fatal("Anthropic recovery imposed a summary-only restriction")
				}
			})
		}
	}
}
