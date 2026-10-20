"use client";
import { useCallback, useEffect, useRef, useState } from "react";

import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import { type RecordExport, recordExportAPI } from "@/lib/record-exports";

const states: Record<string, string> = {
  queued: "排队中",
  running: "生成中",
  ready: "可下载",
  failed: "生成失败",
  canceled: "已取消",
  deleting: "删除副本中",
  deleted: "副本已删除",
};
const phases: Record<string, string> = {
  snapshot: "准备快照",
  database: "数据库记录",
  files: "文件与会话",
  index: "离线索引",
  package: "校验与打包",
};
export function CreateRecordExport({
  taskIds,
  onCreated,
}: {
  taskIds: number[];
  onCreated?: (job: RecordExport) => void;
}) {
  const [open, setOpen] = useState(false),
    [redacted, setRedacted] = useState(false),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  const request = useRef({ fingerprint: "", id: "" });
  async function create() {
    setBusy(true);
    setError("");
    const fingerprint = JSON.stringify([taskIds, redacted]);
    if (request.current.fingerprint !== fingerprint) request.current = { fingerprint, id: crypto.randomUUID() };
    try {
      const job = await recordExportAPI.create(taskIds, redacted, request.current.id);
      onCreated?.(job);
      toast.success(`资料包 #${job.id} 已进入队列，可在所选任务的“记录与收尾”查看。`);
      setOpen(false);
      request.current = { fingerprint: "", id: "" };
    } catch (e) {
      setError(e instanceof Error ? e.message : "导出排队结果未确认，原参数重试可复用回执。");
    } finally {
      setBusy(false);
    }
  }
  return (
    <Dialog
      open={open}
      onOpenChange={(v) => {
        if (!busy) setOpen(v);
      }}
    >
      <DialogTrigger asChild>
        <Button variant="outline" size="sm" disabled={taskIds.length === 0 || taskIds.length > 20}>
          导出完整记录{taskIds.length > 1 ? ` (${taskIds.length})` : ""}
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>生成任务记录资料包</DialogTitle>
        </DialogHeader>
        <p className="break-all text-sm">
          仅导出任务 {taskIds.map((id) => `#${id}`).join("、")}；关联任务不会自动加入。
        </p>
        <p className="text-muted-foreground text-sm">
          包含实际留存的会话、工具参数与返回、文件、资源版本和绑定证据，提供 JSONL、Markdown 与离线
          HTML。运行中有变化或未留存的内容会列入缺失清单。生成不会暂停原任务。
        </p>
        <label className="flex items-start gap-2 text-sm">
          <input type="checkbox" checked={redacted} disabled={busy} onChange={(e) => setRedacted(e.target.checked)} />
          <span>过滤常见凭据字段（排除二进制附件；外发前仍需审阅）</span>
        </label>
        <p className="text-muted-foreground text-xs">
          默认原始内部资料包。每次生成独立快照；上限 20 GiB / 20 万文件，副本保留 7 天，删除副本不影响原始记录。
        </p>
        {error && (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        )}
        <Button disabled={busy} onClick={create}>
          {busy ? "提交中…" : "开始生成"}
        </Button>
      </DialogContent>
    </Dialog>
  );
}
export function RecordExports({ taskId }: { taskId: string }) {
  const [jobs, setJobs] = useState<RecordExport[]>([]),
    [more, setMore] = useState(false),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  const paginated = useRef(false);
  const load = useCallback(
    async (before = 0) => {
      const result = await recordExportAPI.list(taskId, before);
      paginated.current = before > 0;
      setJobs((v) => (before ? [...v, ...result.filter((r) => !v.some((j) => j.id === r.id))] : result));
      setMore(result.length === 50);
    },
    [taskId],
  );
  useEffect(() => {
    let alive = true,
      pending = false;
    paginated.current = false;
    setJobs([]);
    const refresh = async () => {
      if (pending || document.hidden) return;
      pending = true;
      try {
        const rows = await recordExportAPI.list(taskId);
        if (alive) {
          setJobs((v) => (paginated.current ? [...rows, ...v.filter((j) => !rows.some((r) => r.id === j.id))] : rows));
          if (!paginated.current) setMore(rows.length === 50);
        }
      } catch (e) {
        if (alive) setError(String(e));
      } finally {
        pending = false;
      }
    };
    void refresh();
    const timer = setInterval(refresh, 5000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [taskId]);
  async function action(id: number, act: string) {
    setBusy(true);
    setError("");
    try {
      await recordExportAPI.action(id, act);
      await load();
    } catch (e) {
      setError(e instanceof Error ? e.message : "操作结果未确认，请刷新。");
    } finally {
      setBusy(false);
    }
  }
  return (
    <section className="space-y-3 rounded-lg border p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h3 className="font-medium">完整记录资料包</h3>
        <CreateRecordExport
          taskIds={[Number(taskId)]}
          onCreated={() => void load().catch((e) => setError(String(e)))}
        />
      </div>
      <p className="text-muted-foreground text-sm">
        在其他软件撰写报告时，可解压资料包并打开 index.html。下载支持断点续传；manifest.json 列出校验值和缺失范围。
      </p>
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      {jobs.length === 0 && <p className="text-muted-foreground text-sm">尚无导出副本。</p>}
      {jobs.map((job) => (
        <article key={job.id} className="space-y-2 rounded border p-3">
          <div className="flex flex-wrap justify-between gap-2">
            <strong className="text-sm">
              #{job.id} · {states[job.state] ?? job.state} · {job.redacted ? "凭据字段过滤" : "原始内部记录"}
            </strong>
            <span className="text-muted-foreground text-xs">任务 {job.task_ids.map((id) => `#${id}`).join("、")}</span>
          </div>
          <p className="text-muted-foreground text-xs">
            {phases[job.phase] ?? states[job.phase] ?? job.phase} · {job.files.toLocaleString()} 个文件 ·{" "}
            {(job.bytes / 1048576).toFixed(1)} MiB
            {job.cutoff_at ? ` · 快照 ${new Date(job.cutoff_at).toLocaleString()}` : ""}
          </p>
          {job.error && <p className="text-destructive text-sm">{job.error}</p>}
          {job.sha256 && <p className="break-all font-mono text-xs">SHA-256 {job.sha256}</p>}
          <div className="flex flex-wrap gap-2">
            {job.state === "ready" && (
              <Button size="sm" asChild>
                <a href={`/api/task-record-exports/${job.id}/download`} download>
                  下载 ZIP
                </a>
              </Button>
            )}
            {["queued", "running"].includes(job.state) && (
              <Button size="sm" variant="outline" disabled={busy} onClick={() => action(job.id, "cancel")}>
                取消生成
              </Button>
            )}
            {["failed", "canceled"].includes(job.state) && (
              <Button size="sm" variant="outline" disabled={busy} onClick={() => action(job.id, "retry")}>
                重试生成新快照
              </Button>
            )}
            {["ready", "failed", "canceled"].includes(job.state) && (
              <Button size="sm" variant="ghost" disabled={busy} onClick={() => action(job.id, "delete")}>
                删除导出副本
              </Button>
            )}
          </div>
          {job.state === "ready" && (
            <p className="text-muted-foreground text-xs">
              副本保留至 {new Date(job.expires_at).toLocaleString()}；原始任务记录保留。
            </p>
          )}
        </article>
      ))}
      {more && (
        <Button
          variant="outline"
          disabled={busy}
          onClick={() => load(jobs.at(-1)?.id).catch((e) => setError(String(e)))}
        >
          加载更早的导出
        </Button>
      )}
    </section>
  );
}
