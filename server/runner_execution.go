package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/RC-CHN/ARTEX/agent"
	"github.com/RC-CHN/ARTEX/db"
	"github.com/RC-CHN/ARTEX/norma/permission"
	"github.com/RC-CHN/ARTEX/norma/skill"
	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/resourcelib"
	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

type remoteExecutionKey struct{}

// This marker is only returned before a worker model/tool run has started.
// Raw broker errors from an existing runtime must never cause a whole-run retry.
var errRunnerPreparationUnavailable = errors.New("runner execution preparation unavailable")

func runnerPreparationError(err error) error {
	if !errors.Is(err, runner.ErrUnknownOutcome) && (errors.Is(err, runner.ErrClosed) || errors.Is(err, runner.ErrStoreUnavailable) || errors.Is(err, runner.ErrStoreConflict)) {
		return errors.Join(errRunnerPreparationUnavailable, err)
	}
	return err
}

type remoteExecution struct {
	*runner.Session
	spec              runner.RunSpec
	controllerMCP     map[string]bool
	environmentPrompt string
}

// Environment is a versioned run snapshot. In particular, never forward the
// controller's inherited proxy variables or local CA paths with each tool call.
func (r *remoteExecution) Call(ctx context.Context, name string, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	copyTC := actool.ToolContext{}
	if tc != nil {
		copyTC = *tc
	}
	copyTC.Env = nil
	return r.Session.Call(ctx, name, in, &copyTC)
}
func (s *Server) wireRunnerExecution() {
	s.runnerFiles = runnerfiles.NewStore()
	agent.PrepareExecution = func(ctx context.Context, key string, info agent.RunInfo, dir string) (context.Context, actool.ExecutionRuntime, func(), error) {
		cfg, hub, err := s.runnerSnapshot()
		if err != nil {
			return ctx, nil, nil, errors.Join(errRunnerPreparationUnavailable, err)
		}
		if runnerNeedsRemote(cfg, key) && (hub == nil || hub.IsClosed()) {
			return ctx, nil, nil, errors.Join(errRunnerPreparationUnavailable, runner.ErrClosed)
		}
		// Closed server instances can remain in the optional global hook in
		// embedded/test hosts. Do not read their released DB for local runs.
		if info.TaskID > 0 && s.m != nil && (s.ctx == nil || s.ctx.Err() == nil) {
			snapshot, e := s.taskResourceSnapshot(ctx, info.TaskID)
			if e != nil {
				return ctx, nil, nil, e
			}
			if snapshot != "" {
				ctx = agent.WithResourceSnapshot(ctx, snapshot)
				ctx = agent.WithResourceUpdates(ctx, func(queryCtx context.Context) ([]resourcelib.UpdateState, error) {
					return s.resourceLibrary().UpdateStates(queryCtx, info.TaskID)
				})
			}
		}
		if !runnerNeedsRemote(cfg, key) {
			return ctx, nil, nil, nil
		}
		spec, localMCP, err := s.runnerSpec(ctx, cfg, key, info, dir)
		if err != nil {
			return ctx, nil, nil, err
		}
		// Timeout only queue/open, not the subsequent lifetime of the execution.
		runCtx, cancelCause := context.WithCancelCause(ctx)
		cancel := func() { cancelCause(context.Canceled) }
		timeoutDone := make(chan struct{})
		timer := time.AfterFunc(time.Duration(cfg.AcquireTimeoutSeconds)*time.Second, func() {
			cancelCause(context.DeadlineExceeded)
			close(timeoutDone)
		})
		session, err := hub.Acquire(runCtx, spec)
		if !timer.Stop() {
			<-timeoutDone
		}
		if err == nil && runCtx.Err() != nil {
			err = errors.Join(context.Cause(runCtx), session.Close())
		}
		if err != nil {
			if errors.Is(context.Cause(runCtx), context.DeadlineExceeded) {
				cause := error(context.DeadlineExceeded)
				if errors.Is(err, runner.ErrCapacityUnavailable) {
					cause = errors.Join(runner.ErrCapacityUnavailable, cause)
				}
				err = fmt.Errorf("等待 Runner 或准备执行环境超过 %d 秒（包括文件同步与 Pod 启动）: %w", cfg.AcquireTimeoutSeconds, cause)
			}
			cancel()
			return ctx, nil, nil, fmt.Errorf("远程执行环境分配失败（不会退回本机）: %w", runnerPreparationError(err))
		}
		rt := &remoteExecution{Session: session, spec: spec, controllerMCP: localMCP, environmentPrompt: cfg.Environments[spec.Environment].EnvironmentPrompt}
		go func() {
			if failure := session.WaitFailure(runCtx); failure != nil {
				cancelCause(agent.ExecutionSessionLost(failure))
			}
		}()
		return context.WithValue(runCtx, remoteExecutionKey{}, rt), rt, cancel, nil
	}
}

func (s *Server) runnerSpec(ctx context.Context, cfg runnerConfig, key string, info agent.RunInfo, dir string) (runner.RunSpec, map[string]bool, error) {
	var placement agent.RunnerPlacement
	if info.IntentID > 0 && info.TaskID > 0 {
		if task, ok := s.m.Task(strconv.FormatInt(info.TaskID, 10)); ok {
			n, err := task.Store.GetNode(info.IntentID)
			if err != nil {
				return runner.RunSpec{}, nil, err
			}
			if n != nil {
				var payload struct {
					Runner agent.RunnerPlacement `json:"runner"`
				}
				if err = json.Unmarshal(n.Payload, &payload); err != nil {
					return runner.RunSpec{}, nil, err
				}
				placement = payload.Runner
			}
		}
	}
	envName := placement.Environment
	if envName == "" {
		envName = cfg.DefaultEnvironment
	}
	env, ok := cfg.Environments[envName]
	if !ok {
		return runner.RunSpec{}, nil, fmt.Errorf("unknown runner environment %q", envName)
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return runner.RunSpec{}, nil, err
	}
	spec := runner.RunSpec{ThreadID: fmt.Sprintf("%s:task:%d:intent:%d:session:%s", key, info.TaskID, info.IntentID, info.SessionID), TaskID: strconv.FormatInt(info.TaskID, 10), AgentKey: key, NodeID: placement.NodeID, Environment: envName, Image: env.Image, Labels: map[string]string{}, Env: map[string]string{}, Resources: env.Resources, WorkingDir: dir, MaxOutputChars: 30000}
	for k, v := range env.Labels {
		spec.Labels[k] = v
	}
	for k, v := range placement.Labels {
		spec.Labels[k] = v
	}
	for k, v := range env.Env {
		spec.Env[k] = v
	}
	r := placement.Resources
	if r.CPUMillis > 0 {
		spec.Resources.CPUMillis = r.CPUMillis
	}
	if r.MemoryMiB > 0 {
		spec.Resources.MemoryMiB = r.MemoryMiB
	}
	if r.GPUCount > 0 {
		spec.Resources.GPUCount = r.GPUCount
	}
	if r.GPUModel != "" {
		spec.Resources.GPUModel = r.GPUModel
	}
	if r.GPUMemoryMiB > 0 {
		spec.Resources.GPUMemoryMiB = r.GPUMemoryMiB
	}
	a, err := s.m.pg.GetAgentByKey(key)
	if err != nil {
		return spec, nil, err
	}
	if a == nil {
		return spec, nil, fmt.Errorf("unknown agent %q", key)
	}
	base := actool.DefaultTools()
	base = append(base, actool.NewTaskOutput(), actool.NewTaskStop(), actool.NewTaskList(), actool.NewMonitor(), actool.NewWebFetch(actool.WebFetchConfig{}))
	if a.InteractiveShell {
		base = append(base, actool.ShellSessionTools()...)
	}
	rows, err := s.m.pg.ListTools()
	if err != nil {
		return spec, nil, err
	}
	rowMap := map[string]*db.Tool{}
	for _, row := range rows {
		rowMap[row.Key] = row
	}
	for _, t := range base {
		if !actool.IsExecutionTool(t.Name()) {
			continue
		}
		if row := rowMap[t.Name()]; row != nil && (!row.Enabled || !contains(row.Agents, key)) {
			continue
		}
		spec.Tools = append(spec.Tools, runner.ToolDefinition{Name: t.Name(), Description: t.Description(), InputSchema: t.InputSchema(), Kind: "builtin"})
	}
	if search := s.m.WebSearchOpts(); search.Enabled && a.WebSearch {
		sc := actool.WebSearchConfig{Backend: search.Backend, BraveAPIKey: search.BraveKey, TavilyAPIKey: search.TavilyKey, DeepSeekBaseURL: search.DeepSeekBaseURL, DeepSeekAPIKey: search.DeepSeekAPIKey, DeepSeekModel: search.DeepSeekModel, Proxy: search.Proxy}
		st, e := actool.NewWebSearch(sc)
		if e != nil {
			return spec, nil, e
		}
		b, _ := json.Marshal(sc)
		spec.Tools = append(spec.Tools, runner.ToolDefinition{Name: st.Name(), InputSchema: st.InputSchema(), Kind: "builtin", Exec: b})
	}
	for _, row := range rows {
		if row.System || !row.Enabled || !contains(row.Agents, key) || row.Kind == "shell" {
			continue
		}
		spec.Tools = append(spec.Tools, runner.ToolDefinition{Name: row.Key, Description: row.Description, InputSchema: ensureSchema(row.Schema), Kind: row.Kind, Exec: row.Exec})
	}
	wantIDs, err := s.m.pg.AgentVisible(a.ID, "mcp")
	if err != nil {
		return spec, nil, err
	}
	want := idSet(wantIDs)
	gated := map[string]bool{}
	names, err := s.m.pg.AgentSkillNames(a.ID)
	if err != nil {
		return spec, nil, err
	}
	if reg, e := skill.LoadDir(s.skillDir); e == nil && reg != nil {
		for _, sk := range reg.List() {
			if contains(names, filepath.Base(sk.Dir)) {
				for _, name := range sk.MCPs {
					gated[name] = true
				}
			}
		}
	}
	mcps, err := s.m.pg.ListMCP()
	if err != nil {
		return spec, nil, err
	}
	central := map[string]bool{}
	for _, m := range mcps {
		if !m.Enabled || (!want[m.ID] && !gated[m.Name]) {
			continue
		}
		policy := cfg.MCP[m.Name]
		if policy.Location == "controller" {
			central[m.Name] = true
			continue
		}
		mc := runner.MCPConfig{ID: m.ID, Name: m.Name, Transport: m.Transport, Command: m.Command, Args: jsonStrSlice(m.Args), Env: jsonStrMap(m.Env), URL: m.URL, Insecure: m.Insecure, WorkingDir: dir}
		if m.Transport != "stdio" {
			mc.Headers = mc.Env
			mc.Env = nil
		}
		if policy.Override != nil {
			mc = *policy.Override
			mc.ID = m.ID
			mc.Name = m.Name
			if mc.WorkingDir == "" {
				mc.WorkingDir = dir
			}
		}
		spec.MCPs = append(spec.MCPs, mc)
	}
	plan, err := s.runnerFilePlan(info, dir, names)
	if err != nil {
		return spec, nil, err
	}
	if err = s.runnerNetwork(&spec, &plan, env, cfg); err != nil {
		return spec, nil, err
	}
	if env.FileMode == runnerfiles.SharedMode {
		plan.Mode = runnerfiles.SharedMode
		spec.Labels[runnerfiles.SharedCapability] = "v1"
	}
	spec.Files, _ = json.Marshal(plan)
	versionBytes, _ := json.Marshal(struct {
		Config runnerConfig
		Spec   runner.RunSpec
	}{cfg, spec})
	sum := sha256.Sum256(versionBytes)
	spec.ConfigVersion = hex.EncodeToString(sum[:])
	return spec, central, nil
}

func (s *Server) runnerFilePlan(info agent.RunInfo, dir string, skills []string) (runnerfiles.Plan, error) {
	root := dir
	writable := []string{"."}
	if info.TaskID > 0 {
		root = filepath.Join(s.m.dir, "tasks", strconv.FormatInt(info.TaskID, 10))
		if info.IntentID > 0 {
			writable = []string{fmt.Sprintf("i%d", info.IntentID), "shared"}
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return runnerfiles.Plan{}, err
	}
	if err = os.MkdirAll(root, 0755); err != nil {
		return runnerfiles.Plan{}, err
	}
	plan := runnerfiles.Plan{Mounts: []runnerfiles.Mount{{Name: "workspace", Source: root, Target: root, Writable: writable}}}
	if info.TaskID > 0 {
		// Keep a separate read view: downloading over a background process's
		// writable files would lose updates. Preserve existing working paths.
		plan.Mounts = append(plan.Mounts, runnerfiles.Mount{Name: "published", Source: root, Target: filepath.Join(filepath.Dir(root), ".runner-published", filepath.Base(root))})
		if task, ok := s.m.Task(strconv.FormatInt(info.TaskID, 10)); ok {
			drafts, err := filepath.Abs(filepath.Join(s.m.dir, "drafts"))
			if err != nil {
				return plan, err
			}
			// Only explicitly referenced staging scopes are distributed.
			pattern := regexp.MustCompile(regexp.QuoteMeta(drafts) + `/([A-Za-z0-9_-]+)/uploads/`)
			seen := map[string]bool{}
			for _, match := range pattern.FindAllStringSubmatch(task.Description+"\n"+task.Goal, -1) {
				id := match[1]
				if seen[id] {
					continue
				}
				seen[id] = true
				p := filepath.Join(drafts, id, "uploads")
				if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
					return plan, fmt.Errorf("referenced staging uploads are unavailable: %s", id)
				}
				plan.Mounts = append(plan.Mounts, runnerfiles.Mount{Name: "draft-" + id, Source: p, Target: p})
			}
		}
	}
	if info.TaskID > 0 && s.m.pg != nil {
		versions, err := s.resourceLibrary().TaskVersions(context.Background(), info.TaskID)
		if err != nil {
			return plan, fmt.Errorf("load pinned resources: %w", err)
		}
		for _, v := range versions {
			source := s.resourceLibrary().VersionPath(v)
			if fi, err := os.Stat(source); err != nil || !fi.IsDir() {
				return plan, errors.New("pinned resource files unavailable")
			}
			plan.Mounts = append(plan.Mounts, runnerfiles.Mount{Name: fmt.Sprintf("resource-%d", v.ID), Source: source, Target: source})
		}
	}
	for i, name := range skills {
		if filepath.Base(name) != name || name == "." || name == ".." {
			return plan, errors.New("invalid skill directory")
		}
		p, err := filepath.Abs(filepath.Join(s.skillDir, name))
		if err != nil {
			return plan, err
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			plan.Mounts = append(plan.Mounts, runnerfiles.Mount{Name: fmt.Sprintf("skill-%d", i), Source: p, Target: p})
		}
	}
	return plan, nil
}

type remoteMCPClient struct {
	rt   *remoteExecution
	name string
}

func (c *remoteMCPClient) Close() error { return nil } // the execution session owns all MCP lifetimes
func (c *remoteMCPClient) Tools(ctx context.Context) ([]actool.CoreTool, error) {
	defs, err := c.rt.MCPTools(ctx)
	if err != nil {
		return nil, err
	}
	out := []actool.CoreTool{}
	for _, d := range defs {
		if !strings.HasPrefix(d.Name, "mcp__"+c.name+"__") {
			continue
		}
		name := d.Name
		out = append(out, actool.Build(actool.Spec{Name: name, Description: d.Description, Schema: d.InputSchema, ReadOnly: func(json.RawMessage) bool { return d.ReadOnly }, Concurrent: func(json.RawMessage) bool { return d.Concurrent }, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.AskUser("执行 MCP 工具 " + name)
		}, Run: func(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
			return c.rt.Call(ctx, name, in, tc)
		}}))
	}
	return out, nil
}
func connectAgentMCP(ctx context.Context, m *db.MCPServer) (mcpClient, error) {
	if rt, ok := ctx.Value(remoteExecutionKey{}).(*remoteExecution); ok && !rt.controllerMCP[m.Name] {
		return &remoteMCPClient{rt, m.Name}, nil
	}
	return connectMCP(ctx, m)
}

func (s *Server) runnerProtocolHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg, h, err := s.runnerSnapshot()
		if !cfg.Enabled || h == nil || err != nil {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/runner/v1/files/") {
			handler := runnerfiles.Handler(s.runnerFiles, func(r *http.Request, id string, write bool) (runnerfiles.Plan, error) {
				spec, e := h.AuthorizeSession(r, id, write)
				if e != nil {
					return runnerfiles.Plan{}, e
				}
				var p runnerfiles.Plan
				e = json.Unmarshal(spec.Files, &p)
				return p, e
			}, h.WithSessionWrite)
			http.StripPrefix("/runner/v1/files", handler).ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/runner/v1/poll" {
			if _, err := h.Authenticate(r); err == nil {
				s.refreshRunnerPool(cfg, h)
			}
		}
		http.StripPrefix("/runner/v1", h.Handler()).ServeHTTP(w, r)
	})
}
func (s *Server) runnerTools() []actool.CoreTool {
	cfg, _, _ := s.runnerSnapshot()
	if !cfg.Enabled {
		return nil
	}
	return []actool.CoreTool{actool.Build(actool.Spec{Name: "list_runners", Description: "查看实验性执行节点的在线状态、并发槽、CPU/内存/GPU、标签和环境。创建意图前按需要调用，再在 add_intent 的 runner 字段声明 node_id/environment/labels/resources；不要假设每台机器都有 GPU 或相同网络。活跃线程固定执行节点，资源不足排队，不能自动退回本机。", Schema: map[string]any{"type": "object", "properties": map[string]any{}}, ReadOnly: func(json.RawMessage) bool { return true }, Concurrent: func(json.RawMessage) bool { return true }, Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
		return permission.Allowed()
	}, Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
		c, h, err := s.runnerSnapshot()
		if err != nil {
			return actool.Errorf(err.Error()), nil
		}
		if h == nil {
			return actool.Errorf("Runner 功能已关闭"), nil
		}
		return jsonResult(map[string]any{"nodes": h.ListNodes(), "default_environment": c.DefaultEnvironment, "environments": runnerEnvironmentSummary(c)})
	}})}
}
func runnerEnvironmentSummary(c runnerConfig) map[string]any {
	out := map[string]any{}
	for name, e := range c.Environments {
		out[name] = map[string]any{"resources": e.Resources, "labels": e.Labels}
	}
	return out
}

func runnerFileGuidance(plan runnerfiles.Plan) string {
	if plan.Mode == runnerfiles.SharedMode {
		return "。文件采用共享存储直接挂载，无需同步或发布操作；读写实时可见（受文件系统缓存约束），没有快照版本冲突检测。不要同时覆盖同一个文件；协作产物使用唯一文件名，先写同目录临时文件再原子重命名，避免其他线程读到未完成内容。HOME、/tmp 和 /workspace 为本次执行私有临时空间，结束即回收；需恢复的结果写入任务目录。"
	}
	return "。文件采用版本快照，同任务已发布产物在工具调用前刷新；读取新结果使用只读产物目录，原可写目录不会被下载覆盖。共享目录发布有版本冲突检测；冲突时保留现场，不重放已经执行的工具。快照准备上限为 10,000 个文件、1 GiB；超限需缩小输入范围或使用共享存储。"
}
