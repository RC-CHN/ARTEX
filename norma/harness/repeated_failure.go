package harness

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

type failureStreak struct {
	signature [32]byte
	count     int
}

// Only completed error results count. Successful polling and long-running tools
// never trigger this guidance. Bounded per-run state contains no payload copies.
func (l *loop) noteRepeatedFailures(uses, results []llm.ContentBlock) {
	if l.ctx.Err() != nil || l.settling.Load() {
		return
	}
	byID := map[string]llm.ContentBlock{}
	for _, u := range uses {
		byID[u.ID] = u
	}
	for _, r := range results {
		u, ok := byID[r.ToolUseID]
		if !ok {
			continue
		}
		if !r.IsError {
			delete(l.failureStreaks, u.Name)
			continue
		}
		if l.failureStreaks == nil {
			l.failureStreaks = map[string]failureStreak{}
		}
		input := u.Input
		// Whitespace/key order changes do not constitute a new attempt. Preserve
		// large integers when canonicalizing tool arguments.
		var parsed any
		d := json.NewDecoder(bytes.NewReader(input))
		d.UseNumber()
		if d.Decode(&parsed) == nil {
			if b, e := json.Marshal(parsed); e == nil {
				input = b
			}
		}
		h := sha256.New()
		h.Write(input)
		h.Write([]byte{0})
		for _, b := range r.Content {
			h.Write([]byte(b.Text))
		}
		var signature [32]byte
		copy(signature[:], h.Sum(nil))
		s := l.failureStreaks[u.Name]
		if s.signature != signature {
			s = failureStreak{signature: signature}
		}
		s.count++
		if len(l.failureStreaks) >= 64 {
			delete(l.failureStreaks, u.Name)
			if len(l.failureStreaks) >= 64 {
				l.failureStreaks = map[string]failureStreak{}
			}
		}
		l.failureStreaks[u.Name] = s
		if s.count != 3 && s.count != 6 {
			continue
		}
		text := fmt.Sprintf("[repeated-tool-failure] Tool %q returned the same error for the same arguments %d times. Inspect the error and current state, then change the arguments or approach; do not repeat the unchanged call. This is a correction notice, not a task timeout.", u.Name, s.count)
		if s.count == 6 {
			if _, ok := l.in.Tools.Get("ask_main_agent"); ok {
				text += " If still blocked, submit one ask_main_agent request with the error summary and attempted fixes; continue independent work while awaiting steering. Do not duplicate an already pending request."
			} else {
				text += " Explain the blocker and seek missing information through the available coordination tools."
			}
		}
		m := llm.UserText(text)
		l.messages = append(l.messages, m)
		l.record(m, llm.Usage{})
	}
}
