package server

import (
	"context"
	"reflect"
	"strconv"
	"testing"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
)

func schedulerFixture(t *testing.T) (*Scheduler, func(string) *Task) {
	t.Helper()
	requireTestPostgres(t)
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	for _, key := range []string{schedKeyLastFinding, schedKeyFiredGoals, schedKeyLastTimeout, schedKeyLastToolCall, schedKeyLastTaskCreate} {
		value, err := m.pg.GetSchedState(key)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if value == "" {
				_, _ = m.pg.Exec(`DELETE FROM scheduler_state WHERE key=$1`, key)
			} else if err := m.pg.SetSchedState(key, value); err != nil {
				t.Errorf("restore scheduler state: %v", err)
			}
		})
		if _, err := m.pg.Exec(`DELETE FROM scheduler_state WHERE key=$1`, key); err != nil {
			t.Fatal(err)
		}
	}
	// Keep the real trigger queue paused so the test can inspect exactly what
	// would be dispatched, without running an LLM or relying on scheduler ticks.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := &Server{m: m, ctx: ctx, chatAgent: new(agent.ChatAgent), triggerQ: map[string][]triggeredRun{}, triggerActive: map[string]int{}, triggerCfg: map[string]triggerBehavior{}}
	create := func(description string) *Task {
		t.Helper()
		task, err := m.CreateTask(description, "scheduler fixture", nil, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := m.DeleteTask(task.ID, DeleteTaskOptions{}); err != nil {
				t.Errorf("delete scheduler fixture: %v", err)
			}
		})
		return task
	}
	return newScheduler(s), create
}

func queuedTriggerTasks(s *Server, key string) []string {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	var ids []string
	for _, run := range s.triggerQ[key] {
		ids = append(ids, strconv.FormatInt(run.taskID, 10))
	}
	return ids
}

func TestSchedulerTaskCreateWatermarkSurvivesRestart(t *testing.T) {
	sc, create := schedulerFixture(t)
	create("historical task before first scheduler startup")
	sc.init()
	triggers := []*db.AgentTrigger{{AgentKey: "scheduler-regression", OnTaskCreate: true}}
	sc.fireTaskCreates(triggers)
	if got := queuedTriggerTasks(sc.s, "scheduler-regression"); len(got) != 0 {
		t.Fatalf("historical task replayed on first startup: %v", got)
	}
	live := create("created while scheduler is running")
	sc.fireTaskCreates(triggers)
	sc.fireTaskCreates(triggers)
	if got := queuedTriggerTasks(sc.s, "scheduler-regression"); !reflect.DeepEqual(got, []string{live.ID}) {
		t.Fatalf("task was missed or dispatched twice: %v", got)
	}
	duringRestart := create("created while scheduler is stopped")
	restarted := newScheduler(sc.s)
	restarted.init()
	restarted.fireTaskCreates(triggers)
	restarted.fireTaskCreates(triggers)
	if got := queuedTriggerTasks(sc.s, "scheduler-regression"); !reflect.DeepEqual(got, []string{live.ID, duringRestart.ID}) {
		t.Fatalf("restart replayed old events or lost downtime events: %v", got)
	}
}

func TestSchedulerEnablingTriggerDoesNotReplayConsumedEvents(t *testing.T) {
	sc, create := schedulerFixture(t)
	enabled, err := sc.pg.ListEnabledTriggers()
	if err != nil {
		t.Fatal(err)
	}
	for _, trigger := range enabled {
		if _, err := sc.pg.Exec(`UPDATE agent_triggers SET enabled=false WHERE id=$1`, trigger.ID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := sc.pg.Exec(`UPDATE agent_triggers SET enabled=true WHERE id=$1`, trigger.ID); err != nil {
				t.Errorf("restore trigger: %v", err)
			}
		})
	}
	sc.init()
	disabled := create("created while all triggers are disabled")
	sc.step()
	if got := sc.mustState(schedKeyLastTaskCreate); got != disabled.ID {
		t.Fatalf("scheduler did not consume events while all triggers were disabled: watermark=%q want=%q", got, disabled.ID)
	}
	triggers := []*db.AgentTrigger{{AgentKey: "scheduler-regression", OnTaskCreate: true}}
	sc.fireTaskCreates(triggers)
	if got := queuedTriggerTasks(sc.s, "scheduler-regression"); len(got) != 0 {
		t.Fatalf("newly enabled trigger replayed historical events: %v", got)
	}
	live := create("created after enabling task-create trigger")
	sc.fireTaskCreates(triggers)
	if got := queuedTriggerTasks(sc.s, "scheduler-regression"); !reflect.DeepEqual(got, []string{live.ID}) {
		t.Fatalf("new trigger did not receive its first live event: %v", got)
	}
}
