package recordexport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuilderLimitsPathsAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	b, err := New(ctx, t.TempDir(), []int64{1}, false, 5)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape", "/absolute", "a/../../b", "a\\b"} {
		if _, err = b.Open(name); err == nil {
			t.Fatal("accepted path", name)
		}
	}
	if err = b.Write("safe.txt", []byte("12345")); err != nil {
		t.Fatal(err)
	}
	if err = b.Write("too-large.txt", []byte("6")); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	cancel()
	if _, err = b.Open("after-cancel"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type closingBuffer struct{ bytes.Buffer }

func (*closingBuffer) Close() error { return nil }
func TestRedactedRowsRemainValidAndKeepUsage(t *testing.T) {
	out := &closingBuffer{}
	w := RedactJSON(out)
	row := []byte(`{"id":9007199254740993,"password":"private","input_tokens":123,"text":"Authorization: Bearer secret\nresult retained","nested":{"api_key":"secret"}}` + "\n")
	for _, part := range [][]byte{row[:19], row[19:]} {
		if _, err := w.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(out.Bytes()))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["id"] != json.Number("9007199254740993") || result["input_tokens"] != json.Number("123") || strings.Contains(out.String(), "private") || strings.Contains(out.String(), "secret") || !strings.Contains(out.String(), "result retained") {
		t.Fatal(out.String())
	}
}
func TestTranscriptPartialTailAndHTMLInjection(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.jsonl")
	now := time.Now().UTC()
	data := `{"uuid":"<script>bad()</script>","timestamp":"` + now.Add(-time.Minute).Format(time.RFC3339Nano) + `","message":{"role":"user","content":[{"text":"<img src=x onerror=bad()>"}]}}` + "\n{"
	if err := os.WriteFile(source, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	b, err := New(t.Context(), filepath.Join(dir, "output"), []int64{1}, false, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	b.Manifest.Cutoff = now
	if err = b.Transcript(source, "tasks/1/transcripts/synthetic"); err != nil {
		t.Fatal(err)
	}
	if len(b.Manifest.Omissions) != 1 || b.Manifest.Omissions[0].Reason != "invalid_or_partial_transcript_tail" {
		t.Fatal(b.Manifest.Omissions)
	}
	view, err := os.ReadFile(filepath.Join(b.Dir, "tasks/1/transcripts/synthetic.html"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(view, []byte("<script>bad()")) || bytes.Contains(view, []byte("<img src=x")) {
		t.Fatal("unescaped HTML")
	}
	f, err := os.Open(filepath.Join(b.Dir, "tasks/1/transcripts/synthetic.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	var row any
	if err = decoder.Decode(&row); err != nil {
		t.Fatal(err)
	}
	if err = decoder.Decode(&row); err != io.EOF {
		t.Fatal("partial row retained", err)
	}
}

func TestRedactionOfJSONEncodedInsideStrings(t *testing.T) {
	encoded := `{"raw_request":"{\"password\":\"synthetic-secret\",\"nested\":{\"api_key\":\"synthetic-secret\"},\"input_tokens\":123}","id":9007199254740993}`
	result := Redact([]byte(encoded))
	if !json.Valid(result) || bytes.Contains(result, []byte("synthetic-secret")) || !bytes.Contains(result, []byte("9007199254740993")) {
		t.Fatal(string(result))
	}
}

func TestIndexOnlyOpensGeneratedHTMLInline(t *testing.T) {
	b, err := New(t.Context(), t.TempDir(), []int64{1}, false, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"tasks/1/transcripts/session.html", "tasks/1/workspace/transcripts/untrusted.html"} {
		if err = b.Write(p, []byte("synthetic")); err != nil {
			t.Fatal(err)
		}
	}
	if err = b.Index(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(b.Dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`href="./tasks/1/workspace/transcripts/untrusted.html" download`)) || bytes.Contains(raw, []byte(`href="./tasks/1/transcripts/session.html" download`)) {
		t.Fatal(string(raw))
	}
}
