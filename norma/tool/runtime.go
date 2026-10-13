package tool

import (
	"context"
	"encoding/json"
	"errors"
)

// NotificationSource reports completed background work between model turns.
// Implementations must drain atomically and tolerate concurrent tool calls.
type NotificationSource interface {
	DrainNotifications() []Notification
}

// ExecutionRuntime owns a session's execution environment. Calls must honor ctx
// cancellation. Close must stop its managed processes and release the session;
// implementations must allow Close to interrupt an in-flight Call. Runtime
// failures are returned to the model and never fall back to local execution.
// Results may reference artifacts only by paths accessible through this runtime.
type ExecutionRuntime interface {
	NotificationSource
	Call(context.Context, string, json.RawMessage, *ToolContext) (Result, error)
	Close() error
}

// RuntimeToolMatcher optionally routes additional host-defined execution tools
// (for example custom commands or MCP tools). It should not match domain tools:
// their database writes and approval decisions belong to the host.
type RuntimeToolMatcher interface {
	Handles(string) bool
}

// ExecutionRuntimeDecorator lets transparent host decorators preserve their
// Call behavior (defaults, metering, recovery) while redirecting the inner leaf.
// Implementations must copy themselves and bind their inner tool, never mutate
// a tool instance that may also be used by a local session.
type ExecutionRuntimeDecorator interface {
	WithExecutionRuntime(ExecutionRuntime) CoreTool
}

// FatalExecutionError marks a failed execution whose effects cannot safely be
// retried (for example a lost connection after dispatch). The harness stops the
// run and preserves the original error instead of asking the model to retry.
type FatalExecutionError interface {
	error
	FatalExecution() bool
}

func IsFatalExecution(err error) bool {
	var fatal FatalExecutionError
	return errors.As(err, &fatal) && fatal.FatalExecution()
}

// IsExecutionTool identifies the built-in tools tied to an execution environment.
// Sleep, Skill, TodoWrite and approval/discovery tools remain on the host.
func IsExecutionTool(name string) bool {
	switch name {
	case "Bash", "Read", "Write", "Edit", "MultiEdit", "LS", "Glob", "Grep",
		"WebFetch", "web_search", "TaskOutput", "TaskStop", "TaskList", "Monitor",
		"shell_open", "shell_send", "shell_read", "shell_close", "shell_list":
		return true
	}
	return false
}

// BindExecutionRuntime redirects execution while preserving each tool's schema,
// permission checks and scheduling flags. Call after automatic tool assembly so
// background-management tools cannot replace their runtime-backed counterparts.
// The returned slice is independent of tools; the caller's slice is unchanged.
func BindExecutionRuntime(tools []CoreTool, runtime ExecutionRuntime) []CoreTool {
	if runtime == nil {
		return tools
	}
	out := append([]CoreTool(nil), tools...)
	matcher, _ := runtime.(RuntimeToolMatcher)
	for i, t := range out {
		switch t.Name() {
		case "Skill", "TodoWrite", "AskUserQuestion", "EnterPlanMode", "ExitPlanMode",
			"RecallMemory", "RecordMemory", SearchExtraToolsName, ExecuteExtraToolName:
			continue // session state, disclosure and approval always remain central
		}
		if IsExecutionTool(t.Name()) || (matcher != nil && matcher.Handles(t.Name())) {
			if decorator, ok := t.(ExecutionRuntimeDecorator); ok {
				out[i] = decorator.WithExecutionRuntime(runtime)
			} else {
				out[i] = &executionTool{CoreTool: t, runtime: runtime}
			}
		}
	}
	return out
}

type executionTool struct {
	CoreTool
	runtime ExecutionRuntime
}

func (t *executionTool) AcceptsRawInput() bool {
	v, ok := t.CoreTool.(interface{ AcceptsRawInput() bool })
	return ok && v.AcceptsRawInput()
}

func (t *executionTool) Call(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
	return t.runtime.Call(ctx, t.Name(), input, tc)
}
