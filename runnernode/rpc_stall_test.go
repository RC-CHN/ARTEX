package runnernode

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"
)

func TestSandboxRequestCancellationInterruptsBlockedWrite(t *testing.T) {
	reader, writer := io.Pipe() // child stopped reading its RPC stdin
	defer reader.Close()
	defer writer.Close()
	s := &rpcSandbox{stdin: writer, pending: map[string]chan SandboxReply{}, done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Request(ctx, SandboxRequest{Method: "close"}); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked request reported success")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("RPC write ignored cancellation; close would never reach process cleanup")
	}
}

type notifyingPipe struct {
	*io.PipeWriter
	entered chan struct{}
	once    sync.Once
}

func (p *notifyingPipe) Write(b []byte) (int, error) {
	p.once.Do(func() { close(p.entered) })
	return p.PipeWriter.Write(b)
}

func TestCanceledQueuedRPCWriterDoesNotInterruptActiveWriter(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	pipe := &notifyingPipe{PipeWriter: writer, entered: make(chan struct{})}
	s := &rpcSandbox{stdin: pipe}
	done := make(chan error, 1)
	go func() { done <- s.write(t.Context(), SandboxRequest{Method: "heartbeat"}) }()
	select {
	case <-pipe.entered:
	case <-time.After(time.Second):
		t.Fatal("first write did not start")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	if err := s.write(ctx, SandboxRequest{Method: "notifications"}); err == nil {
		t.Fatal("queued write ignored cancellation")
	}
	var request SandboxRequest
	if err := json.NewDecoder(reader).Decode(&request); err != nil || request.Method != "heartbeat" {
		t.Fatalf("queued cancellation broke active frame: %+v %v", request, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
