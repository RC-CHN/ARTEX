package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RC-CHN/ARTEX/runner"
)

func TestGlobalEnvironmentPromptPersistsWithoutChangingNetwork(t *testing.T) {
	m := runnerTestManager(t)
	s := &Server{m: m}
	original, _, _ := m.pg.GetSetting(settingEnvironmentPrompt)
	t.Cleanup(func() { _ = m.pg.SetSetting(settingEnvironmentPrompt, original) })
	proxy, capture := m.GlobalProxy(), m.TrafficEnabled()
	put := func(value string) int {
		body, _ := json.Marshal(map[string]string{"environment_prompt": value})
		w := httptest.NewRecorder()
		s.putSettings(w, httptest.NewRequest("PUT", "/api/settings", bytes.NewReader(body)))
		return w.Code
	}
	const text = "可选代理 http://proxy.example.org:8080；{{字面值}}"
	if code := put(text); code != 200 {
		t.Fatalf("save returned %d", code)
	}
	stored, _, err := m.pg.GetSetting(settingEnvironmentPrompt)
	if err != nil || stored != text || s.settingsPayload()["environment_prompt"] != text {
		t.Fatal("not persisted", stored, err)
	}
	if code := put(strings.Repeat("中", 16001)); code != 400 {
		t.Fatal("oversize prompt accepted")
	}
	if s.settingsPayload()["environment_prompt"] != text {
		t.Fatal("rejected save modified prompt")
	}
	if m.GlobalProxy() != proxy || m.TrafficEnabled() != capture {
		t.Fatal("soft proxy changed network configuration")
	}
	if code := put(""); code != 200 || s.settingsPayload()["environment_prompt"] != "" {
		t.Fatal("failed to clear global prompt")
	}
}

func TestNodeEnvironmentPromptAPIStrictDecode(t *testing.T) {
	h, err := runner.New(runner.Options{Store: &runner.MemoryStore{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	token, _, _ := h.CreateRegistrationToken(context.Background(), time.Minute)
	body, _ := json.Marshal(runner.RegisterRequest{Token: token, Name: "node", BootID: "boot", Protocol: runner.ProtocolVersion, Capabilities: runner.Capabilities{Slots: 1}})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/register", bytes.NewReader(body)))
	var node runner.RegisterResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &node) != nil {
		t.Fatal("registration failed", w.Body.String())
	}
	s := &Server{runners: h}
	put := func(raw string) int {
		r := httptest.NewRequest("PUT", "/environment-prompt", strings.NewReader(raw))
		r.SetPathValue("id", node.NodeID)
		w := httptest.NewRecorder()
		s.setRunnerEnvironmentPrompt(w, r)
		return w.Code
	}
	if put(`{"environment_prompt":"node only"}`) != 200 {
		t.Fatal("valid notes rejected")
	}
	for _, invalid := range []string{`null`, `{}`, `{"environment_prompt":null}`, `{"environment_prompt":123}`, `{"environment_prompt":"","unexpected":true}`, `{"environment_prompt":""} {}`} {
		if put(invalid) != 400 {
			t.Fatal("invalid update accepted", invalid)
		}
		if h.ListNodes()[0].EnvironmentPrompt != "node only" {
			t.Fatal("invalid update changed notes")
		}
	}
	if put(`{"environment_prompt":""}`) != 200 || h.ListNodes()[0].EnvironmentPrompt != "" {
		t.Fatal("cannot clear notes")
	}
}
