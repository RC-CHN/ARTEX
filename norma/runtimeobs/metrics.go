// Package runtimeobs records bounded, content-free process-local timings.
package runtimeobs

import (
	"sync"
	"time"
)

type Stage uint8

const (
	ToolQueue Stage = iota
	ToolExecute
	GraphStructure
	UsageWrite
	ResourcePrepare
	ResourceCommit
	ExportBuild
	ExportPackage
	ModelRequest
	stageCount
)

var names = [stageCount]string{"tool_queue", "tool_execute", "graph_structure", "usage_write", "resource_prepare", "resource_commit_wait", "export_build", "export_package", "model_request"}

// Buckets are disjoint and bound the retained data independently of request count.
var bounds = [7]time.Duration{time.Millisecond, 10 * time.Millisecond, 100 * time.Millisecond, time.Second, 10 * time.Second, time.Minute, 10 * time.Minute}

type Timing struct {
	Count   uint64    `json:"count"`
	Errors  uint64    `json:"errors"`
	TotalMS int64     `json:"total_ms"`
	MaxMS   int64     `json:"max_ms"`
	Buckets [8]uint64 `json:"buckets"`
}
type Collector struct {
	mu     sync.Mutex
	values [stageCount]Timing
}

var Default Collector

func (c *Collector) Record(stage Stage, d time.Duration, failed bool) {
	if stage >= stageCount {
		return
	}
	if d < 0 {
		d = 0
	}
	i := 0
	for i < len(bounds) && d > bounds[i] {
		i++
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v := &c.values[stage]
	v.Count++
	if failed {
		v.Errors++
	}
	ms := d.Milliseconds()
	v.TotalMS += ms
	if ms > v.MaxMS {
		v.MaxMS = ms
	}
	v.Buckets[i]++
}
func (c *Collector) Snapshot() map[string]Timing {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]Timing, len(names))
	for i, n := range names {
		out[n] = c.values[i]
	}
	return out
}
func Record(s Stage, start time.Time, failed bool) { Default.Record(s, time.Since(start), failed) }
