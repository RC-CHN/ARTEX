package traffic

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

var ErrClosed = errors.New("traffic recorder is closed")

const proxyShutdownGrace = 5 * time.Second

// Start runs the proxy once (blocking). A concurrent Close closes the HTTP
// server even if Start has not reached Serve yet, then joins this call.
func (t *Traffic) Start() error {
	t.lifecycleMu.Lock()
	if t.closing {
		t.lifecycleMu.Unlock()
		return ErrClosed
	}
	if t.started {
		t.lifecycleMu.Unlock()
		return errors.New("traffic proxy already started")
	}
	t.started = true
	t.startDone = make(chan struct{})
	done := t.startDone
	t.lifecycleMu.Unlock()
	defer close(done)
	err := t.proxy.Start()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops accepting proxy traffic and gives HTTP requests a bounded grace
// period to finish recording. After that no callback may start a file/DB write;
// admitted recordings and reclamation finish before the database is closed.
// Other application operations (archive/delete/read) must be drained by the
// owner before Close, just like HTTP API handlers before Manager.Close.
func (t *Traffic) Close() error { return t.closeWithTimeout(proxyShutdownGrace) }

func (t *Traffic) closeWithTimeout(grace time.Duration) error {
	t.closeOnce.Do(func() {
		t.lifecycleMu.Lock()
		t.closing = true
		if t.closed != nil {
			close(t.closed)
		}
		done := t.startDone
		t.lifecycleMu.Unlock()

		if done != nil {
			ctx, cancel := context.WithTimeout(context.Background(), grace)
			err := t.proxy.Shutdown(ctx)
			cancel()
			if err != nil {
				t.closeErr = errors.Join(err, t.proxy.Close())
			}
			<-done
		}

		// Shutdown does not join go-mitmproxy's internal MITM server or hijacked
		// CONNECT handlers. Close their sockets and refuse any late Response hook
		// before draining admitted callbacks, so they cannot recreate blob files.
		t.lifecycleMu.Lock()
		t.recordsClosed = true
		clients := make([]net.Conn, 0, len(t.clients))
		for _, conn := range t.clients {
			clients = append(clients, conn)
		}
		t.lifecycleMu.Unlock()
		for _, conn := range clients {
			_ = conn.Close()
		}
		t.recording.Wait()
		t.reaping.Wait()
		if t.db != nil {
			t.closeErr = errors.Join(t.closeErr, t.db.Close())
		}
	})
	return t.closeErr
}

func (t *Traffic) beginRecording() bool {
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	if t.recordsClosed {
		return false
	}
	t.recording.Add(1)
	return true
}

func (t *Traffic) startReaper(run func()) bool {
	t.lifecycleMu.Lock()
	defer t.lifecycleMu.Unlock()
	if t.closing {
		return false
	}
	t.reaping.Go(run)
	return true
}

func (s *sink) ClientConnected(client *mproxy.ClientConn) {
	s.t.lifecycleMu.Lock()
	if s.t.closing {
		s.t.lifecycleMu.Unlock()
		_ = client.Conn.Close()
		return
	}
	if s.t.clients == nil {
		s.t.clients = make(map[*mproxy.ClientConn]net.Conn)
	}
	s.t.clients[client] = client.Conn
	s.t.lifecycleMu.Unlock()
}

func (s *sink) ClientDisconnected(client *mproxy.ClientConn) {
	s.t.lifecycleMu.Lock()
	delete(s.t.clients, client)
	s.t.lifecycleMu.Unlock()
}
