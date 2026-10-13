package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/runner"
)

func runnerSwitchServer(t *testing.T) *Server {
	t.Helper()
	m := runnerTestManager(t)
	if _, err := m.pg.Exec("DELETE FROM runner_state"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM runner_state") })
	h, err := runner.New(runner.Options{Store: runnerPGStore{m.pg.DB}, PollWait: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	cfg := defaultRunnerConfig()
	cfg.Enabled = true
	env := cfg.Environments["default"]
	env.Image = "example.invalid/runtime@sha256:" + strings.Repeat("a", 64)
	cfg.Environments["default"] = env
	s := &Server{m: m, runnerCfg: cfg, runners: h}
	t.Cleanup(func() {
		if s.runners != nil {
			_ = s.runners.Close()
		}
	})
	t.Cleanup(func() { _, _ = m.pg.Exec("DELETE FROM settings WHERE key=$1", runnerConfigKey) })
	return s
}
func switchConfig(t *testing.T, s *Server, cfg runnerConfig) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.saveRunnerConfig(w, httptest.NewRequest("PUT", "/api/runners/config", bytes.NewReader(b)))
	return w
}

func TestRunnerModeSwitchFencesPendingAcquireAndKeepsSameModeHub(t *testing.T) {
	s := runnerSwitchServer(t)
	old := s.runners
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiting := make(chan error, 1)
	go func() {
		_, err := old.Acquire(ctx, runner.RunSpec{ThreadID: "pending-mode-switch", WorkingDir: "/workspace"})
		waiting <- err
	}()
	cfg := s.runnerCfg
	cfg.DefaultMode = "local"
	if w := switchConfig(t, s, cfg); w.Code != 200 {
		t.Fatalf("mode switch: %d %s", w.Code, w.Body.String())
	}
	select {
	case err := <-waiting:
		if !errors.Is(err, runner.ErrClosed) {
			t.Fatalf("stale acquisition: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("mode switch left pending acquire active")
	}
	if s.runners == old || s.runners == nil || s.runners.IsClosed() || !old.IsClosed() {
		t.Fatal("mode switch did not replace admission owner")
	}
	current := s.runners
	if w := switchConfig(t, s, cfg); w.Code != 200 || s.runners != current {
		t.Fatalf("same mode unnecessarily restarted broker: %d", w.Code)
	}
	raw, ok, err := s.m.pg.GetSetting(runnerConfigKey)
	var saved runnerConfig
	if err != nil || !ok || json.Unmarshal([]byte(raw), &saved) != nil || saved.DefaultMode != "local" {
		t.Fatalf("mode was not persisted: %s %v", raw, err)
	}
}

func TestRunnerModeSwitchRejectsOpeningAndFencedReservations(t *testing.T) {
	s := runnerSwitchServer(t)
	h := s.runners
	token, _, err := h.CreateRegistrationToken(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	reg := runner.RegisterRequest{Token: token, Name: "node", BootID: "boot", Protocol: runner.ProtocolVersion, Capabilities: runner.Capabilities{Slots: 1, CPUMillis: 1000, MemoryMiB: 1024}}
	b, _ := json.Marshal(reg)
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/register", bytes.NewReader(b)))
	var node runner.RegisterResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &node) != nil {
		t.Fatalf("register: %d %s", w.Code, w.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opening := make(chan error, 1)
	go func() {
		_, err := h.Acquire(ctx, runner.RunSpec{ThreadID: "reserved-mode-switch", WorkingDir: "/workspace"})
		opening <- err
	}()
	deadline := time.Now().Add(time.Second)
	for h.ListNodes()[0].ActiveSessions == 0 {
		if time.Now().After(deadline) {
			t.Fatal("session not reserved")
		}
		time.Sleep(time.Millisecond)
	}
	cfg := s.runnerCfg
	cfg.AgentModes = map[string]string{"worker": "local"}
	for _, state := range []string{"opening", "fenced"} {
		if state == "fenced" {
			cancel()
			select {
			case <-opening:
			case <-time.After(time.Second):
				t.Fatal("opening cancellation did not settle")
			}
		}
		w := switchConfig(t, s, cfg)
		if w.Code != 409 || h.IsClosed() || s.runners != h || len(s.runnerCfg.AgentModes) != 0 {
			t.Fatalf("%s reservation bypassed: %d %s", state, w.Code, w.Body.String())
		}
	}
}

func TestRunnerModeSwitchReopenFailureRetainsDurableConfigAndFailsClosed(t *testing.T) {
	s := runnerSwitchServer(t)
	old := s.runners
	if _, err := s.m.pg.Exec(`UPDATE runner_state SET payload='{"version":999}'::jsonb WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	cfg := s.runnerCfg
	cfg.DefaultMode = "local"
	w := switchConfig(t, s, cfg)
	if w.Code != 500 || s.runnerCfg.DefaultMode != "local" || s.runnerErr == nil || s.runners != nil || !old.IsClosed() {
		t.Fatalf("reopen failed open: %d %s", w.Code, w.Body.String())
	}
	oldPrepare := agent.PrepareExecution
	t.Cleanup(func() { agent.PrepareExecution = oldPrepare })
	s.wireRunnerExecution()
	if _, runtime, _, err := agent.PrepareExecution(context.Background(), "worker", agent.RunInfo{}, t.TempDir()); err == nil || runtime != nil {
		t.Fatal("failed reopen fell back to local")
	}
	raw, _, err := s.m.pg.GetSetting(runnerConfigKey)
	var stored runnerConfig
	if err != nil || json.Unmarshal([]byte(raw), &stored) != nil || stored.DefaultMode != "local" {
		t.Fatalf("durable switch was reverted: %s %v", raw, err)
	}
}

func TestRunnerMissingHubRecoveryChecksPersistedLeasesBeforeLocal(t *testing.T) {
	for _, recovery := range []string{"disabled", "local-default", "local-override"} {
		t.Run(recovery, func(t *testing.T) {
			s := runnerSwitchServer(t)
			h := s.runners
			token, _, err := h.CreateRegistrationToken(context.Background(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(runner.RegisterRequest{Token: token, Name: "previous-node", BootID: "boot", Protocol: runner.ProtocolVersion, Capabilities: runner.Capabilities{Slots: 1, CPUMillis: 1000, MemoryMiB: 1024}})
			registered := httptest.NewRecorder()
			h.Handler().ServeHTTP(registered, httptest.NewRequest("POST", "/register", bytes.NewReader(body)))
			if registered.Code != 201 {
				t.Fatal(registered.Body.String())
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pending := make(chan error, 1)
			go func() {
				_, err := h.Acquire(ctx, runner.RunSpec{ThreadID: "recover-old-attempt", WorkingDir: "/workspace"})
				pending <- err
			}()
			deadline := time.Now().Add(time.Second)
			for h.ListNodes()[0].ActiveSessions == 0 {
				if time.Now().After(deadline) {
					t.Fatal("missing old reservation")
				}
				time.Sleep(time.Millisecond)
			}
			if err = h.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-pending:
			case <-time.After(time.Second):
				t.Fatal("old acquisition did not close")
			}
			// Reproduce startup configuration failure: no published broker, but a
			// previous process may still execute under the durable unexpired lease.
			s.runners = nil
			s.runnerCfg = defaultRunnerConfig()
			s.runnerErr = errors.New("configuration could not be loaded")
			cfg := defaultRunnerConfig()
			if recovery != "disabled" {
				cfg.Enabled = true
				env := cfg.Environments["default"]
				env.Image = "example.invalid/runtime@sha256:" + strings.Repeat("a", 64)
				cfg.Environments["default"] = env
				if recovery == "local-default" {
					cfg.DefaultMode = "local"
				} else {
					cfg.AgentModes["worker"] = "local"
				}
			}
			w := switchConfig(t, s, cfg)
			if w.Code != 409 || s.runnerErr == nil || s.runners != nil {
				t.Fatalf("missing Hub bypassed previous lease: %d %s", w.Code, w.Body.String())
			}
			// Advance the durable lease into the past without sleeping through a
			// production heartbeat window; no broker currently owns this fixture.
			snap, err := (runnerPGStore{s.m.pg.DB}).Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]any
			if err = json.Unmarshal(snap.Data, &state); err != nil {
				t.Fatal(err)
			}
			for _, raw := range state["sessions"].(map[string]any) {
				raw.(map[string]any)["lease_until"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
			}
			body, _ = json.Marshal(state)
			if _, err = s.m.pg.Exec("UPDATE runner_state SET payload=$1 WHERE id=1", string(body)); err != nil {
				t.Fatal(err)
			}
			w = switchConfig(t, s, cfg)
			if w.Code != 200 || s.runnerErr != nil || s.runnerCfg.Enabled != cfg.Enabled {
				t.Fatalf("expired lease prevented recovery: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
