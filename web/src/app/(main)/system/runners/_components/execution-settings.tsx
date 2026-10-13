"use client";
import * as React from "react";
import { Check, Loader2, Plus, Save, Settings2 } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { useUnsavedChanges } from "@/hooks/use-unsaved-changes";
import { http } from "@/lib/api";
import { parseObject, pretty, runnerError, type Mode, type RunnerConfig, type RunnerEnvironment } from "@/lib/runners";

const roles: Record<string, string> = {
  worker: "工作 Agent",
  planner: "规划 Agent",
  mainagent: "主 Agent",
  pentest: "独立渗透 Agent",
  auto: "Auto",
  retester: "漏洞复测",
};

export function ExecutionSettings({
  stored,
  brokerError,
  saved,
}: {
  stored: RunnerConfig;
  brokerError?: string;
  saved: (config: RunnerConfig) => void;
}) {
  const [config, setConfig] = React.useState(stored);
  const [environments, setEnvironments] = React.useState(pretty(stored.environments));
  const [modes, setModes] = React.useState(pretty(stored.agent_modes));
  const [mcp, setMCP] = React.useState(pretty(stored.mcp));
  const [selected, setSelected] = React.useState(stored.default_environment);
  const [newName, setNewName] = React.useState("");
  const [dirty, setDirty] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [status, setStatus] = React.useState("");
  const [confirm, setConfirm] = React.useState<RunnerConfig | null>(null);
  const [discard, setDiscard] = React.useState(false);
  const [networkDrafts, setNetworkDrafts] = React.useState<Record<string, string>>({});
  const dirtyRef = React.useRef(false);
  useUnsavedChanges(dirty);
  const apply = React.useCallback((next: RunnerConfig) => {
    setConfig(next);
    setEnvironments(pretty(next.environments));
    setModes(pretty(next.agent_modes));
    setMCP(pretty(next.mcp));
    setNetworkDrafts({});
    setSelected((previous) => (Object.hasOwn(next.environments, previous) ? previous : next.default_environment));
    dirtyRef.current = false;
    setDirty(false);
  }, []);
  React.useEffect(() => {
    if (!dirtyRef.current) apply(stored);
  }, [stored, apply]);
  function edit() {
    dirtyRef.current = true;
    setDirty(true);
    setStatus("");
    setError("");
  }
  function patch(value: Partial<RunnerConfig>) {
    edit();
    setConfig((previous) => ({ ...previous, ...value }));
  }
  let envs: Record<string, RunnerEnvironment> | null = null;
  let agentModes: Record<string, Mode> | null = null;
  try {
    envs = parseObject<Record<string, RunnerEnvironment>>(environments, "环境配置");
  } catch {
    /* raw editor can contain incomplete JSON */
  }
  try {
    agentModes = parseObject<Record<string, Mode>>(modes, "Agent 模式");
  } catch {
    /* preserve incomplete JSON */
  }
  const env = envs && Object.hasOwn(envs, selected) ? envs[selected] : null;
  const editable = env && typeof env === "object" && env.resources && typeof env.resources === "object";
  function patchEnvironment(value: Partial<RunnerEnvironment>) {
    if (!envs || !env) return;
    edit();
    setEnvironments(pretty({ ...envs, [selected]: { ...env, ...value } }));
  }
  function addEnvironment() {
    const name = newName.trim();
    if (
      !envs ||
      !/^[a-zA-Z0-9][a-zA-Z0-9._-]*$/.test(name) ||
      ["__proto__", "constructor", "prototype"].includes(name) ||
      Object.hasOwn(envs, name)
    ) {
      toast.error("环境名称需唯一，使用字母、数字、点、短横线或下划线");
      return;
    }
    edit();
    setEnvironments(
      pretty({
        ...envs,
        [name]: { image: "", direct_network: true, resources: { cpu_millis: 1000, memory_mib: 1024, pids: 256 } },
      }),
    );
    setSelected(name);
    setNewName("");
  }
  async function persist(next: RunnerConfig) {
    setConfirm(null);
    setBusy(true);
    setError("");
    try {
      const result = await http<RunnerConfig>("/runners/config", { method: "PUT", body: JSON.stringify(next) });
      apply(result);
      saved(result);
      setStatus("配置已保存，对后续执行生效");
      toast.success("Runner 执行配置已保存");
    } catch (err) {
      setError(runnerError(err));
      toast.error("保存失败，修改已保留");
    } finally {
      setBusy(false);
    }
  }
  function submit() {
    try {
      const next = {
        ...config,
        environments: parseObject<Record<string, RunnerEnvironment>>(environments, "环境配置"),
        agent_modes: parseObject<Record<string, Mode>>(modes, "Agent 模式"),
        mcp: parseObject<Record<string, unknown>>(mcp, "MCP 配置"),
      };
      if (!Object.hasOwn(next.environments, next.default_environment)) throw new Error("请选择已定义的默认环境");
      if (
        !Number.isInteger(next.acquire_timeout_seconds) ||
        next.acquire_timeout_seconds < 1 ||
        next.acquire_timeout_seconds > 3600
      )
        throw new Error("等待上限需为 1–3600 秒的整数");
      for (const [name, env] of Object.entries(next.environments)) {
        if (!Number.isInteger(env.warm_idle ?? 0) || (env.warm_idle ?? 0) < 0 || (env.warm_idle ?? 0) > 32)
          throw new Error(`环境 ${name} 的预热数量需为 0–32 的整数`);
        if (!env || !env.resources || (next.enabled && !/^[^\s@]+@sha256:[a-f0-9]{64}$/.test(env.image ?? "")))
          throw new Error(`环境 ${name} 需填写固定摘要的镜像和资源额度`);
        if (
          ![env.resources.cpu_millis, env.resources.memory_mib, env.resources.pids].every(Number.isInteger) ||
          env.resources.cpu_millis <= 0 ||
          env.resources.memory_mib <= 0 ||
          env.resources.pids < 16
        )
          throw new Error(`环境 ${name} 的 CPU/内存必须为正数，进程数至少 16`);
        if (env.direct_network && env.proxy_url) throw new Error(`环境 ${name} 不能同时配置直连和统一代理`);
        if (networkDrafts[name] === "proxy" && !env.proxy_url) throw new Error(`请填写环境 ${name} 的代理地址`);
        if (env.proxy_url) {
          const proxy = new URL(env.proxy_url);
          if (!proxy.hostname || !["http:", "https:", "socks5:"].includes(proxy.protocol))
            throw new Error(`环境 ${name} 的代理地址需使用 HTTP、HTTPS 或 SOCKS5`);
        }
      }
      if (Object.entries(next.agent_modes).some(([key, value]) => !key || !["local", "remote"].includes(value)))
        throw new Error("Agent 模式只能是 local 或 remote");
      if (
        stored.enabled &&
        (!next.enabled ||
          stored.default_mode !== next.default_mode ||
          pretty(stored.agent_modes) !== pretty(next.agent_modes))
      )
        setConfirm(next);
      else void persist(next);
    } catch (err) {
      setError(runnerError(err));
      toast.error(runnerError(err));
    }
  }
  return (
    <>
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-base">
            <Settings2 className="size-4" />
            执行策略与环境
          </CardTitle>
          <CardDescription>
            集群池和外部节点共享策略，按环境、资源和标签匹配；一个节点可同时承载多个 Agent。
          </CardDescription>
        </CardHeader>
        <CardContent className="space-y-5">
          <fieldset disabled={busy} className="space-y-5 disabled:opacity-70">
            <div className="flex items-center justify-between gap-4 rounded-xl border bg-muted/25 p-4">
              <div>
                <Label htmlFor="runners-enabled">启用实验性 Runner</Label>
                <p className="mt-1 text-muted-foreground text-xs">远程模式等待可用容量；不开启时使用原有本机环境。</p>
              </div>
              <Switch id="runners-enabled" checked={config.enabled} onCheckedChange={(enabled) => patch({ enabled })} />
            </div>
            <div className="grid gap-4 md:grid-cols-3">
              <div className="space-y-2">
                <Label htmlFor="runner-mode">默认执行方式</Label>
                <Select value={config.default_mode} onValueChange={(mode) => patch({ default_mode: mode as Mode })}>
                  <SelectTrigger id="runner-mode">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="local">控制端本机</SelectItem>
                    <SelectItem value="remote">Runner 远程执行</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label htmlFor="runner-default-environment">默认环境</Label>
                <Select
                  value={config.default_environment}
                  onValueChange={(default_environment) => patch({ default_environment })}
                >
                  <SelectTrigger id="runner-default-environment">
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {Object.keys(envs ?? {}).map((name) => (
                      <SelectItem key={name} value={name}>
                        {name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2">
                <Label htmlFor="runner-timeout">分配等待上限（秒）</Label>
                <Input
                  id="runner-timeout"
                  type="number"
                  min={1}
                  max={3600}
                  value={config.acquire_timeout_seconds}
                  onChange={(e) => patch({ acquire_timeout_seconds: Number(e.target.value) })}
                />
              </div>
            </div>
            <div className="space-y-3">
              <h3 className="font-medium text-sm">按 Agent 指定执行方式</h3>
              <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                {Object.entries({
                  ...roles,
                  ...Object.fromEntries(
                    Object.keys(agentModes ?? {})
                      .filter((key) => !roles[key])
                      .map((key) => [key, key]),
                  ),
                }).map(([key, label]) => (
                  <div key={key} className="space-y-1.5">
                    <Label htmlFor={`mode-${key}`}>{label}</Label>
                    <Select
                      disabled={!agentModes}
                      value={agentModes?.[key] ?? "inherit"}
                      onValueChange={(value) => {
                        const next = { ...agentModes };
                        if (value === "inherit") delete next[key];
                        else next[key] = value as Mode;
                        edit();
                        setModes(pretty(next));
                      }}
                    >
                      <SelectTrigger id={`mode-${key}`}>
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="inherit">跟随默认</SelectItem>
                        <SelectItem value="local">本机</SelectItem>
                        <SelectItem value="remote">Runner</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                ))}
              </div>
            </div>
            <section className="space-y-4 rounded-xl border p-4">
              <div className="flex flex-wrap items-end gap-3">
                <div className="min-w-40 flex-1 space-y-2">
                  <Label htmlFor="edit-environment">编辑执行环境</Label>
                  <Select value={selected} onValueChange={setSelected}>
                    <SelectTrigger id="edit-environment">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      {Object.keys(envs ?? {}).map((name) => (
                        <SelectItem key={name} value={name}>
                          {name}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </div>
                <Input
                  aria-label="新环境名称"
                  placeholder="新环境名称，如 gpu"
                  className="w-44"
                  value={newName}
                  onChange={(e) => setNewName(e.target.value)}
                />
                <Button variant="outline" onClick={addEnvironment} disabled={!envs || !newName.trim()}>
                  <Plus className="size-4" />
                  添加环境
                </Button>
              </div>
              {editable ? (
                <>
                  <div className="space-y-2 rounded-lg bg-muted/40 p-3">
                    <Label htmlFor="runner-warm-idle">Kubernetes 预热 Pod 上限</Label>
                    <Input
                      id="runner-warm-idle"
                      type="number"
                      min={0}
                      max={32}
                      step={1}
                      value={env.warm_idle ?? 0}
                      onChange={(e) => patchEnvironment({ warm_idle: Number(e.target.value) })}
                    />
                    <p className="text-muted-foreground text-xs leading-relaxed">
                      0 关闭。按待执行任务的完整配置提前准备，领取时严格匹配；任务结束后销毁。预热与运行中的 Pod
                      共用节点数量和 CPU / 内存额度，保存后动态生效。外部 Docker Runner 保持原有方式。
                    </p>
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="runner-image">执行镜像</Label>
                    <Input
                      id="runner-image"
                      className="font-mono text-xs"
                      value={env.image ?? ""}
                      placeholder="registry.example.org/artex@sha256:完整摘要"
                      onChange={(e) => patchEnvironment({ image: e.target.value })}
                    />
                    <p className="text-muted-foreground text-xs">
                      使用固定摘要；镜像包含 ARTEX Runner 和所需工具。注册节点需声明支持这个环境名称。
                    </p>
                  </div>
                  <div className="grid gap-3 sm:grid-cols-3">
                    <div className="space-y-2">
                      <Label htmlFor="env-cpu">单次执行 CPU（核）</Label>
                      <Input
                        id="env-cpu"
                        type="number"
                        min={0.001}
                        step={0.1}
                        value={env.resources.cpu_millis / 1000}
                        onChange={(e) =>
                          patchEnvironment({
                            resources: { ...env.resources, cpu_millis: Math.round(Number(e.target.value) * 1000) },
                          })
                        }
                      />
                    </div>
                    <div className="space-y-2">
                      <Label htmlFor="env-memory">单次执行内存（MiB）</Label>
                      <Input
                        id="env-memory"
                        type="number"
                        min={1}
                        value={env.resources.memory_mib}
                        onChange={(e) =>
                          patchEnvironment({ resources: { ...env.resources, memory_mib: Number(e.target.value) } })
                        }
                      />
                    </div>
                    <div className="space-y-2">
                      <Label htmlFor="env-pids">进程上限</Label>
                      <Input
                        id="env-pids"
                        type="number"
                        min={16}
                        value={env.resources.pids}
                        onChange={(e) =>
                          patchEnvironment({ resources: { ...env.resources, pids: Number(e.target.value) } })
                        }
                      />
                    </div>
                  </div>
                  <div className="space-y-2">
                    <Label htmlFor="env-network">网络方式</Label>
                    <Select
                      value={
                        networkDrafts[selected] ?? (env.proxy_url ? "proxy" : env.direct_network ? "direct" : "unset")
                      }
                      onValueChange={(value) => {
                        setNetworkDrafts((previous) => ({ ...previous, [selected]: value }));
                        patchEnvironment({
                          direct_network: value === "direct",
                          proxy_url: value === "proxy" ? env.proxy_url || "" : "",
                        });
                      }}
                    >
                      <SelectTrigger id="env-network">
                        <SelectValue />
                      </SelectTrigger>
                      <SelectContent>
                        <SelectItem value="direct">默认直连 · 代理可在提示词中按需说明</SelectItem>
                        <SelectItem value="proxy">统一使用指定代理</SelectItem>
                        <SelectItem value="unset">尚未明确配置</SelectItem>
                      </SelectContent>
                    </Select>
                  </div>
                  {(networkDrafts[selected] ?? (env.proxy_url ? "proxy" : "")) === "proxy" && (
                    <div className="space-y-2">
                      <Label htmlFor="env-proxy">Runner 可访问的统一代理</Label>
                      <Input
                        id="env-proxy"
                        value={env.proxy_url ?? ""}
                        placeholder="http://proxy.example.org:8080"
                        onChange={(e) => patchEnvironment({ proxy_url: e.target.value })}
                      />
                      <p className="text-muted-foreground text-xs">
                        此选项会设置执行环境代理变量。只需告知模型可选代理时，请选择直连并填写下方说明。
                      </p>
                    </div>
                  )}
                  <div className="space-y-2">
                    <Label htmlFor="environment-prompt">此执行环境的补充说明</Label>
                    <Textarea
                      id="environment-prompt"
                      maxLength={16000}
                      value={env.environment_prompt ?? ""}
                      onChange={(e) => patchEnvironment({ environment_prompt: e.target.value })}
                      placeholder="此环境可用的工具服务、按需代理及网络可达性……"
                    />
                  </div>
                </>
              ) : (
                <p role="alert" className="text-destructive text-sm">
                  请先修正高级配置中的环境 JSON。
                </p>
              )}
            </section>
            <details className="rounded-xl border p-4">
              <summary className="cursor-pointer font-medium text-sm">高级设置 · GPU、标签、MCP 与完整配置</summary>
              <div className="mt-4 space-y-4">
                <p className="text-muted-foreground text-xs leading-relaxed">
                  GPU 需求可在 resources 中指定 gpu_count、gpu_model、gpu_memory_mib；labels 用于节点匹配。MCP
                  仍统一配置，默认在 Runner 连接；location=controller 可明确留在控制端。
                </p>
                <div className="space-y-2">
                  <Label htmlFor="runner-agent-modes">Agent 模式覆盖 · JSON</Label>
                  <Textarea
                    id="runner-agent-modes"
                    className="font-mono text-xs"
                    value={modes}
                    onChange={(e) => {
                      edit();
                      setModes(e.target.value);
                    }}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="runner-environments">完整环境配置 · JSON</Label>
                  <Textarea
                    id="runner-environments"
                    className="min-h-64 font-mono text-xs"
                    value={environments}
                    onChange={(e) => {
                      edit();
                      setEnvironments(e.target.value);
                    }}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="runner-mcp">MCP 执行位置与覆盖 · JSON</Label>
                  <Textarea
                    id="runner-mcp"
                    className="font-mono text-xs"
                    value={mcp}
                    onChange={(e) => {
                      edit();
                      setMCP(e.target.value);
                    }}
                  />
                </div>
              </div>
            </details>
          </fieldset>
          {error && (
            <p role="alert" className="rounded-lg bg-destructive/5 p-3 text-destructive text-sm">
              {error}
            </p>
          )}
          <div className="sticky bottom-3 flex flex-wrap items-center gap-3 rounded-xl border bg-background/95 p-3 shadow-sm backdrop-blur">
            <Button onClick={submit} disabled={busy || (!dirty && !brokerError)}>
              {busy ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}保存执行配置
            </Button>
            <Button variant="ghost" disabled={busy || !dirty} onClick={() => setDiscard(true)}>
              放弃修改
            </Button>
            {dirty ? (
              <span className="text-amber-600 text-xs">有未保存的修改</span>
            ) : (
              status && (
                <span role="status" className="flex items-center gap-1 text-emerald-600 text-xs">
                  <Check className="size-3" />
                  {status}
                </span>
              )
            )}
          </div>
        </CardContent>
      </Card>
      <AlertDialog
        open={confirm !== null}
        onOpenChange={(open) => {
          if (!open) setConfirm(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>应用执行方式变更？</AlertDialogTitle>
            <AlertDialogDescription>
              此操作会改变后续 Agent 的执行位置。有远程会话仍在运行或清理时，服务端会拒绝切换；届时你的修改会保留。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>继续编辑</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (confirm) void persist(confirm);
              }}
            >
              确认保存
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <AlertDialog open={discard} onOpenChange={setDiscard}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>放弃未保存的配置？</AlertDialogTitle>
            <AlertDialogDescription>恢复为服务端当前保存的配置。</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>继续编辑</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                apply(stored);
                setError("");
                setStatus("已恢复保存的配置");
              }}
            >
              确认放弃
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
