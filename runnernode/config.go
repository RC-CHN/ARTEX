// Package runnernode runs leased execution sandboxes on an independently enrolled
// Linux host. Agent reasoning and all controller credentials stay on the server.
package runnernode

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

// MCPOverride is an explicit per-node replacement for controller-local settings.
// It never mounts arbitrary host paths. Dependencies belong in the runtime image.
type MCPOverride struct {
	Command    *string           `json:"command,omitempty"`
	Args       *[]string         `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	URL        *string           `json:"url,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	WorkingDir *string           `json:"working_dir,omitempty"`
}

type Config struct {
	ServerURL         string                 `json:"server_url"`
	Name              string                 `json:"name"`
	DataDir           string                 `json:"data_dir"`
	WorkspaceDir      string                 `json:"workspace_dir,omitempty"`
	Capabilities      runner.Capabilities    `json:"capabilities"`
	AllowedImages     []string               `json:"allowed_images,omitempty"`
	ImageOverride     string                 `json:"image_override,omitempty"`
	MCPOverrides      map[string]MCPOverride `json:"mcp_overrides,omitempty"`
	Env               map[string]string      `json:"env,omitempty"`
	DockerBinary      string                 `json:"docker_binary,omitempty"`
	Executor          string                 `json:"executor,omitempty"`
	DockerHostDataDir string                 `json:"docker_host_data_dir,omitempty"`
	DockerNetwork     string                 `json:"docker_network,omitempty"`
	Kubernetes        KubernetesConfig       `json:"kubernetes,omitempty"`
	// UnsafeProcess executes on the host without OS isolation. Development only.
	UnsafeProcess     bool   `json:"unsafe_process,omitempty"`
	AllowInsecureHTTP bool   `json:"allow_insecure_http,omitempty"`
	SandboxBinary     string `json:"sandbox_binary,omitempty"`
}

var imageDigest = regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`)
var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c *Config) Validate() error {
	u, err := url.Parse(c.ServerURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("server_url must be an absolute HTTPS URL without credentials")
	}
	if u.Scheme != "https" && !(c.AllowInsecureHTTP && u.Scheme == "http") {
		return fmt.Errorf("HTTPS is required; HTTP needs explicit allow_insecure_http for development")
	}
	if c.Name == "" {
		return fmt.Errorf("node name is required")
	}
	if c.DataDir == "" || !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("data_dir must be absolute")
	}
	if c.WorkspaceDir == "" {
		c.WorkspaceDir = filepath.Join(c.DataDir, "sessions")
	}
	if !filepath.IsAbs(c.WorkspaceDir) || c.WorkspaceDir == "/" {
		return fmt.Errorf("workspace_dir must be an absolute private directory")
	}
	if c.Executor != "kubernetes" && c.WorkspaceDir != filepath.Join(c.DataDir, "sessions") {
		return fmt.Errorf("Docker workspace_dir must be data_dir/sessions")
	}
	if c.Capabilities.Slots < 1 {
		return fmt.Errorf("capabilities.slots must be positive")
	}
	if c.DockerBinary == "" {
		c.DockerBinary = "docker"
	}
	if c.Executor == "" {
		c.Executor = "docker"
	}
	if c.Executor != "docker" && c.Executor != "kubernetes" {
		return fmt.Errorf("executor must be docker or kubernetes")
	}
	if c.DockerHostDataDir != "" && (!filepath.IsAbs(c.DockerHostDataDir) || strings.ContainsAny(c.DockerHostDataDir, ",\n\x00")) {
		return fmt.Errorf("docker_host_data_dir must be an absolute daemon-host path")
	}
	if c.Executor == "kubernetes" {
		if c.UnsafeProcess {
			return fmt.Errorf("kubernetes cannot use unsafe_process")
		}
		if len(c.Capabilities.GPUs) != 0 {
			return fmt.Errorf("Kubernetes pool GPU scheduling is not supported; register GPU hosts with the Docker executor")
		}
		if err := c.Kubernetes.validate(); err != nil {
			return err
		}
	}
	c.Capabilities.Executor = c.Executor
	// Existing protocol labels let older controllers ignore this capability.
	if c.Capabilities.Labels == nil {
		c.Capabilities.Labels = map[string]string{}
	}
	delete(c.Capabilities.Labels, runnerfiles.SharedCapability)
	if c.Executor == "kubernetes" && len(c.Kubernetes.SharedMounts) > 0 {
		c.Capabilities.Labels[runnerfiles.SharedCapability] = "v1"
		for _, m := range c.Kubernetes.SharedMounts {
			if within(c.DataDir, m.Path) || within(m.Path, c.DataDir) || within(c.WorkspaceDir, m.Path) || within(m.Path, c.WorkspaceDir) {
				return fmt.Errorf("shared storage must be separate from runner identity and session caches")
			}
		}
	}
	if c.SandboxBinary == "" {
		c.SandboxBinary = "/usr/local/bin/artex-runner"
	}
	for _, im := range append(append([]string{}, c.AllowedImages...), c.ImageOverride) {
		if im != "" && !imageDigest.MatchString(im) {
			return fmt.Errorf("runtime images must be pinned by sha256 digest")
		}
	}
	return validEnv(c.Env)
}

func LoadConfig(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	// Expand only explicit deployment fields, never arbitrary MCP strings.
	for key, dst := range map[string]*string{
		"ARTEX_RUNNER_SERVER_URL": &c.ServerURL, "ARTEX_RUNNER_NAME": &c.Name,
		"ARTEX_RUNNER_DATA_DIR": &c.DataDir, "ARTEX_RUNNER_DOCKER_HOST_DATA_DIR": &c.DockerHostDataDir,
		"ARTEX_RUNNER_DOCKER_NETWORK": &c.DockerNetwork,
	} {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	return c, c.Validate()
}

func validEnv(env map[string]string) error {
	for k, v := range env {
		if !envKey.MatchString(k) || strings.ContainsRune(v, 0) {
			return fmt.Errorf("invalid environment variable %q", k)
		}
	}
	return nil
}
func envList(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}
func mergeEnv(base map[string]string, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
func within(root, p string) bool {
	rel, e := filepath.Rel(root, p)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func inMounts(p string, plan runnerfiles.Plan) bool {
	for _, m := range plan.Mounts {
		if within(m.Target, p) {
			return true
		}
	}
	return false
}

func decodePlan(raw json.RawMessage) (runnerfiles.Plan, error) {
	var p runnerfiles.Plan
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("file plan: %w", err)
		}
	}
	if p.Mode != "" && p.Mode != "snapshot" && p.Mode != runnerfiles.SharedMode {
		return p, fmt.Errorf("unsupported file mode")
	}
	if len(p.Mounts) > 128 {
		return p, fmt.Errorf("file plan exceeds 128 mounts")
	}
	names := map[string]bool{}
	targets := map[string]bool{}
	for _, m := range p.Mounts {
		if m.Name == "" || names[m.Name] {
			return p, fmt.Errorf("duplicate or empty file mount name")
		}
		if len(m.Writable) > 128 {
			return p, fmt.Errorf("file mount exceeds 128 writable prefixes")
		}
		for _, w := range m.Writable {
			if w != "." && !safeSubPath(w) {
				return p, fmt.Errorf("invalid writable mount prefix")
			}
		}
		names[m.Name] = true
		if !filepath.IsAbs(m.Target) || filepath.Clean(m.Target) != m.Target || strings.ContainsAny(m.Target, "\x00,\n") {
			return p, fmt.Errorf("invalid sandbox mount target")
		}
		// Preserve the controller's logical absolute paths, including native
		// installations under /home or /tmp, but never mask the runtime itself.
		for _, core := range []string{"/proc", "/sys", "/dev", "/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/run", "/var/run", "/opt/artex-tools", "/opt/browsers", "/app/artex"} {
			if within(core, m.Target) || within(m.Target, core) {
				return p, fmt.Errorf("sandbox mount target overlaps a protected runtime directory")
			}
		}
		if m.Target == "/home" || m.Target == "/tmp" || m.Target == "/home/runner" {
			return p, fmt.Errorf("sandbox mount target cannot replace a session's home or temporary root")
		}
		if targets[m.Target] {
			return p, fmt.Errorf("duplicate sandbox mount target")
		}
		targets[m.Target] = true
	}
	for a := range targets {
		for b := range targets {
			if a != b && within(a, b) {
				return p, fmt.Errorf("nested sandbox mounts are not supported")
			}
		}
	}
	return p, nil
}

func controllerLocalURL(raw string) bool {
	u, e := url.Parse(raw)
	if e != nil {
		return false
	}
	h := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// Prepare applies only explicit node overrides and rejects known controller-local
// references. Shared sources require separately provisioned Kubernetes mappings.
func (c Config) Prepare(spec runner.RunSpec) (runner.RunSpec, runnerfiles.Plan, error) {
	spec.MCPs = append([]runner.MCPConfig(nil), spec.MCPs...)
	plan, err := decodePlan(spec.Files)
	if err != nil {
		return spec, plan, err
	}
	if plan.Mode == runnerfiles.SharedMode && (c.Executor != "kubernetes" || len(c.Kubernetes.SharedMounts) == 0) {
		return spec, plan, fmt.Errorf("shared file mode requires configured Kubernetes shared_mounts")
	}
	if c.ImageOverride != "" {
		spec.Image = c.ImageOverride
	}
	if !c.UnsafeProcess {
		if !imageDigest.MatchString(spec.Image) {
			return spec, plan, fmt.Errorf("runtime image requires an immutable sha256 digest")
		}
		if len(c.AllowedImages) > 0 {
			ok := false
			for _, im := range c.AllowedImages {
				ok = ok || im == spec.Image
			}
			if !ok {
				return spec, plan, fmt.Errorf("runtime image is not in this node's allowlist")
			}
		}
	}
	if !filepath.IsAbs(spec.WorkingDir) || (!inMounts(spec.WorkingDir, plan) && spec.WorkingDir != "/workspace") {
		return spec, plan, fmt.Errorf("working directory must be an absolute sandbox mount path")
	}
	spec.Env = mergeEnv(spec.Env, c.Env)
	if err = validEnv(spec.Env); err != nil {
		return spec, plan, err
	}
	for k, v := range spec.Env {
		if strings.Contains(strings.ToLower(k), "proxy") && controllerLocalURL(v) {
			if _, explicit := c.Env[k]; !explicit {
				return spec, plan, fmt.Errorf("environment %s references controller loopback; configure a node env override", k)
			}
		}
	}
	for i, orig := range spec.MCPs {
		m := orig
		m.Env = mergeEnv(nil, m.Env)
		m.Headers = mergeEnv(nil, m.Headers)
		o, overridden := c.MCPOverrides[m.Name]
		if !overridden {
			o, overridden = c.MCPOverrides[fmt.Sprint(m.ID)]
		}
		if o.Command != nil {
			m.Command = *o.Command
		}
		if o.Args != nil {
			m.Args = append([]string{}, (*o.Args)...)
		}
		if o.URL != nil {
			m.URL = *o.URL
		}
		if o.WorkingDir != nil {
			m.WorkingDir = *o.WorkingDir
		}
		m.Env = mergeEnv(m.Env, o.Env)
		m.Headers = mergeEnv(m.Headers, o.Headers)
		if controllerLocalURL(m.URL) && o.URL == nil {
			return spec, plan, fmt.Errorf("MCP %s URL is controller-local; configure mcp_overrides", m.Name)
		}
		if m.WorkingDir == "" {
			m.WorkingDir = spec.WorkingDir
		}
		if !inMounts(m.WorkingDir, plan) && m.WorkingDir != "/workspace" {
			return spec, plan, fmt.Errorf("MCP %s cwd is outside transferred mounts", m.Name)
		}
		if filepath.IsAbs(m.Command) && !inMounts(m.Command, plan) && !strings.HasPrefix(m.Command, "/usr/bin/") && !strings.HasPrefix(m.Command, "/usr/local/bin/") && !strings.HasPrefix(m.Command, "/bin/") && !strings.HasPrefix(m.Command, "/opt/") {
			return spec, plan, fmt.Errorf("MCP %s binary must be in the runtime image or transferred mounts", m.Name)
		}
		for _, arg := range m.Args {
			v := arg
			if j := strings.IndexByte(v, '='); j >= 0 {
				v = v[j+1:]
			}
			if filepath.IsAbs(v) && !inMounts(v, plan) && !strings.HasPrefix(v, "/opt/") && !strings.HasPrefix(v, "/usr/") {
				return spec, plan, fmt.Errorf("MCP %s argument contains an untransferred absolute path", m.Name)
			}
			if controllerLocalURL(v) && o.Args == nil {
				return spec, plan, fmt.Errorf("MCP %s argument references controller loopback; configure mcp_overrides", m.Name)
			}
		}
		for k, v := range m.Env {
			if controllerLocalURL(v) {
				if _, ok := o.Env[k]; !ok {
					return spec, plan, fmt.Errorf("MCP %s environment %s needs a node override", m.Name, k)
				}
			}
		}
		if err = validEnv(m.Env); err != nil {
			return spec, plan, err
		}
		spec.MCPs[i] = m
	}
	return spec, plan, nil
}
