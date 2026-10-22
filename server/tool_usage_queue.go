package server

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RC-CHN/ARTEX/db"
)

type toolUsageBatchRecorder interface {
	InsertToolUsageBatch(context.Context, []db.ToolUsage) error
}

// Only best-effort usage counters go here, never execution receipts or budgets.
// The channel and one writer bound both memory and outstanding DB operations.
type toolUsageQueue struct {
	mu                                 sync.Mutex
	closed                             bool
	queue                              chan db.ToolUsage
	done                               chan struct{}
	cancel                             context.CancelFunc
	once                               sync.Once
	accepted, dropped, failed, written atomic.Uint64
}

func newToolUsageQueue(recorder toolUsageBatchRecorder, capacity int) *toolUsageQueue {
	ctx, cancel := context.WithCancel(context.Background())
	q := &toolUsageQueue{queue: make(chan db.ToolUsage, capacity), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(q.done)
		for first := range q.queue {
			batch := []db.ToolUsage{first}
		collect:
			for len(batch) < 64 {
				select {
				case row, ok := <-q.queue:
					if !ok {
						break collect
					}
					batch = append(batch, row)
				default:
					break collect
				}
			}
			bounded, stop := context.WithTimeout(ctx, 2*time.Second)
			err := recorder.InsertToolUsageBatch(bounded, batch)
			stop()
			if err != nil {
				q.failed.Add(uint64(len(batch)))
			} else {
				q.written.Add(uint64(len(batch)))
			}
			if ctx.Err() != nil {
				for range q.queue {
					q.failed.Add(1)
				}
				return
			}
		}
	}()
	return q
}

func (q *toolUsageQueue) InsertToolUsage(row *db.ToolUsage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		select {
		case q.queue <- *row:
			q.accepted.Add(1)
			return nil
		default:
		}
	}
	q.dropped.Add(1)
	return nil
}

func (q *toolUsageQueue) close() {
	if q == nil {
		return
	}
	q.once.Do(func() {
		q.mu.Lock()
		q.closed = true
		close(q.queue)
		q.mu.Unlock()
		timer := time.AfterFunc(2*time.Second, q.cancel)
		defer timer.Stop()
		defer q.cancel()
		<-q.done
	})
}

func (q *toolUsageQueue) snapshot() map[string]uint64 {
	if q == nil {
		return nil
	}
	return map[string]uint64{"accepted": q.accepted.Load(), "dropped": q.dropped.Load(), "failed_or_unknown": q.failed.Load(), "written": q.written.Load(), "queued": uint64(len(q.queue))}
}
