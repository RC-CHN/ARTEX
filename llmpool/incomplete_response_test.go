package llmpool

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

type incompleteProvider struct {
	calls int
	err   error
}

func (p *incompleteProvider) Stream(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		p.calls++
		if !yield(llm.StreamEvent{Type: llm.SEStreamDiagnostics, Usage: llm.Usage{InputTokens: 17}, Diagnostics: &llm.StreamDiagnostics{StreamEnd: llm.StreamEndEOF}}, nil) {
			return
		}
		yield(llm.StreamEvent{}, p.err)
	}
}

func (p *incompleteProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	p.calls++
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock("synthetic partial")}}, "", llm.Usage{InputTokens: 17}, p.err
}

func TestIncompleteResponseDoesNotMultiplyRetriesOrLoseUsage(t *testing.T) {
	for _, nonstream := range []bool{false, true} {
		t.Run(fmt.Sprint(nonstream), func(t *testing.T) {
			cause := &llm.IncompleteResponseError{Reason: llm.IncompleteMissingTerminal}
			primary := &incompleteProvider{err: fmt.Errorf("synthetic wrapper: %w", cause)}
			backup := okProv("backup", "must not replay")
			pool := New([]*Member{member(1, "primary", 10, primary), member(2, "backup", 5, backup)}, NewRegistry(nil, nil))
			if nonstream {
				msg, _, usage, err := pool.Complete(t.Context(), llm.CompletionRequest{})
				if !errors.Is(err, cause) || msg.Text() != "synthetic partial" || usage.InputTokens != 17 {
					t.Fatalf("lost atomic response evidence: msg=%+v usage=%+v err=%v", msg, usage, err)
				}
			} else {
				acc := llm.NewAccumulator()
				var streamErr error
				for ev, err := range pool.Stream(t.Context(), llm.CompletionRequest{}) {
					if err != nil {
						streamErr = err
						break
					}
					acc.Add(ev)
				}
				if !errors.Is(streamErr, cause) || acc.Usage.InputTokens != 17 {
					t.Fatalf("lost streaming evidence: usage=%+v err=%v", acc.Usage, streamErr)
				}
			}
			if primary.calls != 1 || backup.calls != 0 {
				t.Fatalf("repeated exhausted adapter recovery: primary=%d backup=%d", primary.calls, backup.calls)
			}
		})
	}
}
