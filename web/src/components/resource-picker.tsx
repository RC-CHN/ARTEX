"use client";

import { useEffect, useRef, useState } from "react";
import { Loader2, Search, X } from "lucide-react";
import { toast } from "sonner";
import { resourceAPI, type Resource } from "@/lib/resources";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";

type Selection = { resource_id: number; version_id: number };
export function ResourcePicker({
  value,
  onChange,
  active,
  onPendingChange,
}: {
  value: Selection[];
  onChange: (value: Selection[]) => void;
  active: boolean;
  onPendingChange?: (pending: boolean) => void;
}) {
  const [q, setQ] = useState("");
  const [rows, setRows] = useState<Resource[]>([]);
  const [names, setNames] = useState<Record<number, string>>({});
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [pending, setPending] = useState<number | null>(null);
  const latest = useRef(value);
  latest.current = value;
  useEffect(() => {
    if (!active) return;
    let alive = true;
    setLoading(true);
    const timer = setTimeout(() => {
      resourceAPI
        .list(q)
        .then((items) => {
          if (alive) {
            setRows(items.filter((r) => r.enabled));
            setNames((old) => ({ ...old, ...Object.fromEntries(items.map((r) => [r.id, r.name])) }));
            setError("");
          }
        })
        .catch((e) => {
          if (alive) setError(e.message);
        })
        .finally(() => {
          if (alive) setLoading(false);
        });
    }, 200);
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [q, active]);
  async function select(r: Resource) {
    if (pending !== null) return;
    if (value.some((v) => v.resource_id === r.id)) {
      onChange(value.filter((v) => v.resource_id !== r.id));
      return;
    }
    if (value.length >= 32) {
      toast.error("最多选择 32 个资源");
      return;
    }
    setPending(r.id);
    onPendingChange?.(true);
    try {
      const versions = await resourceAPI.versions(r.id);
      if (!versions.length) throw new Error("该资源尚未发布，请先在资源库发布一个版本");
      onChange([
        ...latest.current.filter((v) => v.resource_id !== r.id),
        { resource_id: r.id, version_id: versions[0].id },
      ]);
    } catch (e) {
      toast.error(e instanceof Error ? e.message : "资源选择失败");
    } finally {
      setPending(null);
      onPendingChange?.(false);
    }
  }
  return (
    <div className="space-y-2 rounded-lg border p-3">
      <div className="relative">
        <Search className="absolute left-2.5 top-2.5 size-4 text-muted-foreground" />
        <Input
          aria-label="搜索可用资源"
          className="pl-8"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          placeholder="搜索自有工具、资料或服务…"
        />
      </div>
      {value.length > 0 && (
        <div className="flex flex-wrap gap-1">
          {value.map((v) => (
            <span key={v.resource_id} className="inline-flex items-center gap-1 rounded-md bg-muted px-2 py-1 text-xs">
              {names[v.resource_id] ?? `资源 #${v.resource_id}`} · 版本 ID {v.version_id}
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className="size-5"
                disabled={pending !== null}
                aria-label={`取消选择资源 ${v.resource_id}`}
                onClick={() => onChange(value.filter((x) => x.resource_id !== v.resource_id))}
              >
                <X className="size-3" />
              </Button>
            </span>
          ))}
        </div>
      )}
      {error && (
        <p role="alert" className="text-xs text-destructive">
          {error}
        </p>
      )}
      <div className="max-h-44 space-y-1 overflow-y-auto" aria-busy={loading}>
        {loading ? (
          <p className="p-2 text-xs text-muted-foreground">正在加载…</p>
        ) : rows.length ? (
          rows.map((r) => (
            <label key={r.id} className="flex cursor-pointer items-start gap-2 rounded-md p-2 hover:bg-muted">
              <input
                type="checkbox"
                className="mt-0.5"
                disabled={pending !== null}
                checked={value.some((v) => v.resource_id === r.id)}
                onChange={() => select(r)}
              />
              <span className="min-w-0 flex-1">
                <span className="block text-sm">
                  {r.name}
                  {r.access_mode === "restricted" && (
                    <span className="ml-2 text-xs text-muted-foreground">受限 · 选择即授权本任务</span>
                  )}
                </span>
                <span className="block line-clamp-2 text-xs text-muted-foreground">{r.description}</span>
              </span>
              {pending === r.id && <Loader2 className="size-4 animate-spin" />}
            </label>
          ))
        ) : (
          <p className="p-2 text-xs text-muted-foreground">暂无匹配资源，可先前往“自有资源库”登记并发布。</p>
        )}
      </div>
      {rows.length === 100 && <p className="text-xs text-muted-foreground">显示前 100 项，请输入关键词缩小范围。</p>}
    </div>
  );
}
