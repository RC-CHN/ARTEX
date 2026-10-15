"use client";

import * as React from "react";
import {
  ArrowUpRight,
  CheckCheck,
  ChevronDown,
  CircleHelp,
  Clock3,
  Loader2,
  MessageSquare,
  Send,
  UserRound,
} from "lucide-react";
import { toast } from "sonner";
import { api } from "@/lib/api";
import { useApiRefresh } from "@/hooks/use-api-refresh";
import { Markdown } from "@/components/markdown";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Textarea } from "@/components/ui/textarea";
import type { MainRequest } from "@/lib/types";

const stateView = {
  pending: {
    label: "排队中",
    icon: Clock3,
    tone: "text-amber-600 dark:text-amber-400",
    hint: "Worker 继续执行，主 Agent 会依次处理请示。",
  },
  processing: {
    label: "正在处理",
    icon: Loader2,
    tone: "text-blue-600 dark:text-blue-400",
    hint: "主 Agent 正在查证或协调，过程会显示在主会话。",
  },
  answered: {
    label: "已回复 · 待确认送达",
    icon: Send,
    tone: "text-violet-600 dark:text-violet-400",
    hint: "答复将在 Worker 下一步动作前送达；暂停的 Worker 恢复后接收。",
  },
  delivered: {
    label: "已送达",
    icon: CheckCheck,
    tone: "text-emerald-600 dark:text-emerald-400",
    hint: "回复已进入 Worker 的会话记录。",
  },
  cancelled: {
    label: "已失效",
    icon: CircleHelp,
    tone: "text-muted-foreground",
    hint: "意图或任务已结束，这条请示不再投递。",
  },
} satisfies Record<MainRequest["state"], { label: string; icon: typeof Clock3; tone: string; hint: string }>;

const isPending = (r: MainRequest) => r.state === "pending" || r.state === "processing" || r.state === "answered";
const canReply = (r: MainRequest) => r.state === "pending" || r.state === "processing";
const sourceLabel = (r: MainRequest) =>
  r.answer_source === "human" ? "人工回复" : r.answer_source === "system" ? "系统回执" : "主 Agent 回复";

export function MainRequests({ taskId, onOpenWorker }: { taskId: string; onOpenWorker: (id: number) => void }) {
  const [items, setItems] = React.useState<MainRequest[]>([]);
  const [open, setOpen] = React.useState(false);
  const [history, setHistory] = React.useState(false);
  const [expanded, setExpanded] = React.useState<number | null>(null);
  const [replyId, setReplyId] = React.useState<number | null>(null);
  const [answer, setAnswer] = React.useState("");
  const [sending, setSending] = React.useState(false);
  const live = React.useRef(true);
  const inFlight = React.useRef(false);
  const drafts = React.useRef<Record<number, string>>({});
  React.useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);
  useApiRefresh(() => api.mainRequests(taskId), setItems, [taskId], {
    intervalMs: 5000,
    keys: [`/tasks/${taskId}/main-requests`],
  });
  const pending = items.filter(isPending);
  const archived = items.filter((r) => !isPending(r));
  const visible = history ? archived : pending;
  const processing = pending.some((r) => r.state === "processing");
  const queued = pending.some((r) => r.state === "pending");
  const replyTarget = items.find((r) => r.id === replyId);
  async function reply(id: number) {
    if (!answer.trim() || inFlight.current) return;
    inFlight.current = true;
    setSending(true);
    try {
      await api.answerMainRequest(taskId, id, answer.trim());
      if (!live.current) return;
      delete drafts.current[id];
      setReplyId(null);
      setAnswer("");
      toast.success("人工回复已提交，将送回 Worker 会话");
    } catch (err) {
      if (live.current) toast.error(err instanceof Error ? err.message : "回复失败，草稿已保留");
    } finally {
      if (live.current) {
        try {
          const latest = await api.mainRequests(taskId);
          if (live.current) setItems(latest);
        } catch {
          /* periodic refresh retries */
        }
        if (live.current) setSending(false);
      }
      inFlight.current = false;
    }
  }
  if (!items.length) return null;
  return (
    <>
      <button
        type="button"
        className="flex w-full shrink-0 items-center gap-2.5 border-b bg-primary/5 px-4 py-2.5 text-left transition-colors hover:bg-primary/10"
        onClick={() => {
          setOpen(true);
          setHistory(pending.length === 0);
        }}
      >
        <div className="rounded-md border border-primary/15 bg-background p-1.5 text-primary">
          <MessageSquare className="size-3.5" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex items-center gap-2 text-xs font-medium">
            协作请示{" "}
            {pending.length > 0 && (
              <Badge variant="secondary" className="h-4 px-1.5 text-[10px]">
                {pending.length} 条进行中
              </Badge>
            )}
          </div>
          <div className="mt-0.5 truncate text-[11px] text-muted-foreground">
            {processing
              ? "主 Agent 正在处理 · Worker 继续执行"
              : queued
                ? "已进入处理队列 · Worker 继续执行"
                : pending.length
                  ? "已有答复 · 等待 Worker 确认送达"
                  : "查看已回复和已失效的请示"}
          </div>
        </div>
        <span className="shrink-0 text-xs text-primary">查看</span>
        <ArrowUpRight className="size-3.5 text-primary" />
      </button>
      <Sheet open={open} onOpenChange={setOpen}>
        <SheetContent className="gap-0 data-[side=right]:w-full data-[side=right]:sm:max-w-lg">
          <SheetHeader className="shrink-0 border-b pb-4 pr-12">
            <SheetTitle className="flex items-center gap-2">
              <MessageSquare className="size-4 text-primary" />
              协作请示
            </SheetTitle>
            <SheetDescription>Worker 提问后继续执行。主 Agent 查证、协调后，答复会送回原会话。</SheetDescription>
          </SheetHeader>
          <div className="flex shrink-0 gap-1 border-b px-4 py-2" role="group" aria-label="请示筛选">
            <Button
              size="sm"
              variant={history ? "ghost" : "secondary"}
              aria-pressed={!history}
              onClick={() => setHistory(false)}
            >
              进行中 <span className="ml-1 tabular-nums">{pending.length}</span>
            </Button>
            <Button
              size="sm"
              variant={history ? "secondary" : "ghost"}
              aria-pressed={history}
              onClick={() => setHistory(true)}
            >
              已处理 <span className="ml-1 tabular-nums">{archived.length}</span>
            </Button>
          </div>
          <div className="min-h-0 flex-1 space-y-3 overflow-y-auto p-4">
            {!visible.length && (
              <div className="flex flex-col items-center gap-2 py-12 text-center text-sm text-muted-foreground">
                <CheckCheck className="size-7 opacity-50" />
                <p>{history ? "还没有历史请示" : "当前没有待处理的请示"}</p>
                <p className="text-xs">提问、查证和回复会保留在主会话中。</p>
              </div>
            )}
            {visible.map((r) => {
              const view = stateView[r.state];
              const Icon = view.icon;
              const show = expanded === r.id;
              return (
                <article key={r.id} className="overflow-hidden rounded-xl border bg-card shadow-xs">
                  <div className="p-3">
                    <div className="mb-2 flex flex-wrap items-center justify-between gap-2 text-[11px]">
                      <span className={`inline-flex items-center gap-1.5 ${view.tone}`}>
                        <Icon className={`size-3.5 ${r.state === "processing" ? "animate-spin" : ""}`} />
                        {view.label}
                      </span>
                      <span className="text-muted-foreground">
                        请示 #{r.id} ·{" "}
                        {new Date(r.created_at).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" })}
                      </span>
                    </div>
                    <p
                      className={`whitespace-pre-wrap break-words text-sm leading-relaxed ${show ? "" : "line-clamp-3"}`}
                    >
                      {r.question}
                    </p>
                    <div className="mt-2 flex items-center justify-between gap-2">
                      <button
                        type="button"
                        className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
                        onClick={() => {
                          setOpen(false);
                          onOpenWorker(r.intent_id);
                        }}
                      >
                        Worker #{r.intent_id}
                        <ArrowUpRight className="size-3" />
                      </button>
                      <Button
                        size="sm"
                        variant="ghost"
                        className="h-6 px-1.5 text-xs"
                        aria-expanded={show}
                        onClick={() => setExpanded(show ? null : r.id)}
                      >
                        {show ? "收起" : "查看详情"}
                        <ChevronDown className={`ml-1 size-3 transition-transform ${show ? "rotate-180" : ""}`} />
                      </Button>
                    </div>
                  </div>
                  {show && (
                    <div className="space-y-3 border-t bg-muted/25 p-3">
                      <p className="text-xs text-muted-foreground">{view.hint}</p>
                      {r.context && (
                        <div>
                          <p className="mb-1 text-[11px] font-medium text-muted-foreground">Worker 提供的上下文</p>
                          <div className="min-w-0 break-words text-xs">
                            <Markdown text={r.context} />
                          </div>
                        </div>
                      )}
                      {r.answer && (
                        <div className="rounded-lg border bg-background p-2.5">
                          <p className="mb-1.5 text-[11px] font-medium text-primary">{sourceLabel(r)}</p>
                          <Markdown text={r.answer} />
                        </div>
                      )}
                      {r.last_error && r.state !== "cancelled" && (
                        <details className="text-xs text-muted-foreground">
                          <summary className="cursor-pointer">处理记录 · 已尝试 {r.attempts} 次</summary>
                          <p className="mt-1 whitespace-pre-wrap break-words">{r.last_error}</p>
                        </details>
                      )}
                    </div>
                  )}
                  {canReply(r) && replyId !== r.id && (
                    <div className="border-t px-3 py-2">
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={sending}
                        className="h-7 gap-1.5 text-xs"
                        onClick={() => {
                          setReplyId(r.id);
                          setAnswer(drafts.current[r.id] ?? "");
                        }}
                      >
                        <UserRound className="size-3.5" />
                        人工接答
                      </Button>
                    </div>
                  )}
                </article>
              );
            })}
          </div>
          {replyTarget && (
            <div className="shrink-0 space-y-2 border-t bg-background p-4">
              <div className="flex items-center justify-between text-xs">
                <span className="font-medium">人工回复请示 #{replyTarget.id}</span>
                <Button
                  variant="ghost"
                  size="sm"
                  disabled={sending}
                  onClick={() => {
                    setReplyId(null);
                    setAnswer("");
                  }}
                >
                  关闭
                </Button>
              </div>
              {!canReply(replyTarget) && (
                <p className="text-xs text-amber-600 dark:text-amber-400">
                  这条请示已被处理或失效。草稿仍在，不能覆盖已有答复。
                </p>
              )}
              <label className="sr-only" htmlFor={`main-request-reply-${taskId}`}>
                给 Worker 的人工回复
              </label>
              <Textarea
                id={`main-request-reply-${taskId}`}
                value={answer}
                onChange={(e) => {
                  setAnswer(e.target.value);
                  drafts.current[replyTarget.id] = e.target.value;
                }}
                placeholder="写下判断、依据或下一步建议…"
                maxLength={8000}
                disabled={sending}
                className="min-h-24 max-h-48"
              />
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] text-muted-foreground">将明确记录为人工回复</span>
                <Button
                  size="sm"
                  disabled={sending || !answer.trim() || !canReply(replyTarget)}
                  onClick={() => void reply(replyTarget.id)}
                >
                  {sending ? (
                    <Loader2 className="mr-1.5 size-3.5 animate-spin" />
                  ) : (
                    <Send className="mr-1.5 size-3.5" />
                  )}
                  发送回复
                </Button>
              </div>
            </div>
          )}
        </SheetContent>
      </Sheet>
    </>
  );
}
