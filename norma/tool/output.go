package tool

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

// OutputReceipt is internal result metadata, never inferred from body text.
// Revalidate it at runtime boundaries, and always enforce an absolute size cap.
type OutputReceipt struct {
	Digest string `json:"digest"`
	Limit  int    `json:"limit"`
}

func CapResult(tc *ToolContext, r Result) Result {
	limit := maxOut(tc)
	var text strings.Builder
	for _, b := range r.Content {
		if b.Type == llm.BlockText {
			text.WriteString(b.Text)
		}
	}
	s := text.String()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
	if len(s) > limit+512 || (r.Captured == nil || r.Captured.Digest != digest || r.Captured.Limit > limit) {
		if len(s) > limit {
			s = CaptureOnce(tc, s)
			// Bound trailers too; a very long filesystem path cannot inflate output.
			if len(s) > limit+512 {
				s = truncate(s, limit)
			}
			blocks := make([]llm.ContentBlock, 0, len(r.Content))
			added := false
			for _, b := range r.Content {
				if b.Type == llm.BlockText {
					if !added {
						blocks = append(blocks, llm.TextBlock(s))
						added = true
					}
				} else {
					blocks = append(blocks, b)
				}
			}
			r.Content = blocks
		}
	}
	// Extra carries skill instructions, so use a separate, bounded allowance and
	// retain the full messages as an artifact when they exceed it.
	if len(r.Extra) > 0 {
		if raw, err := json.Marshal(r.Extra); err == nil && len(raw) > max(4*limit, 128<<10) {
			copyTC := ToolContext{}
			if tc != nil {
				copyTC = *tc
			}
			copyTC.MaxOutputChars = limit
			r.Extra = []llm.Message{llm.UserText("Additional tool context exceeded the inline budget; read the saved artifact as needed.\n" + Capture(&copyTC, string(raw)))}
		}
	}
	r.Captured = &OutputReceipt{Digest: fmt.Sprintf("%x", sha256.Sum256([]byte(s))), Limit: limit}
	return r
}
