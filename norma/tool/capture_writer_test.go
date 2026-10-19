package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamingCaptureCompatibility(t *testing.T) {
	for _, limit := range []int{1, 2, 7, 30000} {
		for _, data := range []string{"", "a", "\n", strings.Repeat("中文\n", 10000)} {
			for _, chunk := range []int{1, 37, 8192, 100000} {
				tc := &ToolContext{MaxOutputChars: limit}
				w := newCaptureWriter(tc)
				for at := 0; at < len(data); at += chunk {
					w.Write([]byte(data[at:min(at+chunk, len(data))]))
				}
				if got, want := w.finish(), Capture(tc, data); got != want {
					t.Fatalf("changed capture limit=%d chunk=%d", limit, chunk)
				}
			}
		}
	}
}

func TestStreamingCaptureArtifactsAndFailure(t *testing.T) {
	for _, broken := range []bool{false, true} {
		dir := t.TempDir()
		spill := filepath.Join(dir, "output")
		if broken {
			if err := os.WriteFile(spill, []byte("occupied"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		tc := &ToolContext{MaxOutputChars: 100, WorkingDir: dir, OutputDir: spill}
		w := newCaptureWriter(tc)
		chunk := []byte(strings.Repeat("line\n", 1000))
		for range 100 {
			w.Write(chunk)
		}
		got := w.finish()
		if len(w.head) > 100 || len(w.tail) > 50 {
			t.Fatal("unbounded capture memory")
		}
		if broken {
			if want := truncate(strings.Repeat(string(chunk), 100), 100); got != want {
				t.Fatal("disk failure lost fallback")
			}
			continue
		}
		files, err := os.ReadDir(spill)
		if err != nil || len(files) != 1 {
			t.Fatal("missing output artifact", err)
		}
		data, err := os.ReadFile(filepath.Join(spill, files[0].Name()))
		if err != nil || string(data) != strings.Repeat(string(chunk), 100) {
			t.Fatal("artifact lost output", err)
		}
		if !strings.Contains(got, "full 500000 bytes / 100001 lines") || !strings.Contains(got, filepath.Join("output", files[0].Name())) {
			t.Fatal("capture metadata changed", got)
		}
	}
	// Small commands do not create an output directory or file.
	tc := &ToolContext{OutputDir: filepath.Join(t.TempDir(), "unused")}
	w := newCaptureWriter(tc)
	w.Write([]byte("small"))
	if w.finish() != "small" {
		t.Fatal("small output changed")
	}
	if _, err := os.Stat(tc.OutputDir); !os.IsNotExist(err) {
		t.Fatal("small output created artifacts")
	}
}

func BenchmarkStreamingCommandCapture(b *testing.B) {
	chunk := []byte(strings.Repeat("line\n", 6553))
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk) * 1024))
	for b.Loop() {
		w := newCaptureWriter(nil)
		for range 1024 {
			w.Write(chunk)
		}
		if len(w.finish()) > 30512 {
			b.Fatal("unbounded result")
		}
	}
}
