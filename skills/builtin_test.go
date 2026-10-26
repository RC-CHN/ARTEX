package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/skill"
)

func TestConcurrentSeedAndNativeInvocation(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := SeedDefaults(dir); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	reg, err := skill.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range builtinNames {
		s, ok := reg.Get(name)
		if !ok || s.Description == "" || s.Compatibility == "" {
			t.Fatalf("missing skill: %s", name)
		}
		raw, _ := json.Marshal(map[string]string{"name": name})
		result, err := reg.Tool().Call(context.Background(), raw, nil)
		if err != nil || result.IsError || len(result.Extra) != 1 {
			t.Fatalf("native Skill failed: %+v %v", result, err)
		}
		files, _ := os.ReadDir(filepath.Join(dir, name))
		if name != "tack" && (len(files) != 1 || files[0].Name() != "SKILL.md") {
			t.Fatalf("partial seed files: %v", files)
		}
	}
}

func TestTackSupportingFilesSurvivePartialSeedAndOperatorEdits(t *testing.T) {
	dir := t.TempDir()
	if err := SeedDefaults(dir); err != nil {
		t.Fatal(err)
	}
	ref := filepath.Join(dir, "tack", "references", "operations.md")
	script := filepath.Join(dir, "tack", "scripts", "select-node-artifact.py")
	if err := os.WriteFile(ref, []byte("operator notes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(script); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaults(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ref); string(b) != "operator notes" {
		t.Fatal("overwrote edited reference")
	}
	if b, err := os.ReadFile(script); err != nil || !strings.Contains(string(b), "def select(") {
		t.Fatal("missing recovered supporting script", err)
	}
	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(dir, "tack", "references")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "tack", "references")); err != nil {
		t.Skip(err)
	}
	if err := SeedDefaults(dir); err == nil {
		t.Fatal("accepted escaping supporting directory")
	}
	if files, _ := os.ReadDir(outside); len(files) != 0 {
		t.Fatal("wrote outside skill directory")
	}
}

func TestSeedPreservesOperatorEditsAndRejectsEscapingDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := SeedDefaults(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "nuclei", "SKILL.md")
	if err := os.WriteFile(path, []byte("operator edited"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaults(dir); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "operator edited" {
		t.Fatal("operator skill overwritten")
	}
	outside := t.TempDir()
	dir = t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "nuclei")); err != nil {
		t.Skip(err)
	}
	if err := SeedDefaults(dir); err == nil {
		t.Fatal("seed escaped configured skill directory")
	}
	if files, _ := os.ReadDir(outside); len(files) != 0 {
		t.Fatal("wrote outside configured skill directory")
	}
}

func TestBundledInstructionsDoNotClaimEveryEnvironmentHasTools(t *testing.T) {
	for _, name := range []string{"nuclei", "vulnx"} {
		b, err := bundled.ReadFile(name + "/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "command -v "+name) || !strings.Contains(string(b), "Runner") {
			t.Fatal("missing environment capability check")
		}
	}
}

func TestUpgradeOnlyUneditedResourceSkill(t *testing.T) {
	dir := t.TempDir()
	old, err := os.ReadFile("testdata/resource-creator-v0128.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "resource-creator"), 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "resource-creator", "SKILL.md")
	if err := os.WriteFile(file, old, 0644); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaults(dir); err != nil {
		t.Fatal(err)
	}
	now, err := os.ReadFile(file)
	if err != nil || !strings.Contains(string(now), "manage_deployments") {
		t.Fatalf("missing upgrade: %v", err)
	}
	custom := append(old, []byte("\noperator note")...)
	if err := os.WriteFile(file, custom, 0644); err != nil {
		t.Fatal(err)
	}
	if err := SeedDefaults(dir); err != nil {
		t.Fatal(err)
	}
	if now, _ = os.ReadFile(file); string(now) != string(custom) {
		t.Fatal("operator edit overwritten")
	}
}
