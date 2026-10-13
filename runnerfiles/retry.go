package runnerfiles

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sync"
	"time"
)

// ErrUnavailable is a bounded, retryable file transport failure. It does not
// authorize replaying tool calls or ignoring a conflicting file version.
var ErrUnavailable = errors.New("file service temporarily unavailable")

var errTransferIdle = fmt.Errorf("%w: transfer made no progress", ErrUnavailable)

func transferError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var network *net.OpError
	var timeout net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &network) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return err
}

func retryTransfer(ctx context.Context, transfer func() error) error {
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := transfer()
		if !errors.Is(err, ErrUnavailable) || attempt == 4 {
			return err
		}
		// Jitter avoids synchronized reconnects from every session in a pool.
		delay := time.Duration(1<<attempt) * 250 * time.Millisecond
		timer := time.NewTimer(delay + time.Duration(rand.Int64N(int64(delay/4))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type progressBody struct {
	io.ReadCloser
	ctx    context.Context
	touch  func()
	finish func()
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.touch()
	}
	if err != nil && errors.Is(context.Cause(b.ctx), errTransferIdle) {
		err = errTransferIdle
	}
	return n, err
}

func (b *progressBody) Close() error {
	if b.finish != nil {
		b.finish()
	}
	return b.ReadCloser.Close()
}

// Bound stalls, not total transfer duration. A large, progressing transfer can
// outlive many heartbeats. The timer belongs to this request, never a shared
// connection (which may multiplex other sessions via HTTP/2).
func (c *Client) transferRequest(req *http.Request) (*http.Response, error) {
	idle := c.IdleTimeout
	if idle <= 0 {
		idle = time.Minute
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	var mu sync.Mutex
	last := time.Now()
	finished := false
	var timer *time.Timer
	mu.Lock()
	timer = time.AfterFunc(idle, func() {
		mu.Lock()
		defer mu.Unlock()
		if finished {
			return
		}
		if remaining := idle - time.Since(last); remaining > 0 {
			timer.Reset(remaining)
			return
		}
		cancel(errTransferIdle)
	})
	mu.Unlock()
	touch := func() { mu.Lock(); last = time.Now(); mu.Unlock() }
	finish := func() {
		mu.Lock()
		finished = true
		timer.Stop()
		mu.Unlock()
		cancel(nil)
	}
	r := req.Clone(ctx)
	if r.Body != nil {
		r.Body = &progressBody{ReadCloser: r.Body, ctx: ctx, touch: touch}
	}
	cl := c.HTTP
	if cl == nil {
		cl = http.DefaultClient
	}
	res, err := cl.Do(r)
	if err != nil {
		if errors.Is(context.Cause(ctx), errTransferIdle) {
			err = errTransferIdle
		}
		finish()
		return nil, transferError(req.Context(), err)
	}
	touch()
	res.Body = &progressBody{ReadCloser: res.Body, ctx: ctx, touch: touch, finish: finish}
	return res, nil
}
