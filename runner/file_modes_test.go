package runner

import (
	"encoding/json"
	"github.com/RC-CHN/ARTEX/runnerfiles"
	"testing"
)

func TestFileModeIsolationIncludesOverlappingInputs(t *testing.T) {
	plan := func(mode, source string, writable bool) json.RawMessage {
		m := runnerfiles.Mount{Source: source}
		if writable {
			m.Writable = []string{"."}
		}
		raw, _ := json.Marshal(runnerfiles.Plan{Mode: mode, Mounts: []runnerfiles.Mount{m}})
		return raw
	}
	shared := plan("shared", "/tasks/42", true)
	for _, tc := range []struct {
		mode, source string
		write, want  bool
	}{
		{"snapshot", "/tasks/42", true, true}, {"", "/tasks/42/i1", false, true}, {"snapshot", "/tasks", false, true},
		{"snapshot", "/tasks/420", true, false}, {"shared", "/tasks/42", true, false},
	} {
		if got := incompatibleFileModes(shared, plan(tc.mode, tc.source, tc.write)); got != tc.want {
			t.Errorf("%+v got %v", tc, got)
		}
	}
	if incompatibleFileModes(plan("shared", "/skills", false), plan("snapshot", "/skills", false)) {
		t.Fatal("immutable inputs conflict")
	}
}
