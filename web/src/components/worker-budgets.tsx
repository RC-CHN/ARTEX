"use client";
import { useCallback, useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { http } from "@/lib/api";

type Budget = {
  intent_id: number;
  normal_limit: number;
  wrapup_limit: number;
  normal_used: number;
  wrapup_used: number;
  phase: string;
  revision: number;
};
export function WorkerBudgets({ taskId }: { taskId: string }) {
  const [rows, setRows] = useState<Budget[]>([]),
    [more, setMore] = useState(false),
    [selected, setSelected] = useState<Budget | null>(null),
    [normal, setNormal] = useState(50),
    [wrapup, setWrapup] = useState(10),
    [reason, setReason] = useState(""),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  const request = useRef({ fingerprint: "", id: "" });
  const load = useCallback(
    async (after = 0) => {
      const result = await http<Budget[]>(`/tasks/${taskId}/worker-budgets?after=${after}`, { cache: "no-store" });
      setRows((v) => (after ? [...v, ...result] : result));
      setMore(result.length === 100);
    },
    [taskId],
  );
  useEffect(() => {
    load().catch((e) => setError(String(e)));
  }, [load]);
  async function save() {
    if (!selected) return;
    setBusy(true);
    setError("");
    const input = {
      intent_id: selected.intent_id,
      expected_revision: selected.revision,
      additional_turns: normal,
      additional_wrapup_turns: wrapup,
      reason,
    };
    const fingerprint = JSON.stringify(input);
    if (request.current.fingerprint !== fingerprint) request.current = { fingerprint, id: crypto.randomUUID() };
    try {
      await http(`/tasks/${taskId}/worker-budgets`, {
        method: "POST",
        body: JSON.stringify({ ...input, request_id: request.current.id }),
      });
      setSelected(null);
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message : "结果未确认，请查询并用原参数重试。");
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="space-y-3 rounded-lg border p-4">
      <div className="flex justify-between">
        <h3 className="font-medium">Worker 预算</h3>
        <Button size="sm" variant="outline" disabled={busy} onClick={() => load().catch((e) => setError(String(e)))}>
          刷新预算
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">
        默认 150 轮执行 + 10
        轮收尾。按意图累计执行轮次，重试和恢复不重置；底层网络重试、压缩调用不另计。旧意图从升级后的首次执行开始计数。追加不会自动重跑，已停止的意图请从会话继续。
      </p>
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      {rows.length === 0 && <p className="text-muted-foreground text-sm">首次执行时建立预算记录。</p>}
      {rows.map((b) => (
        <div key={b.intent_id} className="flex flex-wrap items-center justify-between gap-2 text-sm">
          <span>
            意图 #{b.intent_id} · 执行 {b.normal_used}/{b.normal_limit || "不限"} · 收尾 {b.wrapup_used}/
            {b.wrapup_limit} ·{" "}
            {({ normal: "有执行额度", wrapup: "仅余收尾额度", exhausted: "额度用尽" } as Record<string, string>)[b.phase] ??
              b.phase}
          </span>
          <Button
            size="sm"
            variant="outline"
            disabled={busy || b.normal_limit === 0}
            onClick={() => {
              setSelected(b);
              setReason("");
              setError("");
            }}
          >
            追加预算
          </Button>
        </div>
      ))}
      {more && (
        <Button
          variant="outline"
          disabled={busy}
          onClick={() => load(rows.at(-1)?.intent_id).catch((e) => setError(String(e)))}
        >
          加载更多
        </Button>
      )}
      {selected && (
        <form
          className="space-y-3 rounded border p-3"
          onSubmit={(e) => {
            e.preventDefault();
            void save();
          }}
        >
          <p className="text-sm">
            追加给意图 #{selected.intent_id}（r{selected.revision}）；冲突时刷新并重新选择。
          </p>
          <div className="flex gap-3">
            <label htmlFor="budget-normal-turns" className="text-sm">
              执行轮数
              <Input
                id="budget-normal-turns"
                type="number"
                min={1}
                max={1000}
                required
                disabled={busy}
                value={normal}
                onChange={(e) => setNormal(Number(e.target.value))}
              />
            </label>
            <label htmlFor="budget-wrapup-turns" className="text-sm">
              收尾轮数
              <Input
                id="budget-wrapup-turns"
                type="number"
                min={0}
                max={100}
                required
                disabled={busy}
                value={wrapup}
                onChange={(e) => setWrapup(Number(e.target.value))}
              />
            </label>
          </div>
          <Input
            aria-label="追加预算理由"
            placeholder="说明剩余工作和追加理由"
            required
            disabled={busy}
            value={reason}
            onChange={(e) => setReason(e.target.value)}
          />
          <div className="flex gap-2">
            <Button type="submit" disabled={busy || !reason.trim()}>
              确认追加
            </Button>
            <Button type="button" variant="ghost" disabled={busy} onClick={() => setSelected(null)}>
              取消
            </Button>
          </div>
        </form>
      )}
    </section>
  );
}
