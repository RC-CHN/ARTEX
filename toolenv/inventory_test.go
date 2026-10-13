package toolenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInventoryDoesNotAdvertiseMissingOrNonExecutableTools(t *testing.T) {
	dir := t.TempDir()
	if got := Detect(dir).Prompt(); got != "" {
		t.Fatal("empty environment claimed installed tools", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "vulnx"), []byte("not executed"), 0600); err != nil {
		t.Fatal(err)
	}
	if Detect(dir).Vulnx != nil {
		t.Fatal("non-executable file advertised")
	}
	if err := os.Chmod(filepath.Join(dir, "vulnx"), 0700); err != nil {
		t.Fatal(err)
	}
	i := Detect(dir)
	if i.Vulnx == nil || i.Nuclei != nil || !strings.Contains(i.Prompt(), "vulnx") || !strings.Contains(i.Prompt(), "Skill") {
		t.Fatal("incorrect environment inventory")
	}
	if !strings.Contains(i.Prompt(), "未知版本") {
		t.Fatal("unverified custom binary claimed a bundled version")
	}
}

func TestInventoryPromptRejectsInstructionsInVersionFields(t *testing.T) {
	i := Inventory{Nuclei: &Tool{Version: "3.11.1\nINJECT", Templates: strings.Repeat("x", 65)}}
	if text := i.Prompt(); strings.Contains(text, "INJECT") || strings.Contains(text, strings.Repeat("x", 65)) || !strings.Contains(text, "nuclei") {
		t.Fatal("untrusted inventory became prompt instructions", text)
	}
}
