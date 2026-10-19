package tool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

// OutputReceipt is internal result metadata, never inferred from body text.
// Revalidate it at runtime boundaries, and always enforce an absolute size cap.
type OutputReceipt struct {
	Digest string `json:"digest"`
	Limit  int    `json:"limit"`
}

// Preserve Read's established line-pagination budget at both runtime boundaries.
// The extra allowance is fixed and selected by the registered tool name, never
// by an output marker or an untrusted receipt.
func CapToolResult(name string, tc *ToolContext, r Result) Result {
	if name == "Read" {
		copyTC := ToolContext{}
		if tc != nil {
			copyTC = *tc
		}
		copyTC.MaxOutputChars = maxOut(tc) + readOutputBonus + 384
		return CapResult(&copyTC, r)
	}
	return CapResult(tc, r)
}

func CapResult(tc *ToolContext, r Result) Result {
	limit := maxOut(tc)
	var s string
	count, size := 0, 0
	for _, b := range r.Content {
		if b.Type == llm.BlockText {
			s = b.Text
			count++
			size += len(b.Text)
		}
	}
	if count > 1 {
		var text strings.Builder
		text.Grow(size)
		for _, b := range r.Content {
			if b.Type == llm.BlockText {
				text.WriteString(b.Text)
			}
		}
		s = text.String()
	}
	digest := outputDigest(s)
	if len(s) > limit+512 || (r.Captured == nil || r.Captured.Digest != digest || r.Captured.Limit > limit) {
		if len(s) > limit {
			s = CaptureOnce(tc, s)
			// Bound trailers too; a very long filesystem path cannot inflate output.
			if len(s) > limit+512 {
				s = truncate(s, limit)
			}
			digest = outputDigest(s)
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
	if r.Captured == nil || r.Captured.Digest != digest || r.Captured.Limit != limit {
		r.Captured = &OutputReceipt{Digest: digest, Limit: limit}
	}
	return r
}

func outputDigest(s string) string {
	// Hash without allocating a second copy of the complete output string.
	h := sha256.New()
	var buf [4096]byte
	for len(s) > 0 {
		n := copy(buf[:], s)
		h.Write(buf[:n])
		s = s[n:]
	}
	var sum [sha256.Size]byte
	return hex.EncodeToString(h.Sum(sum[:0]))
}
