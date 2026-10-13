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

func TestRunnerLimitsAPIValidationAndPersistence(t *testing.T) {
	store := &runner.MemoryStore{}
	h, err := runner.New(runner.Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	token, _, err := h.CreateRegistrationToken(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(runner.RegisterRequest{Token: token, Name: "pool", BootID: "boot", Protocol: runner.ProtocolVersion, Capabilities: runner.Capabilities{Slots: 4, CPUMillis: 4000, MemoryMiB: 8192}})
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/register", bytes.NewReader(body)))
	var node runner.RegisterResponse
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &node) != nil {
		t.Fatalf("registration: %d", w.Code)
	}
	s := &Server{runners: h}
	put := func(body string) int {
		r := httptest.NewRequest("PUT", "/api/runners/"+node.NodeID+"/limits", strings.NewReader(body))
		r.SetPathValue("id", node.NodeID)
		w := httptest.NewRecorder()
		s.setRunnerLimits(w, r)
		return w.Code
	}
	if got := put(`{"slots":2,"cpu_millis":1500,"memory_mib":2048}`); got != 200 {
		t.Fatalf("valid request: %d", got)
	}
	for _, bad := range []string{
		`null`, `{}`, `{"slots":1}`, `{"slots":1,"cpu_millis":1.5,"memory_mib":2048}`,
		`{"slots":5,"cpu_millis":1500,"memory_mib":2048}`,
		`{"slots":1,"cpu_millis":4001,"memory_mib":2048}`,
		`{"slots":1,"cpu_millis":1500,"memory_mib":8193}`,
		`{"slots":-1,"cpu_millis":1500,"memory_mib":2048}`,
		`{"slots":1,"cpu_millis":1500,"memory_mib":2048,"unknown":1}`,
		`{"slots":1,"cpu_millis":1500,"memory_mib":2048} {}`,
	} {
		if got := put(bad); got != 400 {
			t.Errorf("invalid request accepted: %s (%d)", bad, got)
		}
		v := h.ListNodes()[0]
		if v.EffectiveSlots != 2 || v.EffectiveCPUMillis != 1500 || v.EffectiveMemoryMiB != 2048 {
			t.Fatal("failed update partially changed budgets")
		}
	}
	_ = h.Close()
	reopened, err := runner.New(runner.Options{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	v := reopened.ListNodes()[0]
	if v.EffectiveSlots != 2 || v.EffectiveCPUMillis != 1500 || v.EffectiveMemoryMiB != 2048 {
		t.Fatal("API limits lost on restart")
	}
	s.runners = reopened
	if got := put(`{"slots":0,"cpu_millis":0,"memory_mib":0}`); got != 200 {
		t.Fatalf("reset: %d", got)
	}
	v = reopened.ListNodes()[0]
	if v.EffectiveSlots != 4 || v.EffectiveCPUMillis != 4000 || v.EffectiveMemoryMiB != 8192 {
		t.Fatal("reset did not restore node hard capacity")
	}
}
