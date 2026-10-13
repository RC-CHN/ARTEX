"use client";

import * as React from "react";
import { Activity, Box, Cpu, Loader2, Plus, RefreshCw, Server } from "lucide-react";
import { toast } from "sonner";
import { EnvironmentPromptCard } from "@/components/environment-prompt-card";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { http } from "@/lib/api";
import { invalidateApiData, subscribeRefresh } from "@/lib/api-refresh";
import { formatMemory, isPool, runnerError, type RunnerNode, type RunnerSnapshot } from "@/lib/runners";
import { EnrollmentDialog } from "./_components/enrollment-dialog";
import { ExecutionSettings } from "./_components/execution-settings";
import { NodeCard, NodeEditor } from "./_components/node-card";

export default function RunnersPage() {
  const [snapshot, setSnapshot] = React.useState<RunnerSnapshot | null>(null);
  const [error, setError] = React.useState("");
  const [refreshing, setRefreshing] = React.useState(false);
  const [updated, setUpdated] = React.useState("");
  const [query, setQuery] = React.useState("");
  const [filter, setFilter] = React.useState("all");
  const [enrolling, setEnrolling] = React.useState(false);
  const [editing, setEditing] = React.useState<string | null>(null);
  const [toggle, setToggle] = React.useState<RunnerNode | null>(null);
  const [nodeBusy, setNodeBusy] = React.useState<string | null>(null);
  const generation = React.useRef(0);
  const mounted = React.useRef(false);
  const refresh = React.useCallback(async (manual = false) => {
    const request = ++generation.current;
    if (manual) setRefreshing(true);
    try {
      const next = await http<RunnerSnapshot>("/runners", manual ? { cache: "no-store" } : undefined);
      if (!mounted.current || request !== generation.current) return;
      setSnapshot(next);
      setError("");
      setUpdated(new Date().toLocaleTimeString());
      if (manual) toast.success("Runner 状态已刷新");
    } catch (err) {
      if (mounted.current && request === generation.current) setError(runnerError(err));
    } finally {
      if (mounted.current && manual) setRefreshing(false);
    }
  }, []);
  React.useEffect(() => {
    mounted.current = true;
    const stop = subscribeRefresh(() => refresh(), { keys: ["/runners"], intervalMs: 15_000 });
    return () => {
      mounted.current = false;
      generation.current++;
      stop();
    };
  }, [refresh]);
  function changed() {
    generation.current++;
    invalidateApiData(["/runners"]);
  }
  async function applyToggle() {
    if (!toggle) return;
    const node = toggle;
    setNodeBusy(node.id);
    generation.current++;
    try {
      await http(`/runners/${encodeURIComponent(node.id)}/enabled`, {
        method: "PUT",
        body: JSON.stringify({ enabled: !node.enabled }),
      });
      toast.success(node.enabled ? "节点已停用，现有执行停止续租" : "节点已启用，等待心跳确认");
      setToggle(null);
      changed();
    } catch (err) {
      toast.error(runnerError(err));
    } finally {
      setNodeBusy(null);
    }
  }
  const nodes = snapshot?.nodes ?? [];
  const ready = nodes.filter((node) => node.online && node.enabled);
  const selected = nodes.find((node) => node.id === editing);
  const filtered = nodes.filter((node) => {
    const matches =
      `${node.name} ${node.id} ${JSON.stringify(node.capabilities.labels)} ${(node.capabilities.environments ?? []).join(" ")}`
        .toLowerCase()
        .includes(query.toLowerCase());
    return (
      matches &&
      (filter === "all" || (filter === "online" ? node.online && node.enabled : !node.online || !node.enabled))
    );
  });
  const groups = [
    {
      key: "pool",
      title: "集群动态执行池",
      description: "K3s / Kubernetes 管理器按需创建 Pod，并发数控制执行环境数量。",
      icon: Box,
      list: filtered.filter(isPool),
    },
    {
      key: "external",
      title: "独立注册节点",
      description: "外部主机或 Docker Runner；可与集群池同时使用，GPU 能力由节点单独声明。",
      icon: Server,
      list: filtered.filter((node) => !isPool(node)),
    },
  ];
  const metrics = [
    { label: "在线可调度", value: `${ready.length} / ${nodes.length}`, detail: "管理器与独立节点", icon: Server },
    {
      label: "集群执行池",
      value: String(nodes.filter(isPool).length),
      detail: `${nodes.filter((node) => !isPool(node)).length} 个独立注册节点`,
      icon: Box,
    },
    {
      label: "执行环境预留",
      value: `${nodes.reduce((n, node) => n + node.active_sessions, 0)} / ${ready.reduce((n, node) => n + node.effective_slots, 0)}`,
      detail: "预留数 / 在线节点总额度",
      icon: Activity,
    },
    {
      label: "在线资源额度",
      value: `${ready.reduce((n, node) => n + node.effective_cpu_millis, 0) / 1000} 核`,
      detail: formatMemory(ready.reduce((n, node) => n + node.effective_memory_mib, 0)),
      icon: Cpu,
    },
  ];
  return (
    <div className="mx-auto flex w-full max-w-7xl flex-col gap-6 p-4 md:p-6">
      <div className="flex flex-wrap items-start justify-between gap-4 rounded-2xl border bg-gradient-to-br from-primary/5 via-background to-background p-5 md:p-6">
        <div className="space-y-2">
          <div className="flex items-center gap-2">
            <h1 className="font-semibold text-xl tracking-tight">Runner</h1>
            <Badge variant="outline">实验功能</Badge>
            {snapshot && (
              <Badge variant={snapshot.config.enabled ? "default" : "secondary"}>
                {snapshot.config.enabled ? "已启用" : "未启用"}
              </Badge>
            )}
          </div>
          <p className="max-w-2xl text-muted-foreground text-sm leading-relaxed">
            统一管理集群执行池与外部算力。Agent 线程按需领取独立执行环境，模型与任务编排保持在控制端。
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" onClick={() => refresh(true)} disabled={refreshing}>
            {refreshing ? <Loader2 className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}刷新
          </Button>
          <Button disabled={!snapshot} onClick={() => setEnrolling(true)}>
            <Plus className="size-4" />
            接入 Runner
          </Button>
        </div>
      </div>
      {(error || snapshot?.error) && (
        <div
          role="alert"
          className="rounded-xl border border-destructive/20 bg-destructive/5 p-4 text-destructive text-sm"
        >
          {error || snapshot?.error}
          {snapshot && error && <p className="mt-1 text-xs">下方保留上次读取的状态，请重试刷新。</p>}
        </div>
      )}
      {!snapshot ? (
        <div className="flex items-center justify-center gap-2 py-16 text-muted-foreground">
          {error ? (
            <Button variant="outline" onClick={() => refresh(true)}>
              重新加载
            </Button>
          ) : (
            <>
              <Loader2 className="size-5 animate-spin" />
              正在读取 Runner 配置…
            </>
          )}
        </div>
      ) : (
        <>
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
            {metrics.map(({ label, value, detail, icon: Icon }) => (
              <Card key={label} className="shadow-none">
                <CardContent className="space-y-2 pt-4">
                  <div className="flex items-center justify-between text-muted-foreground text-xs">
                    {label}
                    <Icon className="size-4" />
                  </div>
                  <p className="font-semibold text-2xl tabular-nums">{value}</p>
                  <p className="text-muted-foreground text-xs">{detail}</p>
                </CardContent>
              </Card>
            ))}
          </div>
          <Tabs defaultValue="nodes" className="gap-5">
            <TabsList>
              <TabsTrigger value="nodes">资源概览</TabsTrigger>
              <TabsTrigger value="settings">执行设置</TabsTrigger>
              <TabsTrigger value="environment">全局环境说明</TabsTrigger>
            </TabsList>
            <TabsContent value="nodes" forceMount className="space-y-6 data-[state=inactive]:hidden">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="flex flex-wrap gap-2">
                  <Input
                    aria-label="搜索 Runner"
                    placeholder="搜索名称、标签或环境"
                    className="w-64"
                    value={query}
                    onChange={(e) => setQuery(e.target.value)}
                  />
                  {[
                    ["all", "全部"],
                    ["online", "在线"],
                    ["unavailable", "离线 / 停用"],
                  ].map(([value, label]) => (
                    <Button
                      key={value}
                      size="sm"
                      variant={filter === value ? "secondary" : "ghost"}
                      onClick={() => setFilter(value)}
                      aria-pressed={filter === value}
                    >
                      {label}
                    </Button>
                  ))}
                </div>
                <span className="text-muted-foreground text-xs">上次更新 {updated} · 每 15 秒刷新</span>
              </div>
              <div className="grid items-start gap-6 xl:grid-cols-2">
                {groups.map(({ key, title, description, icon: Icon, list }) => (
                  <section key={key} className="space-y-3">
                    <div>
                      <h2 className="flex items-center gap-2 font-semibold">
                        <Icon className="size-4 text-muted-foreground" />
                        {title}
                        <Badge variant="secondary">{list.length}</Badge>
                      </h2>
                      <p className="mt-1 text-muted-foreground text-xs">{description}</p>
                    </div>
                    {list.length ? (
                      <div className="grid gap-4">
                        {list.map((node) => (
                          <NodeCard
                            key={node.id}
                            node={node}
                            busy={nodeBusy !== null}
                            manage={() => setEditing(node.id)}
                            toggle={() => setToggle(node)}
                          />
                        ))}
                      </div>
                    ) : (
                      <div className="rounded-xl border border-dashed bg-muted/15 p-6 text-center text-muted-foreground text-sm">
                        {query || filter !== "all"
                          ? "没有符合筛选条件的 Runner"
                          : key === "pool"
                            ? "还没有集群执行池。接入一个管理器即可按需生成执行 Pod。"
                            : "还没有独立节点。可接入集群外的主机或 GPU 机器。"}
                      </div>
                    )}
                  </section>
                ))}
              </div>
              <p className="text-muted-foreground text-xs leading-relaxed">
                资源显示调度预留量。节点标签标识网络位置；执行器类型不代表实际网络位置。任务的 Agent 并发与 Runner
                容量共同决定可运行数量。
              </p>
            </TabsContent>
            <TabsContent value="settings" forceMount className="data-[state=inactive]:hidden">
              <ExecutionSettings
                stored={snapshot.config}
                brokerError={snapshot.error}
                saved={(config) => {
                  generation.current++;
                  setSnapshot((old) => (old ? { ...old, config, error: undefined } : old));
                  changed();
                }}
              />
            </TabsContent>
            <TabsContent value="environment" forceMount className="data-[state=inactive]:hidden">
              <EnvironmentPromptCard />
            </TabsContent>
          </Tabs>
        </>
      )}
      {snapshot && enrolling && <EnrollmentDialog config={snapshot.config} close={() => setEnrolling(false)} />}
      {selected && <NodeEditor key={selected.id} node={selected} close={() => setEditing(null)} changed={changed} />}
      <AlertDialog
        open={toggle !== null}
        onOpenChange={(open) => {
          if (!open && !nodeBusy) setToggle(null);
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{toggle?.enabled ? "停用此 Runner？" : "启用此 Runner？"}</AlertDialogTitle>
            <AlertDialogDescription>
              {toggle?.enabled
                ? `“${toggle.name}”将不再接收新任务。当前 ${toggle.active_sessions} 个已预留执行会停止续租并终止；如只想减少并发，请调整资源额度。`
                : `“${toggle?.name}”重新连接并报告心跳后即可参与调度。`}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={nodeBusy !== null}>取消</AlertDialogCancel>
            <Button
              variant={toggle?.enabled ? "destructive" : "default"}
              onClick={applyToggle}
              disabled={nodeBusy !== null}
            >
              {nodeBusy && <Loader2 className="size-4 animate-spin" />}确认{toggle?.enabled ? "停用" : "启用"}
            </Button>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}
