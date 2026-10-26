// Opt-in live-model evaluation. API credentials enter only through stdin on the
// host. Native ARTEX tools execute in a disposable network-disabled container.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/RC-CHN/ARTEX/norma/agentcore"
	"github.com/RC-CHN/ARTEX/norma/harness"
	"github.com/RC-CHN/ARTEX/norma/llm"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/skill"
	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/skills"
)

type dockerRuntime struct{ container string }

func (d dockerRuntime) Close() error                            { return nil } // Outer fixture owns container cleanup.
func (d dockerRuntime) DrainNotifications() []tool.Notification { return nil }
func (d dockerRuntime) Call(ctx context.Context, name string, input json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 130*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "-w", "/work", d.container, "/probe", "--tool", name)
	cmd.Stdin = bytes.NewReader(input)
	data, err := cmd.Output()
	if err != nil {
		return tool.Result{}, errors.New("isolated_tool_transport_failed")
	}
	var result tool.Result
	if json.Unmarshal(data, &result) != nil {
		return tool.Result{}, errors.New("isolated_tool_invalid_result")
	}
	return result, nil
}

func coreTools() []tool.CoreTool {
	return []tool.CoreTool{tool.NewBash(), tool.NewRead(), tool.NewWrite(), tool.NewEdit()}
}

func run() error {
	toolName := flag.String("tool", "", "execute one native tool inside the isolated fixture")
	seed := flag.String("seed", "", "seed embedded skill files and exit")
	container := flag.String("container", "", "disposable fixture container")
	skillDir := flag.String("skills", "", "seeded skill directory on host")
	promptFile := flag.String("prompt", "", "synthetic task file")
	flag.Parse()
	if *seed != "" {
		return skills.SeedDefaults(*seed)
	}
	if *toolName != "" {
		input, err := io.ReadAll(io.LimitReader(os.Stdin, 2<<20))
		if err != nil {
			return err
		}
		for _, t := range coreTools() {
			if t.Name() != *toolName {
				continue
			}
			result, err := t.Call(context.Background(), input, &tool.ToolContext{WorkingDir: "/work", MaxOutputChars: 20000})
			if err != nil {
				result = tool.Errorf(err.Error())
			}
			return json.NewEncoder(os.Stdout).Encode(result)
		}
		return errors.New("unsupported_tool")
	}
	if !strings.HasPrefix(*container, "artex-tack-ai-") {
		return errors.New("expected_disposable_fixture")
	}
	var cfg llm.Config
	if json.NewDecoder(io.LimitReader(os.Stdin, 65536)).Decode(&cfg) != nil {
		return errors.New("invalid_model_config")
	}
	provider, err := llm.NewProvider(cfg)
	if err != nil {
		return errors.New("model_setup_failed")
	}
	registry, err := skill.LoadDir(*skillDir)
	if err != nil {
		return err
	}
	s, ok := registry.Get("tack")
	if !ok {
		return errors.New("missing_tack_skill")
	}
	s.Dir = "/skills/tack"
	registry = skill.NewRegistry(s)
	prompt, err := os.ReadFile(*promptFile)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	session := agentcore.NewSession(agentcore.Options{
		Provider: provider, Tools: append(coreTools(), registry.Tool()), Runtime: dockerRuntime{*container},
		SystemPrompt:   []string{"你是 ARTEX Agent。通过工具完成用户任务，选择适用的 Skill。工作目录 /work，Skill 包在 /skills；环境仅有独立合成测试数据。根据实际工具结果报告，记录任务身份与状态。"},
		PermissionMode: permission.ModeBypass, WorkingDir: "/work", MaxTurns: 30, MaxTokens: 32768,
		DisableBackgroundTasks: true, RequireFinalText: false,
	})
	defer session.Close()
	calls := map[string]int{}
	var terminal *harness.Terminal
	failed := false
	for event, err := range session.Prompt(ctx, string(prompt)) {
		if err != nil {
			failed = true
			continue
		}
		if event.Kind == harness.KindToolUse && event.ToolUse != nil {
			calls[event.ToolUse.Name]++
			fmt.Fprintln(os.Stderr, "tool:", event.ToolUse.Name) // Never print inputs, thinking or credentials.
		}
		if event.Kind == harness.KindResult {
			terminal = event.Terminal
		}
	}
	report := map[string]any{"model": cfg.Model, "calls": calls, "provider_error": failed}
	if terminal != nil {
		report["terminal"], report["turns"], report["usage"] = terminal.Reason, terminal.Turns, terminal.Usage
		report["final_text"] = terminal.Text // Synthetic task only; reasoning is never saved.
	}
	return json.NewEncoder(os.Stdout).Encode(report)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "evaluation_failed:", err)
		os.Exit(1)
	}
}
