package runnerfiles

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUploadLostAcknowledgmentKeepsSnapshotAndPrecondition(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		t.Run(fmt.Sprintf("competing-write-%t", takeover), func(t *testing.T) {
			t.Parallel()
			store, m := fixture(t)
			writeFixture(t, m.Source, "result", "old")
			client := fileServer(t, store, m, nil)
			local := t.TempDir()
			writeFixture(t, local, "result", "snapshot")
			transport := client.HTTP.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
			var attempts atomic.Int32
			client.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("If-Match") != digest("old") || r.Header.Get("X-Content-SHA256") != digest("snapshot") {
					t.Error("retry changed the original publication precondition")
				}
				res, err := transport.RoundTrip(r)
				if attempts.Add(1) == 1 && err == nil {
					_, _ = io.Copy(io.Discard, res.Body)
					res.Body.Close()
					writeFixture(t, local, "result", "new local bytes")
					if takeover {
						writeFixture(t, m.Source, "result", "peer")
					}
					return nil, io.ErrUnexpectedEOF // the controller committed, ACK was lost
				}
				return res, err
			})
			err := client.Upload(t.Context(), "allowed", m.Name, local, entry("result", "snapshot"), digest("old"))
			if takeover {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("overwrote a peer: %v", err)
				}
				assertContents(t, m.Source, "result", "peer")
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertContents(t, m.Source, "result", "snapshot")
			}
			assertContents(t, local, "result", "new local bytes")
			if attempts.Load() != 2 {
				t.Fatalf("attempts=%d", attempts.Load())
			}
		})
	}
}

func TestFileTransportRetriesOnlyTransientErrors(t *testing.T) {
	for _, status := range []int{408, 429, 500, 502, 503, 504, 400, 401, 403, 410, 413, 422, 423} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			t.Parallel()
			calls := 0
			c := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				r := response(`[]`)
				if calls == 1 {
					r.StatusCode = status
				}
				return r, nil
			})}}
			_, err := c.Manifest(t.Context(), "s", "data")
			retryable := status == 408 || status == 429 || status >= 500
			if retryable && (err != nil || calls != 2) {
				t.Fatalf("transient error not recovered: %v, calls=%d", err, calls)
			}
			if !retryable && (err == nil || calls != 1) {
				t.Fatalf("permanent failure retried: %v, calls=%d", err, calls)
			}
		})
	}
}

func TestRetryDownloadNeverOverwritesNewLocalVersion(t *testing.T) {
	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprint(concurrent), func(t *testing.T) {
			t.Parallel()
			local := t.TempDir()
			writeFixture(t, local, "result", "old")
			calls := 0
			c := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if concurrent {
						writeFixture(t, local, "result", "local-edit")
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(readerFunc(func(p []byte) (int, error) { return copy(p, "n"), io.ErrUnexpectedEOF }))}, nil
				}
				return response("new"), nil
			})}}
			err := c.Download(t.Context(), "s", "data", entry("result", "new"), local)
			if concurrent {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("local edit overwritten: %v", err)
				}
				assertContents(t, local, "result", "local-edit")
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertContents(t, local, "result", "new")
			}
			assertNoTemps(t, local)
		})
	}
}

func TestFileRetryBudgetAndCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelEarly := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		c := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if cancelEarly {
				cancel()
			}
			r := response("unavailable")
			r.StatusCode = 503
			return r, nil
		})}}
		_, err := c.Manifest(ctx, "s", "data")
		cancel()
		if cancelEarly {
			if !errors.Is(err, context.Canceled) || calls != 1 {
				t.Fatalf("cancel ignored: calls=%d, %v", calls, err)
			}
		} else if !errors.Is(err, ErrUnavailable) || calls != 5 {
			t.Fatalf("unbounded/missing retries: calls=%d, %v", calls, err)
		}
	}
}

func TestIdleTransferTimeoutAndProgress(t *testing.T) {
	for _, mode := range []string{"headers", "body", "progress"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "headers" {
					<-r.Context().Done()
					return
				}
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				if mode == "body" {
					<-r.Context().Done()
					return
				}
				for range 12 {
					_, _ = io.WriteString(w, "x")
					w.(http.Flusher).Flush()
					time.Sleep(40 * time.Millisecond)
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), IdleTimeout: 200 * time.Millisecond}
			r, _ := c.request(ctx, "GET", "blob", "s", "data", "result", nil)
			res, err := c.do(r)
			if err == nil {
				_, err = io.ReadAll(res.Body)
				res.Body.Close()
			}
			if mode == "progress" {
				if err != nil {
					t.Fatalf("active transfer timed out: %v", err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("stalled %s did not time out: %v", mode, err)
			}
		})
	}
}

func TestUploadRetriesTruncatedResponse(t *testing.T) {
	t.Parallel()
	local := t.TempDir()
	writeFixture(t, local, "result", "new")
	calls := 0
	c := &Client{BaseURL: "http://controller.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if string(b) != "new" {
			t.Errorf("retry body changed: %q", b)
		}
		if calls == 1 {
			return response(`{"path":"result"`), nil
		}
		b, _ = json.Marshal(entry("result", "new"))
		return response(string(b)), nil
	})}}
	if err := c.Upload(t.Context(), "s", "data", local, entry("result", "new"), Missing); err != nil || calls != 2 {
		t.Fatalf("lost upload ACK: calls=%d err=%v", calls, err)
	}
	// A syntactically valid but wrong checksum is never transport-retried.
	calls = 0
	c.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(`{}`), nil })
	if err := c.Upload(t.Context(), "s", "data", local, entry("result", "new"), Missing); !errors.Is(err, ErrDigest) || calls != 1 {
		t.Fatalf("corrupt ACK accepted/retried: calls=%d err=%v", calls, err)
	}
}
