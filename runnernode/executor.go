package runnernode

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

type Sandbox interface {
	Request(context.Context, SandboxRequest) (SandboxReply, error)
	Stop() error
}
type Executor interface {
	Start(context.Context, string, runner.RunSpec, runnerfiles.Plan, map[string]string, []string) (Sandbox, error)
}

type DockerExecutor struct {
	Binary        string
	SandboxBinary string
	NodeID        string
	DataDir       string
	HostDataDir   string
	Network       string
	Log           io.Writer
}

// DockerArgs keeps all host-side authority in the daemon. The session receives
// no socket, credentials, host PID namespace, privileged mode or host network.
func (d *DockerExecutor) DockerArgs(id string, spec runner.RunSpec, plan runnerfiles.Plan, roots map[string]string, gpus []string) ([]string, error) {
	if !imageDigest.MatchString(spec.Image) {
		return nil, fmt.Errorf("Docker runtime image is not digest-pinned")
	}
	name := containerName(d.NodeID, id)
	uid := strconv.Itoa(os.Getuid())
	gid := strconv.Itoa(os.Getgid())
	args := []string{"run", "--rm", "-i", "--init", "--name", name, "--label", "site.artex.runner=" + safeID(d.NodeID), "--label", "site.artex.session=" + safeID(id), "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user", uid + ":" + gid, "--workdir", spec.WorkingDir, "--tmpfs", "/tmp:rw,nosuid,nodev,size=512m,mode=1777", "--tmpfs", "/home/runner:rw,nosuid,nodev,size=256m,mode=0700,uid=" + uid + ",gid=" + gid}
	if !inMounts("/workspace", plan) {
		args = append(args, "--tmpfs", "/workspace:rw,nosuid,nodev,size=512m,mode=0700,uid="+uid+",gid="+gid)
	}
	if d.Network != "" {
		args = append(args, "--network", d.Network)
	}
	pids := spec.Resources.PIDs
	if pids <= 0 {
		pids = 256
	}
	args = append(args, "--pids-limit", strconv.FormatInt(pids, 10))
	cpus := spec.Resources.CPUMillis
	if cpus <= 0 {
		cpus = 1000
	}
	args = append(args, "--cpus", strconv.FormatFloat(float64(cpus)/1000, 'f', 3, 64))
	memory := spec.Resources.MemoryMiB
	if memory <= 0 {
		memory = 1024
	}
	args = append(args, "--memory", strconv.FormatInt(memory, 10)+"m", "--memory-swap", strconv.FormatInt(memory, 10)+"m")
	if len(gpus) > 0 {
		for _, id := range gpus {
			if id == "" || strings.ContainsAny(id, "\n,\" ") {
				return nil, fmt.Errorf("invalid GPU device ID")
			}
		}
		args = append(args, "--gpus", `"device=`+strings.Join(gpus, ",")+`"`)
	}
	// Docker's client config can inject host proxy credentials automatically.
	// Explicit empty defaults prevent that hidden inheritance; central/node
	// environment values still opt into a declared proxy normally.
	env := map[string]string{}
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		env[key] = ""
	}
	env = mergeEnv(env, spec.Env)
	env = mergeEnv(env, map[string]string{"HOME": "/home/runner", "TMPDIR": "/tmp", "TMP": "/tmp", "TEMP": "/tmp"})
	for _, e := range envList(env) {
		args = append(args, "--env", e)
	}
	for _, m := range plan.Mounts {
		root := roots[m.Name]
		if !filepath.IsAbs(root) || strings.ContainsAny(root, ",\n\x00") {
			return nil, fmt.Errorf("invalid private mount cache")
		}
		readOnly := true
		for _, p := range m.Writable {
			if p == "." {
				readOnly = false
			}
		}
		hostRoot, err := d.hostPath(root)
		if err != nil {
			return nil, err
		}
		mount := "type=bind,source=" + hostRoot + ",target=" + m.Target + ",bind-propagation=rprivate"
		if readOnly {
			mount += ",readonly"
		}
		args = append(args, "--mount", mount)
		if readOnly {
			for _, p := range m.Writable {
				if p == "" || filepath.IsAbs(p) || filepath.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || strings.ContainsAny(p, ",\x00\n") {
					return nil, fmt.Errorf("invalid writable mount prefix")
				}
				child := filepath.Join(root, p)
				if e := prepareWritable(root, child); e != nil {
					return nil, e
				}
				hostChild, err := d.hostPath(child)
				if err != nil {
					return nil, err
				}
				args = append(args, "--mount", "type=bind,source="+hostChild+",target="+filepath.Join(m.Target, p)+",bind-propagation=rprivate")
			}
		}
	}
	binary := d.SandboxBinary
	if binary == "" {
		binary = "/usr/local/bin/artex-runner"
	}
	args = append(args, "--entrypoint", binary, spec.Image, "--sandbox")
	return args, nil
}

func (d *DockerExecutor) hostPath(p string) (string, error) {
	if d.HostDataDir == "" {
		return p, nil
	}
	if !filepath.IsAbs(d.DataDir) || !filepath.IsAbs(d.HostDataDir) || !within(filepath.Join(d.DataDir, "sessions"), p) {
		return "", fmt.Errorf("Docker mount is outside the runner session cache")
	}
	rel, err := filepath.Rel(d.DataDir, p)
	if err != nil {
		return "", err
	}
	return filepath.Join(d.HostDataDir, rel), nil
}
func prepareWritable(root, target string) error {
	// The private cache has no symlinks after Pull; still reject any component
	// replaced by another sandbox before Docker resolves the host bind source.
	rel, err := filepath.Rel(root, target)
	if err != nil || !within(root, target) {
		return fmt.Errorf("invalid writable cache path")
	}
	p := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		st, e := os.Lstat(p)
		if errors.Is(e, os.ErrNotExist) {
			if e = os.Mkdir(p, 0700); e != nil {
				return e
			}
			continue
		}
		if e != nil {
			return e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in writable cache path")
		}
		if !st.IsDir() && p != target {
			return fmt.Errorf("non-directory writable cache parent")
		}
	}
	return nil
}
func safeID(s string) string               { sum := sha256.Sum256([]byte(s)); return fmt.Sprintf("%x", sum[:12]) }
func containerName(node, id string) string { return "artex-runner-" + safeID(node) + "-" + safeID(id) }
func (d *DockerExecutor) bin() string {
	if d.Binary == "" {
		return "docker"
	}
	return d.Binary
}
func (d *DockerExecutor) Start(ctx context.Context, id string, spec runner.RunSpec, plan runnerfiles.Plan, roots map[string]string, gpus []string) (Sandbox, error) {
	args, err := d.DockerArgs(id, spec, plan, roots, gpus)
	if err != nil {
		return nil, err
	}
	inspect := exec.CommandContext(ctx, d.bin(), "image", "inspect", spec.Image)
	inspect.Stdout = io.Discard
	inspect.Stderr = io.Discard
	if inspect.Run() != nil {
		pull := exec.CommandContext(ctx, d.bin(), "pull", spec.Image)
		pull.Stdout = d.Log
		pull.Stderr = d.Log
		if err = pull.Run(); err != nil {
			return nil, fmt.Errorf("pull fixed runtime image: %w", err)
		}
	}
	cmd := exec.CommandContext(ctx, d.bin(), args...)
	stop := func() error {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		name := containerName(d.NodeID, id)
		for {
			output, err := exec.CommandContext(stopCtx, d.bin(), "rm", "-f", name).CombinedOutput()
			if err == nil || strings.Contains(string(output), "No such container") {
				return nil
			}
			// --rm may already be deleting a normally closed container. This is
			// not an unknown outcome once Docker confirms it no longer exists.
			inspect, inspectErr := exec.CommandContext(stopCtx, d.bin(), "inspect", "--format={{.State.Running}}", name).CombinedOutput()
			if inspectErr != nil && (strings.Contains(string(inspect), "No such object") || strings.Contains(string(inspect), "No such container")) {
				return nil
			}
			if !strings.Contains(strings.ToLower(string(output)), "removal") {
				return fmt.Errorf("Docker could not confirm sandbox removal: %w", err)
			}
			select {
			case <-stopCtx.Done():
				return stopCtx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return startRPC(cmd, stop, d.Log)
}

// CleanupOrphans is called before polling with a new boot ID. A process restart
// cannot inherit old leases, even if Docker kept its containers running.
func (d *DockerExecutor) CleanupOrphans(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, d.bin(), "ps", "-aq", "--filter", "label=site.artex.runner="+safeID(d.NodeID)).Output()
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(string(output)) {
		if err = exec.CommandContext(ctx, d.bin(), "rm", "-f", id).Run(); err != nil {
			return err
		}
	}
	return nil
}

// ProcessExecutor is explicitly unsafe: it isolates process lifetimes and env,
// but has the host user's complete filesystem/network authority. Tests/dev only.
type ProcessExecutor struct {
	Binary string
	Args   []string
	Env    []string
	Log    io.Writer
}

func (p *ProcessExecutor) Start(ctx context.Context, id string, spec runner.RunSpec, plan runnerfiles.Plan, roots map[string]string, gpus []string) (Sandbox, error) {
	if len(plan.Mounts) > 0 || len(gpus) > 0 {
		return nil, fmt.Errorf("unsafe process executor cannot mount files or allocate GPUs; use Docker")
	}
	cmd := exec.CommandContext(ctx, p.Binary, append(append([]string{}, p.Args...), "--sandbox")...)
	cmd.Dir = spec.WorkingDir
	cmd.Env = append([]string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + spec.WorkingDir, "TMPDIR=" + filepath.Join(spec.WorkingDir, "tmp")}, p.Env...)
	if err := os.MkdirAll(filepath.Join(spec.WorkingDir, "tmp"), 0700); err != nil {
		return nil, err
	}
	configureProcess(cmd)
	return startRPC(cmd, func() error {
		err := killProcess(cmd)
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}, p.Log)
}

type rpcSandbox struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stop      func() error
	stopErr   error
	stopMu    sync.Mutex
	stopped   bool
	killOnce  sync.Once
	writeGate chan struct{} // initialized under mu; waiting writers can cancel
	mu        sync.Mutex
	pending   map[string]chan SandboxReply
	done      chan struct{}
	err       error
	next      atomic.Uint64
}

func startRPC(cmd *exec.Cmd, stop func() error, log io.Writer) (*rpcSandbox, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		stdin.Close()
		return nil, err
	}
	s := &rpcSandbox{cmd: cmd, stdin: stdin, stop: stop, pending: map[string]chan SandboxReply{}, done: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 16<<20)
		for sc.Scan() {
			var r SandboxReply
			if e := json.Unmarshal(sc.Bytes(), &r); e != nil {
				s.finish(fmt.Errorf("sandbox emitted invalid JSON"))
				return
			}
			s.mu.Lock()
			ch := s.pending[r.ID]
			delete(s.pending, r.ID)
			s.mu.Unlock()
			if ch != nil {
				ch <- r
			}
		}
		err := sc.Err()
		if e := cmd.Wait(); err == nil {
			err = e
		}
		if err == nil {
			err = io.EOF
		}
		s.finish(err)
	}()
	return s, nil
}
func (s *rpcSandbox) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return
	default:
		s.err = err
		close(s.done)
	}
}
func (s *rpcSandbox) write(ctx context.Context, req SandboxRequest) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	s.mu.Lock()
	if s.writeGate == nil {
		s.writeGate = make(chan struct{}, 1)
	}
	gate := s.writeGate
	s.mu.Unlock()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A child/attach process may stop reading stdin. A partially written JSON
	// frame cannot be safely resumed; closing the pipe interrupts the write and
	// lets the owner fence this attempt and confirm process cleanup.
	interrupt := context.AfterFunc(ctx, func() { _ = s.stdin.Close() })
	err := json.NewEncoder(s.stdin).Encode(req)
	interrupt()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func (s *rpcSandbox) Request(ctx context.Context, req SandboxRequest) (SandboxReply, error) {
	req.ID = strconv.FormatUint(s.next.Add(1), 10)
	ch := make(chan SandboxReply, 1)
	s.mu.Lock()
	select {
	case <-s.done:
		err := s.err
		s.mu.Unlock()
		return SandboxReply{}, err
	default:
	}
	s.pending[req.ID] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, req.ID); s.mu.Unlock() }()
	if err := s.write(ctx, req); err != nil {
		return SandboxReply{}, err
	}
	select {
	case r := <-ch:
		return r, nil
	case <-s.done:
		// The reader delivers a final reply before observing EOF. Prefer that
		// completed response when both channels become ready together.
		select {
		case r := <-ch:
			return r, nil
		default:
		}
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		return SandboxReply{}, err
	case <-ctx.Done():
		cancelCtx, done := context.WithTimeout(context.Background(), time.Second)
		_ = s.write(cancelCtx, SandboxRequest{Method: "cancel", CancelID: req.ID})
		done()
		return SandboxReply{}, ctx.Err()
	}
}
func (s *rpcSandbox) Stop() error {
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	if s.stopped {
		return nil
	}
	s.stdin.Close()
	s.stopErr = s.stop()
	s.killOnce.Do(func() { _ = killProcess(s.cmd) })
	s.stopped = s.stopErr == nil
	return s.stopErr
}
