import type { RunnerAdmission } from "@/lib/runners";

const stages: Record<string, string> = {
  pod_capacity: "等待执行池容量",
  quota: "等待 Kubernetes 配额",
  pod_create: "创建 Pod",
  pod_ready: "等待 Pod 就绪",
};
const resources: Record<string, string> = {
  "count/pods": "Pod 数",
  pods: "Pod 数",
  "limits.cpu": "CPU limit",
  "requests.cpu": "CPU request",
  "limits.memory": "内存 limit",
  "requests.memory": "内存 request",
  "limits.ephemeral-storage": "临时存储 limit",
  "requests.ephemeral-storage": "临时存储 request",
};
const clock = (value: string) => new Date(value).toLocaleTimeString();

export function AdmissionStatus({ value }: { value: RunnerAdmission }) {
  const quota = value.last_quota_rejection;
  const recovered = quota && value.last_create_at && Date.parse(value.last_create_at) > Date.parse(quota.at);
  const waiting = value.waiting ?? [];
  return (
    <div className="space-y-2 border-t pt-2" data-testid="runner-admission">
      {Object.entries(stages).map(([stage, label]) => {
        const items = waiting.filter((item) => item.stage === stage);
        if (!items.length) return null;
        const seconds = Math.max(
          0,
          ...items.map((item) => Math.floor((Date.parse(value.observed_at) - Date.parse(item.since)) / 1000) || 0),
        );
        return (
          <p key={stage} role="status">
            {label} {items.length} · 最长准备 {seconds} 秒 · 创建尝试{" "}
            {items.reduce((sum, item) => sum + item.attempts, 0)} 次
          </p>
        );
      })}
      {quota && (
        <details open={!recovered}>
          <summary className="cursor-pointer">
            命名空间配额拒绝 {value.quota_rejections} 次
            {recovered ? " · 随后已成功创建 Pod" : " · 容量不足时自动退避重试"}
          </summary>
          <p className="my-2 text-muted-foreground">
            最近拒绝 {clock(quota.at)} · {quota.name}。下表为当时快照，包含管理 Pod 等命名空间用量。
          </p>
          <div className="overflow-x-auto">
            <table className="w-full text-left tabular-nums">
              <thead>
                <tr>
                  <th>资源</th>
                  <th>本次申请</th>
                  <th>已用</th>
                  <th>上限</th>
                </tr>
              </thead>
              <tbody>
                {Object.keys(quota.hard).map((key) => (
                  <tr key={key}>
                    <td>{resources[key] ?? key}</td>
                    <td>{quota.requested[key] ?? "—"}</td>
                    <td>{quota.used[key] ?? "—"}</td>
                    <td>{quota.hard[key]}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </details>
      )}
      <p className="text-muted-foreground">
        容量状态采样于 {clock(value.observed_at)} · 拒绝次数自 Runner 管理器启动累计
      </p>
    </div>
  );
}
