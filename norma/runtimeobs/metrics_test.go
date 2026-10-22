package runtimeobs

import (
	"sync"
	"testing"
	"time"
)

func TestConcurrentBoundedSnapshot(t *testing.T) {
	var c Collector
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 1000 {
				c.Record(ToolExecute, 20*time.Millisecond, true)
				_ = c.Snapshot()
			}
		})
	}
	wg.Wait()
	c.Record(255, time.Second, false)
	s := c.Snapshot()
	v := s["tool_execute"]
	if len(s) != int(stageCount) || v.Count != 32000 || v.Errors != 32000 || v.Buckets[2] != 32000 || v.TotalMS != 640000 {
		t.Fatal(s)
	}
	s["tool_execute"] = Timing{}
	if c.Snapshot()["tool_execute"].Count != 32000 {
		t.Fatal("mutable snapshot")
	}
}
