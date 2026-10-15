"use client";
import * as React from "react";
import { MainRequests } from "@/components/main-requests";
import { Transcript } from "@/components/transcript";
import type { Activity } from "@/lib/types";

const activity = [
  {
    seq: 1,
    worker: "mainagent",
    kind: "consultation",
    summary: "发现两个环境的接口返回不一致，是否先验证配置差异？",
    ts: new Date().toISOString(),
    metadata: { request_id: 104, intent_id: 32, source: "worker" },
  },
  {
    seq: 2,
    worker: "mainagent",
    kind: "round",
    summary: "自动处理 Worker #32 的请示 #104",
    ts: new Date().toISOString(),
  },
  {
    seq: 3,
    worker: "mainagent",
    kind: "text",
    summary: "我会先核对已有记录，并向负责配置检查的 Worker 确认版本；当前 Worker 可以继续整理已获得的证据。",
    ts: new Date().toISOString(),
  },
  {
    seq: 4,
    worker: "mainagent",
    kind: "consultation_reply",
    summary: "先比较接口版本和配置差异。Worker #28 已在核对配置，请复用其记录，暂时继续整理证据。",
    ts: new Date().toISOString(),
    metadata: { request_id: 103, intent_id: 28, source: "mainagent" },
  },
] as Activity[];

export default function Preview() {
  const [worker, setWorker] = React.useState<number | null>(null);
  return (
    <main className="mx-auto flex h-dvh max-w-4xl flex-col border-x bg-background">
      <header className="border-b px-5 py-4">
        <h1 className="font-semibold">主会话</h1>
        <p className="mt-1 text-xs text-muted-foreground">本地合成数据 · 协作请示交互验证</p>
      </header>
      <MainRequests taskId="fixture" onOpenWorker={setWorker} />
      <div className="min-h-0 flex-1 overflow-y-auto p-5">
        <Transcript
          activity={activity}
          chat
          fetchDetail={async (id) => activity.find((a) => a.seq === id)?.summary ?? ""}
        />
        {worker && <p role="status">已跳转 Worker #{worker}</p>}
      </div>
      <footer className="border-t p-4 text-sm text-muted-foreground">查看主 Agent 的查证和协调过程</footer>
    </main>
  );
}
