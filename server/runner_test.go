package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

func TestRunnerConfigDefaultsAndExplicitCompatibility(t *testing.T) {
	c := defaultRunnerConfig()
	if c.Enabled {
		t.Fatal("experimental feature must default off")
	}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.Enabled = true
	if err := c.validate(); err == nil {
		t.Fatal("missing runtime image accepted")
	}
	e := c.Environments["default"]
	e.Image = "ghcr.io/rc-chn/artex@sha256:" + strings.Repeat("a", 64)
	c.Environments["default"] = e
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.AgentModes["planner"] = "local"
	c.MCP["private"] = runnerMCPPolicy{Location: "controller"}
	if err := c.validate(); err != nil {
		t.Fatal(err)
	}
	c.AgentModes["worker"] = "automatic-fallback"
	if err := c.validate(); err == nil {
		t.Fatal("silent fallback mode accepted")
	}
	delete(c.AgentModes, "worker")
	e.Image = "mutable:latest"
	c.Environments["default"] = e
	if err := c.validate(); err == nil {
		t.Fatal("mutable runtime accepted")
	}
}

func TestRunnerConfigReplacementAndCorruption(t *testing.T) {
	c := defaultRunnerConfig()
	c.DefaultEnvironment = "renamed"
	c.Environments = map[string]runnerEnvironment{"renamed": c.Environments["default"]}
	b, _ := json.Marshal(c)
	decoded, err := decodeRunnerConfig(bytes.NewReader(b))
	if err != nil || len(decoded.Environments) != 1 {
		t.Fatalf("replacement merged old environments: %+v %v", decoded.Environments, err)
	}
	for _, raw := range []string{`null`, `{}`, `{"enabled":true}`, string(b) + `{}`, `{"unknown":true}`} {
		if _, err := decodeRunnerConfig(strings.NewReader(raw)); err == nil {
			t.Fatalf("invalid config accepted: %s", raw)
		}
	}
	c.MCP["service"] = runnerMCPPolicy{Location: "controller", Override: &runner.MCPConfig{Name: "service"}}
	if err := c.validate(); err == nil {
		t.Fatal("ignored controller override accepted")
	}
}

func TestRunnerStoredConfigFailuresNeverFallback(t *testing.T) {
	old := agent.PrepareExecution
	t.Cleanup(func() { agent.PrepareExecution = old })
	m := runnerTestManager(t)
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM settings WHERE key=$1", runnerConfigKey) })
	for _, raw := range []string{`{broken`, `{"enabled":true}`, `{"enabled":false}`} {
		if err := m.pg.SetSetting(runnerConfigKey, raw); err != nil {
			t.Fatal(err)
		}
		s := &Server{m: m}
		s.initRunners()
		_, runtime, _, err := agent.PrepareExecution(context.Background(), "worker", agent.RunInfo{}, t.TempDir())
		if s.runnerErr == nil || err == nil || runtime != nil {
			t.Fatalf("corrupt config fell back to host: %v %v", s.runnerErr, err)
		}
	}
	// Remove the deliberately corrupt shared fixture while the connection is
	// still open; later package tests create their own Managers on this test DB.
	if _, err := m.pg.Exec("DELETE FROM settings WHERE key=$1", runnerConfigKey); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m}
	s.initRunners()
	if _, _, _, err := agent.PrepareExecution(context.Background(), "worker", agent.RunInfo{}, t.TempDir()); err == nil {
		t.Fatal("failed database read fell back to host")
	}
}

func TestRunnerConfigCommitFailureDoesNotPublishBroker(t *testing.T) {
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("ALTER TABLE settings ADD CONSTRAINT runner_test_reject CHECK (key <> 'experimental.runners') NOT VALID"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("ALTER TABLE settings DROP CONSTRAINT runner_test_reject") })
	c := defaultRunnerConfig()
	c.Enabled = true
	e := c.Environments["default"]
	e.Image = "example.invalid/artex@sha256:" + strings.Repeat("a", 64)
	c.Environments["default"] = e
	s := &Server{m: m, runnerCfg: defaultRunnerConfig()}
	b, _ := json.Marshal(c)
	w := httptest.NewRecorder()
	s.saveRunnerConfig(w, httptest.NewRequest("PUT", "/api/runners/config", bytes.NewReader(b)))
	if w.Code != 500 || s.runners != nil || s.runnerCfg.Enabled {
		t.Fatalf("failed commit published runtime: %d %v", w.Code, s.runnerCfg.Enabled)
	}
}

func TestRunnerNewCustomToolCannotRunOnController(t *testing.T) {
	var called bool
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(204) }))
	defer endpoint.Close()
	exec, _ := json.Marshal(httpExec{Method: "POST", URL: endpoint.URL})
	s := &Server{}
	tool := s.buildCustomTool(&db.Tool{Key: "added-after-acquire", Kind: "http", Exec: exec})
	ctx := context.WithValue(context.Background(), remoteExecutionKey{}, &remoteExecution{Session: &runner.Session{}})
	// The protocol rejects malformed input before accessing its session. A
	// stale local closure would instead parse it as empty params and POST.
	if _, err := tool.Call(ctx, json.RawMessage(`{broken`), &actool.ToolContext{}); err == nil {
		t.Fatal("new custom tool used its local closure")
	}
	if called {
		t.Fatal("remote custom tool issued a controller request")
	}
}

func TestRunnerCAPublicationKeepsExistingSnapshotImmutable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ca.pem")
	data := []byte("public certificate snapshot")
	if err := publishRunnerCA(p, data); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if err := publishRunnerCA(p, data); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	after, err := os.Stat(p)
	if err != nil || !os.SameFile(before, after) || before.ModTime() != after.ModTime() {
		t.Fatal("concurrent execution rewrote the CA being downloaded")
	}
	if err := publishRunnerCA(p, []byte("changed")); err == nil {
		t.Fatal("immutable CA snapshot overwritten")
	}
}

func TestRunnerNetworkReplacesControllerProxy(t *testing.T) {
	m := runnerTestManager(t)
	s := &Server{m: m}
	search, _ := json.Marshal(actool.WebSearchConfig{Backend: "ddgs", Proxy: "http://localhost:9999"})
	custom, _ := json.Marshal(httpExec{URL: "https://example.invalid", UseRecordingProxy: true})
	spec := runner.RunSpec{Env: map[string]string{}, Tools: []runner.ToolDefinition{{Name: "WebFetch", Kind: "builtin"}, {Name: "web_search", Kind: "builtin", Exec: search}, {Name: "custom", Kind: "http", Exec: custom}}}
	proxy := "http://proxy.example.invalid:8080"
	if err := s.runnerNetwork(&spec, &runnerfiles.Plan{}, runnerEnvironment{ProxyURL: proxy}, defaultRunnerConfig()); err != nil {
		t.Fatal(err)
	}
	for _, tool := range spec.Tools {
		if strings.Contains(string(tool.Exec), "localhost") || !strings.Contains(string(tool.Exec), proxy) {
			t.Fatalf("controller proxy leaked to %s: %s", tool.Name, tool.Exec)
		}
	}
}

func TestRunnerDisabledAndExplicitLocalPreserveExecution(t *testing.T) {
	old := agent.PrepareExecution
	t.Cleanup(func() { agent.PrepareExecution = old })
	s := &Server{runnerCfg: defaultRunnerConfig()}
	s.wireRunnerExecution()
	for _, enabled := range []bool{false, true} {
		s.runnerCfg.Enabled = enabled
		if enabled {
			s.runnerCfg.AgentModes["worker"] = "local"
		}
		ctx := context.Background()
		next, runtime, cleanup, err := agent.PrepareExecution(ctx, "worker", agent.RunInfo{}, t.TempDir())
		if err != nil || runtime != nil || cleanup != nil || next != ctx {
			t.Fatalf("local execution changed: %v, %v", runtime, err)
		}
	}
	s.runnerCfg.AgentModes["worker"] = "remote"
	_, runtime, _, err := agent.PrepareExecution(context.Background(), "worker", agent.RunInfo{}, t.TempDir())
	if err == nil || runtime != nil {
		t.Fatal("remote silently fell back to local")
	}
}

func TestRunnerProtocolUnavailableWhenDisabled(t *testing.T) {
	s := &Server{runnerCfg: defaultRunnerConfig()}
	for _, p := range []string{"/runner/v1/register", "/runner/v1/poll", "/runner/v1/files/manifest"} {
		w := httptest.NewRecorder()
		s.runnerProtocolHandler().ServeHTTP(w, httptest.NewRequest("POST", p, strings.NewReader(`{}`)))
		if w.Code != 404 {
			t.Fatalf("disabled %s status %d", p, w.Code)
		}
	}
}

func runnerTestManager(t *testing.T) *Manager {
	t.Helper()
	requireTestPostgres(t)
	m, e := NewManager(t.TempDir(), "")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}
func TestRunnerPGStateCASAndGuard(t *testing.T) {
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("DELETE FROM runner_state"); err != nil {
		t.Fatal(err)
	}
	p := runnerPGStore{m.pg.DB}
	ctx := context.Background()
	rev, err := p.Save(ctx, 0, json.RawMessage(`{"epoch":"first"}`))
	if err != nil || rev != 1 {
		t.Fatalf("save: %d %v", rev, err)
	}
	var wg sync.WaitGroup
	success := make(chan uint64, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, e := p.Save(ctx, rev, json.RawMessage(`{"epoch":"second"}`))
			if e == nil {
				success <- n
			} else if !errors.Is(e, runner.ErrStoreConflict) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	close(success)
	if len(success) != 1 {
		t.Fatalf("CAS winners=%d", len(success))
	}
	called := false
	if err = p.Guard(ctx, 1, func() error { called = true; return nil }); !errors.Is(err, runner.ErrStoreConflict) || called {
		t.Fatal("superseded owner committed")
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- p.Guard(ctx, 2, func() error { close(entered); <-release; return nil }) }()
	<-entered
	saved := make(chan error, 1)
	go func() { _, err := p.Save(ctx, 2, json.RawMessage(`{"epoch":"third"}`)); saved <- err }()
	select {
	case err := <-saved:
		t.Fatalf("takeover bypassed commit guard: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if err = <-saved; err != nil {
		t.Fatal(err)
	}
}

func TestRunnerFilePlanScopesAndSkills(t *testing.T) {
	base := t.TempDir()
	skills := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(skills, "example"), 0755); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{dir: base}, skillDir: skills}
	plan, err := s.runnerFilePlan(agent.RunInfo{TaskID: 12, IntentID: 34}, filepath.Join(base, "tasks/12/i34"), []string{"example"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mounts) != 3 || plan.Mounts[0].Source != filepath.Join(base, "tasks/12") || strings.Join(plan.Mounts[0].Writable, ",") != "i34,shared" || len(plan.Mounts[1].Writable) != 0 || plan.Mounts[1].Name != "published" || plan.Mounts[1].Source != plan.Mounts[0].Source || len(plan.Mounts[2].Writable) != 0 {
		t.Fatalf("overbroad file plan: %+v", plan)
	}
	if _, err = s.runnerFilePlan(agent.RunInfo{}, base, []string{"../secret"}); err == nil {
		t.Fatal("invalid skill path accepted")
	}
}

func TestRunnerOnlyTransfersReferencedStagingAttachments(t *testing.T) {
	base := t.TempDir()
	wanted := filepath.Join(base, "drafts", "selected-1", "uploads")
	other := filepath.Join(base, "drafts", "unrelated", "uploads")
	for _, p := range []string{wanted, other} {
		if err := os.MkdirAll(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	task := &Task{ID: "12", Description: "附件: " + filepath.Join(wanted, "input.txt"), Goal: filepath.Join(wanted, "other.txt")}
	s := &Server{m: &Manager{dir: base, tasks: map[string]*Task{"12": task}}}
	plan, err := s.runnerFilePlan(agent.RunInfo{TaskID: 12, IntentID: 34}, filepath.Join(base, "tasks/12/i34"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Mounts) != 3 || plan.Mounts[2].Source != wanted || len(plan.Mounts[2].Writable) != 0 {
		t.Fatalf("staging scope incorrect: %+v", plan)
	}
	task.Description = filepath.Join(base, "drafts", "missing", "uploads", "input.txt")
	if _, err := s.runnerFilePlan(agent.RunInfo{TaskID: 12}, filepath.Join(base, "tasks/12"), nil); err == nil {
		t.Fatal("missing referenced attachment silently ignored")
	}
}

func TestRunnerAPIRequiresBrowserAuthentication(t *testing.T) {
	s := &Server{runnerCfg: defaultRunnerConfig(), jwtKey: []byte(strings.Repeat("k", 32))}
	h := s.Handler()
	for _, method := range []string{"GET", "PUT", "POST"} {
		p := "/api/runners"
		if method == "PUT" {
			p += "/config"
		}
		if method == "POST" {
			p += "/registration-token"
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, p, bytes.NewBufferString(`{}`)))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s is not auth protected: %d", p, w.Code)
		}
	}
}

func TestRunnerPlacementPersistsAndScopesRequirements(t *testing.T) {
	m := runnerTestManager(t)
	task, err := m.CreateTask("runner placement test", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"summary": "offline compute", "runner": map[string]any{"node_id": "gpu-node", "environment": "gpu", "labels": map[string]string{"network": "lab"}, "resources": map[string]any{"gpu_count": 1, "memory_mib": 2048}}}
	id, err := task.Store.AddIntent(payload, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m, skillDir: t.TempDir()}
	cfg := defaultRunnerConfig()
	env := cfg.Environments["default"]
	env.Image = "example.invalid/runtime@sha256:" + strings.Repeat("a", 64)
	cfg.Environments["gpu"] = env
	numericID, err := strconv.ParseInt(task.ID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	spec, _, err := s.runnerSpec(context.Background(), cfg, "worker", agent.RunInfo{TaskID: numericID, IntentID: id}, filepath.Join(m.dir, "tasks", task.ID, "i1"))
	if err != nil {
		t.Fatal(err)
	}
	if spec.NodeID != "gpu-node" || spec.Environment != "gpu" || spec.Resources.GPUCount != 1 || spec.Resources.MemoryMiB != 2048 || spec.Labels["network"] != "lab" {
		t.Fatalf("lost placement: %+v", spec)
	}
	if strings.Contains(spec.ThreadID, "work#") {
		t.Fatal("thread identity used a recyclable worker slot")
	}
}
