package tool

import (
	"encoding/json"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

// NormalizeError only enriches legacy plain-text errors. Structured receipts
// retain their original contract; successful results are byte-for-byte intact.
// Never infer "not executed" from a tool returning an error after admission.
func NormalizeError(r Result, readOnly bool) Result {
	if !r.IsError || r.Failure != nil {
		return r
	}
	for _, block := range r.Content {
		if block.Type != llm.BlockText {
			return r
		}
	}
	text := r.Flatten()
	var existing map[string]any
	if json.Unmarshal([]byte(text), &existing) == nil && existing["code"] != nil {
		return r
	}
	next := "Inspect the error, arguments and dependencies before trying again. A failed read is not evidence that no records exist."
	if !readOnly {
		next = "The operation may have changed state before failing. Verify current state before retrying; reuse the original idempotency key where supported."
	}
	b, _ := json.Marshal(map[string]any{"code": "tool_failed", "message": text, "execution_status": "unknown", "retryable": false, "next_action": next})
	r.Content = []llm.ContentBlock{llm.TextBlock(string(b))}
	return r
}
