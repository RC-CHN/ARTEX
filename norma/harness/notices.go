package harness

import (
	"context"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

type turnNoticesKey struct{}

// WithTurnNotices supplies host notifications at safe model-turn boundaries.
// The source must bound its work and deduplicate delivery. Notifications append
// to the conversation, never mutate system instructions or interrupt tools.
func WithTurnNotices(ctx context.Context, source func(context.Context, []llm.Message) string) context.Context {
	return context.WithValue(ctx, turnNoticesKey{}, source)
}

func (l *loop) injectHostNotices() bool {
	source, _ := l.ctx.Value(turnNoticesKey{}).(func(context.Context, []llm.Message) string)
	if source == nil || l.ctx.Err() != nil || l.settling.Load() {
		return false
	}
	text := source(l.ctx, l.messages)
	if text == "" {
		return false
	}
	message := llm.UserText(text)
	l.messages = append(l.messages, message)
	l.record(message, llm.Usage{})
	return true
}
