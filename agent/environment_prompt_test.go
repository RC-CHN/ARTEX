package agent

import (
	"os"
	"strings"
	"testing"
)

func TestEnvironmentPromptLiteralIndependentOfCustomTemplate(t *testing.T) {
	previous, override := EnvironmentPrompt, PromptOverride
	t.Cleanup(func() { EnvironmentPrompt, PromptOverride = previous, override })
	const notes = "可选代理 http://proxy.example.org:8080\n保留字面文本 {{.UnregisteredVariable}}"
	EnvironmentPrompt = func() string { return notes }
	proxy := os.Getenv("HTTP_PROXY")
	for _, key := range []string{"goals", "worker", "planner", "mainagent", "pentest", "custom-agent"} {
		PromptOverride = func(string) (string, bool) { return "自定义正文", true }
		got := renderSystem(key, "默认正文", WorkerVars{})
		if !strings.HasPrefix(got, "自定义正文") || strings.Count(got, notes) != 1 {
			t.Fatalf("%s lost literal environment notes: %s", key, got)
		}
		PromptOverride = func(string) (string, bool) { return "{{.Bad}}", true }
		if got = renderSystem(key, "默认正文", WorkerVars{}); !strings.HasPrefix(got, "默认正文") || !strings.Contains(got, notes) {
			t.Fatal("template fallback removed independent environment notes")
		}
	}
	if os.Getenv("HTTP_PROXY") != proxy || len(proxyEnv("", "")) != 0 {
		t.Fatal("prompt altered process networking")
	}
	EnvironmentPrompt = func() string { return "" }
	if got := renderSystem("worker", "默认正文", WorkerVars{}); got != "默认正文" {
		t.Fatal("cleared notes survived", got)
	}
}
