package db

import (
	"slices"
	"sync"
	"testing"
)

func TestBuiltinSecuritySkillsDefaultOnAndRespectDisabledState(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, key := range []string{"mainagent", "planner", "worker", "auto"} {
		a, err := d.GetAgentByKey(key)
		if err != nil || a == nil {
			t.Fatalf("agent %s: %v", key, err)
		}
		original, err := d.AgentSkillNames(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer d.SetAgentSkillVisibility(a.ID, original)
		for _, name := range []string{"nuclei", "vulnx"} {
			if !slices.Contains(original, name) {
				t.Fatalf("%s is not enabled for %s", name, key)
			}
			if err := d.ToggleSkillVisibility(a.ID, name, false); err != nil {
				t.Fatal(err)
			}
		}
		if err := d.seedBuiltinSkillVisibility(); err != nil {
			t.Fatal(err)
		}
		got, err := d.AgentSkillNames(a.ID)
		if err != nil || slices.Contains(got, "nuclei") || slices.Contains(got, "vulnx") {
			t.Fatalf("disabled skill resurrected: %v, %v", got, err)
		}
		if err := d.SetAgentSkillVisibility(a.ID, []string{"nuclei"}); err != nil {
			t.Fatal(err)
		}
		if err := d.seedBuiltinSkillVisibility(); err != nil {
			t.Fatal(err)
		}
		got, err = d.AgentSkillNames(a.ID)
		if err != nil || !slices.Equal(got, []string{"nuclei"}) {
			t.Fatalf("bulk choice lost on seed: %v, %v", got, err)
		}
	}
}

func TestConcurrentSkillVisibilityEditsAndSeeding(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, err := d.GetAgentByKey("worker")
	if err != nil || a == nil {
		t.Fatal(err)
	}
	original, _ := d.AgentSkillNames(a.ID)
	defer d.SetAgentSkillVisibility(a.ID, original)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			for j := range 12 {
				var err error
				switch i % 3 {
				case 0:
					err = d.ToggleSkillVisibility(a.ID, "vulnx", j%2 == 0)
				case 1:
					err = d.SetAgentSkillVisibility(a.ID, []string{"nuclei"})
				case 2:
					err = d.seedBuiltinSkillVisibility()
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if err := d.SetAgentSkillVisibility(a.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.seedBuiltinSkillVisibility(); err != nil {
		t.Fatal(err)
	}
	got, err := d.AgentSkillNames(a.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty choice resurrected: %v %v", got, err)
	}
}
