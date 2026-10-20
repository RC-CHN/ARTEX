"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { RecordExports } from "@/components/record-exports";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { WorkerBudgets } from "@/components/worker-budgets";
import { type Deployment, deploymentAPI, deploymentStates, type Handoff } from "@/lib/deployments";

const labels = {
  name: "名称",
  kind: "类型",
  location: "部署位置",
  identifier: "准确标识 / 路径",
  cleanup: "撤除与验证方法",
  verification: "实际验证结果 / 保留依据",
  note: "备注",
} as const;
const checkLabels: Record<string, string> = {
  cleanup_pending: "待处理部署",
  deployments_retained: "保留部署",
  workers_running: "运行中的 Worker",
  workers_incomplete: "未完成的 Worker",
  main_requests_pending: "待处理问答",
};

export function HandoffTab({ taskId }: { taskId: string }) {
  const [items, setItems] = useState<Deployment[]>([]);
  const [checks, setChecks] = useState<Handoff["checks"] | null>(null);
  const [entry, setEntry] = useState<Partial<Deployment> | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [more, setMore] = useState(false);
  const request = useRef({ fingerprint: "", id: "" });
  const refresh = useCallback(async () => {
    const [rows, handoff] = await Promise.all([deploymentAPI.list(taskId), deploymentAPI.handoff(taskId)]);
    setItems(rows);
    setMore(rows.length === 100);
    setChecks(handoff.checks);
  }, [taskId]);
  useEffect(() => {
    refresh().catch((e) => setError(String(e)));
  }, [refresh]);
  async function run(fn: () => Promise<void>) {
    setBusy(true);
    setError("");
    try {
      await fn();
    } catch (e) {
      setError(e instanceof Error ? e.message : "操作结果未确认，请刷新核对。");
    } finally {
      setBusy(false);
    }
  }
  async function save() {
    if (!entry) return;
    const body = { ...entry, expected_revision: entry.revision };
    const fingerprint = JSON.stringify(body);
    if (request.current.fingerprint !== fingerprint) request.current = { fingerprint, id: crypto.randomUUID() };
    await deploymentAPI.save(taskId, { ...body, request_id: request.current.id });
    setEntry(null);
    await refresh();
  }
  return (
    <div className="space-y-5 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="font-semibold">记录与收尾</h2>
          <p className="mt-1 text-muted-foreground text-sm">逐项登记实际部署及撤除结果。修改登记不会执行远端命令。</p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" disabled={busy} onClick={() => run(refresh)}>
            刷新检查
          </Button>
          <Button
            disabled={busy}
            onClick={() => {
              setError("");
              setEntry({ state: "active", kind: "file" });
            }}
          >
            登记部署
          </Button>
        </div>
      </div>
      <WorkerBudgets taskId={taskId} />
      <RecordExports taskId={taskId} />
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      <div className="space-y-2 rounded-lg border p-4">
        <h3 className="font-medium">交接检查</h3>
        {checks === null ? (
          <p>正在检查…</p>
        ) : checks.length === 0 ? (
          <p className="text-sm">已记录事项没有待处理项；仍需核对是否存在未登记的外部部署。</p>
        ) : (
          checks.map((c) => (
            <div key={c.code} className="text-sm">
              <strong>
                {checkLabels[c.code] ?? c.code} · {c.count}
              </strong>
              <p className="text-muted-foreground">{c.next_action}</p>
            </div>
          ))
        )}
      </div>
      {items.length === 0 && (
        <p className="rounded border border-dashed p-6 text-muted-foreground text-sm">
          尚无部署登记。可由 Agent 使用 manage_deployments，或在此手动登记。
        </p>
      )}
      {items.map((d) => (
        <div key={d.id} className="space-y-2 rounded-lg border p-4">
          <div className="flex justify-between gap-2">
            <h3 className="font-medium">
              {d.name}{" "}
              <span className="text-muted-foreground text-sm">
                #{d.id} · {deploymentStates[d.state]}
              </span>
            </h3>
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={() => {
                setError("");
                setEntry(d);
              }}
            >
              更新记录
            </Button>
          </div>
          <p className="break-all text-sm">
            {d.location} · {d.identifier}
          </p>
          <p className="whitespace-pre-wrap text-sm">{d.cleanup}</p>
          {d.verification && (
            <p className="whitespace-pre-wrap rounded bg-muted p-2 text-sm">验证 / 依据：{d.verification}</p>
          )}
          {d.note && <p className="whitespace-pre-wrap text-muted-foreground text-sm">{d.note}</p>}
          <p className="text-muted-foreground text-xs">
            r{d.revision} · {new Date(d.updated_at).toLocaleString()}
            {d.resource_id ? ` · 关联资源 #${d.resource_id}` : ""}
          </p>
        </div>
      ))}
      {more && (
        <Button
          variant="outline"
          disabled={busy}
          onClick={() =>
            run(async () => {
              const next = await deploymentAPI.list(taskId, items.at(-1)?.id);
              setItems((v) => [...v, ...next]);
              setMore(next.length === 100);
            })
          }
        >
          加载更多部署
        </Button>
      )}
      <Dialog
        open={entry !== null}
        onOpenChange={(open) => {
          if (!open && !busy) setEntry(null);
        }}
      >
        <DialogContent className="max-h-[90vh] overflow-auto">
          <DialogHeader>
            <DialogTitle>{entry?.id ? `更新部署 #${entry.id}` : "登记独立部署"}</DialogTitle>
          </DialogHeader>
          {entry && (
            <form
              className="space-y-3"
              onSubmit={(e) => {
                e.preventDefault();
                void run(save);
              }}
            >
              {Object.entries(labels).map(([key, label]) => (
                <label key={key} htmlFor={`deployment-${key}`} className="block space-y-1 text-sm">
                  <span>{label}</span>
                  {["cleanup", "verification", "note"].includes(key) ? (
                    <Textarea
                      id={`deployment-${key}`}
                      disabled={busy}
                      required={
                        key === "cleanup" ||
                        (key === "verification" && ["removed", "retained"].includes(entry.state ?? ""))
                      }
                      value={String(entry[key as keyof Deployment] ?? "")}
                      onChange={(e) => setEntry({ ...entry, [key]: e.target.value })}
                    />
                  ) : (
                    <Input
                      id={`deployment-${key}`}
                      disabled={busy}
                      required
                      value={String(entry[key as keyof Deployment] ?? "")}
                      onChange={(e) => setEntry({ ...entry, [key]: e.target.value })}
                    />
                  )}
                </label>
              ))}
              <label className="block space-y-1 text-sm">
                <span>状态</span>
                <select
                  aria-label="部署状态"
                  disabled={busy}
                  className="h-9 w-full rounded border bg-background px-2"
                  value={entry.state}
                  onChange={(e) =>
                    setEntry({ ...entry, state: e.target.value as Deployment["state"], verification: "" })
                  }
                >
                  {Object.entries(deploymentStates).map(([key, label]) => (
                    <option key={key} value={key}>
                      {label}
                    </option>
                  ))}
                </select>
              </label>
              <label htmlFor="deployment-resource-id" className="block space-y-1 text-sm">
                <span>关联资源 ID（可选）</span>
                <Input
                  id="deployment-resource-id"
                  type="number"
                  min={0}
                  disabled={busy}
                  value={entry.resource_id ?? ""}
                  onChange={(e) => setEntry({ ...entry, resource_id: Number(e.target.value) })}
                />
              </label>
              <p className="text-muted-foreground text-xs">
                确认实际操作和验证后再标为已撤除。若版本冲突，请关闭后刷新，重新合并修改。
              </p>
              {error && (
                <p role="alert" className="text-destructive text-sm">
                  {error}
                </p>
              )}
              <Button type="submit" disabled={busy}>
                {busy ? "保存中…" : "保存登记"}
              </Button>
            </form>
          )}
        </DialogContent>
      </Dialog>
    </div>
  );
}
