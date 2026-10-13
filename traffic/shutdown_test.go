package traffic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func startShutdownTestProxy(t *testing.T) (*Traffic, *http.Client) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	tr, err := Open(t.TempDir(), addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	started := make(chan error, 1)
	go func() { started <- tr.Start() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-started:
			t.Fatalf("proxy start failed: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("proxy did not start listening")
		}
		time.Sleep(time.Millisecond)
	}
	u, _ := url.Parse(tr.ProxyAddr())
	transport := &http.Transport{Proxy: http.ProxyURL(u), DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	return tr, &http.Client{Transport: transport, Timeout: 5 * time.Second}
}

func waitShutdownSignal(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func TestTrafficCloseDrainsHTTPRecording(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	body := strings.Repeat("recorded while proxy drains 中文正文 ", 10000)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(target.Close)
	tr, client := startShutdownTestProxy(t)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	requested := make(chan error, 1)
	go func() {
		resp, err := client.Get(target.URL + "/drain")
		if err == nil {
			var got []byte
			got, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil && string(got) != body {
				err = fmt.Errorf("response body changed during shutdown")
			}
		}
		requested <- err
	}()
	waitShutdownSignal(t, started, "request did not reach target")
	closed := make(chan error, 1)
	go func() { closed <- tr.Close() }()
	waitShutdownSignal(t, tr.closed, "proxy shutdown did not begin")
	// Serve must stop accepting before the active request completes. Its
	// listener exits early while Shutdown continues waiting for the handler.
	waitShutdownSignal(t, tr.startDone, "proxy listener remained open during shutdown")
	if conn, err := net.DialTimeout("tcp", tr.addr, 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("proxy accepted a new connection during shutdown")
	}
	if err := tr.db.Ping(); err != nil {
		t.Fatalf("index closed before HTTP recording drained: %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before active HTTP request: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-requested; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not finish after HTTP recording drained")
	}
	// Re-open the index independently: the admitted capture must be committed
	// with the complete blob before Close returns, not merely kept in memory.
	d, err := sql.Open("sqlite", filepath.Join(tr.dir, "_index", "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var count int
	if err := d.QueryRow(`SELECT count(*) FROM exchanges`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("drained capture count: %d %v", count, err)
	}
	var hash string
	if err := d.QueryRow(`SELECT resp_blob FROM exchange_bodies`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	path, err := tr.blobPath(hash)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatalf("drained capture blob changed: %v", err)
	}
	if err := tr.Start(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("repeat Close: %v", err)
	}
}

func TestTrafficCloseTimeoutBlocksLateCallbacks(t *testing.T) {
	started := make(chan struct{})
	finished := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(finished)
	}))
	t.Cleanup(target.Close)
	tr, client := startShutdownTestProxy(t)
	requested := make(chan struct{})
	go func() {
		defer close(requested)
		if resp, err := client.Get(target.URL + "/blocked"); err == nil {
			resp.Body.Close()
		}
	}()
	waitShutdownSignal(t, started, "request did not reach target")
	closed := make(chan error, 1)
	go func() { closed <- tr.closeWithTimeout(30 * time.Millisecond) }()
	select {
	case err := <-closed:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("bounded shutdown error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked upstream request held shutdown open")
	}
	waitShutdownSignal(t, requested, "proxy did not close the blocked client connection")
	waitShutdownSignal(t, finished, "proxy did not cancel the upstream request")
	if err := os.RemoveAll(tr.dir); err != nil {
		t.Fatal(err)
	}
	// The third-party MITM server can still deliver a late response callback.
	// It must be rejected before spill() recreates any blob directory.
	flow := newFlow("late.example", "GET", "/late", nil, []byte(strings.Repeat("late-body", maxInlineBody)))
	if err := tr.recordFlow(flow); !errors.Is(err, ErrClosed) {
		t.Fatalf("late recording was admitted: %v", err)
	}
	if _, err := os.Stat(tr.dir); !os.IsNotExist(err) {
		t.Fatalf("late callback recreated data files: %v", err)
	}
	if err := tr.Close(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("repeat Close did not retain first result: %v", err)
	}
}

func TestTrafficCloseJoinsReaperAndRejectsNewWork(t *testing.T) {
	tr, _ := openTraffic(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	finished := make(chan error, 1)
	if !tr.startReaper(func() {
		<-release
		finished <- tr.db.Ping()
	}) {
		t.Fatal("reaper was not admitted")
	}
	closed := make(chan error, 2)
	for range 2 {
		go func() { closed <- tr.Close() }()
	}
	waitShutdownSignal(t, tr.closed, "shutdown did not begin")
	if tr.startReaper(func() { t.Error("reaper started after close") }) {
		t.Fatal("new reaper admitted during shutdown")
	}
	if err := tr.Start(); !errors.Is(err, ErrClosed) {
		t.Fatalf("unstarted proxy started during Close: %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before reaper drained: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	for range 2 {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent Close did not join reaper")
		}
	}
	if err := <-finished; err != nil {
		t.Fatalf("database closed before reaper finished: %v", err)
	}
}
