export type Mode = "local" | "remote";
export type RunnerEnvironment = {
  file_mode?: "snapshot" | "shared";
  warm_idle?: number;
  image: string;
  environment_prompt?: string;
  direct_network?: boolean;
  proxy_url?: string;
  resources: { cpu_millis: number; memory_mib: number; pids: number; [key: string]: unknown };
  [key: string]: unknown;
};
export type RunnerConfig = {
  enabled: boolean;
  default_mode: Mode;
  agent_modes: Record<string, Mode>;
  default_environment: string;
  environments: Record<string, RunnerEnvironment>;
  mcp: Record<string, unknown>;
  acquire_timeout_seconds: number;
};
export type RunnerNode = {
  pool_status?: {
    idle: number;
    preparing: number;
    pods: number;
    cpu_millis: number;
    memory_mib: number;
    error?: string;
  };
  id: string;
  name: string;
  enabled: boolean;
  online: boolean;
  last_seen: string;
  environment_prompt?: string;
  active_sessions: number;
  concurrency_limit: number;
  effective_slots: number;
  cpu_limit_millis: number;
  memory_limit_mib: number;
  effective_cpu_millis: number;
  effective_memory_mib: number;
  reserved_cpu_millis: number;
  reserved_memory_mib: number;
  reserved_gpus?: string[];
  capabilities: {
    executor?: string;
    slots: number;
    cpu_millis?: number;
    memory_mib?: number;
    environments?: string[];
    labels?: Record<string, string>;
    gpus?: { id: string; model?: string; memory_mib?: number }[];
  };
};
export type RunnerSnapshot = { config: RunnerConfig; nodes: RunnerNode[]; experimental: boolean; error?: string };
export type NodeLimits = { slots: number; cpu_millis: number; memory_mib: number };
export const runnerError = (error: unknown) => (error instanceof Error ? error.message : String(error));
export const pretty = (value: unknown) => JSON.stringify(value ?? {}, null, 2);
export const isPool = (node: RunnerNode) => node.capabilities.executor === "kubernetes";
export const formatMemory = (mib: number) => (mib >= 1024 ? `${Number((mib / 1024).toFixed(1))} GiB` : `${mib} MiB`);
export function parseObject<T extends object>(raw: string, label: string): T {
  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    throw new Error(`${label}的 JSON 格式不正确`);
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${label}必须是 JSON 对象`);
  return value as T;
}
export async function copyRunnerText(text: string) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text);
      return;
    } catch {
      // Browser policy can deny clipboard access even on HTTPS.
    }
  }
  // The private cluster can be served over HTTP, where Clipboard API is absent.
  const input = document.createElement("textarea");
  input.value = text;
  input.style.position = "fixed";
  input.style.opacity = "0";
  const previous = document.activeElement;
  // A modal focus trap would immediately move focus away from a body-level
  // textarea. Keep the temporary selection inside the current dialog.
  (previous?.closest('[role="dialog"]') ?? document.body).appendChild(input);
  input.focus({ preventScroll: true });
  input.select();
  try {
    if (!document.execCommand("copy")) throw new Error("请手动选中文字复制");
  } finally {
    input.remove();
    if (previous instanceof HTMLElement) previous.focus({ preventScroll: true });
  }
}
