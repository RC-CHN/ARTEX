"use client";

import { useEffect, useState } from "react";

import { RecordExports } from "@/components/record-exports";
import { TaskTokenBudget } from "@/components/task-token-budget";
import { Button } from "@/components/ui/button";
import { WorkerBudgets } from "@/components/worker-budgets";
import { http } from "@/lib/api";

type Handoff = { checks: { code: string; count: number; next_action: string }[] };
const checkLabels: Record<string, string> = {
  workers_running: "运行中的 Worker",
  workers_incomplete: "未完成的 Worker",
  main_requests_pending: "待处理问答",
};

export function HandoffTab({ taskId }: { taskId: string }) {
  const [checks, setChecks] = useState<Handoff["checks"] | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(true);
  const [refreshTick, setRefreshTick] = useState(0);
  // biome-ignore lint/correctness/useExhaustiveDependencies: Explicit refresh repeats the same task query.
  useEffect(() => {
    const controller = new AbortController();
    setBusy(true);
    setChecks(null);
    setError("");
    http<Handoff>(`/tasks/${taskId}/handoff`, { cache: "no-store", signal: controller.signal })
      .then((result) => {
        if (!controller.signal.aborted) setChecks(result.checks);
      })
      .catch((e) => {
        if (!controller.signal.aborted) setError(e instanceof Error ? e.message : "检查失败，请刷新重试。");
      })
      .finally(() => {
        if (!controller.signal.aborted) setBusy(false);
      });
    return () => controller.abort();
  }, [taskId, refreshTick]);
  return (
    <div className="space-y-5 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="font-semibold">记录与收尾</h2>
          <p className="mt-1 text-muted-foreground text-sm">
            检查未完成执行、管理预算并导出记录。实例说明与撤除结果统一维护在自有资源中。
          </p>
        </div>
        <Button variant="outline" disabled={busy} onClick={() => setRefreshTick((n) => n + 1)}>
          刷新检查
        </Button>
      </div>
      <WorkerBudgets taskId={taskId} />
      <TaskTokenBudget key={taskId} taskId={taskId} />
      <RecordExports taskId={taskId} />
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      <div className="space-y-2 rounded-lg border p-4">
        <h3 className="font-medium">交接检查</h3>
        {busy && <p>正在检查…</p>}
        {checks?.length === 0 && (
          <p className="text-sm">已记录的 Worker 与主 Agent 问答没有待处理项。实例的使用与撤除记录请查看自有资源。</p>
        )}
        {checks?.map((c) => (
          <div key={c.code} className="text-sm">
            <strong>
              {checkLabels[c.code] ?? c.code} · {c.count}
            </strong>
            <p className="text-muted-foreground">{c.next_action}</p>
          </div>
        ))}
      </div>
    </div>
  );
}
