package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// stdioCloser shuts down a spawned MCP server process and its pipes.
type stdioCloser struct {
	cmd   *exec.Cmd
	stdin io.Closer
	once  sync.Once
}

func (s *stdioCloser) Close() error {
	s.once.Do(func() {
		if s.stdin != nil {
			s.stdin.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			_ = killStdioProcess(s.cmd)
			_ = s.cmd.Wait()
		}
	})
	return nil
}

// NewStdioClient spawns an MCP server as a subprocess, performs the handshake,
// and returns a connected Client (FR-10.1). Call Close to terminate it.
//
// env holds extra environment variables (e.g. API tokens an MCP server needs)
// applied on top of the parent process environment; pass nil for none.
func NewStdioClient(ctx context.Context, server, command string, env map[string]string, args ...string) (*Client, error) {
	opts := StdioOptions{InheritEnv: true}
	for k, v := range env {
		opts.Env = append(opts.Env, k+"="+v)
	}
	return NewStdioClientWithOptions(ctx, server, command, opts, args...)
}

// StdioOptions makes the MCP process's filesystem and environment explicit.
// The zero value inherits no environment; callers must supply any required PATH.
type StdioOptions struct {
	WorkingDir string
	Env        []string
	InheritEnv bool
}

// NewStdioClientWithOptions binds the process lifetime to ctx and terminates its
// process group on Linux. The compatibility constructor retains inherited env.
func NewStdioClientWithOptions(ctx context.Context, server, command string, opts StdioOptions, args ...string) (*Client, error) {
	env := make([]string, 0, len(opts.Env))
	if opts.InheritEnv {
		env = os.Environ()
	}
	env = append(env, opts.Env...)
	resolved, err := resolveStdioCommand(command, opts.WorkingDir, env)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, resolved, args...)
	cmd.Dir = opts.WorkingDir
	cmd.Env = env
	configureStdioProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &Client{
		server: server,
		conn:   newConn(stdout, stdin),
		closer: &stdioCloser{cmd: cmd, stdin: stdin},
	}
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func resolveStdioCommand(command, cwd string, env []string) (string, error) {
	if strings.ContainsAny(command, `/\`) {
		return command, nil
	}
	pathEnv := ""
	extensions := []string{""}
	if runtime.GOOS == "windows" {
		extensions = []string{"", ".com", ".exe", ".bat", ".cmd"}
	}
	for _, pair := range env {
		if k, v, ok := strings.Cut(pair, "="); ok && (k == "PATH" || runtime.GOOS == "windows" && strings.EqualFold(k, "PATH")) {
			pathEnv = v
		}
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		for _, ext := range extensions {
			candidate := filepath.Join(dir, command+ext)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() && (runtime.GOOS == "windows" || st.Mode()&0111 != 0) {
				return candidate, nil
			}
		}
	}
	return "", fmt.Errorf("MCP executable %q not found in its configured PATH", command)
}

// newClientWithConn builds a Client over an arbitrary transport (used by tests).
func newClientWithConn(ctx context.Context, server string, r io.Reader, w io.Writer, closer io.Closer) (*Client, error) {
	c := &Client{server: server, conn: newConn(r, w), closer: closer}
	if err := c.initialize(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
