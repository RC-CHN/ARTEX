"use client";
import * as React from "react";
import { Check, Loader2, MessageSquareText, Save } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { useUnsavedChanges } from "@/hooks/use-unsaved-changes";
import { api } from "@/lib/api";
import { runnerError } from "@/lib/runners";

export function EnvironmentPromptCard() {
  const [value, setValue] = React.useState("");
  const [saved, setSaved] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [status, setStatus] = React.useState("");
  const dirty = saved !== null && value !== saved;
  useUnsavedChanges(dirty);
  const load = React.useCallback(async () => {
    setError("");
    try {
      const s = await api.settings();
      setValue(s.environment_prompt ?? "");
      setSaved(s.environment_prompt ?? "");
    } catch (error) {
      setError(runnerError(error));
    }
  }, []);
  React.useEffect(() => {
    void load();
  }, [load]);
  async function save() {
    setBusy(true);
    setError("");
    setStatus("");
    try {
      const result = await api.setSettings({ environment_prompt: value });
      setSaved(result.environment_prompt ?? "");
      setValue(result.environment_prompt ?? "");
      setStatus("已保存，新启动的 Agent 执行会自动读取");
      toast.success("全局环境提示词已保存");
    } catch (error) {
      setError(runnerError(error));
      toast.error("保存失败，请重试");
    } finally {
      setBusy(false);
    }
  }
  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <MessageSquareText className="size-4" />
          额外环境提示词
        </CardTitle>
        <CardDescription>向所有 Agent 介绍可用的代理、工具服务和共享资源；本机与 Runner 执行都会收到。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <Label htmlFor="global-environment-prompt">全局环境说明</Label>
        <Textarea
          id="global-environment-prompt"
          className="min-h-40 leading-relaxed"
          maxLength={16000}
          disabled={busy || saved === null}
          value={value}
          placeholder={
            "默认直连。需要访问外部资源时，可显式使用 HTTP 代理 http://proxy.example.org:8080。\n外部 Runner 需先确认代理可达；不要求设置全局代理变量。"
          }
          onChange={(event) => {
            setValue(event.target.value);
            setStatus("");
          }}
        />
        <p className="text-muted-foreground text-xs leading-relaxed">
          仅注入提示词，不设置 HTTP_PROXY / ALL_PROXY。节点特有地址请在 Runner
          的“环境说明”中填写；这里的地址和说明会发送给模型。
        </p>
        {error && (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        )}
        {saved === null ? (
          <Button variant="outline" onClick={load}>
            {error ? "重新加载" : "正在加载…"}
          </Button>
        ) : (
          <div className="flex flex-wrap items-center gap-3">
            <Button onClick={save} disabled={busy || !dirty}>
              {busy ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}保存环境提示词
            </Button>
            {dirty ? (
              <span className="text-amber-600 text-xs">有未保存的修改</span>
            ) : (
              status && (
                <span role="status" className="flex items-center gap-1 text-emerald-600 text-xs">
                  <Check className="size-3" />
                  {status}
                </span>
              )
            )}
          </div>
        )}
      </CardContent>
    </Card>
  );
}
