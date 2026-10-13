package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

const runnerConfigKey = "experimental.runners"

var runnerImageDigest = regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`)

type runnerEnvironment struct {
	WarmIdle          int                 `json:"warm_idle,omitempty"`
	EnvironmentPrompt string              `json:"environment_prompt,omitempty"`
	Image             string              `json:"image"`
	Env               map[string]string   `json:"env,omitempty"`
	Resources         runner.Resources    `json:"resources"`
	Labels            map[string]string   `json:"labels,omitempty"`
	ProxyURL          string              `json:"proxy_url,omitempty"`
	DirectNetwork     bool                `json:"direct_network,omitempty"`
	Inputs            []runnerfiles.Mount `json:"inputs,omitempty"`
}
type runnerMCPPolicy struct {
	Location string            `json:"location"` // runner (default) or controller (explicit)
	Override *runner.MCPConfig `json:"override,omitempty"`
}
type runnerConfig struct {
	Enabled               bool                         `json:"enabled"`
	DefaultMode           string                       `json:"default_mode"`
	AgentModes            map[string]string            `json:"agent_modes"`
	DefaultEnvironment    string                       `json:"default_environment"`
	Environments          map[string]runnerEnvironment `json:"environments"`
	MCP                   map[string]runnerMCPPolicy   `json:"mcp"`
	AcquireTimeoutSeconds int                          `json:"acquire_timeout_seconds"`
}

func defaultRunnerConfig() runnerConfig {
	return runnerConfig{DefaultMode: "remote", AgentModes: map[string]string{}, DefaultEnvironment: "default", Environments: map[string]runnerEnvironment{"default": {Resources: runner.Resources{CPUMillis: 1000, MemoryMiB: 1024, PIDs: 256}}}, MCP: map[string]runnerMCPPolicy{}, AcquireTimeoutSeconds: 120}
}
func (c runnerConfig) validate() error {
	if c.DefaultMode != "local" && c.DefaultMode != "remote" {
		return errors.New("default_mode must be local or remote")
	}
	for k, v := range c.AgentModes {
		if k == "" || (v != "local" && v != "remote") {
			return errors.New("agent_modes must map agent keys to local or remote")
		}
	}
	if c.AcquireTimeoutSeconds < 1 || c.AcquireTimeoutSeconds > 3600 {
		return errors.New("acquire_timeout_seconds must be 1..3600")
	}
	if _, ok := c.Environments[c.DefaultEnvironment]; !ok {
		return errors.New("default_environment is not defined")
	}
	for name, e := range c.Environments {
		if e.WarmIdle < 0 || e.WarmIdle > 32 {
			return fmt.Errorf("环境 %s 的预热数量必须为 0..32", name)
		}
		if err := runner.ValidateEnvironmentPrompt(e.EnvironmentPrompt); err != nil {
			return fmt.Errorf("环境 %s: %w", name, err)
		}
		if name == "" {
			return errors.New("empty environment name")
		}
		if c.Enabled && !runnerImageDigest.MatchString(e.Image) {
			return fmt.Errorf("environment %s needs an immutable image@sha256:digest", name)
		}
		if e.Resources.CPUMillis <= 0 || e.Resources.MemoryMiB <= 0 || e.Resources.PIDs < 16 || e.Resources.GPUCount < 0 || e.Resources.GPUMemoryMiB < 0 {
			return fmt.Errorf("environment %s needs positive CPU/memory, pids >= 16, nonnegative GPU count/memory", name)
		}
		if e.DirectNetwork && e.ProxyURL != "" {
			return errors.New("direct_network and proxy_url are mutually exclusive")
		}
		if e.ProxyURL != "" {
			u, err := url.Parse(e.ProxyURL)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
				return errors.New("invalid runner proxy_url")
			}
		}
		for _, m := range e.Inputs {
			if m.Name == "" || !filepath.IsAbs(m.Source) || !filepath.IsAbs(m.Target) || len(m.Writable) > 0 {
				return errors.New("environment inputs require name, absolute source/target, and read-only access")
			}
		}
	}
	for name, p := range c.MCP {
		if name == "" || (p.Location != "runner" && p.Location != "controller") {
			return errors.New("MCP location must be runner or controller")
		}
		if p.Override != nil && p.Override.Name != name {
			return errors.New("MCP override must preserve its name")
		}
		if p.Override != nil && p.Location == "controller" {
			return errors.New("controller MCP configuration belongs in the MCP page; override is only for runner MCPs")
		}
	}
	return nil
}

// Decode into a fresh value: JSON map decoding merges existing entries, which
// would otherwise resurrect a deleted default environment.
func decodeRunnerConfig(r io.Reader) (runnerConfig, error) {
	var cfg runnerConfig
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return cfg, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("expected one configuration object")
	}
	return cfg, cfg.validate()
}

type runnerPGStore struct{ db *sql.DB }

func (p runnerPGStore) Guard(ctx context.Context, revision uint64, commit func() error) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current uint64
	if err = tx.QueryRowContext(ctx, "SELECT revision FROM runner_state WHERE id=1 FOR UPDATE").Scan(&current); err != nil {
		return err
	}
	if current != revision {
		return runner.ErrStoreConflict
	}
	if err = commit(); err != nil {
		return err
	}
	return tx.Commit()
}

func (p runnerPGStore) Load(ctx context.Context) (runner.Snapshot, error) {
	var s runner.Snapshot
	err := p.db.QueryRowContext(ctx, "SELECT revision, payload FROM runner_state WHERE id=1").Scan(&s.Revision, &s.Data)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return s, err
}
func (p runnerPGStore) Save(ctx context.Context, revision uint64, data json.RawMessage) (uint64, error) {
	var next uint64
	var err error
	if revision == 0 {
		err = p.db.QueryRowContext(ctx, "INSERT INTO runner_state(id, revision, payload) VALUES(1,1,$1) ON CONFLICT DO NOTHING RETURNING revision", string(data)).Scan(&next)
	} else {
		err = p.db.QueryRowContext(ctx, "UPDATE runner_state SET revision=revision+1,payload=$1 WHERE id=1 AND revision=$2 RETURNING revision", string(data), revision).Scan(&next)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = runner.ErrStoreConflict
	}
	return next, err
}
func (s *Server) initRunners() {
	cfg := defaultRunnerConfig()
	raw, ok, err := s.m.pg.GetSetting(runnerConfigKey)
	if err != nil {
		s.runnerErr = fmt.Errorf("load runner configuration: %w", err)
	} else if ok {
		cfg, s.runnerErr = decodeRunnerConfig(strings.NewReader(raw))
	}
	s.runnerCfg = cfg
	if cfg.Enabled && s.runnerErr == nil {
		s.runners, s.runnerErr = runner.New(runner.Options{Store: runnerPGStore{s.m.pg.DB}})
	}
	if s.runners != nil {
		s.runners.SetWarmProfiles(s.runnerWarmProfiles(cfg))
	}
	s.wireRunnerExecution()
}
func (s *Server) runnerSnapshot() (runnerConfig, *runner.Hub, error) {
	s.runnerMu.RLock()
	defer s.runnerMu.RUnlock()
	return s.runnerCfg, s.runners, s.runnerErr
}
func (s *Server) getRunners(w http.ResponseWriter, r *http.Request) {
	cfg, hub, err := s.runnerSnapshot()
	nodes := []runner.NodeView{}
	if hub != nil {
		nodes = hub.ListNodes()
		if err == nil && hub.IsClosed() {
			err = errors.New("Runner broker 已停止，请重新保存配置以恢复；旧执行租约会保留到清理确认或到期")
		}
	}
	out := map[string]any{"config": cfg, "nodes": nodes, "experimental": true}
	if err != nil {
		out["error"] = err.Error()
	}
	writeJSON(w, 200, out)
}
func (s *Server) saveRunnerConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := decodeRunnerConfig(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	s.runnerMu.Lock()
	defer s.runnerMu.Unlock()
	b, _ := json.Marshal(cfg)
	persist := func() error { return s.m.pg.SetSetting(runnerConfigKey, string(b)) }
	modeChanged := cfg.Enabled && s.runnerCfg.Enabled && (cfg.DefaultMode != s.runnerCfg.DefaultMode || !maps.Equal(cfg.AgentModes, s.runnerCfg.AgentModes))
	allowsLocal := cfg.DefaultMode == "local"
	for _, mode := range cfg.AgentModes {
		allowsLocal = allowsLocal || mode == "local"
	}
	unknownReservations := s.runners == nil || s.runners.IsClosed() || s.runnerErr != nil
	if !cfg.Enabled || modeChanged || (unknownReservations && allowsLocal) {
		// A local execution must not bypass a previous remote lease. If startup
		// failed before publishing a broker, reload persisted reservations first.
		previous := s.runners
		created := false
		if previous == nil || previous.IsClosed() {
			if previous != nil {
				_ = previous.Close()
			}
			previous, err = runner.New(runner.Options{Store: runnerPGStore{s.m.pg.DB}})
			created = true
		}
		if err == nil {
			err = previous.DisableIfIdle(persist)
		}
		if err != nil {
			if created && previous != nil {
				_ = previous.Close()
			}
			if created {
				s.runnerErr = err
			}
			status := http.StatusInternalServerError
			if errors.Is(err, runner.ErrSessionsActive) {
				status = http.StatusConflict
			}
			writeErr(w, status, err.Error())
			return
		}
		// The switch is now durable and the predecessor refuses admissions.
		// Never restore it if reinitialization fails; keep the new configuration
		// with an explicit error so execution stays closed until the next save.
		s.runnerCfg = cfg
		s.runners, s.runnerErr = nil, nil
		if cfg.Enabled {
			s.runners, s.runnerErr = runner.New(runner.Options{Store: runnerPGStore{s.m.pg.DB}})
		}
		if s.runnerErr != nil {
			writeErr(w, 500, s.runnerErr.Error())
			return
		}
	} else {
		candidate := s.runners
		created := false
		if cfg.Enabled && (candidate == nil || candidate.IsClosed()) {
			if candidate != nil {
				_ = candidate.Close()
			}
			candidate, err = runner.New(runner.Options{Store: runnerPGStore{s.m.pg.DB}})
			created = true
		}
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if err = persist(); err != nil {
			if created && candidate != nil {
				_ = candidate.Close()
			}
			writeErr(w, 500, err.Error())
			return
		}
		s.runners = candidate
	}
	s.runnerCfg = cfg
	s.runnerErr = nil
	if s.runners != nil {
		s.runners.SetWarmProfiles(s.runnerWarmProfiles(cfg))
	}
	writeJSON(w, 200, cfg)
}
func (s *Server) runnerRegistration(w http.ResponseWriter, r *http.Request) {
	cfg, h, err := s.runnerSnapshot()
	if !cfg.Enabled || h == nil || err != nil {
		writeErr(w, 409, "实验性 Runner 未启用或初始化失败")
		return
	}
	token, expiry, err := h.CreateRegistrationToken(r.Context(), 10*time.Minute)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]any{"token": token, "expires_at": expiry})
}
func (s *Server) setRunnerConcurrency(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.runnerSnapshot()
	if h == nil || err != nil {
		writeErr(w, 409, "Runner 未启用或初始化失败")
		return
	}
	var req struct {
		Limit *int `json:"limit"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || req.Limit == nil || decoder.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "需要整数 limit；0 表示使用节点上限")
		return
	}
	if err := h.SetNodeConcurrencyLimit(r.PathValue("id"), *req.Limit); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) setRunnerLimits(w http.ResponseWriter, r *http.Request) {
	_, h, err := s.runnerSnapshot()
	if h == nil || err != nil {
		writeErr(w, 409, "Runner 未启用或初始化失败")
		return
	}
	var req struct {
		Slots  *int   `json:"slots"`
		CPU    *int64 `json:"cpu_millis"`
		Memory *int64 `json:"memory_mib"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&req) != nil || req.Slots == nil || req.CPU == nil || req.Memory == nil || d.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "需要整数 slots、cpu_millis 和 memory_mib；0 表示使用节点硬上限")
		return
	}
	if err := h.SetNodeLimits(r.PathValue("id"), *req.Slots, *req.CPU, *req.Memory); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) setRunnerEnvironmentPrompt(w http.ResponseWriter, r *http.Request) {
	_, hub, err := s.runnerSnapshot()
	if hub == nil || err != nil {
		writeErr(w, 409, "Runner 未启用或初始化失败")
		return
	}
	var body struct {
		Prompt *string `json:"environment_prompt"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || body.Prompt == nil || d.Decode(new(any)) != io.EOF {
		writeErr(w, 400, "需要 environment_prompt 字符串；空字符串表示清除")
		return
	}
	if err := hub.SetNodeEnvironmentPrompt(r.PathValue("id"), *body.Prompt); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"environment_prompt": *body.Prompt})
}

func (s *Server) setRunnerEnabled(w http.ResponseWriter, r *http.Request) {
	_, h, _ := s.runnerSnapshot()
	if h == nil {
		writeErr(w, 409, "Runner 未启用")
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid request")
		return
	}
	if err := h.SetNodeEnabled(r.PathValue("id"), req.Enabled); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
