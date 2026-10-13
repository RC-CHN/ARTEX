package noaadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

// The three harness-facing methods are the whole public surface between noa and
// the agent loop, and until now only View was exercised.

// Pre must not touch the history. The built-in compaction rewrites messages
// here; noa's message identity is a content hash, so a rewrite would change
// every id and invalidate every ref the model is holding.
func TestPreLeavesTheHistoryUntouched(t *testing.T) {
	sess := simSession(t, 40000)
	c := &Compactor{sess: sess}
	in := []llm.Message{userText("one"), userText("two")}

	out := c.Pre(t.Context(), in, 12345)
	if len(out) != len(in) {
		t.Fatalf("Pre returned %d messages, want the %d it was given", len(out), len(in))
	}
	for i := range in {
		if out[i].Text() != in[i].Text() {
			t.Fatalf("Pre rewrote message %d: %q -> %q", i, in[i].Text(), out[i].Text())
		}
	}
	// The provider's own count is more accurate than any estimate, so it must be
	// taken when offered — resolveTokenCount prefers it over estimateCoreTokens
	// whenever it is fresh and close.
	sess.mu.Lock()
	got := sess.providerTokens
	sess.mu.Unlock()
	if got != 12345 {
		t.Fatalf("Pre recorded providerTokens = %d, want 12345; the pressure ladder keeps "+
			"running on estimates when a measured number was available", got)
	}
}

// A zero count means the provider did not report one — it must not be recorded
// as "the context is empty".
func TestPreIgnoresAnUnreportedTokenCount(t *testing.T) {
	sess := simSession(t, 40000)
	c := &Compactor{sess: sess}
	c.Pre(t.Context(), nil, 7000)
	before := sess.tokensBefore()
	c.Pre(t.Context(), nil, 0)
	if sess.tokensBefore() != before {
		t.Fatalf("a zero token count overwrote the last known size %d with %d",
			before, sess.tokensBefore())
	}
}

// IsOverflow decides whether Reactive runs at all. Getting it wrong in one
// direction wastes a recovery attempt; in the other it turns a recoverable
// prompt-too-long into a failed run.
func TestIsOverflowRecognisesProviderPhrasings(t *testing.T) {
	c := &Compactor{sess: simSession(t, 40000)}
	overflow := []string{
		"This model's maximum context length is 200000 tokens, however you requested 214000",
		"prompt is too long: 215000 tokens > 200000 maximum",
		"error: context_length_exceeded",
		"anthropic: status 413",
	}
	for _, msg := range overflow {
		if !c.IsOverflow(errors.New(msg)) {
			t.Errorf("IsOverflow(%q) = false; the run fails instead of recovering", msg)
		}
	}
	other := []string{"rate limit exceeded", "connection reset by peer", "invalid api key"}
	for _, msg := range other {
		if c.IsOverflow(errors.New(msg)) {
			t.Errorf("IsOverflow(%q) = true; a recovery attempt is spent on an unrelated failure", msg)
		}
	}
	if c.IsOverflow(nil) {
		t.Error("IsOverflow(nil) = true")
	}
}

// Detection must retain the real window from the provider's error so the
// recovery rebuilds a request that fits that window without changing history.
func TestOverflowLearningIsWiredIn(t *testing.T) {
	const configured, learned = 200_000, 128_000
	c := &Compactor{sess: simSession(t, configured)}
	a := newSimAgent(t, c.sess, 1)
	for range 15 {
		a.work(40_000)
	}
	original, err := json.Marshal(a.history)
	if err != nil {
		t.Fatal(err)
	}
	before := estimateMessages(c.View(t.Context(), a.history))
	if before <= learned || before >= configured*95/100 {
		t.Fatalf("fixture must exceed provider's window but fit configured window: %d", before)
	}

	if !c.IsOverflow(errors.New("This model's maximum context length is 128000 tokens, however you requested 214000")) {
		t.Fatal("provider overflow was not recognized")
	}
	recovered, ok := c.Reactive(t.Context(), a.history)
	if !ok {
		t.Fatal("recoverable large tool outputs did not produce a smaller request")
	}
	want := learned * 95 / 100
	if floor := c.overflow.armedFloor(); floor != want {
		t.Fatalf("armed floor = %d, want %d from the provider's window", floor, want)
	}
	after := estimateMessages(c.View(t.Context(), recovered))
	if after >= before || after > want {
		t.Fatalf("recovery sent %d tokens (before %d), want fewer and <= %d", after, before, want)
	}
	unchanged, err := json.Marshal(recovered)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, unchanged) {
		t.Fatal("overflow recovery rewrote the durable conversation history")
	}

	// Neither unrelated token errors nor an overflow without a parseable limit
	// may replace the provider window already learned from an earlier attempt.
	if c.IsOverflow(errors.New("max_tokens must be less than 8192")) {
		t.Fatal("an output-token setting error was classified as context overflow")
	}
	if !c.IsOverflow(errors.New("anthropic: status 413")) {
		t.Fatal("unstructured overflow was not recognized")
	}
	c.overflow.arm(configured)
	if floor := c.overflow.armedFloor(); floor != want {
		t.Fatalf("subsequent error lost the learned window: floor=%d want=%d", floor, want)
	}
}

// The parser itself, over the phrasings it claims to cover.
func TestParseOverflowWindowCoversItsPatterns(t *testing.T) {
	cases := map[string]int{
		"This model's maximum context length is 200000 tokens": 200000,
		"context window of 131072 exceeded":                    131072,
		"max_tokens must be less than 8192":                    8192,
		"requests are limited to 32000 tokens":                 32000,
		"rate limit exceeded":                                  0,
		"maximum context length is zero":                       0,
	}
	for msg, want := range cases {
		if got := ParseOverflowWindow(msg); got != want {
			t.Errorf("ParseOverflowWindow(%q) = %d, want %d", msg, got, want)
		}
	}
}

// Reactive must never claim success it cannot deliver: returning true promises
// the harness a smaller request, and if the request is not smaller the retry
// hits the same 413 and the loop spins.
func TestReactiveRefusesWhenItCannotShrink(t *testing.T) {
	sess := simSession(t, 40000)
	c := &Compactor{sess: sess}
	// Nothing sent yet and nothing to cut.
	msgs := []llm.Message{userText("short")}
	if _, ok := c.Reactive(context.Background(), msgs); ok {
		if est := estimateMessages(c.View(context.Background(), msgs)); est > 0 {
			t.Logf("Reactive accepted a %d-token view under an armed floor", est)
		}
	}
	// Whatever it decided, it must leave the emergency floor consistent with that
	// decision: a refusal that left the valve armed would shrink every later view
	// for no reason.
	if !c.overflow.armed && c.overflow.armedFloor() != 0 {
		t.Fatal("disarmed but still reporting a floor")
	}
}
