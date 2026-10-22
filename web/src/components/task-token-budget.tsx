"use client";
import { useCallback, useEffect, useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { http } from "@/lib/api";

type Budget = { limit: number; used: number; revision: number; reached: boolean; usage_available: boolean };
export function TaskTokenBudget({ taskId }: { taskId: string }) {
  const [budget, setBudget] = useState<Budget | null>(null),
    [limit, setLimit] = useState("0"),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  const load = useCallback(async () => {
    setBusy(true);
    setError("");
    try {
      const b = await http<Budget>(`/tasks/${taskId}/token-budget`, { cache: "no-store" });
      setBudget(b);
      setLimit(String(b.limit));
    } finally {
      setBusy(false);
    }
  }, [taskId]);
  useEffect(() => {
    load().catch((e) => setError(String(e)));
  }, [load]);
  async function save() {
    if (!budget) return;
    const n = Number(limit);
    if (!limit.trim() || !Number.isSafeInteger(n) || n < 0 || n > 1e12) {
      setError("填写0至1万亿的整数，0表示不限额。");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const b = await http<Budget>(`/tasks/${taskId}/token-budget`, {
        method: "POST",
        body: JSON.stringify({ limit: n, expected_revision: budget.revision }),
      });
      setBudget(b);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="space-y-3 rounded-lg border p-4">
      <h3 className="font-medium">任务累计 Token 软预算</h3>
      <p className="text-muted-foreground text-sm">
        默认不限额。按本任务已记录的输入＋输出累计，包含各 Agent 与压缩调用。达到后停止自动规划和领取新工作，运行中的
        Worker 在下一轮进入收尾；正在执行的请求和收尾可能超出阈值。主 Agent 协调与人工对话仍可使用，不折算金额。
      </p>
      {budget && (
        <p className="text-sm">
          已记录 {budget.usage_available ? budget.used.toLocaleString() : "暂不可查询"} /{" "}
          {budget.limit ? budget.limit.toLocaleString() : "不限"}
          {budget.reached ? " · 已达到软预算" : ""}
        </p>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <label htmlFor="task-token-limit">累计 Token 上限</label>
        <Input
          id="task-token-limit"
          className="w-48"
          type="number"
          min="0"
          max="1000000000000"
          step="1"
          value={limit}
          onChange={(e) => setLimit(e.target.value)}
        />
        <Button disabled={busy || !budget} onClick={save}>
          保存软预算
        </Button>
        <Button variant="outline" disabled={busy} onClick={() => load().catch((e) => setError(String(e)))}>
          刷新累计用量
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">
        修改不重置用量或 Worker 轮数，不自动重跑已停止的 Worker。冲突或回执不明时先刷新核对。
      </p>
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
    </section>
  );
}
