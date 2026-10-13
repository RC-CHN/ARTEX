"use client";

import * as React from "react";
import { BrainCircuit, Check, Loader2, Save, Workflow, Wrench } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { useUnsavedChanges } from "@/hooks/use-unsaved-changes";
import { api } from "@/lib/api";
import { subscribeRefresh } from "@/lib/api-refresh";
import type { Agent, LLMProfile } from "@/lib/types";

const roles = [
  {
    key: "mainagent",
    title: "主 Agent",
    detail: "理解目标、协调任务与交互，可选择推理能力较强的模型。",
    icon: BrainCircuit,
  },
  {
    key: "planner",
    title: "Planner 规划",
    detail: "拆解计划、判断覆盖与后续方向，可独立选择高能力模型。",
    icon: Workflow,
  },
  { key: "worker", title: "Worker 执行", detail: "按计划执行工具与收集证据，可选择成本较低的模型。", icon: Wrench },
  { key: "goals", title: "目标拆解", detail: "创建任务时拆解目标；独立于 Planner，可配置同一个模型。", icon: Workflow },
];

function RoleModel({ role, agent, profiles }: { role: (typeof roles)[number]; agent: Agent; profiles: LLMProfile[] }) {
  const initial = agent.llm_profile_id == null ? "inherit" : String(agent.llm_profile_id);
  const [selected, setSelected] = React.useState(initial);
  const [saved, setSaved] = React.useState(initial);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [status, setStatus] = React.useState("");
  const dirty = selected !== saved;
  const dirtyRef = React.useRef(false);
  dirtyRef.current = dirty || busy;
  useUnsavedChanges(dirty);
  React.useEffect(() => {
    if (!dirtyRef.current) {
      setSelected(initial);
      setSaved(initial);
    }
  }, [initial]);
  const valid = selected === "inherit" || profiles.some((profile) => profile.id === selected);
  async function save() {
    setBusy(true);
    setError("");
    setStatus("");
    try {
      await api.saveAgentConfig(role.key, { llm_profile_id: selected === "inherit" ? null : Number(selected) });
      setSaved(selected);
      setStatus("已保存，后续模型请求按此绑定解析");
      toast.success(`${role.title} 模型已保存`);
    } catch (error) {
      setError(error instanceof Error ? error.message : String(error));
      toast.error(`${role.title} 模型保存失败，选择已保留`);
    } finally {
      setBusy(false);
    }
  }
  const Icon = role.icon;
  return (
    <section className="space-y-3 rounded-xl border bg-muted/20 p-4" aria-label={`${role.title} 模型配置`}>
      <h3 className="flex items-center gap-2 font-medium">
        <Icon className="size-4 text-primary" />
        {role.title}
      </h3>
      <p className="text-muted-foreground text-xs leading-relaxed">{role.detail}</p>
      <Label htmlFor={`role-model-${role.key}`}>{role.title} 使用的模型</Label>
      <Select
        value={selected}
        disabled={busy}
        onValueChange={(value) => {
          setSelected(value);
          setStatus("");
          setError("");
        }}
      >
        <SelectTrigger id={`role-model-${role.key}`} className="w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="inherit">跟随任务 / 会话 / 全局配置</SelectItem>
          {!valid && (
            <SelectItem value={selected} disabled>
              配置 #{selected} 不可用
            </SelectItem>
          )}
          {profiles.map((profile) => (
            <SelectItem key={profile.id} value={profile.id}>
              {profile.name} · {profile.model}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      <div className="flex flex-wrap items-center gap-2">
        <Button variant="outline" size="sm" disabled={busy || !dirty || !valid} onClick={save}>
          {busy ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}保存{role.title}模型
        </Button>
        {dirty && <span className="text-amber-600 text-xs">未保存</span>}
      </div>
      {error && (
        <p role="alert" className="text-destructive text-xs">
          {error}
        </p>
      )}
      {status && !dirty && (
        <p role="status" className="flex items-center gap-1 text-emerald-600 text-xs">
          <Check className="size-3" />
          {status}
        </p>
      )}
    </section>
  );
}

export function RoleModels({ profiles }: { profiles: LLMProfile[] }) {
  const [agents, setAgents] = React.useState<Agent[] | null>(null);
  const [error, setError] = React.useState("");
  const load = React.useCallback(async () => {
    try {
      setAgents(await api.agents());
      setError("");
    } catch (error) {
      setError(error instanceof Error ? error.message : String(error));
    }
  }, []);
  React.useEffect(() => subscribeRefresh(load, { keys: ["/agents"], intervalMs: 60_000 }), [load]);
  return (
    <Card className="mb-5">
      <CardHeader>
        <CardTitle className="text-base">按角色选择模型</CardTitle>
        <CardDescription>规划与执行分别选型，所有 Worker 共用 Worker 绑定。无需改变 Runner 配置。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {error && (
          <div role="alert" className="flex items-center gap-3 text-destructive text-sm">
            {error}
            <Button variant="outline" size="sm" onClick={load}>
              重试加载
            </Button>
          </div>
        )}
        {!agents ? (
          <p className="text-muted-foreground text-sm">正在读取角色配置…</p>
        ) : (
          <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-4">
            {roles.map((role) => {
              const agent = agents.find((agent) => agent.key === role.key);
              return agent ? <RoleModel key={role.key} role={role} agent={agent} profiles={profiles} /> : null;
            })}
          </div>
        )}
        <p className="text-muted-foreground text-xs leading-relaxed">
          角色绑定优先于任务模型链和全局配置；选“跟随”可恢复原有策略。显式绑定不参与任务模型链切换；如开启“绑定回退”，则按轮询策略处理故障。正在进行的请求不会被强行切换。
        </p>
      </CardContent>
    </Card>
  );
}
