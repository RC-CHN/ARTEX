"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import {
  Archive,
  Check,
  Download,
  FilePlus2,
  FolderOpen,
  History,
  KeyRound,
  Library,
  Link2,
  Loader2,
  Plus,
  RefreshCw,
  Save,
  Search,
  Trash2,
} from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Markdown } from "@/components/markdown";
import { ResourceLifecycle } from "@/components/resource-lifecycle";
import { Badge } from "@/components/ui/badge";
import { Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger } from "@/components/ui/dialog";
import {
  resourceAPI,
  type Resource,
  type ResourceDraft,
  type ResourceEndpoint,
  type ResourceEvent,
  type ResourceManifest,
  type ResourceVersion,
} from "@/lib/resources";

const kinds: Record<string, string> = {
  tool: "工具",
  file: "文件",
  dataset: "资料集",
  proxy: "代理",
  host: "主机",
  beacon: "已有服务 / Beacon",
};
const selectClass = "h-9 rounded-md border bg-background px-3 text-sm";
const emptyResource: Partial<Resource> = {
  name: "",
  kind: "tool",
  description: "",
  tags: [],
  access_mode: "global",
  enabled: true,
};
const formatSize = (n: number) =>
  n < 1024 ? `${n} B` : n < 1048576 ? `${(n / 1024).toFixed(1)} KiB` : `${(n / 1048576).toFixed(1)} MiB`;
const message = (e: unknown) => (e instanceof Error ? e.message : "操作失败");
const when = (s: string) => new Date(s).toLocaleString();

export default function ResourcesPage() {
  const [items, setItems] = useState<Resource[]>([]);
  const [q, setQ] = useState("");
  const [kind, setKind] = useState("");
  const [selected, setSelected] = useState<Resource | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [more, setMore] = useState(false);
  const [paging, setPaging] = useState(false);
  const queryGeneration = useRef(0);
  useEffect(() => {
    queryGeneration.current++;
    let alive = true;
    setLoading(true);
    const timer = setTimeout(() => {
      resourceAPI
        .list(q, kind)
        .then((rows) => {
          if (alive) {
            setItems(rows);
            setMore(rows.length === 100);
            setError("");
          }
        })
        .catch((e) => {
          if (alive) setError(message(e));
        })
        .finally(() => {
          if (alive) setLoading(false);
        });
    }, 200);
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [q, kind, refresh]);
  const saved = (r: Resource) => {
    setSelected(r);
    setRefresh((n) => n + 1);
  };
  return (
    <div className="flex flex-col gap-5 p-4 md:p-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-semibold">
            <Library className="size-6" />
            自有资源库
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            跨任务复用的文件与服务。支持 Agent 维护、版本发布和使用记录。
          </p>
        </div>
        <ResourceEditor onSaved={saved} />
      </div>
      <div className="flex flex-wrap gap-2">
        <div className="relative min-w-48 flex-1">
          <Search className="absolute left-3 top-2.5 size-4 text-muted-foreground" />
          <Input
            aria-label="搜索自有资源"
            className="pl-9"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            placeholder="搜索名称或描述…"
          />
        </div>
        <select aria-label="资源类别" className={selectClass} value={kind} onChange={(e) => setKind(e.target.value)}>
          <option value="">所有类别</option>
          {Object.entries(kinds).map(([key, title]) => (
            <option key={key} value={key}>
              {title}
            </option>
          ))}
        </select>
        <Button variant="outline" size="icon" aria-label="刷新资源" onClick={() => setRefresh((n) => n + 1)}>
          <RefreshCw className="size-4" />
        </Button>
      </div>
      {error && (
        <div role="alert" className="rounded-lg border border-destructive/30 p-3 text-sm text-destructive">
          {error}
        </div>
      )}
      <div className="grid items-start gap-5 lg:grid-cols-[300px_minmax(0,1fr)]">
        <div className="space-y-2" aria-busy={loading}>
          {loading ? (
            <p className="flex items-center gap-2 p-6 text-muted-foreground">
              <Loader2 className="size-4 animate-spin" />
              加载资源…
            </p>
          ) : items.length === 0 ? (
            <div className="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">
              <Archive className="mx-auto mb-3 size-8" />
              {q || kind ? "没有匹配的资源" : "还没有自有资源，先登记一个工具或服务。"}
            </div>
          ) : (
            items.map((r) => (
              <button
                key={r.id}
                type="button"
                onClick={() => setSelected(r)}
                aria-pressed={selected?.id === r.id}
                className={`w-full rounded-xl border p-4 text-left transition-colors hover:bg-muted/50 ${selected?.id === r.id ? "border-primary bg-primary/5" : "bg-card"}`}
              >
                <div className="flex items-start justify-between gap-2">
                  <span className="break-all font-medium">{r.name}</span>
                  {!r.enabled && <Badge variant="secondary">已停用</Badge>}
                </div>
                <p className="my-2 line-clamp-2 text-xs text-muted-foreground">{r.description || "尚无说明"}</p>
                <div className="flex gap-2">
                  <Badge variant="outline">{kinds[r.kind]}</Badge>
                  <span className="text-xs text-muted-foreground">
                    {r.access_mode === "global" ? "全局可用" : "按任务授权"}
                  </span>
                </div>
              </button>
            ))
          )}
          {more && !loading && (
            <Button
              variant="outline"
              className="w-full"
              disabled={paging}
              onClick={async () => {
                const generation = queryGeneration.current;
                setPaging(true);
                try {
                  const rows = await resourceAPI.list(q, kind, items.at(-1)?.id);
                  if (queryGeneration.current !== generation) return;
                  setItems((old) => [...old, ...rows]);
                  setMore(rows.length === 100);
                } catch (e) {
                  toast.error(message(e));
                } finally {
                  setPaging(false);
                }
              }}
            >
              加载更多
            </Button>
          )}
        </div>
        {selected ? (
          <ResourceDetail
            key={selected.id}
            resource={selected}
            onSaved={(r) => {
              setSelected((current) => (current?.id === r.id ? (r.deleted_at ? null : r) : current));
              setRefresh((n) => n + 1);
            }}
          />
        ) : (
          <div className="grid min-h-72 place-content-center rounded-xl border bg-card p-8 text-center text-muted-foreground">
            <FolderOpen className="mx-auto mb-3 size-10" />
            <p>选择一个资源查看文件、连接配置和历史</p>
            <p className="mt-2 text-xs">新版本发布后，已绑定任务仍使用原版本。</p>
          </div>
        )}
      </div>
    </div>
  );
}
function ResourceEditor({
  resource,
  onSaved,
  accessOnly = false,
}: {
  accessOnly?: boolean;
  resource?: Resource;
  onSaved: (r: Resource) => void;
}) {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<Partial<Resource>>(resource ?? emptyResource);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  return (
    <Dialog
      open={open}
      onOpenChange={(v) => {
        setOpen(v);
        if (v) {
          setForm(resource ?? emptyResource);
          setError("");
        }
      }}
    >
      <DialogTrigger asChild>
        <Button variant={resource ? "outline" : "default"}>
          {resource ? (
            "编辑登记信息"
          ) : (
            <>
              <Plus className="size-4" />
              登记资源
            </>
          )}
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{resource ? "编辑资源" : "登记自有资源"}</DialogTitle>
        </DialogHeader>
        <form
          className="space-y-4"
          onSubmit={async (e) => {
            e.preventDefault();
            setBusy(true);
            setError("");
            try {
              const r = await resourceAPI.save(form);
              onSaved(r);
              setOpen(false);
            } catch (e) {
              setError(message(e));
            } finally {
              setBusy(false);
            }
          }}
        >
          <div className="space-y-2">
            <Label htmlFor="resource-name">名称</Label>
            <Input
              id="resource-name"
              readOnly={accessOnly}
              required
              maxLength={120}
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
            />
          </div>
          <div className="flex flex-wrap gap-3">
            <label className="space-y-1 text-sm">
              类别
              <select
                className={`${selectClass} block`}
                disabled={!!resource}
                value={form.kind}
                onChange={(e) => setForm({ ...form, kind: e.target.value })}
              >
                {Object.entries(kinds).map(([key, title]) => (
                  <option key={key} value={key}>
                    {title}
                  </option>
                ))}
              </select>
            </label>
            <label className="space-y-1 text-sm">
              使用范围
              <select
                className={`${selectClass} block`}
                value={form.access_mode}
                onChange={(e) => setForm({ ...form, access_mode: e.target.value as Resource["access_mode"] })}
              >
                <option value="global">全局可用</option>
                <option value="restricted">按任务授权</option>
              </select>
            </label>
          </div>
          <div className="space-y-2">
            <Label htmlFor="resource-description">用途与说明</Label>
            <Textarea
              id="resource-description"
              readOnly={accessOnly}
              value={form.description}
              maxLength={8000}
              onChange={(e) => setForm({ ...form, description: e.target.value })}
              placeholder="说明使用方式，不要在这里填写凭据"
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="resource-tags">标签（逗号分隔）</Label>
            <Input
              id="resource-tags"
              readOnly={accessOnly}
              value={form.tags?.join(", ")}
              onChange={(e) =>
                setForm({
                  ...form,
                  tags: e.target.value
                    .split(",")
                    .map((t) => t.trim())
                    .filter(Boolean),
                })
              }
            />
          </div>
          {accessOnly && (
            <p className="text-xs text-muted-foreground">
              名称、摘要和标签由已发布 RESOURCE.md 生成，请在草稿中编辑说明文档后发布。
            </p>
          )}
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={form.enabled}
              onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
            />
            启用资源
          </label>
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <Button type="submit" disabled={busy}>
            {busy ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}保存
          </Button>
        </form>
      </DialogContent>
    </Dialog>
  );
}
function ResourceDetail({ resource, onSaved }: { resource: Resource; onSaved: (r: Resource) => void }) {
  const [tab, setTab] = useState("overview");
  const [versions, setVersions] = useState<ResourceVersion[]>([]);
  const [drafts, setDrafts] = useState<ResourceDraft[]>([]);
  const [events, setEvents] = useState<ResourceEvent[]>([]);
  const [draft, setDraft] = useState<ResourceDraft | null>(null);
  const [versionID, setVersionID] = useState(0);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [ready, setReady] = useState(false);
  const [task, setTask] = useState("");
  const [updateAvailable, setUpdateAvailable] = useState(false);
  useEffect(() => {
    let alive = true;
    let pending = false;
    const timer = setInterval(async () => {
      if (document.hidden || pending) return;
      pending = true;
      try {
        const latest = await resourceAPI.get(resource.id);
        if (alive) setUpdateAvailable(latest.revision !== resource.revision);
      } catch {
        /* Retain the current editor on transient connection errors. */
      } finally {
        pending = false;
      }
    }, 10000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [resource.id, resource.revision]);
  const load = useCallback(async () => {
    const [v, d, ev] = await Promise.all([
      resourceAPI.versions(resource.id),
      resourceAPI.drafts(resource.id),
      resourceAPI.events(resource.id),
    ]);
    setVersions(v);
    setDrafts(d);
    setEvents(ev);
    setReady(true);
  }, [resource.id]);
  useEffect(() => {
    load().catch((e) => setError(message(e)));
  }, [load]);
  const run = async (f: () => Promise<void>) => {
    setBusy(true);
    setError("");
    try {
      await f();
      await load();
      onSaved(await resourceAPI.get(resource.id));
    } catch (e) {
      setError(message(e));
    } finally {
      setBusy(false);
    }
  };
  const current = versions.find((v) => v.id === versionID) ?? versions[0];
  return (
    <section className="min-w-0 rounded-xl border bg-card">
      <div className="border-b p-5">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h2 className="break-all text-xl font-semibold">{resource.name}</h2>
            <p className="mt-1 text-xs text-muted-foreground">
              资源 #{resource.id} · {kinds[resource.kind]} ·{" "}
              {resource.access_mode === "global" ? "全局可用" : "按任务授权"}
            </p>
          </div>
          <ResourceEditor resource={resource} accessOnly={versions.length > 0} onSaved={onSaved} />
        </div>
        <div className="mt-4 flex flex-wrap gap-1" role="tablist" aria-label="资源详情">
          {[
            ["overview", "概览与文件"],
            ["drafts", `草稿 (${drafts.length})`],
            ["versions", "版本与任务"],
            ["events", "变更与观察"],
            ["lifecycle", "引用与删除"],
          ].map(([key, label]) => (
            <Button
              key={key}
              role="tab"
              aria-selected={tab === key}
              variant={tab === key ? "secondary" : "ghost"}
              size="sm"
              onClick={() => setTab(key)}
            >
              {label}
            </Button>
          ))}
        </div>
      </div>
      <div className="space-y-5 p-5" role="tabpanel">
        {tab === "lifecycle" && <ResourceLifecycle resource={resource} onDeleted={onSaved} />}
        {updateAvailable && (
          <div role="status" className="rounded-md border p-3 text-sm">
            资源已更新。当前草稿编辑内容保留；刷新后可查看新版本。
            <Button
              className="ml-2"
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={() =>
                run(async () => {
                  await load();
                  setUpdateAvailable(false);
                })
              }
            >
              查看更新
            </Button>
          </div>
        )}
        {error && (
          <div role="alert" className="rounded-md border border-destructive/30 p-3 text-sm text-destructive">
            {error}
            <Button className="ml-2" size="sm" variant="outline" onClick={() => run(load)}>
              刷新版本
            </Button>
          </div>
        )}
        {!ready && !error && <p className="text-sm text-muted-foreground">正在读取资源详情…</p>}
        {tab === "overview" && (
          <>
            <p className="whitespace-pre-wrap text-sm">{resource.description || "尚无说明"}</p>
            <div className="flex flex-wrap gap-1">
              {resource.tags.map((t) => (
                <Badge key={t} variant="outline">
                  {t}
                </Badge>
              ))}
            </div>
            {current ? (
              <>
                <VersionPicker versions={versions} current={current} onChange={setVersionID} />
                <ResourceDocument resource={resource.id} version={current.id} />
                <ManifestView
                  manifest={current}
                  onDownload={(p) =>
                    resourceAPI.download(resource.id, current.id, p).catch((e) => toast.error(message(e)))
                  }
                />
              </>
            ) : (
              ready && <p className="rounded-lg bg-muted p-4 text-sm">尚未发布文件或端点。从草稿开始添加内容。</p>
            )}
            <Button
              disabled={busy || !ready || !resource.enabled}
              onClick={() =>
                run(async () => {
                  const d = await resourceAPI.createDraft(resource.id, versions[0]?.id ?? 0);
                  setDraft(d);
                  setTab("drafts");
                })
              }
            >
              <FilePlus2 className="size-4" />
              基于最新版本建立草稿
            </Button>
          </>
        )}
        {tab === "drafts" && (
          <>
            <p className="text-xs text-muted-foreground">
              草稿可反复修改，发布后形成不可变版本。文件目录和端点配置属于同一个版本。
            </p>
            <div className="flex flex-wrap gap-2">
              {drafts.map((d) => (
                <Button
                  key={d.id}
                  variant={draft?.id === d.id ? "secondary" : "outline"}
                  size="sm"
                  onClick={() => setDraft(d)}
                >
                  草稿 #{d.id} · r{d.revision}
                </Button>
              ))}
              <Button
                size="sm"
                variant="outline"
                disabled={busy || !resource.enabled}
                onClick={() =>
                  run(async () => setDraft(await resourceAPI.createDraft(resource.id, versions[0]?.id ?? 0)))
                }
              >
                <Plus className="size-4" />
                新建草稿
              </Button>
            </div>
            {draft ? (
              <DraftEditor
                key={`${draft.id}:${draft.revision}`}
                draft={draft}
                busy={busy}
                run={run}
                onChange={setDraft}
                onPublished={() => {
                  setDraft(null);
                  setTab("versions");
                  setVersionID(0);
                }}
              />
            ) : (
              <p className="py-8 text-center text-sm text-muted-foreground">选择草稿，或建立一个新草稿。</p>
            )}
          </>
        )}
        {tab === "versions" && (
          <>
            {versions.length ? (
              <>
                <VersionPicker versions={versions} current={current} onChange={setVersionID} />
                <div className="rounded-lg bg-muted/40 p-4 text-sm">
                  <p className="font-medium">{current.note}</p>
                  <p className="mt-2 whitespace-pre-wrap text-muted-foreground">
                    验证：{current.validation || "未记录验证"}
                  </p>
                  <p className="mt-3 text-xs text-muted-foreground">
                    {when(current.created_at)} ·{" "}
                    {current.actor.task_id ? `Agent / 任务 #${current.actor.task_id}` : "用户"}
                  </p>
                </div>
                <ManifestView
                  manifest={current}
                  onDownload={(p) =>
                    resourceAPI.download(resource.id, current.id, p).catch((e) => toast.error(message(e)))
                  }
                />
                <form
                  className="space-y-2 border-t pt-4"
                  onSubmit={(e) => {
                    e.preventDefault();
                    run(async () => {
                      await resourceAPI.pin(resource.id, Number(task), current.id);
                      toast.success(`已为任务 #${task} 固定 v${current.version}`);
                    });
                  }}
                >
                  <Label htmlFor="resource-task">绑定到任务 / 显式更新任务版本</Label>
                  <div className="flex gap-2">
                    <Input
                      id="resource-task"
                      type="number"
                      min={1}
                      required
                      value={task}
                      onChange={(e) => setTask(e.target.value)}
                      placeholder="任务 ID"
                    />
                    <Button disabled={busy || !resource.enabled} type="submit">
                      <Link2 className="size-4" />
                      绑定此版本
                    </Button>
                  </div>
                  <p className="text-xs text-muted-foreground">
                    任务不会跟随最新版本自动升级；受限资源绑定后，该任务的 Agent 才能使用与维护。
                  </p>
                </form>
              </>
            ) : (
              <p className="text-sm text-muted-foreground">尚无已发布版本。</p>
            )}
          </>
        )}
        {tab === "events" && (
          <>
            <p className="text-xs text-muted-foreground">
              可达性记录仅代表标注的 Runner 和检查时间，不会自动停用全局服务。
            </p>
            <div className="divide-y">
              {events.map((ev) => (
                <div key={ev.id} className="space-y-1 py-3 text-sm">
                  <div className="flex items-center gap-2">
                    <History className="size-3.5 text-muted-foreground" />
                    <span className="font-medium">{eventLabel(ev.kind)}</span>
                    <span className="ml-auto text-xs text-muted-foreground">{when(ev.created_at)}</span>
                  </div>
                  <p className="whitespace-pre-wrap break-all">{ev.detail}</p>
                  <p className="text-xs text-muted-foreground">
                    {ev.actor.task_id
                      ? `任务 #${ev.actor.task_id} · ${ev.actor.thread_id} · Runner ${ev.actor.runner_id}`
                      : "用户操作"}
                    {ev.version_id ? ` · 版本 ID ${ev.version_id}` : ""}
                  </p>
                </div>
              ))}
              {!events.length && <p className="py-8 text-center text-sm text-muted-foreground">暂无记录</p>}
            </div>
            {events.length > 0 && events.length % 100 === 0 && (
              <Button
                disabled={busy}
                variant="outline"
                onClick={async () => {
                  setBusy(true);
                  try {
                    const old = await resourceAPI.events(resource.id, events.at(-1)?.id);
                    setEvents((v) => [...v, ...old]);
                  } catch (e) {
                    setError(message(e));
                  } finally {
                    setBusy(false);
                  }
                }}
              >
                更早的记录
              </Button>
            )}
          </>
        )}
      </div>
    </section>
  );
}
function VersionPicker({
  versions,
  current,
  onChange,
}: {
  versions: ResourceVersion[];
  current: ResourceVersion;
  onChange: (id: number) => void;
}) {
  return (
    <label className="flex flex-wrap items-center gap-2 text-sm">
      查看版本
      <select className={selectClass} value={current.id} onChange={(e) => onChange(Number(e.target.value))}>
        {versions.map((v) => (
          <option key={v.id} value={v.id}>
            v{v.version} · {when(v.created_at)}
          </option>
        ))}
      </select>
      <Badge variant="outline">
        {formatSize(current.size)} · {current.files.length} 个文件
      </Badge>
    </label>
  );
}
function ManifestView({
  manifest,
  onDownload,
  onRemove,
}: {
  manifest: ResourceManifest;
  onDownload?: (p: string) => void;
  onRemove?: (p: string) => void;
}) {
  return (
    <div className="space-y-4">
      <div>
        <p className="mb-2 text-sm font-medium">
          文件 {manifest.os && `· ${manifest.os}`} {manifest.arch}
        </p>
        <div className="divide-y rounded-lg border">
          {manifest.files.map((f) => (
            <div key={f.path} className="flex items-center gap-2 p-3 text-sm">
              <FolderOpen className="size-4 shrink-0 text-muted-foreground" />
              <div className="min-w-0 flex-1">
                <p className="break-all font-mono text-xs">
                  {f.path}
                  {f.executable && (
                    <Badge className="ml-2" variant="outline">
                      可执行
                    </Badge>
                  )}
                </p>
                <p className="mt-1 truncate text-[10px] text-muted-foreground" title={f.sha256}>
                  SHA-256 {f.sha256}
                </p>
              </div>
              <span className="whitespace-nowrap text-xs text-muted-foreground">{formatSize(f.size)}</span>
              {onDownload && (
                <Button variant="ghost" size="icon" aria-label={`下载 ${f.path}`} onClick={() => onDownload(f.path)}>
                  <Download className="size-4" />
                </Button>
              )}
              {onRemove && f.path !== "RESOURCE.md" && (
                <Button variant="ghost" size="icon" aria-label={`移除 ${f.path}`} onClick={() => onRemove(f.path)}>
                  <Trash2 className="size-4" />
                </Button>
              )}
            </div>
          ))}
          {!manifest.files.length && (
            <p className="p-4 text-xs text-muted-foreground">没有文件。服务资源可以只登记端点。</p>
          )}
        </div>
      </div>
      <div>
        <p className="mb-2 text-sm font-medium">连接配置</p>
        {manifest.endpoints.map((e) => (
          <div key={e.name} className="mb-2 rounded-lg border p-3 text-sm">
            <p className="flex items-center gap-2 font-medium">
              <Link2 className="size-4" />
              {e.name}
              {e.has_credential && (
                <Badge variant="secondary">
                  <KeyRound className="mr-1 size-3" />
                  凭据已加密
                </Badge>
              )}
            </p>
            <p className="mt-1 break-all font-mono text-xs text-muted-foreground">{e.url}</p>
          </div>
        ))}
        {!manifest.endpoints.length && <p className="text-xs text-muted-foreground">没有端点配置。</p>}
      </div>
    </div>
  );
}
function DraftEditor({
  draft,
  busy,
  run,
  onChange,
  onPublished,
}: {
  draft: ResourceDraft;
  busy: boolean;
  run: (f: () => Promise<void>) => Promise<void>;
  onChange: (d: ResourceDraft | null) => void;
  onPublished: () => void;
}) {
  const [documentDirty, setDocumentDirty] = useState(false);
  const [config, setConfig] = useState(draft);
  const [credentials, setCredentials] = useState<Record<string, string>>({});
  const [dirty, setDirty] = useState(false);
  const [prefix, setPrefix] = useState("");
  const [executable, setExecutable] = useState(false);
  const [note, setNote] = useState("");
  const [validation, setValidation] = useState("");
  const [discard, setDiscard] = useState(false);
  const endpoint = (i: number, value: Partial<ResourceEndpoint>) => {
    setConfig({ ...config, endpoints: config.endpoints.map((e, n) => (n === i ? { ...e, ...value } : e)) });
    setDirty(true);
  };
  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Badge variant="outline">
          草稿 #{draft.id} · r{draft.revision} · 基础版本 ID {draft.base_version_id || "无"}
        </Badge>
        <Button variant="ghost" size="sm" disabled={busy} onClick={() => setDiscard((v) => !v)}>
          <Trash2 className="size-4" />
          丢弃草稿
        </Button>
      </div>
      {discard && (
        <div className="flex items-center justify-between rounded-md border border-destructive/30 p-3 text-sm">
          丢弃后无法继续编辑此草稿。
          <Button
            variant="destructive"
            size="sm"
            disabled={busy}
            onClick={() =>
              run(async () => {
                await resourceAPI.discard(draft);
                onChange(null);
              })
            }
          >
            确认丢弃
          </Button>
        </div>
      )}
      <DraftDocument
        draft={draft}
        disabled={busy || dirty}
        onDirty={setDocumentDirty}
        save={(content) => run(async () => onChange(await resourceAPI.saveDocument(draft, content)))}
      />
      <fieldset disabled={busy || documentDirty} className="space-y-3">
        <legend className="mb-2 text-sm font-medium">连接配置与平台</legend>
        <div className="flex gap-2">
          <Input
            aria-label="操作系统"
            placeholder="操作系统，如 linux"
            value={config.os}
            onChange={(e) => {
              setConfig({ ...config, os: e.target.value });
              setDirty(true);
            }}
          />
          <Input
            aria-label="架构"
            placeholder="架构，如 arm64"
            value={config.arch}
            onChange={(e) => {
              setConfig({ ...config, arch: e.target.value });
              setDirty(true);
            }}
          />
        </div>
        {config.endpoints.map((e, i) => (
          <div key={i} className="space-y-2 rounded-lg border p-3">
            <div className="flex gap-2">
              <Input
                aria-label={`端点 ${i + 1} 名称`}
                placeholder="端点名称"
                value={e.name}
                onChange={(ev) => {
                  const next = { ...credentials };
                  delete next[e.name];
                  setCredentials(next);
                  endpoint(i, { name: ev.target.value, has_credential: false });
                }}
              />
              <Button
                type="button"
                variant="ghost"
                size="icon"
                aria-label={`移除端点 ${i + 1}`}
                onClick={() => {
                  const next = { ...credentials };
                  delete next[e.name];
                  setCredentials(next);
                  setConfig({ ...config, endpoints: config.endpoints.filter((_, n) => n !== i) });
                  setDirty(true);
                }}
              >
                <Trash2 className="size-4" />
              </Button>
            </div>
            <Input
              aria-label={`端点 ${i + 1} 地址`}
              placeholder="如 socks5://host:1080（不含凭据）"
              value={e.url}
              onChange={(ev) => endpoint(i, { url: ev.target.value })}
            />
            <Input
              type="password"
              autoComplete="new-password"
              aria-label={`端点 ${i + 1} 凭据`}
              placeholder={e.has_credential ? "已加密保存；留空保持原值" : "可选凭据，单独加密保存"}
              value={credentials[e.name] ?? ""}
              onChange={(ev) => {
                const next = { ...credentials };
                if (ev.target.value) next[e.name] = ev.target.value;
                else delete next[e.name];
                setCredentials(next);
                setDirty(true);
              }}
            />
            {e.has_credential && (
              <Button
                type="button"
                size="sm"
                variant="ghost"
                onClick={() => {
                  setCredentials({ ...credentials, [e.name]: "" });
                  setDirty(true);
                }}
              >
                清除已有凭据{credentials[e.name] === "" && "（待保存）"}
              </Button>
            )}
          </div>
        ))}
        <div className="flex flex-wrap gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => {
              setConfig({ ...config, endpoints: [...config.endpoints, { name: "", url: "", has_credential: false }] });
              setDirty(true);
            }}
          >
            <Plus className="size-4" />
            添加端点
          </Button>
          <Button
            type="button"
            size="sm"
            disabled={!dirty}
            onClick={() => run(async () => onChange(await resourceAPI.configure(config, credentials)))}
          >
            <Save className="size-4" />
            保存配置
          </Button>
        </div>
      </fieldset>
      <ManifestView
        manifest={draft}
        onRemove={
          busy || dirty || documentDirty
            ? undefined
            : (p) => {
                run(async () => onChange(await resourceAPI.removeFile(draft, p)));
              }
        }
      />
      <fieldset disabled={busy || dirty || documentDirty} className="space-y-2 rounded-lg border border-dashed p-4">
        <legend className="px-1 text-sm font-medium">添加 / 替换文件</legend>
        <Input
          aria-label="目标子目录"
          placeholder="目标子目录，可留空，如 bin/linux-arm64"
          value={prefix}
          onChange={(e) => setPrefix(e.target.value)}
        />
        <label className="flex items-center gap-2 text-xs">
          <input type="checkbox" checked={executable} onChange={(e) => setExecutable(e.target.checked)} />
          标记本次上传为可执行文件
        </label>
        <Input
          aria-label="选择资源文件"
          type="file"
          multiple
          onChange={(e) => {
            const files = Array.from(e.target.files ?? []);
            e.target.value = "";
            if (!files.length) return;
            run(async () => {
              let current = draft;
              for (const file of files) {
                if (file.size > 256 * 1048576) throw new Error(`${file.name} 超过 256 MiB`);
                current = await resourceAPI.upload(
                  current,
                  file,
                  [prefix.replace(/\/$/, ""), file.name].filter(Boolean).join("/"),
                  executable,
                );
                onChange(current);
              }
            });
          }}
        />
        <p className="text-xs text-muted-foreground">
          单文件最多 256 MiB；每版本最多 2,048 个文件、1 GiB。只上传选择的文件。
        </p>
      </fieldset>
      <form
        className="space-y-3 border-t pt-4"
        onSubmit={(e) => {
          e.preventDefault();
          run(async () => {
            await resourceAPI.publish(draft, note, validation);
            toast.success("新版本已发布，已有任务绑定保持不变");
            onPublished();
          });
        }}
      >
        <Label htmlFor="publish-note">发布说明</Label>
        <Textarea
          id="publish-note"
          required
          maxLength={4000}
          placeholder="本次修改了什么"
          value={note}
          onChange={(e) => setNote(e.target.value)}
        />
        <Label htmlFor="publish-validation">验证记录</Label>
        <Textarea
          id="publish-validation"
          required
          maxLength={8000}
          placeholder="实际做过的测试与结果；尚未验证请明确填写"
          value={validation}
          onChange={(e) => setValidation(e.target.value)}
        />
        {(dirty || documentDirty) && <p className="text-xs text-amber-600">有尚未保存的说明或连接配置，请先保存。</p>}
        <Button
          type="submit"
          disabled={busy || dirty || documentDirty || (!draft.files.length && !draft.endpoints.length)}
        >
          {busy ? <Loader2 className="size-4 animate-spin" /> : <Check className="size-4" />}发布新版本
        </Button>
      </form>
    </div>
  );
}
function eventLabel(kind: string) {
  const labels: Record<string, string> = {
    created: "登记资源",
    metadata_updated: "更新登记信息",
    draft_created: "建立草稿",
    draft_file_updated: "更新草稿文件",
    draft_file_removed: "移除草稿文件",
    draft_configuration_updated: "更新连接配置",
    draft_discarded: "丢弃草稿",
    published: "发布版本",
    task_pin: "任务选择版本",
    "observation:available": "观察：可用",
    "observation:unreachable": "观察：不可达",
    "observation:untested": "观察：尚未验证",
  };
  return labels[kind] ?? kind;
}

function ResourceDocument({ resource, version }: { resource: number; version: number }) {
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  useEffect(() => {
    let alive = true;
    setText("");
    setError("");
    resourceAPI
      .versionDocument(resource, version)
      .then((v) => {
        if (alive) setText(v.content);
      })
      .catch((e) => {
        if (alive) setError(message(e));
      });
    return () => {
      alive = false;
    };
  }, [resource, version]);
  return (
    <div className="rounded-lg border p-4">
      <p className="mb-3 text-xs font-medium text-muted-foreground">RESOURCE.md · 当前版本的使用说明</p>
      {error ? (
        <p role="alert" className="text-destructive">
          {error}
        </p>
      ) : text ? (
        <Markdown text={text.replace(/^---\r?\n[\s\S]*?\r?\n---\r?\n/, "")} />
      ) : (
        <p className="text-xs text-muted-foreground">正在读取说明…</p>
      )}
    </div>
  );
}
function DraftDocument({
  draft,
  disabled,
  onDirty,
  save,
}: {
  draft: ResourceDraft;
  disabled: boolean;
  onDirty: (dirty: boolean) => void;
  save: (content: string) => Promise<void>;
}) {
  const [content, setContent] = useState("");
  const [initial, setInitial] = useState("");
  const [error, setError] = useState("");
  const [ready, setReady] = useState(false);
  const [preview, setPreview] = useState(false);
  useEffect(() => {
    let alive = true;
    resourceAPI
      .draftDocument(draft.id)
      .then((v) => {
        if (!alive) return;
        if (v.revision !== draft.revision) {
          setError("草稿已被更新，请重新选择草稿以刷新。");
          return;
        }
        setContent(v.content);
        setInitial(v.content);
        setReady(true);
      })
      .catch((e) => {
        if (alive) setError(message(e));
      });
    return () => {
      alive = false;
    };
  }, [draft.id, draft.revision]);
  return (
    <fieldset disabled={disabled || !ready} className="space-y-3">
      <legend className="text-sm font-medium">RESOURCE.md · 入口说明</legend>
      <p className="text-xs text-muted-foreground">
        支持纯文字资源。头部 name / description / tags 用于搜索，其余 Markdown 说明会随文件一起发布。
      </p>
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
      {preview ? (
        <div className="rounded-lg border p-3">
          <Markdown text={content.replace(/^---\r?\n[\s\S]*?\r?\n---\r?\n/, "")} />
        </div>
      ) : (
        <Textarea
          aria-label="资源入口文档"
          className="min-h-64 font-mono text-xs"
          value={content}
          onChange={(e) => {
            setContent(e.target.value);
            onDirty(e.target.value !== initial);
          }}
        />
      )}
      <div className="flex gap-2">
        <Button type="button" variant="outline" size="sm" onClick={() => setPreview(!preview)}>
          {preview ? "编辑源码" : "预览说明"}
        </Button>
        <Button type="button" size="sm" disabled={content === initial} onClick={() => save(content)}>
          <Save className="size-4" />
          保存说明
        </Button>
      </div>
    </fieldset>
  );
}
