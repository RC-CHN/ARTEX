package llm

import "context"

// IncompleteResponseReason identifies an unusable provider response without
// retaining any prompt, response payload, tool argument or upstream error text.
type IncompleteResponseReason string

const (
	IncompleteMissingTerminal  IncompleteResponseReason = "missing_terminal"
	IncompleteMalformedFrame   IncompleteResponseReason = "malformed_frame"
	IncompleteEmptyOutput      IncompleteResponseReason = "empty_output"
	IncompleteInvalidToolInput IncompleteResponseReason = "invalid_tool_input"
	IncompleteStreamRead       IncompleteResponseReason = "stream_read_error"
	IncompleteRetryRequest     IncompleteResponseReason = "retry_request_error"
	IncompleteUpstreamError    IncompleteResponseReason = "upstream_error"
)

// IncompleteResponseError is not a successful assistant turn. Callers must keep
// earlier tool results and must not replay an entire run over those effects.
// StopReason and Diagnostics contain protocol metadata only.
type IncompleteResponseError struct {
	Reason      IncompleteResponseReason
	StopReason  string
	Diagnostics StreamDiagnostics
	// cause is for machine classification (e.g. quota exhaustion) only. Never
	// format or persist it: upstream errors may contain private provider text.
	cause error
}

func (e *IncompleteResponseError) Unwrap() error { return e.cause }

func (e *IncompleteResponseError) Error() string {
	switch e.Reason {
	case IncompleteMissingTerminal:
		return "model: incomplete response: stream ended without a terminal marker"
	case IncompleteMalformedFrame:
		return "model: incomplete response: malformed protocol frame"
	case IncompleteEmptyOutput:
		return "model: incomplete response: no answer text or tool call"
	case IncompleteInvalidToolInput:
		return "model: incomplete response: invalid or unfinished tool input"
	case IncompleteStreamRead:
		return "model: incomplete response: stream read failed"
	case IncompleteRetryRequest:
		return "model: incomplete response: retry request failed"
	case IncompleteUpstreamError:
		return "model: incomplete response: upstream signaled an error"
	default:
		return "model: incomplete response"
	}
}

// Once an incomplete response enters this adapter's bounded retry loop, a
// later establishment failure must not escape as a fresh generic model error:
// outer routers could otherwise restart the request or the whole worker run.
func incompleteRetryRequestError(ctx context.Context, err error, attempt int) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if attempt == 0 {
		return err
	}
	return &IncompleteResponseError{Reason: IncompleteRetryRequest, Diagnostics: StreamDiagnostics{StreamEnd: StreamEndRequestError}, cause: err}
}
