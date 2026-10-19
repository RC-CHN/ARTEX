package server

import (
	"github.com/RC-CHN/ARTEX/runnerfiles"
	"strings"
	"testing"
)

func TestRunnerSharedFileModeValidationAndGuidance(t *testing.T) {
	cfg := defaultRunnerConfig()
	env := cfg.Environments["default"]
	for _, mode := range []string{"", "snapshot", "shared", "invalid"} {
		env.FileMode = mode
		cfg.Environments["default"] = env
		err := cfg.validate()
		if (err != nil) != (mode == "invalid") {
			t.Fatalf("mode %q: %v", mode, err)
		}
	}
	shared := runnerFileGuidance(runnerfiles.Plan{Mode: runnerfiles.SharedMode}, "/tasks/42/i7")
	if !strings.Contains(shared, "原子重命名") || !strings.Contains(shared, "没有快照版本冲突检测") {
		t.Fatal("shared semantics missing")
	}
	snapshot := runnerFileGuidance(runnerfiles.Plan{}, "/tasks/42/i7")
	if !strings.Contains(snapshot, "10,000") || !strings.Contains(snapshot, "版本冲突检测") {
		t.Fatal("snapshot bounds missing")
	}
}
