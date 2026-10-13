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
	for _, name := range []string{"nuclei", "vulnx"} {
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
		if len(files) != 1 || files[0].Name() != "SKILL.md" {
			t.Fatalf("partial seed files: %v", files)
		}
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
