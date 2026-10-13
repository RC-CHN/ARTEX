"use client";

import * as React from "react";
import { Copy, KeyRound, Loader2, Save, Server } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { Textarea } from "@/components/ui/textarea";
import { http } from "@/lib/api";
import { invalidateApiData, subscribeRefresh } from "@/lib/api-refresh";

type Mode = "local" | "remote";
type Environment = {
  image: string;
  resources: { cpu_millis?: number; memory_mib?: number; pids?: number; gpu_count?: number; [key: string]: unknown };
  [key: string]: unknown;
};
type RunnerConfig = {
  enabled: boolean;
  default_mode: Mode;
  agent_modes: Record<string, Mode>;
  default_environment: string;
  environments: Record<string, Environment>;
  mcp: Record<string, unknown>;
  acquire_timeout_seconds: number;
  [key: string]: unknown;
};
type RunnerNode = {
  id: string;
  name: string;
  enabled: boolean;
  online: boolean;
  last_seen: string;
  active_sessions: number;
  concurrency_limit: number;
  cpu_limit_millis: number;
  memory_limit_mib: number;
  effective_cpu_millis: number;
  effective_memory_mib: number;
  reserved_cpu_millis: number;
  reserved_memory_mib: number;
  effective_slots: number;
  reserved_gpus?: string[];
  capabilities: {
    executor?: string;
    slots: number;
    cpu_millis?: number;
    memory_mib?: number;
    environments?: string[];
    labels?: Record<string, string>;
    gpus?: { id: string; model?: string; memory_mib?: number }[];
  };
};
type RunnerSnapshot = { config: RunnerConfig; nodes: RunnerNode[]; experimental: boolean; error?: string };
type EnrollmentToken = { token: string; expires_at: string };

const pretty = (value: unknown) => JSON.stringify(value ?? {}, null, 2);
function parseObject<T extends object>(raw: string, label: string): T {
  const value: unknown = JSON.parse(raw);
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label}必须是 JSON 对象`);
  return value as T;
}
const message = (err: unknown) => (err instanceof Error ? err.message : String(err));

type NodeLimits = { slots: number; cpu_millis: number; memory_mib: number };
function ConcurrencyControl({
  node,
  busy,
  save,
}: {
  node: RunnerNode;
  busy: boolean;
  save: (node: RunnerNode, limits: NodeLimits) => Promise<void>;
}) {
  const [slots, setSlots] = React.useState(String(node.concurrency_limit ?? 0));
  const [cpu, setCPU] = React.useState(String((node.cpu_limit_millis ?? 0) / 1000));
  const [memory, setMemory] = React.useState(String(node.memory_limit_mib ?? 0));
  React.useEffect(() => {
    setSlots(String(node.concurrency_limit ?? 0));
    setCPU(String((node.cpu_limit_millis ?? 0) / 1000));
    setMemory(String(node.memory_limit_mib ?? 0));
  }, [node.concurrency_limit, node.cpu_limit_millis, node.memory_limit_mib]);
  const limits = { slots: Number(slots), cpu_millis: Math.round(Number(cpu) * 1000), memory_mib: Number(memory) };
  const valid =
    [slots, cpu, memory].every((value) => value.trim() !== "" && Number.isFinite(Number(value))) &&
    Number.isInteger(limits.slots) &&
    limits.slots >= 0 &&
    limits.slots <= node.capabilities.slots &&
    Number(cpu) >= 0 &&
    limits.cpu_millis <= (node.capabilities.cpu_millis ?? 0) &&
    Number.isInteger(limits.memory_mib) &&
    limits.memory_mib >= 0 &&
    limits.memory_mib <= (node.capabilities.memory_mib ?? 0);
  const changed =
    limits.slots !== node.concurrency_limit ||
    limits.cpu_millis !== node.cpu_limit_millis ||
    limits.memory_mib !== node.memory_limit_mib;
  return (
    <div className="space-y-3 rounded-md border p-3">
      <div className="grid gap-3 sm:grid-cols-3">
        <div className="space-y-1">
          <Label htmlFor={`slots-${node.id}`}>并发环境上限</Label>
          <Input
            id={`slots-${node.id}`}
            type="number"
            min={0}
            max={node.capabilities.slots}
            value={slots}
            onChange={(event) => setSlots(event.target.value)}
          />
        </div>
        <div className="space-y-1">
          <Label htmlFor={`cpu-${node.id}`}>CPU 总配额（核）</Label>
          <Input
            id={`cpu-${node.id}`}
            type="number"
            min={0}
            step={0.1}
            max={(node.capabilities.cpu_millis ?? 0) / 1000}
            value={cpu}
            onChange={(event) => setCPU(event.target.value)}
          />
        </div>
        <div className="space-y-1">
          <Label htmlFor={`memory-${node.id}`}>内存总配额（MiB）</Label>
          <Input
            id={`memory-${node.id}`}
            type="number"
            min={0}
            max={node.capabilities.memory_mib ?? 0}
            value={memory}
            onChange={(event) => setMemory(event.target.value)}
          />
        </div>
      </div>
      <Button size="sm" variant="outline" disabled={busy || !valid || !changed} onClick={() => save(node, limits)}>
        保存配额
      </Button>
      <p className="text-xs text-muted-foreground">
        0 使用节点硬容量：{node.capabilities.slots} 个环境、{(node.capabilities.cpu_millis ?? 0) / 1000} 核、
        {node.capabilities.memory_mib ?? 0} MiB。保存后立即影响后续分配，现有执行保持资源；降额后排队等待容量释放。
      </p>
    </div>
  );
}

export default function RunnersPage() {
  const [snapshot, setSnapshot] = React.useState<RunnerSnapshot | null>(null);
  const [config, setConfig] = React.useState<RunnerConfig | null>(null);
  const [environments, setEnvironments] = React.useState("{}");
  const [agentModes, setAgentModes] = React.useState("{}");
  const [mcp, setMCP] = React.useState("{}");
  const [dirty, setDirty] = React.useState(false);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [nodeBusy, setNodeBusy] = React.useState<string | null>(null);
  const [tokenBusy, setTokenBusy] = React.useState(false);
  const [token, setToken] = React.useState<EnrollmentToken | null>(null);
  const live = React.useRef(false);
  const dirtyRef = React.useRef(false);
  const generation = React.useRef(0);

  const applyConfig = React.useCallback((next: RunnerConfig) => {
    setConfig(next);
    setEnvironments(pretty(next.environments));
    setAgentModes(pretty(next.agent_modes));
    setMCP(pretty(next.mcp));
    dirtyRef.current = false;
    setDirty(false);
  }, []);

  React.useEffect(() => {
    let subscribed = true;
    live.current = true;
    const stop = subscribeRefresh(
      async () => {
        const current = generation.current;
        try {
          const next = await http<RunnerSnapshot>("/runners");
          if (!subscribed) return;
          setSnapshot((prev) =>
            current === generation.current ? next : { ...next, config: prev?.config ?? next.config },
          );
          setError("");
          if (!dirtyRef.current && current === generation.current) applyConfig(next.config);
        } catch (err) {
          if (subscribed) setError(message(err));
        }
      },
      { keys: ["/runners"], intervalMs: 15_000 },
    );
    return () => {
      subscribed = false;
      live.current = false;
      stop();
    };
  }, [applyConfig]);

  function edit() {
    generation.current++;
    dirtyRef.current = true;
    setDirty(true);
  }
  function patch(next: Partial<RunnerConfig>) {
    if (!config) return;
    edit();
    setConfig({ ...config, ...next });
  }

  let parsedEnvironments: Record<string, Environment> | null = null;
  try {
    parsedEnvironments = parseObject<Record<string, Environment>>(environments, "环境配置");
  } catch {
    /* Keep incomplete JSON editable. */
  }
  const defaultImage = config ? (parsedEnvironments?.[config.default_environment]?.image ?? "") : "";

  async function save() {
    if (!config || busy) return;
    let next: RunnerConfig;
    try {
      next = {
        ...config,
        environments: parseObject(environments, "环境配置"),
        agent_modes: parseObject(agentModes, "Agent 模式"),
        mcp: parseObject(mcp, "MCP 配置"),
      };
      if (!next.environments[next.default_environment]) throw new Error("默认环境必须在环境配置中定义");
      for (const [name, env] of Object.entries(next.environments)) {
        if (next.enabled && !/^[^\s@]+@sha256:[a-f0-9]{64}$/.test(env.image ?? "")) {
          throw new Error(`环境 ${name} 需要包含完整 sha256 摘要的镜像地址`);
        }
      }
      if (Object.values(next.agent_modes).some((value) => value !== "local" && value !== "remote")) {
        throw new Error("Agent 模式只能填写 local 或 remote");
      }
    } catch (err) {
      toast.error(message(err));
      return;
    }
    setBusy(true);
    generation.current++;
    try {
      const saved = await http<RunnerConfig>("/runners/config", { method: "PUT", body: JSON.stringify(next) });
      if (!live.current) return;
      generation.current++;
      applyConfig(saved);
      setSnapshot((prev) => (prev ? { ...prev, config: saved } : prev));
      toast.success("Runner 配置已保存");
      invalidateApiData(["/runners"]);
    } catch (err) {
      if (live.current) toast.error(message(err));
    } finally {
      if (live.current) setBusy(false);
    }
  }

  async function enroll() {
    setTokenBusy(true);
    try {
      const created = await http<EnrollmentToken>("/runners/registration-token", { method: "POST" });
      if (live.current) setToken(created);
    } catch (err) {
      if (live.current) toast.error(message(err));
    } finally {
      if (live.current) setTokenBusy(false);
    }
  }
  async function toggleNode(node: RunnerNode) {
    setNodeBusy(node.id);
    try {
      await http(`/runners/${encodeURIComponent(node.id)}/enabled`, {
        method: "PUT",
        body: JSON.stringify({ enabled: !node.enabled }),
      });
      if (live.current) toast.success(node.enabled ? "节点已禁用，运行中的远程会话已停止续租" : "节点已启用");
      invalidateApiData(["/runners"]);
    } catch (err) {
      if (live.current) toast.error(message(err));
    } finally {
      if (live.current) setNodeBusy(null);
    }
  }

  async function saveConcurrency(node: RunnerNode, limits: NodeLimits) {
    setNodeBusy(node.id);
    try {
      await http(`/runners/${encodeURIComponent(node.id)}/limits`, { method: "PUT", body: JSON.stringify(limits) });
      invalidateApiData(["/runners"]);
      if (live.current) toast.success("资源配额已保存，对后续分配立即生效");
    } catch (err) {
      if (live.current) toast.error(message(err));
    } finally {
      if (live.current) setNodeBusy(null);
    }
  }

  return (
    <div className="flex flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="flex items-center gap-2 text-xl font-semibold">
            <Server className="size-5" />
            Runner <Badge variant="outline">实验功能</Badge>
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            为 Agent 线程分配远程执行环境；模型调用与任务调度仍由控制端管理。
          </p>
        </div>
        <Button variant="outline" onClick={() => invalidateApiData(["/runners"])}>
          刷新节点
        </Button>
      </div>
      {(error || snapshot?.error) && (
        <p
          role="alert"
          className="rounded-md border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"
        >
          {error || snapshot?.error}
        </p>
      )}
      {!config ? (
        <p className="text-sm text-muted-foreground">{error ? "配置读取失败，请刷新重试。" : "正在读取配置…"}</p>
      ) : (
        <Card>
          <CardHeader>
            <CardTitle>执行配置</CardTitle>
          </CardHeader>
          <CardContent className="space-y-5">
            <fieldset disabled={busy} className="space-y-5 disabled:opacity-70">
              <div className="flex items-start justify-between gap-4 rounded-md border p-3">
                <div>
                  <Label htmlFor="runners-enabled">启用远程 Runner</Label>
                  <p className="mt-1 text-sm text-muted-foreground">
                    默认关闭。启用后按下方模式执行；remote 模式没有可用节点时等待或报错。
                  </p>
                </div>
                <Switch
                  id="runners-enabled"
                  checked={config.enabled}
                  onCheckedChange={(enabled) => patch({ enabled })}
                />
              </div>
              <div className="grid gap-4 md:grid-cols-3">
                <div className="space-y-2">
                  <Label htmlFor="runner-mode">默认执行模式</Label>
                  <Select value={config.default_mode} onValueChange={(value) => patch({ default_mode: value as Mode })}>
                    <SelectTrigger id="runner-mode">
                      <SelectValue />
                    </SelectTrigger>
                    <SelectContent>
                      <SelectItem value="remote">remote · 远程环境</SelectItem>
                      <SelectItem value="local">local · 控制端本机</SelectItem>
                    </SelectContent>
                  </Select>
                </div>
                <div className="space-y-2">
                  <Label htmlFor="runner-environment">默认环境名称</Label>
                  <Input
                    id="runner-environment"
                    value={config.default_environment}
                    onChange={(event) => patch({ default_environment: event.target.value })}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="runner-timeout">分配等待上限（秒）</Label>
                  <Input
                    id="runner-timeout"
                    type="number"
                    min={1}
                    max={3600}
                    value={config.acquire_timeout_seconds}
                    onChange={(event) => patch({ acquire_timeout_seconds: Number(event.target.value) })}
                  />
                </div>
              </div>
              <div className="space-y-2">
                <Label htmlFor="runner-image">默认环境镜像</Label>
                <Input
                  id="runner-image"
                  className="font-mono text-xs"
                  value={defaultImage}
                  placeholder="registry.example.org/artex-sandbox@sha256:完整摘要"
                  disabled={!parsedEnvironments?.[config.default_environment]}
                  onChange={(event) => {
                    if (!parsedEnvironments?.[config.default_environment]) return;
                    edit();
                    setEnvironments(
                      pretty({
                        ...parsedEnvironments,
                        [config.default_environment]: {
                          ...parsedEnvironments[config.default_environment],
                          image: event.target.value,
                        },
                      }),
                    );
                  }}
                />
                <p className="text-xs text-muted-foreground">
                  启用前，每个环境均需指定 image@sha256: 加 64 位摘要。镜像须包含 ARTEX sandbox 执行程序及需要的工具。
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="runner-agent-modes">Agent 模式覆盖 · JSON</Label>
                <Textarea
                  id="runner-agent-modes"
                  value={agentModes}
                  onChange={(event) => {
                    edit();
                    setAgentModes(event.target.value);
                  }}
                  className="min-h-24 font-mono text-xs"
                  spellCheck={false}
                />
                <p className="text-xs text-muted-foreground">
                  按 Agent key 设置，例如 {`{"planner":"local","worker":"remote","mainagent":"remote","chat":"remote"}`}
                  ；未列出的 Agent 使用默认模式。
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="runner-environments">环境配置 · JSON</Label>
                <Textarea
                  id="runner-environments"
                  value={environments}
                  onChange={(event) => {
                    edit();
                    setEnvironments(event.target.value);
                  }}
                  className="min-h-64 font-mono text-xs"
                  spellCheck={false}
                />
                <p className="text-xs leading-relaxed text-muted-foreground">
                  按环境名定义 image、env、labels 和 resources。cpu_millis 为千分之一核，memory_mib 为 MiB，pids 至少
                  16；GPU 可用 gpu_count、gpu_model、gpu_memory_mib 筛选。proxy_url 指定节点可访问的代理；direct_network
                  显式直连，两者互斥。inputs 可声明只读依赖目录（name、source、target）。
                </p>
              </div>
              <div className="space-y-2">
                <Label htmlFor="runner-mcp">MCP 执行位置与覆盖 · JSON</Label>
                <Textarea
                  id="runner-mcp"
                  value={mcp}
                  onChange={(event) => {
                    edit();
                    setMCP(event.target.value);
                  }}
                  className="min-h-32 font-mono text-xs"
                  spellCheck={false}
                />
                <p className="text-xs leading-relaxed text-muted-foreground">
                  默认沿用现有 MCP 配置并在 Runner 连接。按 MCP 名设置 location（runner 或 controller）。runner 模式可用
                  override 替换连接配置且保留相同 name；controller 模式在 MCP 页面维护连接。例如{" "}
                  {`{"browser":{"location":"runner","override":{"name":"browser","transport":"http","url":"https://mcp.example.org/mcp"}}}`}
                  。
                </p>
              </div>
            </fieldset>
            <div className="flex items-center gap-2">
              <Button onClick={save} disabled={busy || (!dirty && !snapshot?.error)}>
                {busy ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}保存配置
              </Button>
              <Button
                variant="outline"
                disabled={busy || !dirty}
                onClick={() => {
                  if (snapshot) {
                    generation.current++;
                    applyConfig(snapshot.config);
                  }
                }}
              >
                放弃修改
              </Button>
              {dirty && <span className="text-xs text-muted-foreground">有未保存的修改</span>}
            </div>
          </CardContent>
        </Card>
      )}
      <Card>
        <CardHeader>
          <CardTitle>注册节点</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-sm text-muted-foreground">
            节点主动通过 HTTPS 连接控制端，可部署在独立主机或集群。注册令牌 10
            分钟内有效且只能使用一次；节点首次注册后保存自己的长期凭据。
          </p>
          <Button variant="outline" onClick={enroll} disabled={tokenBusy || !snapshot?.config.enabled}>
            {tokenBusy ? <Loader2 className="size-4 animate-spin" /> : <KeyRound className="size-4" />}生成注册令牌
          </Button>
          {token && (
            <div className="space-y-2 rounded-md border p-3">
              <div className="flex gap-2">
                <Input aria-label="注册令牌" readOnly value={token.token} className="font-mono text-xs" />
                <Button
                  variant="outline"
                  aria-label="复制注册令牌"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(token.token);
                      toast.success("令牌已复制");
                    } catch {
                      toast.error("复制失败，请手动选中令牌复制");
                    }
                  }}
                >
                  <Copy className="size-4" />
                </Button>
              </div>
              <p className="text-xs text-muted-foreground">
                有效至 {new Date(token.expires_at).toLocaleString()}。令牌仅保留在当前页面。
              </p>
              <Button size="sm" variant="ghost" onClick={() => setToken(null)}>
                隐藏令牌
              </Button>
            </div>
          )}
        </CardContent>
      </Card>
      <div className="space-y-3">
        <h2 className="font-semibold">节点 · {snapshot?.nodes.length ?? 0}</h2>
        {!snapshot?.nodes.length && (
          <p className="rounded-md border border-dashed p-6 text-sm text-muted-foreground">
            {snapshot?.config.enabled
              ? "暂无已注册节点。生成令牌后启动 Runner，节点将自动出现在这里。"
              : "启用并保存实验配置后，可注册和管理节点。"}
          </p>
        )}
        {snapshot?.nodes.map((node) => (
          <Card key={node.id}>
            <CardContent className="space-y-3 pt-5">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div>
                  <div className="flex items-center gap-2">
                    <span className="font-medium">{node.name}</span>
                    <Badge variant="outline">
                      {node.capabilities.executor === "kubernetes" ? "Kubernetes 动态 Pod 池" : "Docker 执行节点"}
                    </Badge>
                    <Badge variant={node.online ? "default" : "secondary"}>
                      {!node.enabled ? "已禁用" : node.online ? "在线" : "离线"}
                    </Badge>
                  </div>
                  <p className="mt-1 break-all font-mono text-xs text-muted-foreground">{node.id}</p>
                </div>
                <Button variant="outline" size="sm" disabled={nodeBusy !== null} onClick={() => toggleNode(node)}>
                  {nodeBusy === node.id && <Loader2 className="size-3 animate-spin" />}
                  {node.enabled ? "禁用节点" : "启用节点"}
                </Button>
              </div>
              <div className="grid gap-2 text-sm sm:grid-cols-3">
                <span>
                  已预留执行环境：{node.active_sessions} / {node.effective_slots ?? node.capabilities.slots}
                </span>
                <span>
                  CPU 预留：{(node.reserved_cpu_millis ?? 0) / 1000} /{" "}
                  {(node.effective_cpu_millis ?? node.capabilities.cpu_millis ?? 0) / 1000} 核
                </span>
                <span>
                  内存预留：{node.reserved_memory_mib ?? 0} /{" "}
                  {node.effective_memory_mib ?? node.capabilities.memory_mib ?? 0} MiB
                </span>
              </div>
              <p className="text-sm">环境：{node.capabilities.environments?.join("、") || "未声明"}</p>
              <ConcurrencyControl node={node} busy={nodeBusy !== null} save={saveConcurrency} />
              <div className="flex flex-wrap gap-1">
                {Object.entries(node.capabilities.labels ?? {}).map(([key, value]) => (
                  <Badge key={key} variant="outline">
                    {key}={value}
                  </Badge>
                ))}
              </div>
              {node.capabilities.gpus?.length ? (
                <div className="flex flex-wrap gap-2">
                  {node.capabilities.gpus.map((gpu) => (
                    <Badge variant="outline" key={gpu.id}>
                      {gpu.id} · {gpu.model || "GPU"}
                      {gpu.memory_mib ? ` · ${gpu.memory_mib} MiB` : ""} ·{" "}
                      {node.reserved_gpus?.includes(gpu.id) ? "已占用" : "空闲"}
                    </Badge>
                  ))}
                </div>
              ) : (
                <p className="text-xs text-muted-foreground">未声明 GPU</p>
              )}
              <p className="text-xs text-muted-foreground">
                最近心跳：{new Date(node.last_seen).toLocaleString()}
                {node.enabled && node.active_sessions > 0 ? " · 禁用会停止此节点的运行会话" : ""}
              </p>
            </CardContent>
          </Card>
        ))}
      </div>
    </div>
  );
}
