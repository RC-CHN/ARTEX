package runnernode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

func mcpReply(req map[string]any) any {
	var result any
	switch req["method"] {
	case "initialize":
		result = map[string]any{"protocolVersion": "2025-06-18"}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "fixture", "inputSchema": map[string]any{"type": "object"}}}}
	case "tools/call":
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "remote fixture"}}}
	}
	return map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result}
}
func TestHTTPMCPReusedWithinSessionAndClosed(t *testing.T) {
	var initCount, calls, closed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Secret") != "configured" {
			t.Error("MCP header missing")
		}
		if r.Method == http.MethodDelete {
			closed.Add(1)
			w.WriteHeader(204)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["method"] == "initialize" {
			initCount.Add(1)
			w.Header().Set("Mcp-Session-Id", "fixture-session")
		}
		if _, ok := req["id"]; !ok {
			w.WriteHeader(202)
			return
		}
		if req["method"] == "tools/call" {
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mcpReply(req))
	}))
	defer server.Close()
	spec := baseSpec(t.TempDir())
	spec.MCPs = []runner.MCPConfig{{Name: "fixture", Transport: "http", URL: server.URL, Headers: map[string]string{"X-Test-Secret": "configured"}}}
	s, e := OpenLocalSession(context.Background(), t.Name(), spec)
	if e != nil {
		t.Fatal(e)
	}
	if len(s.schemas) != 1 || s.schemas[0].Name != "mcp__fixture__echo" {
		t.Fatalf("schemas: %+v", s.schemas)
	}
	for _, id := range []string{"one", "two"} {
		r := s.Call(context.Background(), runner.ToolCall{ID: id, Name: "mcp__fixture__echo", Input: json.RawMessage(`{}`)})
		if r.Error != "" || r.Result.Flatten() != "remote fixture" {
			t.Fatalf("call: %+v", r)
		}
	}
	s.Close()
	if initCount.Load() != 1 || calls.Load() != 2 || closed.Load() != 1 {
		t.Fatalf("session not reused or closed: initialize=%d call=%d delete=%d", initCount.Load(), calls.Load(), closed.Load())
	}
}
func TestSSEMCPAndFailedLaterMCPClosesEarlierConnection(t *testing.T) {
	var mu sync.Mutex
	var stream http.ResponseWriter
	ready := make(chan struct{})
	gone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
			w.(http.Flusher).Flush()
			mu.Lock()
			stream = w
			mu.Unlock()
			close(ready)
			<-r.Context().Done()
			close(gone)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["id"]; ok {
			b, _ := json.Marshal(mcpReply(req))
			mu.Lock()
			_, _ = fmt.Fprintf(stream, "event: message\ndata: %s\n\n", b)
			stream.(http.Flusher).Flush()
			mu.Unlock()
		}
		w.WriteHeader(202)
	}))
	defer server.Close()
	spec := baseSpec(t.TempDir())
	spec.MCPs = []runner.MCPConfig{{Name: "sse_fixture", Transport: "sse", URL: server.URL}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, e := OpenLocalSession(ctx, t.Name(), spec)
	if e != nil {
		t.Fatal(e)
	}
	<-ready
	r := s.Call(ctx, runner.ToolCall{ID: "sse-call", Name: "mcp__sse_fixture__echo", Input: json.RawMessage(`{}`)})
	if r.Error != "" || r.Result.Flatten() != "remote fixture" {
		t.Fatalf("SSE result: %+v", r)
	}
	s.Close()
	select {
	case <-gone:
	case <-time.After(time.Second):
		t.Fatal("SSE connection survived session close")
	}
}
func TestCancelledMCPInitializationReturnsPromptly(t *testing.T) {
	entered := make(chan struct{})
	gone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(gone)
	}))
	defer server.Close()
	spec := baseSpec(t.TempDir())
	spec.MCPs = []runner.MCPConfig{{Name: "hanging", Transport: "sse", URL: server.URL}}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		s, e := OpenLocalSession(ctx, t.Name(), spec)
		if s != nil {
			s.Close()
		}
		result <- e
	}()
	<-entered
	cancel()
	select {
	case e := <-result:
		if e == nil {
			t.Fatal("cancelled initialization succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not interrupt endpoint discovery")
	}
	select {
	case <-gone:
	case <-time.After(time.Second):
		t.Fatal("partial SSE connection leaked")
	}
}
func TestMCPFailureCleansEarlierConnections(t *testing.T) {
	var closed atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			closed.Add(1)
			w.WriteHeader(204)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["id"]; !ok {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "fixture")
		_ = json.NewEncoder(w).Encode(mcpReply(req))
	}))
	defer server.Close()
	spec := baseSpec(t.TempDir())
	spec.MCPs = []runner.MCPConfig{{Name: "first", Transport: "http", URL: server.URL}, {Name: "invalid", Transport: "invalid"}}
	if s, e := OpenLocalSession(context.Background(), t.Name(), spec); e == nil {
		s.Close()
		t.Fatal("invalid second MCP unexpectedly opened")
	}
	if closed.Load() != 1 {
		t.Fatal("first MCP connection leaked after later initialization failure")
	}
}

func TestSandboxInputEOFInterruptsHalfOpenMCP(t *testing.T) {
	entered := make(chan struct{})
	gone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
		close(gone)
	}))
	defer server.Close()
	spec := baseSpec(t.TempDir())
	spec.MCPs = []runner.MCPConfig{{Name: "half-open", Transport: "sse", URL: server.URL}}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- ServeSandbox(context.Background(), reader, io.Discard) }()
	if e := json.NewEncoder(writer).Encode(SandboxRequest{ID: "open", Method: "open", SessionID: t.Name(), Spec: &spec}); e != nil {
		t.Fatal(e)
	}
	<-entered
	writer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("node stdin EOF did not cancel half-initialized MCP")
	}
	select {
	case <-gone:
	case <-time.After(time.Second):
		t.Fatal("half-initialized MCP connection survived node exit")
	}
}
