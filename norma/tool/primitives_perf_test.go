package tool

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func TestLinePagerCompatibility(t *testing.T) {
	for _, data := range []string{"", "\n", "\n\n", "one", "one\n", "one\n\n", "\none\n\ntwo\n\n", strings.Repeat("x", 80000) + "\ny\nz\n", strings.Repeat("row\n", 10000)} {
		for _, limit := range []int{-1, 0, 1, 3, 250} {
			for _, offset := range []int{-2, 0, 1, 2, 10001} {
				for _, chunk := range []int{1, 7, 4096, 90000} {
					p := &linePager{limit: limit, offset: max(0, offset)}
					for at := 0; at < len(data); at += chunk {
						p.Write([]byte(data[at:min(at+chunk, len(data))]))
					}
					got, more := p.finish()
					var lines []string
					if text := strings.TrimRight(data, "\n"); text != "" {
						lines = strings.Split(text, "\n")
					}
					want, wmore := applyHeadOffset(lines, limit, offset)
					if strings.Join(got, "\n") != strings.Join(want, "\n") || len(got) != len(want) || more != wmore {
						t.Fatalf("page changed limit=%d offset=%d chunk=%d", limit, offset, chunk)
					}
				}
			}
		}
	}
	// Unselected enormous lines must not accumulate in the partial-line buffer.
	p := &linePager{limit: 1, offset: 1}
	chunk := []byte(strings.Repeat("x", 4096))
	for range 1000 {
		p.Write(chunk)
	}
	if p.pending.Len() != 0 {
		t.Fatal("buffered skipped line")
	}
}

func TestPTYWindowCompatibility(t *testing.T) {
	s := &ptySession{notifyCh: make(chan struct{}, 1)}
	rng := rand.New(rand.NewPCG(17, 23))
	var want string
	var total int64
	for i := range 160 {
		n := rng.IntN(20000)
		if i%23 == 0 {
			n = sessRingCap + 2000
		}
		chunk := strings.Repeat(fmt.Sprintf("%03d\n", i), n/4)
		s.append([]byte(chunk))
		total += int64(len(chunk))
		want += chunk
		if len(want) > sessRingCap {
			want = want[len(want)-sessRingCap:]
		}
		if got := s.tailStr(0); got != want {
			t.Fatal("scrollback changed")
		}
		start := total - int64(len(want))
		for _, cursor := range []int64{0, start, start + int64(len(want)/2), total, total + 1} {
			got, next, omitted := s.readSince(cursor, 0, 79, false)
			if cursor >= total {
				if got != "" || next != total || omitted {
					t.Fatal("future cursor")
				}
				continue
			}
			from := max(start, cursor)
			end := min(from+79, total)
			if got != want[from-start:end-start] || next != end || omitted != (cursor < start) {
				t.Fatal("cursor or omission changed")
			}
		}
		if cap(s.storage) > 2*sessRingCap {
			t.Fatal("unbounded backing buffer")
		}
	}
}

func TestReadCursorSurvivesRuntimeCaps(t *testing.T) {
	dir, spill := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "data")
	if err := os.WriteFile(path, []byte(strings.Repeat("line of text\n", 10000)), 0600); err != nil {
		t.Fatal(err)
	}
	tc := &ToolContext{WorkingDir: dir, OutputDir: spill, MaxOutputChars: 200}
	in, _ := json.Marshal(map[string]any{"file_path": "data"})
	r, err := NewRead().Call(t.Context(), in, tc)
	if err != nil {
		t.Fatal(err)
	}
	before := r.Flatten()
	for range 2 {
		r = CapToolResult("Read", tc, r)
	}
	if r.Flatten() != before || !strings.Contains(before, "byte_offset=") || !strings.Contains(before, "expected_version=") {
		t.Fatal("runtime cap lost read cursor")
	}
	if entries, _ := os.ReadDir(spill); len(entries) != 0 {
		t.Fatal("runtime copied source to output artifact")
	}
	for _, suffix := range []string{"", "\n"} {
		tc.MaxOutputChars = 30000
		os.WriteFile(path, []byte(strings.Repeat("x", 2001)+suffix), 0600)
		r, err = NewRead().Call(t.Context(), in, tc)
		if err != nil || !strings.Contains(r.Flatten(), "line truncated") {
			t.Fatal("unterminated long line lost truncation notice", err)
		}
	}
}

func TestOutputDigestCompatibility(t *testing.T) {
	for _, s := range []string{"", "中文", strings.Repeat("x", 100000)} {
		want := fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
		if outputDigest(s) != want {
			t.Fatal("receipt hash changed")
		}
	}
}

func TestSearchFallbackCompatibility(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input.txt")
	rng := rand.New(rand.NewPCG(11, 7))
	fixtures := []string{"", "\n", "x", "x\n", "x\ny", "x\ny\n", strings.Repeat("long", 20000) + "\nx\n", "a\x00x"}
	for range 12 {
		var b strings.Builder
		for range 80 {
			b.WriteString([]string{"a", "x", "", "中文", "xyz"}[rng.IntN(5)])
			b.WriteByte('\n')
		}
		fixtures = append(fixtures, b.String())
	}
	for _, data := range fixtures {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		for _, pattern := range []string{"x", "a.*x", "(?s)x.*?y", "^", "$", "", "\\n", "x\\ny", "中文", "\\b"} {
			for _, multiline := range []bool{false, true} {
				for _, mode := range []string{"files_with_matches", "count", "content"} {
					for _, window := range [][2]int{{0, 0}, {1, 2}, {-1, 2}, {2, -1}, {-1, -1}, {20, 20}} {
						for _, numbers := range []bool{false, true} {
							re := regexp.MustCompile(pattern)
							got := scanFile(path, dir, re, mode, numbers, multiline, window[0], window[1])
							want := referenceScanFile(path, dir, re, mode, numbers, multiline, window[0], window[1])
							if !reflect.DeepEqual(got, want) {
								t.Fatalf("search changed: pattern=%q multiline=%v mode=%s window=%v data=%q\ngot=%+v\nwant=%+v", pattern, multiline, mode, window, data[:min(len(data), 80)], got, want)
							}
						}
					}
				}
			}
		}
	}
}

func BenchmarkToolPrimitives(b *testing.B) {
	b.Run("EditRead8MiB", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "input")
		if err := os.WriteFile(path, []byte(strings.Repeat("x", 8<<20)), 0600); err != nil {
			b.Fatal(err)
		}
		info, _ := os.Stat(path)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			if _, err := readEditFile(context.Background(), path, info); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("CapResult24KiB", func(b *testing.B) {
		r := CapResult(nil, Text(strings.Repeat("x", 24<<10)))
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			r = CapResult(nil, r)
		}
	})
	b.Run("PTYAppend4KiB", func(b *testing.B) {
		s := &ptySession{buf: make([]byte, sessRingCap), notifyCh: make(chan struct{}, 1)}
		chunk := make([]byte, 4096)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			s.append(chunk)
		}
	})
	for _, multi := range []bool{false, true} {
		b.Run(fmt.Sprintf("GrepCountMultiline=%v", multi), func(b *testing.B) {
			dir := b.TempDir()
			path := filepath.Join(dir, "input")
			if err := os.WriteFile(path, []byte(strings.Repeat("match\nother\n", 5000)), 0600); err != nil {
				b.Fatal(err)
			}
			re := regexp.MustCompile("match")
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if got := scanFile(path, dir, re, "count", true, multi, 0, 0); got == nil || got.count != 5000 {
					b.Fatal(got)
				}
			}
		})
	}
}
