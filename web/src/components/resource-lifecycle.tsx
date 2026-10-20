"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { newRequestId } from "@/lib/request-id";
import { type Resource, type ResourceReference, resourceAPI } from "@/lib/resources";

export function ResourceLifecycle({ resource, onDeleted }: { resource: Resource; onDeleted: (r: Resource) => void }) {
  const [refs, setRefs] = useState<ResourceReference[] | null>(null);
  const [error, setError] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const request = useRef({ fingerprint: "", id: "" });
  const load = useCallback(async () => setRefs(await resourceAPI.references(resource.id)), [resource.id]);
  useEffect(() => {
    let alive = true;
    resourceAPI
      .references(resource.id)
      .then((v) => {
        if (alive) setRefs(v);
      })
      .catch((e) => {
        if (alive) setError(String(e));
      });
    return () => {
      alive = false;
    };
  }, [resource.id]);
  async function remove() {
    setBusy(true);
    setError("");
    const fingerprint = JSON.stringify([resource.id, resource.revision, reason]);
    if (request.current.fingerprint !== fingerprint) request.current = { fingerprint, id: newRequestId() };
    try {
      const result = await resourceAPI.delete(resource, reason, request.current.id);
      onDeleted(result.resource);
    } catch (e) {
      setError(e instanceof Error ? e.message : "删除结果未确认，请查询后使用原参数重试。");
      await load().catch(() => {
        /* Keep the original actionable deletion error. */
      });
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h3 className="font-medium">任务与部署引用</h3>
        <Button variant="outline" size="sm" disabled={busy} onClick={() => load().catch((e) => setError(String(e)))}>
          刷新引用
        </Button>
      </div>
      {error && (
        <p role="alert" className="text-destructive text-sm">
          {error}
        </p>
      )}
      {refs === null ? (
        <p>正在读取引用…</p>
      ) : refs.length === 0 ? (
        <p className="text-muted-foreground text-sm">没有已登记的引用。</p>
      ) : (
        <div className="space-y-2">
          {refs.map((r, i) => (
            <div key={`${r.task_id}:${r.deployment_id ?? r.version_id}:${i}`} className="rounded border p-3 text-sm">
              <a className="underline" href={`/function/tasks/detail?id=${r.task_id}&tab=handoff`}>
                任务 #{r.task_id}
              </a>{" "}
              · {r.deployment_id ? `部署 #${r.deployment_id}` : `版本 #${r.version_id}`} · {r.state} ·{" "}
              {r.blocking ? "需处理后删除" : "历史记录保留"}
            </div>
          ))}
        </div>
      )}
      <div className="space-y-3 rounded-lg border p-4">
        <h3 className="font-medium">删除资源登记</h3>
        <p className="text-muted-foreground text-sm">
          将从可用列表移除并阻止新领取，历史版本和证据保留。远端进程和文件需单独撤除并验证；有活动引用或未完成的部署时会拒绝删除。
        </p>
        <Input
          aria-label="资源删除原因"
          placeholder="填写删除原因"
          maxLength={4000}
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          disabled={busy}
        />
        <Button
          variant="destructive"
          disabled={busy || !reason.trim() || refs === null || refs.some((r) => r.blocking)}
          onClick={remove}
        >
          {busy ? "提交中…" : "确认删除登记"}
        </Button>
      </div>
    </div>
  );
}
