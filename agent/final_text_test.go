package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/transcript"
)

// Preserve thinking blocks and missing stop reasons in both provider modes.
type mainFinalProvider struct{ consultationProvider }

const legacyMainRecoveryPrompt = "上一轮正常结束但没有最终文字答复。本轮只用文字向用户汇报已有结果、可核对的记录 ID 或产物路径、未完成项与阻碍；没有完成也请明确说明。禁止调用任何工具，禁止继续执行工作，不推测或编造结果。"

func (p mainFinalProvider) Stream(ctx context.Context, r llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(y func(llm.StreamEvent, error) bool) {
		m, stop, u, err := p.Complete(ctx, r)
		if err != nil {
			y(llm.StreamEvent{}, err)
			return
		}
		for _, b := range m.Content {
			var events []llm.StreamEvent
			switch b.Type {
			case llm.BlockThinking:
				events = []llm.StreamEvent{{Type: llm.SEThinkingDelta, Text: b.Thinking}}
			case llm.BlockText:
				events = []llm.StreamEvent{{Type: llm.SETextDelta, Text: b.Text}}
			case llm.BlockToolUse:
				events = []llm.StreamEvent{{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name}, {Type: llm.SEToolInputJSON, Text: string(b.Input)}}
			}
			for _, ev := range events {
				if !y(ev, nil) {
					return
				}
			}
		}
		if y(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: u}, nil) {
			y(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
		}
	}
}

func mainThinkingOnly(stop string) (llm.Message, string, llm.Usage, error) {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "synthetic intermediate reasoning"}}}, stop, llm.Usage{}, nil
}

func mainToolReceipt(r llm.CompletionRequest, id, contains string) error {
	for _, m := range r.Messages {
		for _, b := range m.Content {
			if b.Type == llm.BlockToolResult && b.ToolUseID == id {
				if b.IsError {
					return fmt.Errorf("synthetic tool %s failed: %+v", id, b.Content)
				}
				raw, _ := json.Marshal(b.Content)
				if !strings.Contains(string(raw), contains) {
					return fmt.Errorf("synthetic tool %s receipt lacks %q", id, contains)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("synthetic tool %s has no receipt", id)
}

func assertMainTerminal(t *testing.T, records []db.Activity, missing bool) {
	t.Helper()
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Kind != "result" {
			continue
		}
		var meta map[string]any
		if err := json.Unmarshal(records[i].Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		if records[i].IsError || meta["summary_missing"] != missing || meta["summary_recovery_attempted"] != false || meta["terminal_reason"] != "completed" {
			t.Fatalf("unexpected main terminal: %+v", meta)
		}
		if meta["summary_recovery_attempts"] != float64(0) || meta["summary_recovery_mode"] != "" {
			t.Fatalf("main unexpectedly attempted empty-reply recovery: %+v", meta)
		}
		return
	}
	t.Fatal("no structured main terminal")
}

func TestMainAgentNaturalCompletionAndLegacyTranscriptDogfood(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	for _, nonstream := range []bool{true, false} {
		for _, stop := range []string{"end_turn", ""} {
			t.Run(fmt.Sprintf("nonstream=%v/stop=%q", nonstream, stop), func(t *testing.T) {
				task, err := d.CreateTaskWithOptions("synthetic empty final", "synthetic", db.TaskCreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				path := filepath.Join(dir, "RESOURCE.md")
				const document = "---\nname: synthetic\ndescription: synthetic before\n---\n\nSynthetic local fixture.\n"
				calls := 0
				p := mainFinalProvider{consultationProvider{complete: func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					if len(r.Tools) == 0 {
						return llm.Message{}, "", llm.Usage{}, fmt.Errorf("main tools disabled at call %d", calls)
					}
					raw, _ := json.Marshal(r.Messages)
					if strings.Contains(string(raw), "禁止调用任何工具") || strings.Contains(string(raw), "禁止继续执行工作") || strings.Contains(string(raw), "上一轮没有向用户给出文字答复") {
						return llm.Message{}, "", llm.Usage{}, fmt.Errorf("main inherited text-only recovery restriction")
					}
					switch calls {
					case 1:
						return consultationCall("Write", "write-fixture", map[string]any{"file_path": path, "content": document})
					case 2:
						if err := mainToolReceipt(r, "write-fixture", ""); err != nil {
							return llm.Message{}, "", llm.Usage{}, err
						}
						return consultationCall("Read", "read-fixture", map[string]any{"file_path": path})
					case 3:
						if err := mainToolReceipt(r, "read-fixture", "synthetic before"); err != nil {
							return llm.Message{}, "", llm.Usage{}, err
						}
						return consultationCall("Edit", "edit-fixture", map[string]any{"file_path": path, "old_string": "synthetic before", "new_string": "synthetic after"})
					case 4:
						if err := mainToolReceipt(r, "edit-fixture", ""); err != nil {
							return llm.Message{}, "", llm.Usage{}, err
						}
						return consultationCall("Read", "verify-fixture", map[string]any{"file_path": path})
					case 5:
						if err := mainToolReceipt(r, "verify-fixture", "synthetic after"); err != nil {
							return llm.Message{}, "", llm.Usage{}, err
						}
						return mainThinkingOnly(stop)
					case 6:
						if !strings.Contains(string(raw), "verify-fixture") {
							return llm.Message{}, "", llm.Usage{}, fmt.Errorf("main transcript did not resume")
						}
						return consultationCall("Read", "next-chat-read", map[string]any{"file_path": path})
					case 7:
						if err := mainToolReceipt(r, "next-chat-read", "synthetic after"); err != nil {
							return llm.Message{}, "", llm.Usage{}, err
						}
						return consultationText("Next human turn can still use tools.")
					default:
						return llm.Message{}, "", llm.Usage{}, fmt.Errorf("unexpected recovery call %d", calls)
					}
				}}}
				tx := transcript.NewStore(filepath.Join(dir, "transcripts"))
				m := NewMainAgent(p, "synthetic", dir, tx, 0, 0)
				m.SetNonStreaming(func() bool { return nonstream })
				var records []db.Activity
				emit := func(a db.Activity) { records = append(records, a) }
				text, err := m.Chat(t.Context(), task.ID, 0, nil, d.Exploration(task.ExplorationID), "synthetic", "create and verify the local synthetic fixture", emit, nil, nil, nil, nil)
				if err != nil || calls != 5 || text != "" {
					t.Fatalf("first chat: text=%q calls=%d err=%v", text, calls, err)
				}
				assertMainTerminal(t, records, true)
				if contents, err := os.ReadFile(path); err != nil || string(contents) != strings.Replace(document, "synthetic before", "synthetic after", 1) {
					t.Fatalf("real file tool effects: %q %v", contents, err)
				}
				// Simulate a transcript created by v0.14.0. The durable record must
				// remain intact while its obsolete restriction is omitted from requests.
				thinking, _, _, _ := mainThinkingOnly("")
				legacy := llm.UserText(legacyMainRecoveryPrompt)
				answer, _, _, _ := consultationText("Synthetic legacy recovery answer.")
				txPath := tx.MainPath(fmt.Sprintf("exp%d-main", task.ExplorationID))
				if err = tx.Append(txPath, transcript.Record{Type: "message", Message: &thinking}, transcript.Record{Type: "message", Message: &legacy}, transcript.Record{Type: "message", Message: &answer}); err != nil {
					t.Fatal(err)
				}
				records = nil
				text, err = m.Chat(t.Context(), task.ID, 0, nil, d.Exploration(task.ExplorationID), "synthetic", "read the existing local synthetic fixture", emit, nil, nil, nil, nil)
				if err != nil || calls != 7 || text != "Next human turn can still use tools." {
					t.Fatalf("resumed chat: text=%q calls=%d err=%v", text, calls, err)
				}
				assertMainTerminal(t, records, false)
				if raw, err := os.ReadFile(txPath); err != nil || !strings.Contains(string(raw), legacyMainRecoveryPrompt) {
					t.Fatalf("legacy audit record was rewritten: %v", err)
				}
			})
		}
	}
}

func TestMainAgentEmptyReplyCompletesWithoutRecoveryOrLosingToolEffects(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	for _, nonstream := range []bool{true, false} {
		for _, tc := range []struct {
			name       string
			noThinking bool
			saveFile   bool
			failedEdit bool
			wantCalls  int
		}{
			{name: "empty_message", noThinking: true, wantCalls: 1},
			{name: "thinking_only", wantCalls: 1},
			{name: "after_write", saveFile: true, wantCalls: 2},
			{name: "after_failed_edit", saveFile: true, failedEdit: true, wantCalls: 3},
		} {
			t.Run(fmt.Sprintf("nonstream=%v/%s", nonstream, tc.name), func(t *testing.T) {
				task, err := d.CreateTaskWithOptions("synthetic empty response guard", "synthetic", db.TaskCreateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				dir := t.TempDir()
				path := filepath.Join(dir, "RESOURCE.md")
				calls := 0
				p := mainFinalProvider{consultationProvider{complete: func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
					calls++
					if calls > tc.wantCalls {
						return llm.Message{}, "", llm.Usage{}, fmt.Errorf("main unexpectedly retried an empty reply")
					}
					if len(r.Tools) == 0 {
						return llm.Message{}, "", llm.Usage{}, fmt.Errorf("main recovery disabled tools")
					}
					phase := calls
					if tc.saveFile {
						if calls == 1 {
							return consultationCall("Write", "saved-resource", map[string]any{"file_path": path, "content": "synthetic preserved resource"})
						}
						if err := mainToolReceipt(r, "saved-resource", ""); err != nil {
							return llm.Message{}, "", llm.Usage{}, err
						}
						phase--
					}
					if phase == 1 && tc.failedEdit {
						return consultationCall("Edit", "failed-resource-edit", map[string]any{"file_path": path, "old_string": "absent string", "new_string": "must not be written"})
					}
					if tc.noThinking {
						return llm.Message{Role: llm.RoleAssistant}, "end_turn", llm.Usage{}, nil
					}
					return mainThinkingOnly("end_turn")
				}}}
				m := NewMainAgent(p, "synthetic", dir, nil, 0, 0)
				m.SetNonStreaming(func() bool { return nonstream })
				var records []db.Activity
				text, err := m.Chat(t.Context(), task.ID, 0, nil, d.Exploration(task.ExplorationID), "synthetic", "verify the synthetic recovery guard", func(a db.Activity) { records = append(records, a) }, nil, nil, nil, nil)
				if err != nil || text != "" || calls != tc.wantCalls {
					t.Fatalf("empty main completion: calls=%d text=%q err=%v", calls, text, err)
				}
				assertMainTerminal(t, records, true)
				result := records[len(records)-1]
				for _, forbidden := range []string{"empty_response", "空回复恢复", "禁止工具调用"} {
					if strings.Contains(result.Detail, forbidden) {
						t.Fatalf("misleading empty-response receipt: %s", result.Detail)
					}
				}
				if tc.saveFile {
					if contents, err := os.ReadFile(path); err != nil || string(contents) != "synthetic preserved resource" {
						t.Fatalf("saved resource lost: %q %v", contents, err)
					}
					wantReceipt := "已正常返回"
					if tc.failedEdit {
						wantReceipt = "返回错误"
					}
					if strings.Contains(result.Detail, "还没有发出工具调用") || !strings.Contains(result.Detail, wantReceipt) {
						t.Fatalf("prior work incorrectly discarded: %s", result.Detail)
					}
				}
				if tc.failedEdit {
					found := false
					for _, rec := range records {
						if rec.Kind == "tool_result" && rec.ToolUseID == "failed-resource-edit" && rec.IsError && strings.Contains(rec.Detail, "match_not_found") {
							found = true
						}
					}
					if !found {
						t.Fatal("missing actual Edit failure receipt")
					}
				}
			})
		}
	}
}

func TestMainAgentUnlimitedDoesNotInheritWorkerOrTaskBudget(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatal(err)
	}
	for _, nonstream := range []bool{true, false} {
		t.Run(fmt.Sprintf("nonstream=%v", nonstream), func(t *testing.T) {
			task, err := d.CreateTaskWithOptions("synthetic unlimited main", "synthetic", db.TaskCreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.SetTaskTokenBudget(t.Context(), task.ID, 1, 1); err != nil {
				t.Fatal(err)
			}
			if err = d.InsertLLMUsage(&db.LLMUsage{TaskID: fmt.Sprint(task.ID), Worker: "worker", InputTokens: 2, Status: "ok"}); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "read-fixture.txt")
			if err = os.WriteFile(path, []byte("synthetic unlimited fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			const toolTurns = 165
			calls := 0
			p := mainFinalProvider{consultationProvider{complete: func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls > 1 {
					if err := mainToolReceipt(r, fmt.Sprintf("read-%d", calls-1), "synthetic unlimited fixture"); err != nil {
						return llm.Message{}, "", llm.Usage{}, err
					}
				}
				raw, _ := json.Marshal(r.Messages)
				for _, forbidden := range []string{"你的步数即将用尽", "Worker 预算已到", "任务累计软预算已到"} {
					if strings.Contains(string(raw), forbidden) {
						return llm.Message{}, "", llm.Usage{}, fmt.Errorf("unlimited main received a budget wrapup")
					}
				}
				if calls <= toolTurns {
					return consultationCall("Read", fmt.Sprintf("read-%d", calls), map[string]any{"file_path": path})
				}
				return consultationText("Main completed 165 real file reads without Worker settlement.")
			}}}
			m := NewMainAgent(p, "synthetic", dir, nil, 0, 0)
			m.SetNonStreaming(func() bool { return nonstream })
			var records []db.Activity
			_, err = m.Chat(t.Context(), task.ID, 0, nil, d.Exploration(task.ExplorationID), "synthetic", "verify the local fixture repeatedly", func(a db.Activity) { records = append(records, a) }, nil, nil, nil, nil)
			if err != nil || calls != toolTurns+1 {
				t.Fatalf("main calls=%d, err=%v", calls, err)
			}
			assertMainTerminal(t, records, false)
		})
	}
}

func TestMainAgentExplicitTurnLimitKeepsSettlementWithoutEmptyRecovery(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	for _, nonstream := range []bool{true, false} {
		t.Run(fmt.Sprintf("nonstream=%v", nonstream), func(t *testing.T) {
			task, err := d.CreateTaskWithOptions("synthetic explicit main limit", "synthetic", db.TaskCreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "fixture.txt")
			if err = os.WriteFile(path, []byte("synthetic bounded main"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			p := mainFinalProvider{consultationProvider{complete: func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
				calls++
				if calls > 3 || len(r.Tools) == 0 {
					return llm.Message{}, "", llm.Usage{}, fmt.Errorf("main settlement retried empty reply or hid tools")
				}
				raw, _ := json.Marshal(r.Messages)
				if strings.Contains(string(raw), mainAgentWrapUpDefault) != (calls == 3) {
					return llm.Message{}, "", llm.Usage{}, fmt.Errorf("main explicit settlement at wrong call %d", calls)
				}
				if calls > 1 {
					if err := mainToolReceipt(r, fmt.Sprintf("limited-read-%d", calls-1), "synthetic bounded main"); err != nil {
						return llm.Message{}, "", llm.Usage{}, err
					}
				}
				if calls == 3 {
					return mainThinkingOnly("end_turn")
				}
				return consultationCall("Read", fmt.Sprintf("limited-read-%d", calls), map[string]any{"file_path": path})
			}}}
			m := NewMainAgent(p, "synthetic", dir, nil, 0, 2)
			m.SetNonStreaming(func() bool { return nonstream })
			var records []db.Activity
			text, err := m.Chat(t.Context(), task.ID, 0, nil, d.Exploration(task.ExplorationID), "synthetic", "verify explicit main limit", func(a db.Activity) { records = append(records, a) }, nil, nil, nil, nil)
			if err != nil || calls != 3 || text != "" || len(records) == 0 {
				t.Fatalf("bounded main: calls=%d text=%q err=%v", calls, text, err)
			}
			last := records[len(records)-1]
			var meta map[string]any
			if err = json.Unmarshal(last.Metadata, &meta); err != nil {
				t.Fatal(err)
			}
			if last.Kind != "result" || last.IsError || meta["terminal_reason"] != "max_turns" || meta["summary_recovery_attempted"] != false || meta["summary_recovery_attempts"] != float64(0) {
				t.Fatalf("explicit main limit changed: %+v", meta)
			}
		})
	}
}

func TestMainAgentAutomaticConsultationKeeps24TurnBound(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("synthetic consultation bound", "synthetic", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.txt")
	if err = os.WriteFile(path, []byte("synthetic consultation fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	p := mainFinalProvider{consultationProvider{complete: func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		calls++
		if calls > 25 {
			return llm.Message{}, "", llm.Usage{}, fmt.Errorf("automatic consultation lost its bound")
		}
		if calls > 1 {
			if err := mainToolReceipt(r, fmt.Sprintf("consult-read-%d", calls-1), "synthetic consultation fixture"); err != nil {
				return llm.Message{}, "", llm.Usage{}, err
			}
		}
		raw, _ := json.Marshal(r.Messages)
		if len(r.Tools) == 0 || strings.Contains(string(raw), legacyMainRecoveryPrompt) {
			return llm.Message{}, "", llm.Usage{}, fmt.Errorf("automatic consultation inherited an obsolete recovery restriction")
		}
		settling := strings.Contains(string(raw), mainAgentWrapUpDefault)
		if settling != (calls == 25) {
			return llm.Message{}, "", llm.Usage{}, fmt.Errorf("automatic settlement at call %d", calls)
		}
		if settling {
			return consultationText("Synthetic consultation checkpoint saved.")
		}
		return consultationCall("Read", fmt.Sprintf("consult-read-%d", calls), map[string]any{"file_path": path})
	}}}
	tx := transcript.NewStore(filepath.Join(dir, "transcripts"))
	txPath := tx.MainPath(fmt.Sprintf("exp%d-main", task.ExplorationID))
	thinking, _, _, _ := mainThinkingOnly("")
	legacy := llm.UserText(legacyMainRecoveryPrompt)
	answer, _, _, _ := consultationText("Synthetic legacy human recovery answer.")
	if err = tx.Append(txPath, transcript.Record{Type: "message", Message: &thinking}, transcript.Record{Type: "message", Message: &legacy}, transcript.Record{Type: "message", Message: &answer}); err != nil {
		t.Fatal(err)
	}
	m := NewMainAgent(p, "synthetic", dir, tx, 0, 0)
	var records []db.Activity
	_, err = m.Consult(t.Context(), task.ID, nil, d.Exploration(task.ExplorationID), "synthetic", &db.MainRequest{ID: 1, Question: "verify synthetic local file"}, func(a db.Activity) { records = append(records, a) }, nil)
	if err != nil || calls != 25 {
		t.Fatalf("automatic consultation calls=%d err=%v", calls, err)
	}
	if len(records) == 0 || records[len(records)-1].Kind != "result" {
		t.Fatal("automatic consultation has no terminal")
	}
	var meta map[string]any
	if err = json.Unmarshal(records[len(records)-1].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	if meta["terminal_reason"] != "max_turns" || meta["summary_recovery_attempted"] != false {
		t.Fatalf("automatic consultation changed: %+v", meta)
	}
	if raw, err := os.ReadFile(txPath); err != nil || !strings.Contains(string(raw), legacyMainRecoveryPrompt) {
		t.Fatalf("legacy human audit record was rewritten during consultation: %v", err)
	}
}

func TestMainAgentNaturalCompletionConcurrentTasksAndSegments(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	tasks := make([]*db.Task, 2)
	for i := range tasks {
		var err error
		tasks[i], err = d.CreateTaskWithOptions("synthetic concurrent main", "synthetic", db.TaskCreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	type scenario struct {
		task    *db.Task
		segment int
		marker  string
		empty   bool
	}
	cases := []scenario{{tasks[0], 0, "synthetic-alpha", true}, {tasks[0], 1, "synthetic-beta", false}, {tasks[1], 0, "synthetic-gamma", true}}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var mu sync.Mutex
	calls := make(map[string]int)
	started := 0
	ready := make(chan struct{})
	p := mainFinalProvider{consultationProvider{complete: func(ctx context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		var current *scenario
		for i := range cases {
			for _, m := range r.Messages {
				if m.Role == llm.RoleUser && m.Text() == cases[i].marker {
					if current != nil {
						return llm.Message{}, "", llm.Usage{}, fmt.Errorf("task or segment transcript crossed")
					}
					current = &cases[i]
				}
			}
		}
		if current == nil || len(r.Tools) == 0 {
			return llm.Message{}, "", llm.Usage{}, fmt.Errorf("missing task context or disabled tools")
		}
		mu.Lock()
		calls[current.marker]++
		n := calls[current.marker]
		if n == 1 {
			started++
			if started == len(cases) {
				close(ready)
			}
		}
		mu.Unlock()
		if n == 1 {
			select {
			case <-ready:
			case <-ctx.Done():
				return llm.Message{}, "", llm.Usage{}, ctx.Err()
			}
		}
		if current.empty {
			return mainThinkingOnly("")
		}
		path := filepath.Join(dir, current.marker+".txt")
		switch n {
		case 1:
			return consultationCall("Write", current.marker+"-write", map[string]any{"file_path": path, "content": current.marker})
		case 2:
			if err := mainToolReceipt(r, current.marker+"-write", ""); err != nil {
				return llm.Message{}, "", llm.Usage{}, err
			}
			return consultationCall("Read", current.marker+"-read", map[string]any{"file_path": path})
		case 3:
			if err := mainToolReceipt(r, current.marker+"-read", current.marker); err != nil {
				return llm.Message{}, "", llm.Usage{}, err
			}
			return consultationText(current.marker + " verified")
		default:
			return llm.Message{}, "", llm.Usage{}, fmt.Errorf("unexpected extra recovery")
		}
	}}}
	m := NewMainAgent(p, "synthetic", dir, transcript.NewStore(filepath.Join(dir, "transcripts")), 0, 0)
	type outcome struct {
		scenario
		text    string
		err     error
		records []db.Activity
	}
	results := make(chan outcome, len(cases))
	for _, c := range cases {
		go func() {
			o := outcome{scenario: c}
			o.text, o.err = m.Chat(ctx, c.task.ID, c.segment, nil, d.Exploration(c.task.ExplorationID), "synthetic", c.marker, func(a db.Activity) { o.records = append(o.records, a) }, nil, nil, nil, nil)
			results <- o
		}()
	}
	for range cases {
		o := <-results
		wantText, wantCalls := o.marker+" verified", 3
		if o.empty {
			wantText, wantCalls = "", 1
		}
		mu.Lock()
		gotCalls := calls[o.marker]
		mu.Unlock()
		if o.err != nil || o.text != wantText || gotCalls != wantCalls {
			t.Errorf("concurrent %s: %q %v", o.marker, o.text, o.err)
			continue
		}
		assertMainTerminal(t, o.records, o.empty)
		contents, err := os.ReadFile(filepath.Join(dir, o.marker+".txt"))
		if o.empty {
			if !os.IsNotExist(err) {
				t.Errorf("empty completion unexpectedly wrote file %s: %v", o.marker, err)
			}
		} else if err != nil || string(contents) != o.marker {
			t.Errorf("concurrent file %s: %q %v", o.marker, contents, err)
		}
	}
}
