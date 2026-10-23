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

type openaiProvider struct{ cfg Config }

type oaMessage struct {
	Role             string       `json:"role"`
	Content          string       `json:"content"` // empty tool results still require this field
	ReasoningContent string       `json:"reasoning_content,omitempty"`
	ToolCalls        []oaToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string       `json:"tool_call_id,omitempty"`
}

type oaToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    int    `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type oaThinking struct {
	Type string `json:"type"`
}

type oaReq struct {
	Model    string      `json:"model"`
	Messages []oaMessage `json:"messages"`
	Tools    []oaTool    `json:"tools,omitempty"`
	// MaxTokens and MaxCompletionTokens are mutually exclusive: buildBody fills
	// exactly one of them per Config.MaxTokensField, and omitempty drops the
	// other. Sending both would make OpenAI reject the request.
	MaxTokens           int           `json:"max_tokens,omitempty"`
	MaxCompletionTokens int           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64      `json:"temperature,omitempty"`
	Stop                []string      `json:"stop,omitempty"`
	Thinking            *oaThinking   `json:"thinking,omitempty"`
	ReasoningEffort     string        `json:"reasoning_effort,omitempty"`
	Stream              bool          `json:"stream"`
	StreamOptions       *oaStreamOpts `json:"stream_options,omitempty"`
}

type oaStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

// toOpenAIMessages flattens the unified model into OpenAI's role-based format:
// assistant tool_use → tool_calls; user-embedded tool_result → standalone
// role:tool messages; system prompt → leading system message (FR-03.6).
func toOpenAIMessages(system string, msgs []Message) []oaMessage {
	out := make([]oaMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, oaMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			om := oaMessage{Role: "assistant"}
			var text strings.Builder
			var reasoning strings.Builder
			for _, b := range m.Content {
				switch b.Type {
				case BlockText:
					text.WriteString(b.Text)
				case BlockThinking:
					// DeepSeek-style thinking mode requires the previous turn's
					// reasoning_content to be passed back verbatim; dropping it
					// makes the API reject the request with a 400. Thinking text
					// lives in b.Thinking (not b.Text) — see ContentBlock.
					reasoning.WriteString(b.Thinking)
				case BlockToolUse:
					tc := oaToolCall{ID: b.ID, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = string(b.Input)
					if tc.Function.Arguments == "" {
						tc.Function.Arguments = "{}"
					}
					om.ToolCalls = append(om.ToolCalls, tc)
				}
			}
			om.Content = text.String()
			if s := reasoning.String(); s != "" {
				om.ReasoningContent = s
			}
			// OpenAI requires every assistant message to carry content or
			// tool_calls; Content's omitempty would otherwise serialize a
			// content-less turn to a bare {"role":"assistant"} and the API 400s
			// with "content or tool_calls must be set". Rather than drop such a
			// turn, keep it with a minimal placeholder: a thinking-only turn must
			// stay so its reasoning_content is passed back (DeepSeek-style
			// thinking mode requires it), and keeping the assistant turn also
			// preserves user/assistant alternation for gateways that enforce it
			// (dropping it would leave two adjacent user messages).
			if om.Content == "" && len(om.ToolCalls) == 0 {
				om.Content = "…"
			}
			out = append(out, om)
		case RoleUser:
			var text strings.Builder
			var toolMsgs []oaMessage
			for _, b := range m.Content {
				switch b.Type {
				case BlockText:
					text.WriteString(b.Text)
				case BlockToolResult:
					toolMsgs = append(toolMsgs, oaMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: flattenText(b.Content)})
				}
			}
			out = append(out, toolMsgs...)
			if s := text.String(); s != "" {
				out = append(out, oaMessage{Role: "user", Content: s})
			}
		}
	}
	return out
}

// reasoningElidedPlaceholder stands in for a thinking turn's reasoning_content
// when it was lost upstream (context compaction) yet the turn still replays
// tool_calls. Sending a minimal non-empty value keeps the request valid for
// providers that only require the field to be present; the real content is
// preserved by the compaction layer, so this path is a last resort.
const reasoningElidedPlaceholder = "(reasoning elided by context compaction)"

// sanitizeOpenAIMessages enforces, at the provider boundary, the two structural
// invariants OpenAI-compatible endpoints validate — regardless of what produced
// req.Messages (noa compaction, the built-in compactor, or a host that assembled
// them directly):
//
//   - Every assistant tool_call is answered in the immediately following run of
//     role:tool messages, and each result answers one call exactly once. A half-pair in
//     either direction is a 400 ("tool_calls must be followed by tool messages"
//     / an unexpected tool message).
//   - In thinking mode, an assistant message replaying tool_calls carries
//     reasoning_content. If a gap slips through despite the upstream repair, a
//     minimal placeholder is sent rather than letting a strict provider reject
//     the bare tool_calls turn.
//
// It is a defensive backstop, not the primary repair: the fixes that keep the
// real content whole upstream should mean the placeholder path never runs. The
// input slice is mutated in place and also returned.
func sanitizeOpenAIMessages(msgs []oaMessage, thinking bool) []oaMessage {
	// Only contiguous tool messages after this assistant can answer its calls.
	// A result beyond another user/assistant turn (or in an earlier exchange)
	// does not make a call valid, even when the ID occurs elsewhere in history.
	acceptedResults := make(map[int]bool)
	for i := range msgs {
		if msgs[i].Role != "assistant" || len(msgs[i].ToolCalls) == 0 {
			continue
		}
		pending := make(map[string]bool)
		for _, tc := range msgs[i].ToolCalls {
			if tc.ID != "" {
				pending[tc.ID] = true
			}
		}
		answered := make(map[string]bool)
		for j := i + 1; j < len(msgs) && msgs[j].Role == "tool"; j++ {
			id := msgs[j].ToolCallID
			if pending[id] {
				answered[id] = true
				acceptedResults[j] = true
				delete(pending, id)
			}
		}
		kept := make([]oaToolCall, 0, len(answered))
		for _, tc := range msgs[i].ToolCalls {
			if answered[tc.ID] {
				kept = append(kept, tc)
				delete(answered, tc.ID)
			}
		}
		msgs[i].ToolCalls = kept
		if thinking && len(kept) > 0 && msgs[i].ReasoningContent == "" {
			msgs[i].ReasoningContent = reasoningElidedPlaceholder
		}
		// An assistant message must carry content or tool_calls; a turn emptied by
		// the drop above would serialize to a bare {"role":"assistant"} and 400.
		if msgs[i].Content == "" && len(msgs[i].ToolCalls) == 0 {
			msgs[i].Content = "…"
		}
	}
	// Drop orphaned, out-of-order, duplicate and empty-ID results.
	out := make([]oaMessage, 0, len(msgs))
	for i, m := range msgs {
		if m.Role == "tool" && !acceptedResults[i] {
			continue
		}
		out = append(out, m)
	}
	return out
}

func flattenText(blocks []ContentBlock) string {
	var s strings.Builder
	for _, b := range blocks {
		if b.Type == BlockText {
			s.WriteString(b.Text)
		}
	}
	return s.String()
}

func (p *openaiProvider) buildBody(req CompletionRequest, stream bool) ([]byte, error) {
	// A non-empty per-request override (req.Thinking) wins over Config.ThinkingType.
	thinkingType := p.cfg.ThinkingType
	if req.Thinking != "" {
		thinkingType = req.Thinking
	}
	// The request is in thinking mode when either a thinking type is set (a
	// `thinking` body field goes out) or a reasoning effort is configured — the
	// same providers that then demand reasoning_content be echoed back.
	thinking := thinkingType != "" || p.cfg.ReasoningEffort != ""

	msgs := toOpenAIMessages(joinSystem(req.System), req.Messages)
	msgs = sanitizeOpenAIMessages(msgs, thinking)

	body := oaReq{
		Model:       p.cfg.Model,
		Messages:    msgs,
		Temperature: req.Temperature,
		Stop:        req.Stop,
		Stream:      stream,
	}
	// The output cap goes into exactly one key. Reasoning models accept only
	// "max_completion_tokens"; everything else still takes "max_tokens", which
	// stays the default so existing configs are untouched.
	if p.cfg.MaxTokensField == MaxTokensFieldCompletion {
		body.MaxCompletionTokens = req.MaxTokens
	} else {
		body.MaxTokens = req.MaxTokens
	}
	// stream_options.include_usage is a streaming-only field; some gateways reject
	// it on a non-streaming request, so send it only when streaming.
	if stream {
		body.StreamOptions = &oaStreamOpts{IncludeUsage: true}
	}
	if thinkingType != "" {
		body.Thinking = &oaThinking{Type: thinkingType}
	}
	body.ReasoningEffort = p.cfg.ReasoningEffort // omitempty drops it when unset
	for _, t := range req.Tools {
		var ot oaTool
		ot.Type = "function"
		ot.Function.Name = t.Name
		ot.Function.Description = t.Description
		ot.Function.Parameters = t.InputSchema
		body.Tools = append(body.Tools, ot)
	}
	return json.Marshal(body)
}

// emptyResponseRetries is the DEFAULT bound on how many times an empty or
// reasoning-only normal stop is re-requested before an incomplete error (override per
// Config with EmptyResponseRetries). Kept low and separate from the
// network-level retries() knob: an empty-response retry re-sends the whole
// prompt (expensive on large contexts), and the failure is effectively binary —
// a transient parser/sampling glitch clears on the next attempt, a deterministic
// stall never does — so a third+ attempt mostly burns tokens.
const emptyResponseRetries = 2

// isEmptyResponseRetryable reports whether a completed response that produced no
// answer text or tool calls should be retried. Only a normal stop qualifies: a max_tokens
// truncation is the harness's to escalate (retrying would fight that), and any
// stop that actually carried answer text or tools never reaches this check.
func isEmptyResponseRetryable(stopReason string) bool {
	return stopReason == "end_turn" || stopReason == ""
}

func (p *openaiProvider) Stream(ctx context.Context, req CompletionRequest) iter.Seq2[StreamEvent, error] {
	return func(yield func(StreamEvent, error) bool) {
		body, err := p.buildBody(req, true)
		if err != nil {
			if !yield(StreamEvent{Type: SEStreamDiagnostics, Diagnostics: &StreamDiagnostics{StreamEnd: StreamEndRequestError}}, nil) {
				return
			}
			yield(StreamEvent{}, err)
			return
		}
		for attempt := 0; ; attempt++ {
			resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/chat/completions", body, func(r *http.Request) {
				r.Header.Set("content-type", "application/json")
				r.Header.Set("authorization", "Bearer "+p.cfg.APIKey)
				r.Header.Set("accept", "text/event-stream")
			}, "openai")
			if err != nil {
				if !yield(StreamEvent{Type: SEStreamDiagnostics, Diagnostics: &StreamDiagnostics{StreamEnd: StreamEndRequestError}}, nil) {
					return
				}
				yield(StreamEvent{}, incompleteRetryRequestError(ctx, err, attempt))
				return
			}
			if attempt > 0 && !yield(StreamEvent{Type: SEMessageStart}, nil) {
				resp.Body.Close()
				return
			}

			// Reasoning alone is not an answer or an action. Empty stops and stream
			// interruptions may retry this exact request within the existing
			// provider retry bound, but never after text or any tool fragment.
			scan := newSSEScanner(resp.Body)
			started := map[int]bool{}
			var stopReason string
			var usage Usage
			diagnostics := StreamDiagnostics{}
			committed, meaningful, observed := false, false, false
			consumerStopped := false
			var streamErr error
			var upstreamCause error
			var incomplete IncompleteResponseReason
			var pendingTool bool
			var toolInput strings.Builder
			validToolInput := func() bool {
				raw := strings.TrimSpace(toolInput.String())
				return !pendingTool || raw == "" || json.Valid([]byte(raw))
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
				data = strings.TrimSpace(data)
				if data == "" {
					continue
				}
				if data == "[DONE]" {
					diagnostics.StreamEnd = StreamEndDone
					break
				}
				evs, sr, u, ok, malformed, perr := parseOpenAIFrameDiagnostics(data, started)
				if malformed {
					diagnostics.ParseErrors++
					diagnostics.StreamEnd = StreamEndProtocolError
					incomplete = IncompleteMalformedFrame
					break
				}
				if sr != "" {
					stopReason = sr
					diagnostics.FinishReasonSeen = true
				}
				if ok {
					usage = u
				}
				if perr != nil {
					diagnostics.StreamEnd = StreamEndProtocolError
					if observed || attempt > 0 {
						incomplete, upstreamCause = IncompleteUpstreamError, perr
					} else {
						streamErr = perr
					}
					break
				}
				for _, ev := range evs {
					// Any non-input event would make the harness dispatch the pending
					// tool. Reject malformed input before exposing that boundary.
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
					case SETextDelta:
						committed, observed = true, true
						meaningful = meaningful || strings.TrimSpace(ev.Text) != ""
						pendingTool = false
					case SEThinkingDelta:
						observed = observed || ev.Text != ""
						pendingTool = false
					}
					if !yield(ev, nil) {
						consumerStopped = true
						break
					}
				}
				if consumerStopped || incomplete != "" {
					break
				}
			}
			resp.Body.Close()

			if consumerStopped {
				// Go iterators may not yield after the consumer returns false. The
				// recorder observes that boundary itself as consumer_stop.
				return
			}
			if incomplete == "" && streamErr == nil && diagnostics.StreamEnd == StreamEndEOF && !diagnostics.FinishReasonSeen {
				incomplete = IncompleteMissingTerminal
			}
			if incomplete == "" && streamErr == nil && !validToolInput() {
				incomplete = IncompleteInvalidToolInput
			}
			if incomplete == "" && streamErr == nil && !meaningful && stopReason != "max_tokens" {
				incomplete = IncompleteEmptyOutput
			}
			retryableIncomplete := incomplete == IncompleteEmptyOutput || incomplete == IncompleteMissingTerminal || incomplete == IncompleteStreamRead
			retryIncomplete := retryableIncomplete && !committed && isEmptyResponseRetryable(stopReason) && attempt < p.cfg.emptyRetries() && ctx.Err() == nil
			diagnostics.DiscardThinking = retryIncomplete
			// Usage and stop evidence must precede an error without finishing a
			// pending tool. The successful terminal delta does not repeat usage.
			if !yield(StreamEvent{Type: SEStreamDiagnostics, Diagnostics: &diagnostics, Usage: usage, StopReason: stopReason}, nil) {
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
				streamErr = &IncompleteResponseError{Reason: incomplete, StopReason: stopReason, Diagnostics: diagnostics, cause: upstreamCause}
				yield(StreamEvent{}, streamErr)
				return
			}
			if !yield(StreamEvent{Type: SEMessageDelta, StopReason: stopReason}, nil) {
				return
			}
			yield(StreamEvent{Type: SEMessageStop}, nil)
			return
		}
	}
}

// Complete performs a real non-streaming completion: it sends stream:false and
// parses the single JSON response body into a full assistant Message. It returns
// the assembled message, the normalized stop reason, and token usage. A 200
// response whose body is an error object (some gateways do this instead of a 4xx)
// is surfaced as an error rather than an empty completion.
func (p *openaiProvider) Complete(ctx context.Context, req CompletionRequest) (Message, string, Usage, error) {
	body, err := p.buildBody(req, false)
	if err != nil {
		return Message{}, "", Usage{}, err
	}
	var totalUsage Usage
	var lastPartial Message
	for attempt := 0; ; attempt++ {
		resp, err := doStream(ctx, p.cfg, p.cfg.BaseURL+"/chat/completions", body, func(r *http.Request) {
			r.Header.Set("content-type", "application/json")
			r.Header.Set("authorization", "Bearer "+p.cfg.APIKey)
			r.Header.Set("accept", "application/json")
		}, "openai")
		if err != nil {
			return lastPartial, "", totalUsage, incompleteRetryRequestError(ctx, err, attempt)
		}
		raw, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			if ctx.Err() != nil {
				return Message{}, "", totalUsage, ctx.Err()
			}
			return Message{}, "", totalUsage, &IncompleteResponseError{Reason: IncompleteStreamRead, Diagnostics: StreamDiagnostics{StreamEnd: StreamEndReadError}}
		}
		msg, stop, usage, perr := parseOpenAIResponse(raw)
		totalUsage.Add(usage)
		if ctx.Err() != nil {
			return msg, stop, totalUsage, ctx.Err()
		}
		if perr != nil {
			var incomplete *IncompleteResponseError
			if attempt > 0 && !errors.As(perr, &incomplete) {
				// Some gateways return HTTP 200 with an error object. This is
				// still failure of the bounded recovery, not a new outer retry.
				return lastPartial, stop, totalUsage, &IncompleteResponseError{Reason: IncompleteRetryRequest, StopReason: stop, Diagnostics: StreamDiagnostics{StreamEnd: StreamEndEOF}, cause: perr}
			}
			return msg, stop, totalUsage, perr
		}
		empty := strings.TrimSpace(msg.Text()) == "" && len(msg.ToolUses()) == 0 && stop != "max_tokens"
		// No tools or answer exist in this atomic response, so retrying the same
		// request cannot replay a tool effect. Thinking does not satisfy an answer.
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

// parseOpenAIResponse decodes a non-streaming Chat Completions response body into
// the neutral Message model. reasoning_content/reasoning (a reasoning model's
// thinking) is mapped to a thinking block; content to a text block; tool_calls to
// tool_use blocks — the same shape the streaming path accumulates.
func parseOpenAIResponse(raw []byte) (Message, string, Usage, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content          string       `json:"content"`
				ReasoningContent string       `json:"reasoning_content"`
				Reasoning        string       `json:"reasoning"`
				ReasoningText    string       `json:"reasoning_text"`
				Refusal          string       `json:"refusal"`
				ToolCalls        []oaToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Message{}, "", Usage{}, &IncompleteResponseError{Reason: IncompleteMalformedFrame, Diagnostics: StreamDiagnostics{StreamEnd: StreamEndProtocolError, ParseErrors: 1}}
	}
	if r.Error != nil {
		return Message{}, "", Usage{}, fmt.Errorf("openai: %s", r.Error.Message)
	}
	var usage Usage
	if r.Usage != nil {
		usage = Usage{
			InputTokens:     r.Usage.PromptTokens,
			OutputTokens:    r.Usage.CompletionTokens,
			CacheReadTokens: r.Usage.PromptTokensDetails.CachedTokens,
		}
	}
	if len(r.Choices) == 0 {
		return Message{}, "", usage, &IncompleteResponseError{Reason: IncompleteEmptyOutput, Diagnostics: StreamDiagnostics{StreamEnd: StreamEndEOF}}
	}
	ch := r.Choices[0]
	var blocks []ContentBlock
	if think := pickReasoning(ch.Message.ReasoningContent, ch.Message.Reasoning, ch.Message.ReasoningText); think != "" {
		blocks = append(blocks, ContentBlock{Type: BlockThinking, Thinking: think})
	}
	if ch.Message.Content != "" {
		blocks = append(blocks, TextBlock(ch.Message.Content))
	} else if ch.Message.Refusal != "" {
		// A refused turn carries content:null and the reason in `refusal`. Reading
		// only content would hand the caller an empty assistant turn — the model
		// did answer, it just declined.
		blocks = append(blocks, TextBlock(ch.Message.Refusal))
	}
	invalidToolInput := false
	for _, tc := range ch.Message.ToolCalls {
		in := tc.Function.Arguments
		if strings.TrimSpace(in) == "" {
			in = "{}"
		}
		if !json.Valid([]byte(in)) {
			invalidToolInput = true
		}
		blocks = append(blocks, ContentBlock{Type: BlockToolUse, ID: tc.ID, Name: tc.Function.Name, Input: []byte(in)})
	}
	msg, stop := Message{Role: RoleAssistant, Content: blocks}, mapOpenAIFinish(ch.FinishReason)
	if invalidToolInput {
		return msg, stop, usage, &IncompleteResponseError{Reason: IncompleteInvalidToolInput, StopReason: stop, Diagnostics: StreamDiagnostics{FinishReasonSeen: stop != "", StreamEnd: StreamEndEOF}}
	}
	return msg, stop, usage, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// pickReasoning returns the thinking text a Chat Completions payload carries,
// taking the first non-empty field in priority order. Gateways disagree on the
// name: reasoning_content is the DeepSeek style; `reasoning` is what OpenRouter
// and vLLM's reasoning parsers emit; `reasoning_text` shows up on some others.
// Reading only one silently drops a whole turn of thinking — and when the model
// puts everything there (an unclosed </think>), the turn decodes to an empty
// message that looks like the model just said nothing. Trying them in order also
// deduplicates: a gateway that echoes the same text in two fields (e.g. chutes.ai
// sends reasoning_content AND reasoning) contributes it once, not twice.
func pickReasoning(fields ...string) string {
	for _, f := range fields {
		if f != "" {
			return f
		}
	}
	return ""
}

func parseOpenAIFrame(data string, started map[int]bool) (evs []StreamEvent, stopReason string, usage Usage, hasUsage bool) {
	evs, stopReason, usage, hasUsage, _, _ = parseOpenAIFrameDiagnostics(data, started)
	return
}

// Decode once so always-on diagnostics do not add another JSON parse per token.
func parseOpenAIFrameDiagnostics(data string, started map[int]bool) (evs []StreamEvent, stopReason string, usage Usage, hasUsage, malformed bool, upstreamErr error) {
	var f struct {
		Choices []struct {
			Delta struct {
				Content          string       `json:"content"`
				ReasoningContent string       `json:"reasoning_content"`
				Reasoning        string       `json:"reasoning"`
				ReasoningText    string       `json:"reasoning_text"`
				Refusal          string       `json:"refusal"`
				ToolCalls        []oaToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &f); err != nil {
		return nil, "", Usage{}, false, true, nil
	}
	if f.Usage != nil {
		usage = Usage{InputTokens: f.Usage.PromptTokens, OutputTokens: f.Usage.CompletionTokens, CacheReadTokens: f.Usage.PromptTokensDetails.CachedTokens}
		hasUsage = true
	}
	if f.Error != nil {
		// An explicit API failure is not an empty content chunk, even if the
		// gateway later sends [DONE]. Retain classification details only as the
		// private cause when Stream has already exposed output or retried.
		var code string
		_ = json.Unmarshal(f.Error.Code, &code)
		return nil, "", usage, hasUsage, false, fmt.Errorf("openai: %s (type=%s code=%s)", f.Error.Message, f.Error.Type, code)
	}
	if len(f.Choices) == 0 {
		return nil, "", usage, hasUsage, false, nil
	}
	ch := f.Choices[0]
	if think := pickReasoning(ch.Delta.ReasoningContent, ch.Delta.Reasoning, ch.Delta.ReasoningText); think != "" {
		evs = append(evs, StreamEvent{Type: SEThinkingDelta, Text: think})
	}
	if ch.Delta.Content != "" {
		evs = append(evs, StreamEvent{Type: SETextDelta, Text: ch.Delta.Content})
	}
	if ch.Delta.Refusal != "" {
		// See parseOpenAIResponse: a refusal streams in `refusal`, not `content`.
		evs = append(evs, StreamEvent{Type: SETextDelta, Text: ch.Delta.Refusal})
	}
	for _, tc := range ch.Delta.ToolCalls {
		if !started[tc.Index] && (tc.ID != "" || tc.Function.Name != "") {
			started[tc.Index] = true
			evs = append(evs, StreamEvent{Type: SEToolUseStart, ToolID: tc.ID, ToolName: tc.Function.Name})
		}
		if tc.Function.Arguments != "" {
			evs = append(evs, StreamEvent{Type: SEToolInputJSON, Text: tc.Function.Arguments})
		}
	}
	if ch.FinishReason != "" {
		stopReason = mapOpenAIFinish(ch.FinishReason)
	}
	return evs, stopReason, usage, hasUsage, false, nil
}

func mapOpenAIFinish(r string) string {
	switch r {
	case "tool_calls":
		return "tool_use"
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		return r
	}
}
