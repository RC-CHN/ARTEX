"use client";
import * as React from "react";
import {
  Activity,
  Box,
  Check,
  Cpu,
  HardDrive,
  Loader2,
  MessageSquareText,
  Save,
  Server,
  Settings2,
} from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { useUnsavedChanges } from "@/hooks/use-unsaved-changes";
import { http } from "@/lib/api";
import { formatMemory, isPool, runnerError, type NodeLimits, type RunnerNode } from "@/lib/runners";

function Meter({
  label,
  used,
  total,
  text,
  icon: Icon,
}: {
  label: string;
  used: number;
  total: number;
  text: string;
  icon: typeof Cpu;
}) {
  const percent = total > 0 ? Math.min(100, (used / total) * 100) : 0;
  return (
    <div className="space-y-2 rounded-xl bg-muted/45 p-3">
      <div className="flex items-center gap-1.5 text-muted-foreground text-xs">
        <Icon className="size-3.5" />
        {label}
      </div>
      <div className="font-medium tabular-nums">{text}</div>
      {/* biome-ignore lint/a11y/useSemanticElements: custom meter styling with a visible numeric label */}
      <div
        className="h-1.5 overflow-hidden rounded-full bg-border"
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={total || 1}
        aria-valuenow={Math.min(used, total || 1)}
      >
        <div
          className={`h-full rounded-full transition-all ${percent >= 100 ? "bg-amber-500" : "bg-primary/70"}`}
          style={{ width: `${percent}%` }}
        />
      </div>
    </div>
  );
}

export function NodeCard({
  node,
  busy,
  manage,
  toggle,
}: {
  node: RunnerNode;
  busy: boolean;
  manage: () => void;
  toggle: () => void;
}) {
  const pool = isPool(node);
  return (
    <Card className="overflow-hidden shadow-sm transition-shadow hover:shadow-md" data-testid={`runner-${node.id}`}>
      <CardContent className="space-y-5 pt-5">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="flex min-w-0 items-center gap-3">
            <div
              className={`rounded-xl p-2.5 ${pool ? "bg-blue-500/10 text-blue-600" : "bg-violet-500/10 text-violet-600"}`}
            >
              {pool ? <Box className="size-5" /> : <Server className="size-5" />}
            </div>
            <div className="min-w-0">
              <h3 className="break-all font-semibold">{node.name}</h3>
              <p className="mt-0.5 text-muted-foreground text-xs">
                {pool
                  ? "Kubernetes · 每次执行创建独立 Pod"
                  : `${node.capabilities.executor ?? "未报告执行器"} · 独立主机运行时`}
              </p>
            </div>
          </div>
          <Badge
            variant="outline"
            className={
              node.online && node.enabled
                ? "border-emerald-500/25 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400"
                : "text-muted-foreground"
            }
          >
            <span
              className={`mr-1 size-1.5 rounded-full ${node.online && node.enabled ? "bg-emerald-500" : "bg-muted-foreground"}`}
            />
            {!node.enabled ? "已停用" : node.online ? "在线" : "离线"}
          </Badge>
        </div>
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
          <Meter
            label={pool ? "执行 Pod" : "执行环境"}
            icon={Activity}
            used={node.active_sessions}
            total={node.effective_slots}
            text={`${node.active_sessions} / ${node.effective_slots}`}
          />
          <Meter
            label="CPU 预留"
            icon={Cpu}
            used={node.reserved_cpu_millis}
            total={node.effective_cpu_millis}
            text={`${node.reserved_cpu_millis / 1000} / ${node.effective_cpu_millis / 1000} 核`}
          />
          <Meter
            label="内存预留"
            icon={HardDrive}
            used={node.reserved_memory_mib}
            total={node.effective_memory_mib}
            text={`${formatMemory(node.reserved_memory_mib)} / ${formatMemory(node.effective_memory_mib)}`}
          />
        </div>
        <div className="flex flex-wrap gap-1.5">
          {(node.capabilities.environments ?? []).map((env) => (
            <Badge key={env} variant="secondary">
              环境 · {env}
            </Badge>
          ))}
          {Object.entries(node.capabilities.labels ?? {}).map(([key, value]) => (
            <Badge key={key} variant="outline">
              {key}: {value}
            </Badge>
          ))}
          {(node.capabilities.gpus ?? []).map((gpu) => (
            <Badge key={gpu.id} variant="outline">
              GPU {gpu.id} · {gpu.model || "未报告型号"} · {formatMemory(gpu.memory_mib ?? 0)}
              {node.reserved_gpus?.includes(gpu.id) ? " · 已预留" : ""}
            </Badge>
          ))}
        </div>
        <div className="flex flex-wrap items-center justify-between gap-3 border-t pt-3">
          <p className="text-muted-foreground text-xs">
            最近心跳 {new Date(node.last_seen).toLocaleString()}
            {node.environment_prompt && (
              <span className="ml-2 inline-flex items-center gap-1">
                <MessageSquareText className="size-3" />
                有环境说明
              </span>
            )}
          </p>
          <div className="flex gap-2">
            <Button size="sm" variant="ghost" disabled={busy} onClick={toggle}>
              {busy && <Loader2 className="size-3 animate-spin" />}
              {node.enabled ? "停用" : "启用"}
            </Button>
            <Button size="sm" variant="outline" onClick={manage}>
              <Settings2 className="size-3.5" />
              管理
            </Button>
          </div>
        </div>
      </CardContent>
    </Card>
  );
}

export function NodeEditor({ node, close, changed }: { node: RunnerNode; close: () => void; changed: () => void }) {
  const [slots, setSlots] = React.useState(String(node.concurrency_limit));
  const [cpu, setCPU] = React.useState(String(node.cpu_limit_millis / 1000));
  const [memory, setMemory] = React.useState(String(node.memory_limit_mib));
  const [savedLimits, setSavedLimits] = React.useState<NodeLimits>({
    slots: node.concurrency_limit,
    cpu_millis: node.cpu_limit_millis,
    memory_mib: node.memory_limit_mib,
  });
  const [prompt, setPrompt] = React.useState(node.environment_prompt ?? "");
  const [savedPrompt, setSavedPrompt] = React.useState(prompt);
  const [busy, setBusy] = React.useState<"limits" | "prompt" | null>(null);
  const [status, setStatus] = React.useState("");
  const [error, setError] = React.useState("");
  const [discard, setDiscard] = React.useState(false);
  const limits = { slots: Number(slots), cpu_millis: Math.round(Number(cpu) * 1000), memory_mib: Number(memory) };
  const limitsDirty =
    slots !== String(savedLimits.slots) ||
    cpu !== String(savedLimits.cpu_millis / 1000) ||
    memory !== String(savedLimits.memory_mib);
  const dirty = limitsDirty || prompt !== savedPrompt;
  const valid =
    [slots, cpu, memory].every((x) => x.trim() !== "" && Number.isFinite(Number(x)) && Number(x) >= 0) &&
    Number.isInteger(limits.slots) &&
    Number.isInteger(limits.memory_mib) &&
    limits.slots <= node.capabilities.slots &&
    limits.cpu_millis <= (node.capabilities.cpu_millis ?? 0) &&
    limits.memory_mib <= (node.capabilities.memory_mib ?? 0);
  useUnsavedChanges(dirty);
  async function save(kind: "limits" | "prompt") {
    setBusy(kind);
    setError("");
    setStatus("");
    try {
      await http(`/runners/${encodeURIComponent(node.id)}/${kind === "limits" ? "limits" : "environment-prompt"}`, {
        method: "PUT",
        body: JSON.stringify(kind === "limits" ? limits : { environment_prompt: prompt }),
      });
      if (kind === "limits") setSavedLimits(limits);
      else setSavedPrompt(prompt);
      const text = kind === "limits" ? "配额已保存，对后续分配生效" : "节点环境说明已保存，对后续执行生效";
      setStatus(text);
      toast.success(text);
      changed();
    } catch (err) {
      setError(runnerError(err));
      toast.error("保存失败，修改已保留");
    } finally {
      setBusy(null);
    }
  }
  return (
    <>
      <Dialog
        open
        onOpenChange={(open) => {
          if (!open && !busy) {
            if (dirty) setDiscard(true);
            else close();
          }
        }}
      >
        <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
          <DialogHeader>
            <DialogTitle>管理 · {node.name}</DialogTitle>
            <DialogDescription className="break-all font-mono text-xs">{node.id}</DialogDescription>
          </DialogHeader>
          <fieldset disabled={busy !== null} className="space-y-6">
            <section className="space-y-3">
              <h3 className="font-medium">资源与并发额度</h3>
              <p className="text-muted-foreground text-xs leading-relaxed">
                填写 0 继承硬容量：{node.capabilities.slots} 个执行环境、{(node.capabilities.cpu_millis ?? 0) / 1000}{" "}
                核、{formatMemory(node.capabilities.memory_mib ?? 0)}。调低额度不改变现有执行的资源。
              </p>
              <div className="grid gap-3 sm:grid-cols-3">
                <div className="space-y-2">
                  <Label htmlFor="node-slots">并发上限</Label>
                  <Input
                    id="node-slots"
                    type="number"
                    min={0}
                    max={node.capabilities.slots}
                    value={slots}
                    onChange={(e) => setSlots(e.target.value)}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="node-cpu">CPU 总额度（核）</Label>
                  <Input
                    id="node-cpu"
                    type="number"
                    min={0}
                    step={0.1}
                    max={(node.capabilities.cpu_millis ?? 0) / 1000}
                    value={cpu}
                    onChange={(e) => setCPU(e.target.value)}
                  />
                </div>
                <div className="space-y-2">
                  <Label htmlFor="node-memory">内存总额度（MiB）</Label>
                  <Input
                    id="node-memory"
                    type="number"
                    min={0}
                    max={node.capabilities.memory_mib ?? 0}
                    value={memory}
                    onChange={(e) => setMemory(e.target.value)}
                  />
                </div>
              </div>
              {!valid && (
                <p role="alert" className="text-destructive text-xs">
                  请输入不超过硬容量的非负数，并发和内存必须是整数。
                </p>
              )}
              <Button
                variant="outline"
                onClick={() => save("limits")}
                disabled={busy !== null || !valid || !limitsDirty}
              >
                {busy === "limits" ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}保存配额
              </Button>
            </section>
            <section className="space-y-3 border-t pt-5">
              <Label htmlFor="node-environment-prompt">此 Runner 的额外环境提示词</Label>
              <Textarea
                id="node-environment-prompt"
                className="min-h-36"
                maxLength={16000}
                value={prompt}
                onChange={(e) => setPrompt(e.target.value)}
                placeholder="例如：此节点在外部网络，可使用本机提供的代理；不能访问控制端内网。工具服务位于……"
              />
              <p className="text-muted-foreground text-xs leading-relaxed">
                只注入分配到此节点的新执行，与全局环境说明叠加。按需代理写在这里不会设置全局代理变量。运行镜像会另外报告实际安装的
                vulnx / Nuclei 及使用方法。
              </p>
              <Button
                variant="outline"
                onClick={() => save("prompt")}
                disabled={busy !== null || prompt === savedPrompt}
              >
                {busy === "prompt" ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                保存节点说明
              </Button>
            </section>
          </fieldset>
          {error && (
            <p role="alert" className="text-destructive text-sm">
              {error}
            </p>
          )}
          {status && (
            <p role="status" className="flex items-center gap-1 text-emerald-600 text-sm">
              <Check className="size-4" />
              {status}
            </p>
          )}
          {dirty && <p className="text-amber-600 text-xs">有未保存的修改</p>}
        </DialogContent>
      </Dialog>
      <AlertDialog open={discard} onOpenChange={setDiscard}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>放弃未保存的修改？</AlertDialogTitle>
            <AlertDialogDescription>已经保存的配额和环境说明会保留。</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>继续编辑</AlertDialogCancel>
            <AlertDialogAction onClick={close}>放弃并关闭</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
