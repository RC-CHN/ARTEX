package recordexport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOfflineViewerShardsKeepEveryRecord(t *testing.T) {
	dir := t.TempDir()
	if p := os.Getenv("ARTEX_DOGFOOD_OUTPUT"); p != "" {
		dir = filepath.Join(p, "offline-large")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(dir, "source.jsonl")
	f, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	for i := range 1005 {
		if _, err = fmt.Fprintf(f, `{"uuid":"fixture-%04d","timestamp":%q,"message":{"role":"user","content":"synthetic record %d <script>bad()</script>"}}`+"\n", i, stamp, i); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	b, err := New(t.Context(), filepath.Join(dir, "bundle"), []int64{1}, false, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	b.Manifest.Cutoff = time.Now().UTC()
	if err = b.Transcript(source, "tasks/1/transcripts/fixture"); err != nil {
		t.Fatal(err)
	}
	if err = b.Index(); err != nil {
		t.Fatal(err)
	}
	page, _ := os.ReadFile(filepath.Join(b.Dir, "tasks/1/transcripts/fixture.html"))
	if strings.Count(string(page), "<details data-record") != 200 {
		t.Fatal("unbounded initial DOM")
	}
	shards, err := filepath.Glob(filepath.Join(b.Dir, "tasks/1/transcripts/fixture.parts/*.js"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, p := range shards {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		data := strings.TrimSuffix(strings.TrimPrefix(string(raw), "window.artexRecords("), ");")
		var rows []viewerRecord
		if err = json.Unmarshal([]byte(data), &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) > 200 {
			t.Fatal("unbounded shard")
		}
		for _, row := range rows {
			if strings.Contains(row.HTML, "<script>bad()") {
				t.Fatal("HTML injection")
			}
		}
		count += len(rows)
	}
	if count != 1005 || len(shards) != 6 {
		t.Fatal(count, len(shards))
	}
	if _, _, err = b.Finish(filepath.Join(dir, "bundle.zip")); err != nil {
		t.Fatal(err)
	}
}
