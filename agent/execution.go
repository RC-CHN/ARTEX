package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/RC-CHN/ARTEX/norma/tool"
)

// PrepareExecution optionally provisions an execution environment before tool
// assembly. Install it at server startup, before any agent runs. The returned
// context reaches AugmentTools (for example to discover MCP tools in that
// environment); workDir is the existing logical directory used in prompts.
//
// The SDK owns the runtime during its session. cleanup releases any additional
// host resources. Both are also released on preparation failure; a successful
// runtime is closed exactly once even if tool assembly or session setup fails.
// A nil hook preserves local execution; errors never fall back to local tools.
var PrepareExecution func(context.Context, string, RunInfo, string) (context.Context, tool.ExecutionRuntime, func(), error)

func prepareExecution(ctx context.Context, agentKey, workDir string) (context.Context, tool.ExecutionRuntime, func(), error) {
	info := RunInfoFrom(ctx)
	info.AgentKey = agentKey
	ctx = WithRunInfo(ctx, info)
	if PrepareExecution == nil {
		return ctx, nil, func() {}, nil
	}
	next, runtime, cleanup, err := PrepareExecution(ctx, agentKey, RunInfoFrom(ctx), workDir)
	var owned *executionLease
	if runtime != nil {
		owned = &executionLease{ExecutionRuntime: runtime}
		runtime = owned
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if owned != nil {
				_ = owned.Close()
			}
			if cleanup != nil {
				cleanup()
			}
		})
	}
	if err == nil && next == nil {
		err = fmt.Errorf("execution preparation returned a nil context")
	}
	if err != nil {
		release()
		return ctx, nil, func() {}, fmt.Errorf("prepare %s execution: %w", agentKey, err)
	}
	return next, runtime, release, nil
}

type executionLease struct {
	tool.ExecutionRuntime
	once sync.Once
	err  error
}

func (r *executionLease) Close() error {
	r.once.Do(func() { r.err = r.ExecutionRuntime.Close() })
	return r.err
}

func (r *executionLease) Handles(name string) bool {
	m, ok := r.ExecutionRuntime.(tool.RuntimeToolMatcher)
	return ok && m.Handles(name)
}
