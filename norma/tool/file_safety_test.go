package tool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/norma/llm"
)

func TestFileToolConcurrentProcesses(t *testing.T) {
	if path := os.Getenv("ARTEX_FILE_EDIT_PROBE"); path != "" {
		key := os.Getenv("ARTEX_FILE_EDIT_KEY")
		raw, _ := json.Marshal(map[string]any{"file_path": path, "old_string": key + "=old", "new_string": key + "=new"})
		res, err := NewEdit().Call(t.Context(), raw, nil)
		if err != nil || res.IsError {
			t.Fatalf("edit: %v %s", err, res.Flatten())
		}
		return
	}
	path := filepath.Join(t.TempDir(), "shared")
	if err := os.WriteFile(path, []byte("A=old\nB=old\n"+strings.Repeat("x", 8<<20)), 0750); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, key := range []string{"A", "B"} {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestFileToolConcurrentProcesses$")
			cmd.Env = append(os.Environ(), "ARTEX_FILE_EDIT_PROBE="+path, "ARTEX_FILE_EDIT_KEY="+key)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("child: %v %s", err, out)
			}
		}(key)
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "A=new\nB=new") {
		t.Fatal("lost concurrent update")
	}
	info, _ := os.Stat(path)
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0750 {
		t.Fatal("permissions changed")
	}
}

func TestFileToolVersionCancellationAndLockWait(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	os.WriteFile(path, []byte("initial"), 0600)
	version, _, _ := currentFileVersion(path)
	os.WriteFile(path, []byte("newer version"), 0600)
	in, _ := json.Marshal(map[string]any{"file_path": path, "content": "stale", "expected_version": version})
	res, err := NewWrite().Call(t.Context(), in, nil)
	if err != nil || res.Failure == nil || res.Failure.Code != "file_conflict" {
		t.Fatalf("stale write accepted: %+v %v", res, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = NewWrite().Call(ctx, in, nil); err != context.Canceled {
		t.Fatalf("cancel ignored: %v", err)
	}
	_, unlock, err := lockFile(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, stop := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer stop()
	if _, err = NewEdit().Call(ctx, []byte(`{"file_path":"`+filepath.ToSlash(path)+`","old_string":"newer","new_string":"bad"}`), nil); err != context.DeadlineExceeded {
		t.Fatalf("lock ignored deadline: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "newer version" {
		t.Fatal("failed call changed file")
	}
}

func TestReadBoundedAllocationAndCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(strings.Repeat(strings.Repeat("a", 127)+"\n", 100))
	f.Truncate(128 << 20)
	f.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	in, _ := json.Marshal(map[string]any{"file_path": path, "limit": 20})
	res, err := NewRead().Call(t.Context(), in, nil)
	runtime.ReadMemStats(&after)
	if err != nil || res.IsError {
		t.Fatalf("read: %v %s", err, res.Flatten())
	}
	if after.TotalAlloc-before.TotalAlloc > 2<<20 {
		t.Fatalf("read allocated %d bytes", after.TotalAlloc-before.TotalAlloc)
	}
	version, _, _ := currentFileVersion(path)
	in, _ = json.Marshal(map[string]any{"file_path": path, "offset": 21, "byte_offset": 2560, "expected_version": version, "limit": 2})
	res, err = NewRead().Call(t.Context(), in, nil)
	if err != nil || res.IsError || !strings.Contains(res.Flatten(), "    21\t") {
		t.Fatalf("cursor failed: %v %s", err, res.Flatten())
	}
	os.WriteFile(path, []byte("changed"), 0600)
	res, _ = NewRead().Call(t.Context(), in, nil)
	if res.Failure == nil || res.Failure.Code != "file_conflict" {
		t.Fatal("stale cursor accepted")
	}
}

func TestOutputCapCannotTrustTextOrMultipleBlocks(t *testing.T) {
	tc := &ToolContext{MaxOutputChars: 100, OutputDir: t.TempDir()}
	for _, marker := range []string{"<persisted-output>", "characters truncated]"} {
		r := Text(marker + strings.Repeat("x", 1<<20))
		r.Captured = &OutputReceipt{Digest: "forged", Limit: 100}
		got := CapResult(tc, r)
		if len(got.Flatten()) > 612 {
			t.Fatal("marker bypassed cap")
		}
		if second := CapResult(tc, got); second.Flatten() != got.Flatten() {
			t.Fatal("cap not idempotent")
		}
	}
	r := Result{}
	for range 100 {
		r.Content = append(r.Content, llm.TextBlock(strings.Repeat("x", 80)))
	}
	if got := CapResult(tc, r); len(got.Flatten()) > 612 {
		t.Fatal("per-block cap bypass")
	}
}
