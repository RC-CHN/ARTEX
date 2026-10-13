"use client";
import * as React from "react";
import { Box, Check, Copy, Eye, EyeOff, KeyRound, Loader2, Server } from "lucide-react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { http } from "@/lib/api";
import { copyRunnerText, pretty, runnerError, type RunnerConfig } from "@/lib/runners";

export function EnrollmentDialog({ config, close }: { config: RunnerConfig; close: () => void }) {
  const [kind, setKind] = React.useState<"external" | "cluster">("external");
  const [name, setName] = React.useState("external-runner");
  const [url, setURL] = React.useState(() => window.location.origin);
  const [insecure, setInsecure] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState("");
  const [token, setToken] = React.useState<{ token: string; expires_at: string } | null>(null);
  const [visible, setVisible] = React.useState(false);
  const [now, setNow] = React.useState(Date.now());
  React.useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, []);
  let validURL = false;
  try {
    const parsed = new URL(url);
    validURL =
      !parsed.username &&
      !parsed.password &&
      !parsed.search &&
      !parsed.hash &&
      (parsed.protocol === "https:" || (insecure && parsed.protocol === "http:"));
  } catch {
    /* editable */
  }
  const usable = config.enabled && validURL && name.trim() !== "";
  const remaining = token ? Math.max(0, Math.ceil((Date.parse(token.expires_at) - now) / 1000)) : 0;
  const image = config.environments[config.default_environment]?.image ?? "";
  const runnerConfig = {
    server_url: url.replace(/\/$/, ""),
    ...(insecure && url.startsWith("http:") ? { allow_insecure_http: true } : {}),
    name: name.trim(),
    executor: kind === "cluster" ? "kubernetes" : "docker",
    data_dir: kind === "cluster" ? "/var/lib/artex-runner/identity" : "/var/lib/artex-runner",
    ...(kind === "cluster"
      ? {
          workspace_dir: "/var/lib/artex-workspace/sessions",
          kubernetes: {
            namespace: "artex-runners",
            workspace_pvc: "artex-runner-workspace",
            workspace_sub_path: "sessions",
            service_account: "artex-sandbox",
            image_pull_secrets: ["registry-pull"],
          },
        }
      : { docker_host_data_dir: "/srv/artex-runner", docker_network: "artex-external-runners" }),
    capabilities: {
      slots: 4,
      cpu_millis: 4000,
      memory_mib: 8192,
      environments: [config.default_environment],
      labels: { network: kind },
    },
  };
  const compose = {
    services: {
      runner: {
        image,
        entrypoint: ["/usr/local/bin/artex-runner"],
        command: ["-config", "/etc/artex-runner/runner.json", "-token-file", "/run/enrollment/token"],
        restart: "unless-stopped",
        init: true,
        read_only: true,
        tmpfs: ["/tmp:size=64m"],
        environment: { HOME: "/tmp" },
        volumes: [
          "./runner.json:/etc/artex-runner/runner.json:ro",
          "./enrollment.token:/run/enrollment/token:ro",
          "/srv/artex-runner:/var/lib/artex-runner",
          "/var/run/docker.sock:/var/run/docker.sock",
        ],
        networks: ["runners"],
      },
    },
    networks: { runners: { name: "artex-external-runners" } },
  };
  async function copy(text: string, label: string) {
    try {
      await copyRunnerText(text);
      toast.success(`${label}已复制`);
    } catch (err) {
      setError(`复制失败：${runnerError(err)}`);
      toast.error("复制失败，请手动选择内容");
    }
  }
  async function enroll() {
    setBusy(true);
    setError("");
    try {
      setToken(await http("/runners/registration-token", { method: "POST" }));
      setVisible(false);
      setNow(Date.now());
      toast.success("一次性注册令牌已生成，有效期 10 分钟");
    } catch (err) {
      setError(runnerError(err));
      toast.error("生成令牌失败");
    } finally {
      setBusy(false);
    }
  }
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) close();
      }}
    >
      <DialogContent className="max-h-[90vh] overflow-y-auto sm:max-w-2xl">
        <DialogHeader>
          <DialogTitle>接入 Runner</DialogTitle>
          <DialogDescription>已有的集群执行池与外部机器可以同时在线，由调度器按任务需求分配。</DialogDescription>
        </DialogHeader>
        <div className="grid grid-cols-2 gap-3">
          {(["external", "cluster"] as const).map((value) => (
            <button
              key={value}
              type="button"
              aria-pressed={kind === value}
              onClick={() => {
                setKind(value);
                setName(value === "cluster" ? "kubernetes-pool" : "external-runner");
              }}
              className={`space-y-2 rounded-xl border p-4 text-left transition-colors ${kind === value ? "border-primary bg-primary/5 ring-1 ring-primary/20" : "hover:bg-muted/50"}`}
            >
              {value === "cluster" ? (
                <Box className="size-5 text-blue-600" />
              ) : (
                <Server className="size-5 text-violet-600" />
              )}
              <span className="block font-medium text-sm">
                {value === "cluster" ? "K3s / Kubernetes 执行池" : "外部主机 / GPU 机器"}
              </span>
              <span className="block text-muted-foreground text-xs">
                {value === "cluster" ? "一个管理器，按需创建独立 Pod" : "注册独立主机，每次执行使用 Docker 容器"}
              </span>
            </button>
          ))}
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          <div className="space-y-2">
            <Label htmlFor="enroll-name">Runner 名称</Label>
            <Input id="enroll-name" value={name} onChange={(e) => setName(e.target.value)} />
          </div>
          <div className="space-y-2">
            <Label htmlFor="enroll-url">此 Runner 可访问的 ARTEX 地址</Label>
            <Input
              id="enroll-url"
              value={url}
              onChange={(e) => setURL(e.target.value)}
              placeholder="https://artex.example.org"
            />
          </div>
        </div>
        <p className="text-muted-foreground text-xs">
          集群内可以使用 Service 地址；外部主机请填写它能访问的 HTTPS 地址。页面当前地址不一定能从 Runner 访问。
        </p>
        {url.startsWith("http:") && (
          <div className="flex items-center justify-between gap-3 rounded-lg border p-3">
            <Label htmlFor="enroll-insecure">明确允许可信内网中的 HTTP 连接</Label>
            <Switch id="enroll-insecure" checked={insecure} onCheckedChange={setInsecure} />
          </div>
        )}
        {!config.enabled && (
          <p role="alert" className="text-amber-600 text-sm">
            请先在执行设置中启用并保存 Runner 功能。
          </p>
        )}
        <section className="space-y-3 rounded-xl bg-muted/40 p-4">
          <h3 className="font-medium text-sm">1. 保存节点配置</h3>
          <p className="text-muted-foreground text-xs">
            默认容量 4 个执行环境、4 核、8 GiB；按这台机器的实际容量修改。GPU 设备需在 capabilities.gpus 中显式登记。
          </p>
          <details>
            <summary className="cursor-pointer text-xs">预览 runner.json</summary>
            <pre className="mt-2 max-h-52 overflow-auto rounded-lg bg-background p-3 text-xs">
              {pretty(runnerConfig)}
            </pre>
          </details>
          {kind === "external" && (
            <details>
              <summary className="cursor-pointer text-xs">预览 compose.runner.json</summary>
              <pre className="mt-2 max-h-52 overflow-auto rounded-lg bg-background p-3 text-xs">{pretty(compose)}</pre>
            </details>
          )}
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              size="sm"
              disabled={!validURL || !name.trim()}
              onClick={() => copy(pretty(runnerConfig), "runner.json")}
            >
              <Copy className="size-3.5" />
              复制节点配置
            </Button>
            {kind === "external" && (
              <Button
                variant="outline"
                size="sm"
                disabled={!usable || !image}
                onClick={() => copy(pretty(compose), "compose.runner.json")}
              >
                <Copy className="size-3.5" />
                复制 Compose 配置
              </Button>
            )}
          </div>
        </section>
        <section className="space-y-3 rounded-xl bg-muted/40 p-4">
          <h3 className="font-medium text-sm">2. 生成一次性注册令牌</h3>
          <Button variant="outline" disabled={!usable || busy} onClick={enroll}>
            {busy ? <Loader2 className="size-4 animate-spin" /> : <KeyRound className="size-4" />}
            {token ? "重新生成令牌" : "生成注册令牌"}
          </Button>
          {token && (
            <div className="space-y-2">
              <div className="flex gap-2">
                <Input
                  aria-label="注册令牌"
                  type={visible ? "text" : "password"}
                  readOnly
                  value={token.token}
                  className="font-mono text-xs"
                />
                <Button
                  variant="outline"
                  aria-label={visible ? "遮蔽令牌" : "显示令牌"}
                  onClick={() => setVisible(!visible)}
                >
                  {visible ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
                </Button>
                <Button
                  variant="outline"
                  aria-label="复制注册令牌"
                  disabled={!remaining}
                  onClick={() => copy(token.token, "令牌")}
                >
                  <Copy className="size-4" />
                </Button>
              </div>
              <p role="status" className={`text-xs ${remaining ? "text-muted-foreground" : "text-destructive"}`}>
                {remaining
                  ? `剩余 ${Math.floor(remaining / 60)} 分 ${remaining % 60} 秒，仅可注册一次。`
                  : "令牌已过期，请重新生成。"}
                关闭窗口后不会保留令牌。
              </p>
            </div>
          )}
        </section>
        <section className="space-y-2 rounded-xl border p-4">
          <h3 className="font-medium text-sm">3. 启动并等待节点上线</h3>
          {kind === "external" ? (
            <>
              <p className="text-muted-foreground text-xs leading-relaxed">
                将两个配置分别保存为 runner.json、compose.runner.json；将令牌写入权限为 0600 的 enrollment.token，创建
                /srv/artex-runner 持久目录，然后启动。管理器需要 Docker socket，任务容器不会挂载它。私有镜像仓库需配置
                Docker 登录。
              </p>
              <code className="block overflow-x-auto rounded-lg bg-muted p-3 text-xs">
                docker compose -f compose.runner.json up -d
              </code>
              <p className="text-muted-foreground text-xs">
                注册后可清空令牌文件，保留空文件供挂载；节点身份保存在持久目录。
              </p>
            </>
          ) : (
            <p className="text-muted-foreground text-xs leading-relaxed">
              使用仓库 deploy/runner/k8s 模板，将节点配置放入 ConfigMap，配置共享 PVC、镜像拉取凭据和
              ResourceQuota。注册令牌通过 artex-runner-enrollment Secret 的 token
              字段挂载。执行池只需一个管理器，不需要按并发数部署管理器副本。
            </p>
          )}
          <a
            className="inline-flex items-center gap-1 text-primary text-xs underline underline-offset-4"
            href="https://github.com/RC-CHN/ARTEX/blob/main/docs/experimental-runners.md"
            target="_blank"
            rel="noreferrer"
          >
            完整部署说明与 GPU 配置
          </a>
          <p className="flex items-center gap-1 text-muted-foreground text-xs">
            <Check className="size-3" />
            注册成功后，节点会自动出现在资源列表中。
          </p>
        </section>
        {error && (
          <p role="alert" className="text-destructive text-sm">
            {error}
          </p>
        )}
      </DialogContent>
    </Dialog>
  );
}
