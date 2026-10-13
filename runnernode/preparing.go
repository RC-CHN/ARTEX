package runnernode

import (
	"context"
	"fmt"
	"sync"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

// Starting the unchanged Pod while inputs transfer hides cold-start latency.
// Open (and all MCP/tool initialization) still waits for complete inputs.
// Stop cancels startup and waits for actual Pod cleanup before returning.
type preparingSandbox struct {
	ready   chan struct{}
	cancel  context.CancelFunc
	sandbox Sandbox
	err     error
	stopMu  sync.Mutex
}

func (s *preparingSandbox) Request(ctx context.Context, req SandboxRequest) (SandboxReply, error) {
	select {
	case <-ctx.Done():
		return SandboxReply{}, ctx.Err()
	case <-s.ready:
	}
	if s.err != nil {
		return SandboxReply{}, s.err
	}
	if s.sandbox == nil {
		return SandboxReply{}, fmt.Errorf("sandbox startup returned no environment")
	}
	return s.sandbox.Request(ctx, req)
}
func (s *preparingSandbox) Stop() error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	s.cancel()
	<-s.ready
	if s.sandbox != nil {
		return s.sandbox.Stop()
	}
	return nil // Start returned only after confirmed cleanup, otherwise a cleanupSandbox.
}
func (k *KubernetesExecutor) PrepareExecution(ctx context.Context, id string, spec runner.RunSpec, plan runnerfiles.Plan, roots map[string]string) Sandbox {
	startCtx, cancel := context.WithCancel(ctx)
	p := &preparingSandbox{ready: make(chan struct{}), cancel: cancel}
	key := profileKey(runner.WarmProfileFromSpec(spec))
	k.poolMu.Lock()
	claimed := k.claimLocked(key, p)
	k.poolMu.Unlock()
	go func() { defer close(p.ready); p.sandbox, p.err = k.Start(startCtx, id, spec, plan, roots, nil) }()
	return claimed
}
func (k *KubernetesExecutor) prewarmingEnabled(environment string) bool {
	p := k.policySnapshot()
	for _, profile := range p.Profiles {
		if profile.Environment == environment && profile.Idle > 0 {
			return true
		}
	}
	return false
}
