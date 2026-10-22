package agent

import (
	"context"
	"encoding/json"
	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/transcript"
	"path/filepath"
	"testing"
)

func TestMainAgentEmptyCompletionDogfood(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTaskWithOptions("synthetic empty final", "synthetic", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, nonstream := range []bool{true, false} {
		calls := 0
		var records []db.Activity
		p := consultationProvider{complete: func(_ context.Context, r llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
			calls++
			if calls == 1 {
				return consultationText("")
			}
			if calls > 2 {
				t.Fatal("unbounded recovery")
			}
			if len(r.Tools) != 0 {
				t.Fatal("recovery tools exposed")
			}
			return consultationText("Synthetic task unchanged; no external work performed.")
		}}
		dir := t.TempDir()
		m := NewMainAgent(p, "synthetic", dir, transcript.NewStore(filepath.Join(dir, "transcripts")), 100000, 8)
		m.SetNonStreaming(func() bool { return nonstream })
		text, err := m.Chat(t.Context(), task.ID, 0, nil, d.Exploration(task.ExplorationID), "synthetic", "synthetic request", func(a db.Activity) { records = append(records, a) }, nil, nil, nil, nil)
		if err != nil || calls != 2 || text != "Synthetic task unchanged; no external work performed." {
			t.Fatal(text, calls, err)
		}
		var found bool
		for _, a := range records {
			if a.Kind == "result" {
				var meta map[string]any
				if err = json.Unmarshal(a.Metadata, &meta); err != nil {
					t.Fatal(err)
				}
				if meta["summary_missing"] != false || meta["summary_recovery_attempted"] != true || meta["terminal_reason"] != "completed" {
					t.Fatal(meta)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("no structured terminal")
		}
	}
}
