package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/runner"
)

func TestRunnerClosedBrokerIsVisibleAndNegativeGPUMemoryRejected(t *testing.T) {
	cfg := defaultRunnerConfig()
	env := cfg.Environments["default"]
	env.Resources.GPUMemoryMiB = -1
	cfg.Environments["default"] = env
	if err := cfg.validate(); err == nil {
		t.Fatal("negative GPU memory accepted by configuration API")
	}
	hub, err := runner.New(runner.Options{Store: &runner.MemoryStore{}})
	if err != nil {
		t.Fatal(err)
	}
	if err = hub.Close(); err != nil {
		t.Fatal(err)
	}
	s := &Server{runnerCfg: defaultRunnerConfig(), runners: hub}
	w := httptest.NewRecorder()
	s.getRunners(w, httptest.NewRequest("GET", "/api/runners", nil))
	var out struct {
		Error string `json:"error"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !strings.Contains(out.Error, "重新保存配置") {
		t.Fatalf("closed broker appeared healthy: %d %s", w.Code, w.Body.String())
	}
}
