package llmrec

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/transcript"
	"github.com/google/uuid"
)

func TestUsageSessionAssociationWithoutBodies(t *testing.T) {
	d := streamDiagnosticsDB(t)
	profile := "sessions-" + uuid.NewString()
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile) })
	recorder := Wrap(meteredStreamProvider{}, d, "fixture", profile, "", "", func() bool { return false })
	sessions := []string{"conv-529", "conv-5290", "exp12-worker-i99", ""}
	var wg sync.WaitGroup
	errs := make(chan error, 2*len(sessions))
	for _, session := range sessions {
		for _, streaming := range []bool{false, true} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx := WithTaskID(transcript.WithSessionID(t.Context(), session), "42")
				if streaming {
					for _, err := range recorder.Stream(ctx, llm.CompletionRequest{Messages: []llm.Message{llm.UserText("private synthetic request")}}) {
						if err != nil {
							errs <- err
							return
						}
					}
				} else if _, _, _, err := recorder.Complete(ctx, llm.CompletionRequest{}); err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	for _, session := range sessions {
		var count, input, output int
		var taskID string
		var expID sql.NullInt64
		if err := d.QueryRow(`SELECT count(*),sum(input_tokens),sum(output_tokens),max(task_id),max(exploration_id)
FROM llm_usage WHERE profile_name=$1 AND session_id IS NOT DISTINCT FROM NULLIF($2,'')`, profile, session).Scan(&count, &input, &output, &taskID, &expID); err != nil {
			t.Fatal(err)
		}
		wantExpID := int64(0)
		if strings.HasPrefix(session, "exp12-") {
			wantExpID = 12
		}
		if count != 2 || input != 30 || output != 8 || taskID != "42" || expID.Int64 != wantExpID {
			t.Fatalf("session=%q count=%d input/output=%d/%d task=%q exp=%+v", session, count, input, output, taskID, expID)
		}
	}
	var records int
	if err := d.QueryRow(`SELECT count(*) FROM llm_records WHERE profile_name=$1`, profile).Scan(&records); err != nil || records != 0 {
		t.Fatalf("recording disabled but body records=%d err=%v", records, err)
	}
}

type diagnosticUsageProvider struct {
	completeProvider
	events []llm.StreamEvent
	err    error
}

func (p diagnosticUsageProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for _, event := range p.events {
			if !yield(event, nil) {
				return
			}
		}
		if p.err != nil {
			yield(llm.StreamEvent{}, p.err)
		}
	}
}

func TestDiagnosticUsageCountsAttemptsAndKeepsLastStop(t *testing.T) {
	d := streamDiagnosticsDB(t)
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("error=%v", fail), func(t *testing.T) {
			profile := "diagnostic-usage-" + uuid.NewString()
			t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE profile_name=$1`, profile) })
			lastStop, streamEnd, parseErrors := "stop", llm.StreamEndDone, 0
			var endErr error
			if fail {
				lastStop, streamEnd, parseErrors = "", llm.StreamEndEOF, 2
				endErr = errors.New("private synthetic provider error")
			}
			provider := diagnosticUsageProvider{events: []llm.StreamEvent{
				{Type: llm.SEStreamDiagnostics, Usage: llm.Usage{InputTokens: 3, OutputTokens: 1}, StopReason: "stop", Diagnostics: &llm.StreamDiagnostics{FinishReasonSeen: true, StreamEnd: llm.StreamEndDone}},
				{Type: llm.SEStreamDiagnostics, Usage: llm.Usage{InputTokens: 7, OutputTokens: 2}, StopReason: lastStop, Diagnostics: &llm.StreamDiagnostics{FinishReasonSeen: !fail, StreamEnd: streamEnd, ParseErrors: parseErrors}},
			}, err: endErr}
			if !fail {
				provider.events = append(provider.events, llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "stop"}, llm.StreamEvent{Type: llm.SEMessageStop})
			}
			recorder := Wrap(provider, d, "fixture", profile, "", "", func() bool { return false })
			var resultErr error
			for _, err := range recorder.Stream(transcript.WithSessionID(t.Context(), "conv-529"), llm.CompletionRequest{}) {
				if err != nil {
					resultErr = err
				}
			}
			if !errors.Is(resultErr, endErr) {
				t.Fatalf("provider error changed: %v", resultErr)
			}
			var count, input, output, parsed int
			var stop, end, status, row string
			var seen bool
			if err := d.QueryRow(`SELECT input_tokens,output_tokens,stop_reason,stream_end,finish_reason_seen,parse_errors,status,row_to_json(llm_usage)::text
FROM llm_usage WHERE profile_name=$1`, profile).Scan(&input, &output, &stop, &end, &seen, &parsed, &status, &row); err != nil {
				t.Fatal(err)
			}
			if err := d.QueryRow(`SELECT count(*) FROM llm_usage WHERE profile_name=$1`, profile).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 || input != 10 || output != 3 || stop != lastStop || end != string(streamEnd) || seen == fail || parsed != parseErrors || (status == "error") != fail {
				t.Fatalf("count=%d tokens=%d/%d stop=%q end=%q seen=%v parsed=%d status=%q", count, input, output, stop, end, seen, parsed, status)
			}
			if strings.Contains(row, "private synthetic") {
				t.Fatal("private error entered usage ledger")
			}
		})
	}
}
