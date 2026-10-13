package traffic

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// BenchmarkTrafficIndex measures the same capture/query API against SQLite WAL
// and PostgreSQL. SQLite is a local-file baseline, never an NFS deployment.
// PostgreSQL requires a disposable ARTEX_TRAFFIC_TEST_DSN; each run gets a unique
// schema. Setup/cleanup and corpus generation are excluded from measured time.
func BenchmarkTrafficIndex(b *testing.B) {
	rows := 10000
	if raw := os.Getenv("ARTEX_TRAFFIC_BENCH_ROWS"); raw != "" {
		var err error
		rows, err = strconv.Atoi(raw)
		if err != nil || rows < 100 {
			b.Fatal("ARTEX_TRAFFIC_BENCH_ROWS must be >=100")
		}
	}
	for _, backend := range []string{"SQLite", "PostgreSQL"} {
		b.Run(backend, func(b *testing.B) {
			b.StopTimer()
			dir := b.TempDir()
			var tr *Traffic
			if backend == "PostgreSQL" {
				tr = openTestPostgres(b, dir)
			} else {
				var err error
				tr, err = Open(dir, "127.0.0.1:0")
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { tr.Close() })
			}
			for i := 0; i < rows; i++ {
				if err := tr.recordFlow(newFlow(fmt.Sprintf("host-%02d.example", i%100), "POST", fmt.Sprintf("/api/items/%d", i), []byte(`{"request":"benchmark"}`), benchmarkBody(i))); err != nil {
					b.Fatal(err)
				}
			}
			if n, err := tr.Count(); err != nil || n != rows {
				b.Fatalf("corpus count=%d err=%v", n, err)
			}
			if tr.db.postgres {
				if err := tr.vacuumPostgres(); err != nil {
					b.Fatal(err)
				}
			} else {
				if _, err := tr.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
					b.Fatal(err)
				}
				if _, err := tr.db.Exec(`ANALYZE`); err != nil {
					b.Fatal(err)
				}
			}
			b.Logf("backend=%s corpus_rows=%d body_bytes=4096 index_bytes=%d", backend, rows, tr.indexBytes())
			page, err := tr.Search("host-00.example", 0, 1)
			if err != nil || len(page) != 1 {
				b.Fatalf("seed lookup: %v %v", page, err)
			}
			id := page[0].ID
			cases := []struct {
				name string
				run  func() error
			}{
				{"Get", func() error { _, _, err := tr.Get(id); return err }},
				{"HostPage", func() error {
					r, err := tr.Search("host-00.example", 0, 50)
					if err == nil && len(r) == 0 {
						return fmt.Errorf("empty host result")
					}
					return err
				}},
				{"BodySearch", func() error {
					r, err := tr.query("host-00.example", "", "needle-marker", 0, 10)
					if err == nil && len(r) == 0 {
						return fmt.Errorf("empty body result")
					}
					return err
				}},
				{"FilteredPage", func() error {
					r, n, err := tr.Page(PageQuery{Body: "needle-marker", Status: "2xx", RespMin: -1, RespMax: -1}, 0, 50)
					if err == nil && (len(r) == 0 || n == 0) {
						return fmt.Errorf("empty page result")
					}
					return err
				}},
			}
			for _, c := range cases {
				b.Run(c.name, func(b *testing.B) { benchmarkLatency(b, c.run) })
			}
			b.Run("ParallelGet", func(b *testing.B) {
				b.ReportAllocs()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						if _, _, err := tr.Get(id); err != nil {
							b.Error(err)
							return
						}
					}
				})
			})
			// Writes run last so read cases all use exactly the same fixed corpus.
			b.Run("Record4KiB", func(b *testing.B) {
				b.StopTimer()
				before, err := tr.Count()
				if err != nil {
					b.Fatal(err)
				}
				flow := newFlow("write.example", "POST", "/api/record", []byte(`{"request":"benchmark"}`), benchmarkBody(7))
				benchmarkLatency(b, func() error { return tr.recordFlow(flow) })
				b.StopTimer()
				if n, err := tr.Count(); err != nil || n != before+b.N {
					b.Fatalf("writes lost: count=%d expected=%d err=%v", n, before+b.N, err)
				}
			})
		})
	}
}

func benchmarkLatency(b *testing.B, run func() error) {
	b.Helper()
	b.StopTimer()
	latencies := make([]time.Duration, b.N)
	// Allocate samples outside the measurement. Do not invoke run here: write
	// cases must execute exactly b.N operations, including calibration runs.
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if err := run(); err != nil {
			b.Fatal(err)
		}
		latencies[i] = time.Since(start)
	}
	b.StopTimer()
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		b.ReportMetric(float64(latencies[len(latencies)/2].Nanoseconds())/1000, "p50-us")
		b.ReportMetric(float64(latencies[(len(latencies)-1)*95/100].Nanoseconds())/1000, "p95-us")
	}
}

// Deterministic 4 KiB text: unique alphanumeric blocks avoid unrealistically
// compressible repeated padding; 1% of captures contain the search marker.
func benchmarkBody(i int) []byte {
	var content strings.Builder
	if i%100 == 0 {
		content.WriteString("needle-marker 中文查询 ")
	}
	for j := 0; content.Len() < 4096; j++ {
		sum := sha256.Sum256(fmt.Appendf(nil, "capture=%d block=%d", i, j))
		content.WriteString(hex.EncodeToString(sum[:]))
		content.WriteByte(' ')
	}
	return []byte(content.String()[:4096])
}
