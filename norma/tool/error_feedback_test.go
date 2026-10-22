package tool

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeErrorKeepsContracts(t *testing.T) {
	for _, r := range []Result{Text("ok"), {IsError: true, Content: Text(`{"code":"conflict","next_action":"refresh"}`).Content}} {
		if got := NormalizeError(r, false); !reflect.DeepEqual(got, r) {
			t.Fatal("contract changed")
		}
	}
	for _, ro := range []bool{false, true} {
		r := Text("synthetic failure")
		r.IsError = true
		out := NormalizeError(r, ro)
		var v map[string]any
		if json.Unmarshal([]byte(out.Flatten()), &v) != nil || v["message"] != "synthetic failure" || v["execution_status"] != "unknown" || v["retryable"] != false {
			t.Fatal(out)
		}
		if !ro && !strings.Contains(v["next_action"].(string), "Verify current state") {
			t.Fatal(v)
		}
	}
}
