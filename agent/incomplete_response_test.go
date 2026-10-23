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

type incompleteChatReply struct {
	text, thinking, tool, id, arguments, finish string
	httpStatus                                  int
	omitDone                                    bool
}

// Exercise the public HTTP provider and actual tools, including retry boundaries.
func incompleteChatFixture(t *testing.T, nonstream bool, replies []incompleteChatReply, retries int) (llm.Provider, func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var requests [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read synthetic request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		n := len(requests)
		requests = append(requests, body)
		mu.Unlock()
		if n >= len(replies) {
			t.Errorf("unexpected request %d; recovery must stay bounded", n+1)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply := replies[n]
		if reply.httpStatus != 0 {
			w.WriteHeader(reply.httpStatus)
			_, _ = fmt.Fprint(w, `{"error":"synthetic retry request failure"}`)
			return
		}
		message := map[string]any{"role": "assistant"}
		if reply.text != "" {
			message["content"] = reply.text
		}
		if reply.thinking != "" {
			message["reasoning_content"] = reply.thinking
		}
		if reply.tool != "" {
			message["tool_calls"] = []any{map[string]any{"index": 0, "id": reply.id, "type": "function", "function": map[string]any{"name": reply.tool, "arguments": reply.arguments}}}
		}
		usage := map[string]int{"prompt_tokens": 11, "completion_tokens": 3}
		if nonstream {
			choice := map[string]any{"message": message}
			if reply.finish != "" {
				choice["finish_reason"] = reply.finish
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{choice}, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frame := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		frame(map[string]any{"choices": []any{map[string]any{"delta": message}}})
		if reply.finish != "" {
			frame(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": reply.finish}}})
		}
		frame(map[string]any{"choices": []any{}, "usage": usage})
		if !reply.omitDone {
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		}
	}))
	t.Cleanup(srv.Close)
	provider, err := llm.NewProvider(llm.Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, Model: "synthetic", MaxRetries: -1, EmptyResponseRetries: retries, EmptyResponseInterval: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	return provider, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), requests...)
	}
}

func incompleteChatJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func assertIncompleteChatTools(t *testing.T, requests [][]byte) {
	t.Helper()
	for i, raw := range requests {
		var request struct {
			Tools []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, item := range request.Tools {
			names[item.Function.Name] = true
		}
		if !names["Write"] || !names["Read"] || !names["Bash"] {
			t.Fatalf("request %d lost normal tools: %v", i+1, names)
		}
		for _, forbidden := range []string{"禁止调用任何工具", "禁止继续执行工作", "上一轮没有向用户给出文字答复", "上一轮没有最终文字答复"} {
			if bytes.Contains(raw, []byte(forbidden)) {
				t.Fatalf("request %d injected a recovery restriction", i+1)
			}
		}
	}
}

func incompleteChatResult(t *testing.T, records []db.Activity) db.Activity {
	t.Helper()
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Kind == "result" {
			return records[i]
		}
	}
	t.Fatal("missing terminal receipt")
	return db.Activity{}
}

func TestAutoChatIncompleteResponseRetryPreservesWorkAndUsage(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	for _, nonstream := range []bool{false, true} {
		t.Run(fmt.Sprintf("nonstream=%v", nonstream), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "RESOURCE.md")
			const document = "# Synthetic resource\n\nLocal fixture, written once and verified.\n"
			provider, requests := incompleteChatFixture(t, nonstream, []incompleteChatReply{
				{thinking: "Synthetic thinking without a final answer."},
				{tool: "Write", id: "synthetic-write", arguments: incompleteChatJSON(map[string]any{"file_path": path, "content": document}), finish: "tool_calls"},
				{tool: "Read", id: "synthetic-read", arguments: incompleteChatJSON(map[string]any{"file_path": path}), finish: "tool_calls"},
				{text: "Synthetic resource written and verified.", finish: "stop"},
			}, 1)
			usageKey := "incomplete-chat-" + filepath.Base(filepath.Dir(dir)) + "-" + filepath.Base(dir)
			metered := llmrec.Wrap(provider, d, "synthetic", usageKey, "", "", func() bool { return false })
			chat := NewChatAgent(metered, "synthetic", dir, nil, 0)
			chat.SetNonStreaming(func() bool { return nonstream })
			ctx, cancel := context.WithTimeout(llmrec.WithTaskID(t.Context(), usageKey), 10*time.Second)
			defer cancel()
			var records []db.Activity
			text, err := chat.Chat(ctx, "auto", "conv-synthetic-incomplete", "Write and verify the local synthetic resource.", 0, 0, false, func(a db.Activity) { records = append(records, a) })
			if err != nil || text != "Synthetic resource written and verified." {
				t.Fatalf("auto chat text=%q err=%v", text, err)
			}
			calls := requests()
			if len(calls) != 4 || !bytes.Equal(calls[0], calls[1]) {
				t.Fatalf("retry changed request or replayed work: calls=%d", len(calls))
			}
			assertIncompleteChatTools(t, calls)
			for index, id := range map[int]string{2: "synthetic-write", 3: "synthetic-read"} {
				var request struct {
					Messages []struct {
						Role, Content string
						ToolCallID    string `json:"tool_call_id"`
					} `json:"messages"`
				}
				if err := json.Unmarshal(calls[index], &request); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, message := range request.Messages {
					if message.Role == "tool" && message.ToolCallID == id {
						found = id != "synthetic-read" || strings.Contains(message.Content, "Local fixture")
					}
				}
				if !found {
					t.Fatalf("request %d lacks actual %s receipt", index+1, id)
				}
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != document {
				t.Fatalf("file effect=%q err=%v", got, err)
			}
			writes := 0
			for _, rec := range records {
				if rec.Kind == "tool_result" && rec.ToolUseID == "synthetic-write" {
					writes++
					if rec.IsError {
						t.Fatalf("Write failed: %s", rec.Detail)
					}
				}
			}
			if writes != 1 {
				t.Fatalf("Write receipts=%d; must execute once", writes)
			}
			result := incompleteChatResult(t, records)
			var meta map[string]any
			if err := json.Unmarshal(result.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			if result.IsError || meta["terminal_reason"] != "completed" || meta["summary_recovery_attempted"] != false {
				t.Fatalf("recovered chat terminal=%+v error=%v", meta, result.IsError)
			}
			if result.InputTokens == nil || *result.InputTokens != 44 || result.OutputTokens == nil || *result.OutputTokens != 12 {
				t.Fatalf("terminal lost retried usage: %+v", result)
			}
			var rows, input, output int
			if err := d.QueryRow(`SELECT count(*), COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0) FROM llm_usage WHERE task_id=$1`, usageKey).Scan(&rows, &input, &output); err != nil {
				t.Fatal(err)
			}
			if rows != 3 || input != 44 || output != 12 {
				t.Fatalf("usage rows=%d input=%d output=%d; expected 3 model calls including all 4 HTTP attempts", rows, input, output)
			}
		})
	}
}

func TestAutoChatIncompleteResponseStopsWithoutExecutingPartialTools(t *testing.T) {
	for _, nonstream := range []bool{false, true} {
		scenarios := []string{"thinking_done", "empty_output", "invalid_tool_input"}
		if !nonstream {
			scenarios = append(scenarios, "missing_terminal", "tool_without_terminal")
		}
		for _, scenario := range scenarios {
			t.Run(fmt.Sprintf("nonstream=%v/%s", nonstream, scenario), func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "must-not-exist.txt")
				reply := incompleteChatReply{thinking: "Synthetic incomplete reasoning."}
				wantCalls := 2
				wantReason := "empty_output"
				if scenario == "empty_output" {
					reply.finish = "stop"
				}
				if scenario == "invalid_tool_input" {
					reply = incompleteChatReply{tool: "Write", id: "synthetic-partial-write", arguments: `{"file_path":` + incompleteChatJSON(path) + `,"content":`, finish: "tool_calls"}
					wantCalls = 1
					wantReason = "invalid_tool_input"
				}
				if scenario == "missing_terminal" || scenario == "tool_without_terminal" {
					reply.omitDone = true
					wantReason = "missing_terminal"
					if scenario == "tool_without_terminal" {
						wantCalls = 1
						reply.tool, reply.id = "Write", "synthetic-unfinished-write"
						reply.arguments = incompleteChatJSON(map[string]any{"file_path": path, "content": "must never be written"})
					}
				}
				provider, requests := incompleteChatFixture(t, nonstream, []incompleteChatReply{reply, reply}, 1)
				chat := NewChatAgent(provider, "synthetic", dir, nil, 0)
				chat.SetNonStreaming(func() bool { return nonstream })
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				var records []db.Activity
				_, err := chat.Chat(ctx, "auto", "conv-synthetic-incomplete", "Use the synthetic local fixture.", 0, 0, false, func(a db.Activity) { records = append(records, a) })
				var incomplete *llm.IncompleteResponseError
				if !errors.As(err, &incomplete) || string(incomplete.Reason) != wantReason {
					t.Fatalf("error=%T %v; want typed %s", err, err, wantReason)
				}
				calls := requests()
				if len(calls) != wantCalls {
					t.Fatalf("calls=%d want=%d", len(calls), wantCalls)
				}
				if wantCalls == 2 && !bytes.Equal(calls[0], calls[1]) {
					t.Fatal("bounded retry changed request")
				}
				assertIncompleteChatTools(t, calls)
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("partial tool had a file effect: %v", err)
				}
				for _, rec := range records {
					if rec.Kind == "tool_result" && !rec.IsError {
						t.Fatal("incomplete tool produced a successful receipt")
					}
				}
				result := incompleteChatResult(t, records)
				var meta map[string]any
				if err := json.Unmarshal(result.Metadata, &meta); err != nil {
					t.Fatal(err)
				}
				if !result.IsError || meta["terminal_reason"] != "incomplete_response" || meta["summary_recovery_attempted"] != false {
					t.Fatalf("incomplete protocol marked completed or triggered summary recovery: %+v", meta)
				}
			})
		}
	}
}

func TestAutoChatIncompleteRetryHTTPFailurePreservesEarlierWrite(t *testing.T) {
	for _, nonstream := range []bool{false, true} {
		t.Run(fmt.Sprintf("nonstream=%v", nonstream), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "RESOURCE.md")
			const document = "Synthetic resource saved before mixed retry failures.\n"
			provider, requests := incompleteChatFixture(t, nonstream, []incompleteChatReply{
				{tool: "Write", id: "synthetic-saved-write", arguments: incompleteChatJSON(map[string]any{"file_path": path, "content": document}), finish: "tool_calls"},
				{thinking: "Synthetic final answer missing.", finish: "stop"},
				{httpStatus: http.StatusBadGateway},
			}, 1)
			chat := NewChatAgent(provider, "synthetic", dir, nil, 0)
			chat.SetNonStreaming(func() bool { return nonstream })
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var records []db.Activity
			_, err := chat.Chat(ctx, "auto", "conv-synthetic-incomplete", "Save the local synthetic resource.", 0, 0, false, func(a db.Activity) { records = append(records, a) })
			var incomplete *llm.IncompleteResponseError
			if !errors.As(err, &incomplete) || incomplete.Reason != "retry_request_error" {
				t.Fatalf("mixed retry failure=%T %v; want typed retry_request_error", err, err)
			}
			calls := requests()
			if len(calls) != 3 || !bytes.Equal(calls[1], calls[2]) {
				t.Fatalf("mixed failure retried work or changed post-write request: calls=%d", len(calls))
			}
			assertIncompleteChatTools(t, calls)
			if got, err := os.ReadFile(path); err != nil || string(got) != document {
				t.Fatalf("saved file was lost: %q %v", got, err)
			}
			writes := 0
			for _, rec := range records {
				if rec.Kind == "tool_result" && rec.ToolUseID == "synthetic-saved-write" && !rec.IsError {
					writes++
				}
			}
			result := incompleteChatResult(t, records)
			var meta map[string]any
			if err := json.Unmarshal(result.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			if writes != 1 || !result.IsError || meta["terminal_reason"] != "incomplete_response" || meta["response_failure"] != "retry_request_error" || meta["summary_recovery_attempted"] != false {
				t.Fatalf("mixed failure lost protocol classification or replayed work: writes=%d metadata=%+v", writes, meta)
			}
			if result.InputTokens == nil || *result.InputTokens != 22 || result.OutputTokens == nil || *result.OutputTokens != 6 {
				t.Fatalf("mixed failure lost successful write/incomplete attempt usage: %+v", result)
			}
		})
	}
}

func TestAutoChatIncompleteResponseAfterWritePreservesEffects(t *testing.T) {
	for _, nonstream := range []bool{false, true} {
		t.Run(fmt.Sprintf("nonstream=%v", nonstream), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "RESOURCE.md")
			const document = "Synthetic resource saved before incomplete output.\n"
			provider, requests := incompleteChatFixture(t, nonstream, []incompleteChatReply{
				{tool: "Write", id: "synthetic-saved-write", arguments: incompleteChatJSON(map[string]any{"file_path": path, "content": document}), finish: "tool_calls"},
				{thinking: "Synthetic final answer missing.", finish: "stop"},
				{thinking: "Synthetic final answer still missing.", finish: "stop"},
			}, 1)
			chat := NewChatAgent(provider, "synthetic", dir, nil, 0)
			chat.SetNonStreaming(func() bool { return nonstream })
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var records []db.Activity
			_, err := chat.Chat(ctx, "auto", "conv-synthetic-incomplete", "Save the local synthetic resource.", 0, 0, false, func(a db.Activity) { records = append(records, a) })
			var incomplete *llm.IncompleteResponseError
			if !errors.As(err, &incomplete) || incomplete.Reason != "empty_output" {
				t.Fatalf("error=%T %v; want incomplete output after write", err, err)
			}
			calls := requests()
			if len(calls) != 3 || !bytes.Equal(calls[1], calls[2]) {
				t.Fatalf("retry did not preserve post-write request: calls=%d", len(calls))
			}
			assertIncompleteChatTools(t, calls)
			if got, err := os.ReadFile(path); err != nil || string(got) != document {
				t.Fatalf("saved file was lost: %q %v", got, err)
			}
			writes := 0
			for _, rec := range records {
				if rec.Kind == "tool_result" && rec.ToolUseID == "synthetic-saved-write" && !rec.IsError {
					writes++
				}
			}
			result := incompleteChatResult(t, records)
			var meta map[string]any
			if err := json.Unmarshal(result.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			if writes != 1 || !result.IsError || meta["terminal_reason"] != "incomplete_response" || meta["summary_recovery_attempted"] != false {
				t.Fatalf("existing work was replayed or incomplete output marked success: writes=%d metadata=%+v", writes, meta)
			}
			if result.InputTokens == nil || *result.InputTokens != 33 || result.OutputTokens == nil || *result.OutputTokens != 9 {
				t.Fatalf("failed run lost earlier/retry usage: %+v", result)
			}
		})
	}
}
