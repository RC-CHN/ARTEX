package runnernode

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/RC-CHN/ARTEX/mcphttp"
	"github.com/RC-CHN/ARTEX/norma/mcp"
	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/runner"
)

type callRecord struct {
	hash  [32]byte
	done  chan struct{}
	reply SandboxReply
}

// LocalSession is only constructed inside the sandbox subprocess. Using it in
// the host daemon would execute untrusted tools on the host and is prohibited.
type LocalSession struct {
	spec     runner.RunSpec
	ctx      context.Context
	cancel   context.CancelFunc
	manager  *tool.Manager
	registry *tool.Registry
	closers  []io.Closer
	schemas  []runner.ToolDefinition
	mu       sync.Mutex
	calls    map[string]*callRecord
	closed   bool
	once     sync.Once
}

func OpenLocalSession(ctx context.Context, id string, spec runner.RunSpec) (*LocalSession, error) {
	ctx, cancel := context.WithCancel(ctx)
	sum := sha256.Sum256([]byte(id))
	manager, err := tool.NewManager(fmt.Sprintf("runner-%x", sum[:12]))
	if err != nil {
		cancel()
		return nil, err
	}
	s := &LocalSession{spec: spec, ctx: ctx, cancel: cancel, manager: manager, registry: tool.NewRegistry(), calls: map[string]*callRecord{}}
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()
	if err = os.MkdirAll(spec.WorkingDir, 0700); err != nil {
		return nil, err
	}
	builtin := tool.NewRegistry(tool.DefaultTools()...)
	for _, t := range []tool.CoreTool{tool.NewTaskOutput(), tool.NewTaskStop(), tool.NewTaskList(), tool.NewMonitor(), tool.NewWebFetch(tool.WebFetchConfig{Proxy: spec.Env["HTTPS_PROXY"], CACert: spec.Env["SSL_CERT_FILE"]})} {
		builtin.Add(t)
	}
	for _, t := range tool.ShellSessionTools() {
		builtin.Add(t)
	}
	for _, def := range spec.Tools {
		if def.Kind == "mcp" {
			continue
		}
		if def.Kind == "command" || def.Kind == "script" || def.Kind == "http" {
			t, e := customTool(def)
			if e != nil {
				return nil, e
			}
			s.registry.Add(t)
			continue
		}
		if def.Name == "WebFetch" {
			cfg := tool.WebFetchConfig{Proxy: spec.Env["HTTPS_PROXY"], CACert: spec.Env["SSL_CERT_FILE"]}
			if len(def.Exec) > 0 {
				if e := json.Unmarshal(def.Exec, &cfg); e != nil {
					return nil, e
				}
			}
			if controllerLocalURL(cfg.Proxy) {
				return nil, fmt.Errorf("WebFetch proxy must be reachable from the sandbox")
			}
			s.registry.Add(tool.NewWebFetch(cfg))
			continue
		}
		if def.Name == "web_search" {
			var cfg tool.WebSearchConfig
			if len(def.Exec) > 0 {
				if e := json.Unmarshal(def.Exec, &cfg); e != nil {
					return nil, e
				}
			}
			if controllerLocalURL(cfg.Proxy) || controllerLocalURL(cfg.DeepSeekBaseURL) {
				return nil, fmt.Errorf("web_search endpoint must be reachable from the sandbox")
			}
			t, e := tool.NewWebSearch(cfg)
			if e != nil {
				return nil, e
			}
			s.registry.Add(t)
			continue
		}
		t, found := builtin.Get(def.Name)
		if !found {
			return nil, fmt.Errorf("runtime does not implement tool %q; configure a remote-capable implementation", def.Name)
		}
		s.registry.Add(t)
	}
	for _, cfg := range spec.MCPs {
		var client interface {
			Tools(context.Context) ([]tool.CoreTool, error)
			Close() error
		}
		switch cfg.Transport {
		case "", "stdio":
			client, err = mcp.NewStdioClientWithOptions(ctx, cfg.Name, cfg.Command, mcp.StdioOptions{WorkingDir: cfg.WorkingDir, Env: envList(mergeEnv(spec.Env, cfg.Env)), InheritEnv: true}, cfg.Args...)
		case "http":
			client, err = mcphttp.New(ctx, cfg.Name, cfg.URL, cfg.Headers, cfg.Insecure)
		case "sse":
			client, err = mcphttp.NewSSE(ctx, cfg.Name, cfg.URL, cfg.Headers, cfg.Insecure)
		default:
			return nil, fmt.Errorf("MCP %s has unsupported transport", cfg.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("MCP %s initialization: %w", cfg.Name, err)
		}
		s.closers = append(s.closers, client)
		ts, e := client.Tools(ctx)
		if e != nil {
			return nil, fmt.Errorf("MCP %s discovery: %w", cfg.Name, e)
		}
		for _, t := range ts {
			if _, exists := s.registry.Get(t.Name()); exists {
				return nil, fmt.Errorf("duplicate MCP tool %q", t.Name())
			}
			s.registry.Add(t)
			s.schemas = append(s.schemas, runner.ToolDefinition{Name: t.Name(), Description: t.Description(), InputSchema: t.InputSchema(), Kind: "mcp"})
		}
	}
	ok = true
	return s, nil
}

func (s *LocalSession) Call(ctx context.Context, c runner.ToolCall) SandboxReply {
	encoded, _ := json.Marshal(c)
	hash := sha256.Sum256(encoded)
	if c.ID == "" {
		return SandboxReply{Error: "tool call ID is required"}
	}
	s.mu.Lock()
	if old, ok := s.calls[c.ID]; ok {
		s.mu.Unlock()
		if old.hash != hash {
			return SandboxReply{Error: runner.ErrCallConflict.Error()}
		}
		select {
		case <-old.done:
			return old.reply
		case <-ctx.Done():
			return SandboxReply{Error: ctx.Err().Error()}
		}
	}
	if s.closed {
		s.mu.Unlock()
		return SandboxReply{Error: "sandbox session closed"}
	}
	// Retain all outcomes for the lease lifetime; fail closed instead of evicting
	// an old ID that could otherwise repeat a side effect on redelivery.
	if len(s.calls) >= 100000 {
		s.mu.Unlock()
		return SandboxReply{Error: "session tool-call limit reached"}
	}
	record := &callRecord{hash: hash, done: make(chan struct{})}
	s.calls[c.ID] = record
	s.mu.Unlock()
	reply := s.execute(ctx, c)
	s.mu.Lock()
	record.reply = reply
	close(record.done)
	s.mu.Unlock()
	return reply
}
func (s *LocalSession) execute(ctx context.Context, c runner.ToolCall) SandboxReply {
	t, ok := s.registry.Get(c.Name)
	if !ok {
		return SandboxReply{Error: fmt.Sprintf("tool %q is not allowed in this session", c.Name)}
	}
	if err := tool.ValidateInput(t.InputSchema(), c.Input); err != nil {
		return SandboxReply{Result: tool.Errorf(err.Error())}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	if !c.Deadline.IsZero() {
		var done context.CancelFunc
		ctx, done = context.WithDeadline(ctx, c.Deadline)
		defer done()
	}
	cwd := c.WorkingDir
	if cwd == "" {
		cwd = s.spec.WorkingDir
	}
	if !filepath.IsAbs(cwd) {
		return SandboxReply{Error: "tool working directory must be absolute"}
	}
	maxChars := c.MaxOutputChars
	if maxChars <= 0 {
		maxChars = s.spec.MaxOutputChars
	}
	if maxChars <= 0 {
		maxChars = 30000
	}
	outputDir := c.OutputDir
	if outputDir == "" {
		outputDir = filepath.Join(s.spec.WorkingDir, ".tool-output")
	}
	tc := &tool.ToolContext{WorkingDir: cwd, AgentID: s.spec.AgentKey, ToolUseID: c.ID, MaxOutputChars: maxChars, OutputDir: outputDir, Tasks: s.manager, Env: append(envList(s.spec.Env), c.Env...)}
	result, err := t.Call(ctx, c.Input, tc)
	reply := SandboxReply{Result: result, Executed: true, Notifications: s.manager.DrainNotifications()}
	if err != nil {
		reply.Error = err.Error()
	}
	return reply
}
func (s *LocalSession) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.cancel()
		s.manager.Cleanup()
		for _, c := range s.closers {
			_ = c.Close()
		}
	})
	return nil
}

// SandboxRequest is the private, attached stdin/stdout protocol, not a public
// listener. Only the host runner sends requests; credentials are never included.
type SandboxRequest struct {
	ID        string           `json:"id"`
	Method    string           `json:"method"`
	SessionID string           `json:"session_id,omitempty"`
	Spec      *runner.RunSpec  `json:"spec,omitempty"`
	Call      *runner.ToolCall `json:"call,omitempty"`
	CancelID  string           `json:"cancel_id,omitempty"`
}
type SandboxReply struct {
	ID            string                  `json:"id"`
	Result        tool.Result             `json:"result"`
	MCPTools      []runner.ToolDefinition `json:"mcp_tools,omitempty"`
	Notifications []tool.Notification     `json:"notifications,omitempty"`
	Error         string                  `json:"error,omitempty"`
	Executed      bool                    `json:"executed,omitempty"`
}

// ServeSandbox runs a single session until EOF or close. stdout must be reserved
// for JSON lines; all diagnostics belong on stderr.
func ServeSandbox(ctx context.Context, in io.Reader, out io.Writer) error {
	return ServeSandboxWithHeartbeat(ctx, in, out, 0)
}

// A Pod must stop its tools even if the manager or Kubernetes API disappears.
// The manager sends heartbeats independently of long-running calls/transfers.
func ServeSandboxWithHeartbeat(ctx context.Context, in io.Reader, out io.Writer, timeout time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idle *time.Timer
	if timeout > 0 {
		idle = time.AfterFunc(timeout, cancel)
		defer idle.Stop()
	}
	if closer, ok := in.(io.Closer); ok {
		defer closer.Close()
	}
	var session *LocalSession
	defer func() {
		if session != nil {
			session.Close()
		}
	}()
	var writes sync.Mutex
	enc := json.NewEncoder(out)
	send := func(r SandboxReply) { writes.Lock(); defer writes.Unlock(); _ = enc.Encode(r) }
	var calls sync.Mutex
	running := map[string]context.CancelFunc{}
	requests := make(chan SandboxRequest, 128)
	readErrors := make(chan error, 1)
	go func() {
		defer cancel()
		defer close(requests)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 64<<10), 16<<20)
		for scanner.Scan() {
			var req SandboxRequest
			if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
				readErrors <- fmt.Errorf("invalid sandbox request: %w", err)
				return
			}
			if idle != nil {
				idle.Reset(timeout)
			}
			// MCP initialization may block the dispatch loop. A live manager must
			// still receive heartbeat replies while an environment is opening.
			if req.Method == "heartbeat" {
				send(SandboxReply{ID: req.ID})
				continue
			}
			select {
			case requests <- req:
			case <-ctx.Done():
				return
			}
		}
		readErrors <- scanner.Err()
	}()
	for {
		var req SandboxRequest
		select {
		case next, ok := <-requests:
			if !ok {
				select {
				case err := <-readErrors:
					return err
				default:
					return ctx.Err()
				}
			}
			req = next
		case <-ctx.Done():
			return ctx.Err()
		}
		switch req.Method {
		case "open":
			if session != nil || req.Spec == nil {
				send(SandboxReply{ID: req.ID, Error: "one open request is required"})
				continue
			}
			var err error
			session, err = OpenLocalSession(ctx, req.SessionID, *req.Spec)
			if err != nil {
				send(SandboxReply{ID: req.ID, Error: err.Error()})
				return nil
			}
			send(SandboxReply{ID: req.ID, MCPTools: session.schemas})
		case "call":
			if session == nil || req.Call == nil {
				send(SandboxReply{ID: req.ID, Error: "session is not open"})
				continue
			}
			callCtx, stop := context.WithCancel(ctx)
			calls.Lock()
			running[req.ID] = stop
			calls.Unlock()
			go func(req SandboxRequest) {
				r := session.Call(callCtx, *req.Call)
				stop()
				calls.Lock()
				delete(running, req.ID)
				calls.Unlock()
				r.ID = req.ID
				send(r)
			}(req)
		case "cancel":
			calls.Lock()
			if stop := running[req.CancelID]; stop != nil {
				stop()
			}
			calls.Unlock()
		case "notifications":
			if session == nil {
				send(SandboxReply{ID: req.ID, Error: "session is not open"})
			} else {
				send(SandboxReply{ID: req.ID, Notifications: session.manager.DrainNotifications()})
			}
		case "close":
			if session != nil {
				session.Close()
			}
			send(SandboxReply{ID: req.ID})
			return nil
		default:
			send(SandboxReply{ID: req.ID, Error: "unknown sandbox method"})
		}
	}
}
