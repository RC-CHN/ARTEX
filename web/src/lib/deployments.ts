import { http } from "@/lib/api";

export const deploymentStates = {
  active: "待撤除",
  removing: "撤除中",
  removed: "已验证撤除",
  retained: "有依据保留",
  unknown: "结果未知",
  failed: "撤除失败",
} as const;
export type Deployment = {
  id: number;
  task_id: number;
  resource_id?: number;
  revision: number;
  name: string;
  kind: string;
  location: string;
  identifier: string;
  cleanup: string;
  state: keyof typeof deploymentStates;
  verification: string;
  note: string;
  updated_at: string;
};
export type Handoff = { checks: { code: string; count: number; next_action: string }[] };
export const deploymentAPI = {
  list: (task: string, before = 0) =>
    http<Deployment[]>(`/tasks/${task}/deployments?before=${before}`, { cache: "no-store" }),
  save: (task: string, entry: Partial<Deployment> & { expected_revision?: number; request_id: string }) => {
    const {
      id,
      resource_id,
      name,
      kind,
      location,
      identifier,
      cleanup,
      state,
      verification,
      note,
      expected_revision,
      request_id,
    } = entry;
    return http<Deployment>(`/tasks/${task}/deployments`, {
      method: "POST",
      body: JSON.stringify({
        id,
        resource_id,
        name,
        kind,
        location,
        identifier,
        cleanup,
        state,
        verification,
        note,
        expected_revision,
        request_id,
      }),
    });
  },
  handoff: (task: string) => http<Handoff>(`/tasks/${task}/handoff`, { cache: "no-store" }),
};
