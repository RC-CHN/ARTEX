package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
)

type anthropicProvider struct{ cfg Config }

type anthropicSystemBlock struct {
	Type         string         `json:"type"`
	Text         string         `json:"text"`
	CacheControl map[string]any `json:"cache_control,omitempty"`
}

type anthropicReq struct {
	Model         string                 `json:"model"`
	System        []anthropicSystemBlock `json:"system,omitempty"`
	Messages      []Message              `json:"messages"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	MaxTokens     int                    `json:"max_tokens"`
	Temperature   *float64               `json:"temperature,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Thinking      *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig  *anthropicOutputConfig `json:"output_config,omitempty"`
	Stream        bool                   `json:"stream"`
}

// anthropicThinking is the "thinking":{"type":"enabled"|"disabled"} switch.
type anthropicThinking struct {
	Type string `json:"type"`
}

// anthropicOutputConfig carries the reasoning strength via
// "output_config":{"effort":"low"|"medium"|"high"|"max"}.
type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

func (p *anthropicProvider) buildBody(req CompletionRequest, stream bool) ([]byte, error) {
	maxTok := req.MaxTokens
	if maxTok == 0 {
		maxTok = 8192
	}
	// Effective thinking mode: a non-empty per-request override (req.Thinking)
	// wins over Config.ThinkingType. Thinking-type content blocks may be replayed
	// only when thinking is actively "enabled"; any other mode (incl. "disabled")
	// strips them so Anthropic does not reject the request.
	thinkingType := p.cfg.ThinkingType
	thinkingEnabled := thinkingType != ""
	if req.Thinking != "" {
		thinkingType = req.Thinking
		thinkingEnabled = req.Thinking == "enabled"
	}
	body := anthropicReq{
		Model:         p.cfg.Model,
		Messages:      mergeAdjacentSameRole(filterThinkingBlocks(req.Messages, thinkingEnabled)),
		MaxTokens:     maxTok,
		Temperature:   req.Temperature,
		StopSequences: req.Stop,
		Stream:        stream,
		System:        buildAnthropicSystem(req),
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	if thinkingType != "" {
		body.Thinking = &anthropicThinking{Type: thinkingType}
	}
	if p.cfg.ReasoningEffort != "" {
		body.OutputConfig = &anthropicOutputConfig{Effort: p.cfg.ReasoningEffort}
	}
	return json.Marshal(body)
}

// buildAnthropicSystem splits the system prompt at DynamicBoundary, placing a
// cache breakpoint on the static prefix (FR-02.2).
func buildAnthropicSystem(req CompletionRequest) []anthropicSystemBlock {
	if len(req.System) == 0 {
		return nil
	}
	b := req.DynamicBoundary
	if b <= 0 {
		// No boundary set → no cache breakpoint.
		return []anthropicSystemBlock{{Type: "text", Text: joinSystem(req.System)}}
	}
	if b >= len(req.System) {
		// Boundary at/after the end → the whole system prompt is static; cache it
		// all in one block (an all-static prompt should still be cacheable).
		return []anthropicSystemBlock{{Type: "text", Text: joinSystem(req.System), CacheControl: map[string]any{"type": "ephemeral"}}}
	}
	return []anthropicSystemBlock{
		{Type: "text", Text: joinSystem(req.System[:b]), CacheControl: map[string]any{"type": "ephemeral"}},
		{Type: "text", Text: joinSystem(req.System[b:])},
	}
}

func (p *anthropicProvider) Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		body, err := p.buildBody(req, true)
		if err != nil {
			yield(StreamEvent{}, err)
			return
		}
		for attempt := 0; ; attempt++ {
			resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/v1/messages", body, func(r *http.Request) {
				r.Header.Set("content-type", "application/json")
				r.Header.Set("x-api-key", p.cfg.APIKey)
				r.Header.Set("anthropic-version", p.cfg.APIVersion)
				r.Header.Set("accept", "text/event-stream")
			}, "anthropic")
			if err != nil {
				if !yield(StreamEvent{Type: SEStreamDiagnostics, Diagnostics: &StreamDiagnostics{StreamEnd: StreamEndRequestError}}, nil) {
					return
				}
				yield(StreamEvent{}, incompleteRetryRequestError(ctx, err, attempt))
				return
			}
			scan := newSSEScanner(resp.Body)
			diagnostics := StreamDiagnostics{}
			var stopReason string
			var terminalDelta *StreamEvent
			var incomplete IncompleteResponseReason
			var streamErr error
			var upstreamCause error
			committed, meaningful, observed, consumerStopped := false, false, false, false
			messageStartSeen := false
			var meter anthropicMeter
			var initialUsage Usage
			pendingTool := false
			var toolInput strings.Builder
			validToolInput := func() bool {
				raw := strings.TrimSpace(toolInput.String())
				return !pendingTool || raw == "" || strings.HasPrefix(raw, "{") && json.Valid([]byte(raw))
			}
			for {
				_, data, serr := scan.next()
				if serr == io.EOF {
					diagnostics.StreamEnd = StreamEndEOF
					break
				}
				if serr != nil {
					diagnostics.StreamEnd = StreamEndReadError
					incomplete = IncompleteStreamRead
					break
				}
				if strings.TrimSpace(data) == "" {
					continue
				}
				ev, ok, perr, kind, rawUsage := parseAnthropicFrameKind(data)
				if perr != nil {
					diagnostics.StreamEnd = StreamEndProtocolError
					var malformed *IncompleteResponseError
					if errors.As(perr, &malformed) {
						diagnostics.ParseErrors++
						incomplete = IncompleteMalformedFrame
					} else if observed || attempt > 0 {
						incomplete, upstreamCause = IncompleteUpstreamError, perr
					} else {
						streamErr = perr
					}
					break
				}
				if kind == "content_block_stop" && !validToolInput() {
					diagnostics.StreamEnd = StreamEndProtocolError
					incomplete = IncompleteInvalidToolInput
					break
				}
				if !ok {
					continue
				}
				if ev.Type == SEMessageStart {
					// A compatible gateway may repeat the start header in one HTTP
					// response. Only its first header defines this attempt's initial
					// usage/boundary; message_delta may later correct that estimate.
					// Ignore duplicates before they can finalize a pending tool.
					if messageStartSeen {
						continue
					}
					messageStartSeen = true
					meter.update(rawUsage)
					meter.output = 0 // The start count is a placeholder.
					initialUsage = meter.usage()
					ev.Usage = initialUsage
				}
				if ev.Type == SEMessageDelta {
					// This event is a tool boundary in the harness. Buffer it until
					// the response is validated; input usage already streamed above.
					if ev.StopReason != "" {
						stopReason = ev.StopReason
						diagnostics.FinishReasonSeen = true
					}
					copy := ev
					copy.StopReason = stopReason
					// Correct the initial estimate once per HTTP attempt. A negative
					// difference is intentional when the final input count is lower.
					meter.update(rawUsage)
					copy.Usage = meter.usage()
					copy.Usage.InputTokens -= initialUsage.InputTokens
					copy.Usage.CacheReadTokens -= initialUsage.CacheReadTokens
					copy.Usage.CacheWriteTokens -= initialUsage.CacheWriteTokens
					terminalDelta = &copy
					continue
				}
				if ev.Type == SEMessageStop {
					diagnostics.StreamEnd = StreamEndDone
					break
				}
				if ev.Type != SEToolInputJSON && !validToolInput() {
					diagnostics.StreamEnd = StreamEndProtocolError
					incomplete = IncompleteInvalidToolInput
					break
				}
				switch ev.Type {
				case SEToolUseStart:
					pendingTool = true
					toolInput.Reset()
					committed, meaningful, observed = true, true, true
				case SEToolInputJSON:
					committed, observed = true, true
					toolInput.WriteString(ev.Text)
					if !pendingTool {
						diagnostics.StreamEnd = StreamEndProtocolError
						incomplete = IncompleteInvalidToolInput
					}
				case SETextDelta:
					committed = committed || ev.Text != ""
					observed = observed || ev.Text != ""
					meaningful = meaningful || strings.TrimSpace(ev.Text) != ""
					pendingTool = false
				case SEThinkingDelta, SEThinkingSignature:
					observed = observed || ev.Text != ""
					pendingTool = false
				}
				initialInput := ""
				if ev.Type == SEToolUseStart {
					// Anthropic normally starts with input:{}, then streams JSON.
					// Compatible gateways may put the full input on the start frame.
					initialInput, ev.Text = ev.Text, ""
				}
				if !yield(ev, nil) {
					consumerStopped = true
					break
				}
				if initialInput != "" {
					toolInput.WriteString(initialInput)
					if !yield(StreamEvent{Type: SEToolInputJSON, Text: initialInput}, nil) {
						consumerStopped = true
						break
					}
				}
				if incomplete != "" {
					break
				}
			}
			resp.Body.Close()
			if consumerStopped {
				return
			}
			if incomplete == "" && streamErr == nil {
				if diagnostics.StreamEnd == StreamEndEOF && !diagnostics.FinishReasonSeen {
					incomplete = IncompleteMissingTerminal
				} else if !validToolInput() {
					incomplete = IncompleteInvalidToolInput
				} else if !meaningful && stopReason != "max_tokens" {
					incomplete = IncompleteEmptyOutput
				}
			}
			retryable := incomplete == IncompleteEmptyOutput || incomplete == IncompleteMissingTerminal || incomplete == IncompleteStreamRead
			retryIncomplete := retryable && !committed && isEmptyResponseRetryable(stopReason) && attempt < p.cfg.emptyRetries() && ctx.Err() == nil
			diagnostics.DiscardThinking = retryIncomplete
			// A failed attempt reports its buffered metering corrections via metadata,
			// which cannot dispatch the still-pending tool.
			var pendingUsage Usage
			if terminalDelta != nil {
				pendingUsage = terminalDelta.Usage
			}
			if incomplete == "" && streamErr == nil && ctx.Err() == nil && terminalDelta != nil {
				if !yield(*terminalDelta, nil) {
					return
				}
				pendingUsage = Usage{}
			}
			if !yield(StreamEvent{Type: SEStreamDiagnostics, StopReason: stopReason, Usage: pendingUsage, Diagnostics: &diagnostics}, nil) {
				return
			}
			if ctx.Err() != nil {
				yield(StreamEvent{}, ctx.Err())
				return
			}
			if streamErr != nil {
				yield(StreamEvent{}, streamErr)
				return
			}
			if retryIncomplete {
				if !backoffSleep(ctx, p.cfg.emptyRetryDelay(attempt)) {
					yield(StreamEvent{}, ctx.Err())
					return
				}
				continue
			}
			if incomplete != "" {
				yield(StreamEvent{}, &IncompleteResponseError{Reason: incomplete, StopReason: stopReason, Diagnostics: diagnostics, cause: upstreamCause})
				return
			}
			yield(StreamEvent{Type: SEMessageStop}, nil)
			return
		}
	}
}

// Complete preserves the same bounded incomplete-response handling as Stream.
func (p *anthropicProvider) Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error) {
	body, err := p.buildBody(req, false)
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	var totalUsage Usage
	var lastPartial Message
	for attempt := 0; ; attempt++ {
		resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/v1/messages", body, func(r *http.Request) {
			r.Header.Set("content-type", "application/json")
			r.Header.Set("x-api-key", p.cfg.APIKey)
			r.Header.Set("anthropic-version", p.cfg.APIVersion)
			r.Header.Set("accept", "application/json")
		}, "anthropic")
		if err != nil {
			return lastPartial, "", totalUsage, incompleteRetryRequestError(ctx, err, attempt)
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			if ctx.Err() != nil {
				return lastPartial, "", totalUsage, ctx.Err()
			}
			return lastPartial, "", totalUsage, &IncompleteResponseError{Reason: IncompleteStreamRead, Diagnostics: StreamDiagnostics{StreamEnd: StreamEndReadError}}
		}
		msg, stop, usage, perr := parseAnthropicResponse(raw)
		totalUsage.Add(usage)
		if ctx.Err() != nil {
			return msg, stop, totalUsage, ctx.Err()
		}
		if perr != nil {
			var incomplete *IncompleteResponseError
			if attempt > 0 && !errors.As(perr, &incomplete) {
				return lastPartial, stop, totalUsage, &IncompleteResponseError{Reason: IncompleteRetryRequest, StopReason: stop, Diagnostics: StreamDiagnostics{StreamEnd: StreamEndEOF}, cause: perr}
			}
			return msg, stop, totalUsage, perr
		}
		empty := strings.TrimSpace(msg.Text()) == "" && len(msg.ToolUses()) == 0 && stop != "max_tokens"
		if empty && isEmptyResponseRetryable(stop) && attempt < p.cfg.emptyRetries() {
			lastPartial = msg
			if !backoffSleep(ctx, p.cfg.emptyRetryDelay(attempt)) {
				return msg, stop, totalUsage, ctx.Err()
			}
			continue
		}
		if empty {
			return msg, stop, totalUsage, &IncompleteResponseError{Reason: IncompleteEmptyOutput, StopReason: stop, Diagnostics: StreamDiagnostics{FinishReasonSeen: stop != "", StreamEnd: StreamEndEOF}}
		}
		return msg, stop, totalUsage, nil
	}
}

// parseAnthropicResponse decodes a non-streaming Messages API response into the
// neutral Message model. Anthropic already speaks the content-block shape, so
// text/thinking/tool_use blocks map across directly.
func parseAnthropicResponse(raw []byte) (Message, string, Usage, error) {
	var r struct {
		Type    string `json:"type"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
		Error      *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Message{}, "", Usage{}, &IncompleteResponseError{Reason: IncompleteMalformedFrame, Diagnostics: StreamDiagnostics{StreamEnd: StreamEndProtocolError, ParseErrors: 1}}
	}
	if r.Error != nil || r.Type == "error" {
		msg := "error response"
		if r.Error != nil {
			msg = r.Error.Message
		}
		return Message{}, "", Usage{}, fmt.Errorf("anthropic: %s", msg)
	}
	var usage Usage
	if r.Usage != nil {
		usage = r.Usage.norm()
	}
	var blocks []ContentBlock
	invalidToolInput := false
	for _, b := range r.Content {
		switch b.Type {
		case "text":
			if b.Text != "" {
				blocks = append(blocks, TextBlock(b.Text))
			}
		case "thinking":
			blocks = append(blocks, ContentBlock{Type: BlockThinking, Thinking: b.Thinking, Signature: b.Signature})
		case "tool_use":
			in := b.Input
			if len(in) == 0 {
				in = json.RawMessage("{}")
			}
			if raw := strings.TrimSpace(string(in)); !json.Valid(in) || !strings.HasPrefix(raw, "{") {
				invalidToolInput = true
			}
			blocks = append(blocks, ContentBlock{Type: BlockToolUse, ID: b.ID, Name: b.Name, Input: in})
		}
	}
	msg := Message{Role: RoleAssistant, Content: blocks}
	if invalidToolInput {
		return msg, r.StopReason, usage, &IncompleteResponseError{Reason: IncompleteInvalidToolInput, StopReason: r.StopReason, Diagnostics: StreamDiagnostics{StreamEnd: StreamEndEOF, FinishReasonSeen: r.StopReason != ""}}
	}
	return msg, r.StopReason, usage, nil
}

func parseAnthropicFrame(data string) (StreamEvent, bool, error) {
	ev, ok, err, _, _ := parseAnthropicFrameKind(data)
	return ev, ok, err
}

func parseAnthropicFrameKind(data string) (ev StreamEvent, ok bool, err error, kind string, usage *anthropicUsage) {
	var f struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type     string          `json:"type"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			PartialJSON string `json:"partial_json"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage   *anthropicUsage `json:"usage"`
		Message struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return StreamEvent{}, false, &IncompleteResponseError{Reason: IncompleteMalformedFrame}, "", nil
	}
	switch f.Type {
	case "message_start":
		if f.Message.Usage != nil {
			// Emit the initial input estimate immediately. Stream reconciles any
			// later cumulative usage; the initial output count is a placeholder.
			u := f.Message.Usage.norm()
			u.OutputTokens = 0
			return StreamEvent{Type: SEMessageStart, Usage: u}, true, nil, f.Type, f.Message.Usage
		}
		// The attempt boundary is meaningful even when a compatible gateway
		// omits usage; it clears stale stop evidence without inventing tokens.
		return StreamEvent{Type: SEMessageStart}, true, nil, f.Type, nil
	case "content_block_start":
		if f.ContentBlock.Type == "tool_use" {
			initial := strings.TrimSpace(string(f.ContentBlock.Input))
			var initialObject map[string]json.RawMessage
			if json.Unmarshal(f.ContentBlock.Input, &initialObject) == nil && initialObject != nil && len(initialObject) == 0 {
				initial = ""
			}
			// Text is internal to this parser until Stream emits the start and
			// its initial input as separate normalized events.
			return StreamEvent{Type: SEToolUseStart, ToolID: f.ContentBlock.ID, ToolName: f.ContentBlock.Name, Text: initial}, true, nil, f.Type, nil
		}
		if f.ContentBlock.Type == "text" && f.ContentBlock.Text != "" {
			return StreamEvent{Type: SETextDelta, Text: f.ContentBlock.Text}, true, nil, f.Type, nil
		}
		if f.ContentBlock.Type == "thinking" && f.ContentBlock.Thinking != "" {
			return StreamEvent{Type: SEThinkingDelta, Text: f.ContentBlock.Thinking}, true, nil, f.Type, nil
		}
	case "content_block_delta":
		switch f.Delta.Type {
		case "text_delta":
			return StreamEvent{Type: SETextDelta, Text: f.Delta.Text}, true, nil, f.Type, nil
		case "thinking_delta":
			return StreamEvent{Type: SEThinkingDelta, Text: f.Delta.Thinking}, true, nil, f.Type, nil
		case "signature_delta":
			return StreamEvent{Type: SEThinkingSignature, Text: f.Delta.Signature}, true, nil, f.Type, nil
		case "input_json_delta":
			return StreamEvent{Type: SEToolInputJSON, Text: f.Delta.PartialJSON}, true, nil, f.Type, nil
		}
	case "message_delta":
		ev := StreamEvent{Type: SEMessageDelta, StopReason: f.Delta.StopReason}
		if f.Usage != nil {
			ev.Usage = f.Usage.norm()
		}
		return ev, true, nil, f.Type, f.Usage
	case "message_stop":
		return StreamEvent{Type: SEMessageStop}, true, nil, f.Type, nil
	case "error":
		msg := "stream error"
		if f.Error != nil {
			msg = f.Error.Message
		}
		return StreamEvent{}, false, fmt.Errorf("anthropic: %s", msg), f.Type, nil
	}
	return StreamEvent{}, false, nil, f.Type, nil
}

// filterThinkingBlocks normalizes message history before it is sent to the
// Anthropic Messages API. It strips thinking-type content blocks when thinking
// is disabled (Anthropic rejects replayed thinking blocks without the thinking
// parameter enabled), and drops any message left with no content blocks —
// Anthropic rejects an empty content array. An empty message arrives here two
// ways: a thinking-only turn that collapses to empty once thinking is stripped,
// or an empty/truncated assistant turn (only reasoning, or nothing) that was
// recorded with no text or tool_use. (A thinking-only turn WITH thinking
// enabled keeps its thinking block — that is valid content and must be
// replayed.)
func filterThinkingBlocks(msgs []Message, thinkingEnabled bool) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		if !thinkingEnabled {
			filtered := make([]ContentBlock, 0, len(content))
			for _, b := range content {
				if b.Type != BlockThinking {
					filtered = append(filtered, b)
				}
			}
			content = filtered
		}
		if len(content) == 0 {
			continue // empty content array is rejected by Anthropic — drop it
		}
		if len(content) == len(m.Content) {
			out = append(out, m)
		} else {
			out = append(out, Message{Role: m.Role, Content: content})
		}
	}
	return out
}

// mergeAdjacentSameRole coalesces consecutive same-role messages into one by
// concatenating their content blocks. Anthropic requires messages to alternate
// user/assistant; dropping an empty turn in filterThinkingBlocks can leave two
// adjacent user messages (e.g. a tool result followed by a resume nudge, once
// the empty assistant between them is gone), which the API would reject with
// "roles must alternate". Merging preserves alternation without losing content.
// A fresh content slice is built on merge so the caller's message backing
// arrays are never mutated.
func mergeAdjacentSameRole(msgs []Message) []Message {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 && out[n-1].Role == m.Role {
			merged := make([]ContentBlock, 0, len(out[n-1].Content)+len(m.Content))
			merged = append(merged, out[n-1].Content...)
			merged = append(merged, m.Content...)
			out[n-1].Content = merged
			continue
		}
		out = append(out, m)
	}
	return out
}
