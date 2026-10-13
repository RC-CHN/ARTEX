package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/toolenv"
)

func TestNodeEnvironmentPromptIsScopedSnapshottedAndPersisted(t *testing.T) {
	store := &MemoryStore{}
	h := newTestHub(t, store, nil)
	n := registerNode(t, h, defaultCapabilities())
	other := registerNode(t, h, defaultCapabilities())
	if err := h.SetNodeEnvironmentPrompt(n.id, "external-only proxy"); err != nil {
		t.Fatal(err)
	}
	spec := spec("notes")
	spec.NodeID = n.id
	ch := acquireAsync(h, context.Background(), spec)
	cmd := n.command(t, CommandOpen, "")
	result := n.result(cmd)
	result.EnvironmentTools = toolenv.Inventory{Vulnx: &toolenv.Tool{Version: "2.0.2"}}
	n.ack(t, result)
	session := await(t, ch)
	if session.err != nil {
		t.Fatal(session.err)
	}
	if session.s.EnvironmentPrompt() != "external-only proxy" {
		t.Fatal("lost node prompt")
	}
	if err := h.SetNodeEnvironmentPrompt(n.id, "new runs only"); err != nil {
		t.Fatal(err)
	}
	if session.s.EnvironmentPrompt() != "external-only proxy" {
		t.Fatal("active prompt changed")
	}
	tools := session.s.EnvironmentTools()
	if tools.Vulnx == nil || tools.Nuclei != nil {
		t.Fatal("tool inventory was not from this sandbox")
	}
	tools.Vulnx.Version = "mutated"
	if session.s.EnvironmentTools().Vulnx.Version != "2.0.2" {
		t.Fatal("inventory escaped by reference")
	}
	closeSession(t, n, session.s)
	for _, bad := range []string{strings.Repeat("中", 16001), "nul\x00text"} {
		if h.SetNodeEnvironmentPrompt(n.id, bad) == nil {
			t.Fatal("invalid notes accepted")
		}
	}
	_ = h.Close()
	reopened := newTestHub(t, store, nil)
	for _, view := range reopened.ListNodes() {
		if view.ID == n.id && view.EnvironmentPrompt != "new runs only" {
			t.Fatal("notes lost on restart")
		}
		if view.ID == other.id && view.EnvironmentPrompt != "" {
			t.Fatal("node notes leaked to another node")
		}
	}
	if err := reopened.SetNodeEnvironmentPrompt(n.id, ""); err != nil {
		t.Fatal(err)
	}
	for _, view := range reopened.ListNodes() {
		if view.EnvironmentPrompt != "" {
			t.Fatal("cleared notes survived")
		}
	}
}
