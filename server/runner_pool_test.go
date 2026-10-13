package server

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/runner"
)

func TestRunnerWarmSettingsPersistAndValidate(t *testing.T) {
	s := runnerSwitchServer(t)
	cfg := s.runnerCfg
	for _, count := range []int{-1, 33, 2, 0} {
		env := cfg.Environments["default"]
		env.WarmIdle = count
		cfg.Environments["default"] = env
		w := switchConfig(t, s, cfg)
		if count < 0 || count > 32 {
			if w.Code != 400 {
				t.Fatalf("accepted invalid prewarm limit %d: %d", count, w.Code)
			}
			continue
		}
		if w.Code != 200 {
			t.Fatalf("save prewarm setting: %d %s", w.Code, w.Body.String())
		}
		raw, ok, err := s.m.pg.GetSetting(runnerConfigKey)
		var restored runnerConfig
		if err != nil || !ok || json.Unmarshal([]byte(raw), &restored) != nil || restored.Environments["default"].WarmIdle != count {
			t.Fatal("prewarm setting did not persist")
		}
	}
}

func TestRunnerQueuedWarmProfileMatchesActualWorker(t *testing.T) {
	s := runnerSwitchServer(t)
	s.skillDir = t.TempDir()
	task, err := s.m.CreateTask("prewarm fixture", "", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := task.Store.AddIntent(map[string]any{"summary": "isolated fixture"}, 5, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	task.lifecycleMu.Lock()
	task.Status = "running"
	task.lifecycleMu.Unlock()
	cfg := s.runnerCfg
	env := cfg.Environments["default"]
	env.WarmIdle = 1
	cfg.Environments["default"] = env
	s.runnerCfg = cfg
	h := s.runners
	token, _, err := h.CreateRegistrationToken(t.Context(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path, credential string, value, out any) {
		t.Helper()
		body, _ := json.Marshal(value)
		r := httptest.NewRequest("POST", path, bytes.NewReader(body))
		if credential != "" {
			r.Header.Set("Authorization", "Bearer "+credential)
		}
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, r)
		if w.Code >= 300 || json.Unmarshal(w.Body.Bytes(), out) != nil {
			t.Fatalf("protocol %s: %d %s", path, w.Code, w.Body.String())
		}
	}
	var node runner.RegisterResponse
	post("/register", "", runner.RegisterRequest{Token: token, Name: "pool", BootID: "boot", Protocol: runner.ProtocolVersion, Capabilities: runner.Capabilities{Executor: "kubernetes", Slots: 2, Environments: []string{"default"}}}, &node)
	s.buildRunnerPool(t.Context(), cfg, h)
	var response runner.PollResponse
	post("/poll", node.Credential, runner.PollRequest{NodeID: node.NodeID, BootID: "boot", Protocol: runner.ProtocolVersion}, &response)
	id, _ := strconv.ParseInt(task.ID, 10, 64)
	dir := filepath.Join(s.m.dir, "tasks", task.ID, "i"+strconv.FormatInt(intent, 10))
	spec, _, err := s.runnerSpec(t.Context(), cfg, "worker", agent.RunInfo{TaskID: id, IntentID: intent}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if response.Pool == nil || len(response.Pool.Profiles) != 2 {
		t.Fatal("queued profile missing")
	}
	actual, _ := json.Marshal(response.Pool.Profiles[1])
	expected, _ := json.Marshal(runner.WarmProfileFromSpec(spec))
	if !bytes.Equal(actual, expected) {
		t.Fatalf("queued profile differs from actual execution: %s / %s", actual, expected)
	}
	// A completed old lookup cannot overwrite a just-disabled configuration.
	current := cfg
	current.Enabled = false
	s.runnerCfg = current
	h.SetWarmProfiles(nil)
	s.buildRunnerPool(t.Context(), cfg, h)
	post("/poll", node.Credential, runner.PollRequest{NodeID: node.NodeID, BootID: "boot", Protocol: runner.ProtocolVersion}, &response)
	if len(response.Pool.Profiles) != 0 {
		t.Fatal("stale lookup restored old prewarm configuration")
	}
}

func TestRunnerWarmProfilesIgnoreGPUAndDisabledFeature(t *testing.T) {
	s := &Server{}
	cfg := defaultRunnerConfig()
	cfg.Environments["default"] = runnerEnvironment{WarmIdle: 2}
	cfg.Environments["gpu"] = runnerEnvironment{WarmIdle: 2, Resources: runner.Resources{GPUCount: 1}}
	if len(s.runnerWarmProfiles(cfg)) != 0 {
		t.Fatal("disabled feature created profiles")
	}
	cfg.Enabled = true
	p := s.runnerWarmProfiles(cfg)
	if len(p) != 1 || p[0].Environment != "default" || p[0].Idle != 2 {
		t.Fatalf("invalid pool profiles: %+v", p)
	}
}
