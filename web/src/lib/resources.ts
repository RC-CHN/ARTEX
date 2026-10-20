import { http } from "@/lib/api";

export type Resource = {
  id: number;
  name: string;
  kind: string;
  description: string;
  tags: string[];
  access_mode: "global" | "restricted";
  enabled: boolean;
  revision: number;
  creator_task_id: number;
  updated_at: string;
  deleted_at?: string;
};
export type ResourceReference = { task_id: number; version_id?: number; deployment_id?: number; state: string; blocking: boolean };
export type ResourceFile = { path: string; sha256: string; size: number; executable: boolean };
export type ResourceEndpoint = { name: string; url: string; has_credential: boolean };
export type ResourceActor = { task_id: number; thread_id: string; runner_id: string };
export type ResourceManifest = {
  files: ResourceFile[];
  endpoints: ResourceEndpoint[];
  os: string;
  arch: string;
  size: number;
};
export type ResourceVersion = ResourceManifest & {
  id: number;
  resource_id: number;
  version: number;
  note: string;
  validation: string;
  actor: ResourceActor;
  created_at: string;
};
export type ResourceDraft = ResourceManifest & {
  id: number;
  resource_id: number;
  base_version_id: number;
  revision: number;
  state: string;
  published_version_id: number;
  actor: ResourceActor;
  updated_at: string;
};
export type ResourceEvent = {
  id: number;
  version_id: number;
  kind: string;
  actor: ResourceActor;
  detail: string;
  created_at: string;
};
export const resourceAPI = {
  list: (q = "", kind = "", before = 0) =>
    http<Resource[]>(`/resources?${new URLSearchParams({ q, kind, before: String(before) })}`),
  get: (id: number) => http<Resource>(`/resources/${id}`, { cache: "no-store" }),
  references: (id: number) => http<ResourceReference[]>(`/resources/${id}/references`, { cache: "no-store" }),
  delete: (resource: Resource, reason: string, request_id: string) =>
    http<{resource: Resource}>(`/resources/${resource.id}`, { method: "DELETE", body: JSON.stringify({expected_revision: resource.revision, reason, request_id}) }),
  save: (resource: Partial<Resource>) =>
    http<Resource>(resource.id ? `/resources/${resource.id}` : "/resources", {
      method: resource.id ? "PUT" : "POST",
      body: JSON.stringify(resource),
    }),
  versions: (id: number) => http<ResourceVersion[]>(`/resources/${id}/versions`),
  drafts: (id: number) => http<ResourceDraft[]>(`/resources/${id}/drafts`),
  events: (id: number, before = 0) => http<ResourceEvent[]>(`/resources/${id}/events?before=${before}`),
  createDraft: (id: number, base_version_id: number) =>
    http<ResourceDraft>(`/resources/${id}/drafts`, { method: "POST", body: JSON.stringify({ base_version_id }) }),
  draftDocument: (id: number) =>
    http<{ content: string; revision: number }>(`/resource-drafts/${id}/document`, { cache: "no-store" }),
  versionDocument: (resource: number, version: number) =>
    http<{ content: string }>(`/resources/${resource}/versions/${version}/document`),
  saveDocument: (draft: ResourceDraft, content: string) =>
    http<ResourceDraft>(`/resource-drafts/${draft.id}/document`, {
      method: "PUT",
      body: JSON.stringify({ revision: draft.revision, content }),
    }),
  configure: (draft: ResourceDraft, credentials: Record<string, string>) =>
    http<ResourceDraft>(`/resource-drafts/${draft.id}/configuration`, {
      method: "PUT",
      body: JSON.stringify({
        revision: draft.revision,
        endpoints: draft.endpoints,
        os: draft.os,
        arch: draft.arch,
        credentials,
      }),
    }),
  upload: (draft: ResourceDraft, file: File, path: string, executable: boolean) =>
    http<ResourceDraft>(
      `/resource-drafts/${draft.id}/files?${new URLSearchParams({ revision: String(draft.revision), path, executable: String(executable) })}`,
      { method: "POST", body: file, headers: { "Content-Type": "application/octet-stream" } },
    ),
  removeFile: (draft: ResourceDraft, path: string) =>
    http<ResourceDraft>(
      `/resource-drafts/${draft.id}/files?${new URLSearchParams({ revision: String(draft.revision), path })}`,
      { method: "DELETE" },
    ),
  discard: (draft: ResourceDraft) =>
    http(`/resource-drafts/${draft.id}?revision=${draft.revision}`, { method: "DELETE" }),
  publish: (draft: ResourceDraft, note: string, validation: string) =>
    http<ResourceVersion>(`/resource-drafts/${draft.id}/publish`, {
      method: "POST",
      body: JSON.stringify({ revision: draft.revision, note, validation }),
    }),
  async pin(id: number, task_id: number, version_id: number) {
    const bindings = await http<ResourceVersion[]>(`/tasks/${task_id}/resources`, { cache: "no-store" });
    const expected_version_id = bindings.find((v) => v.resource_id === id)?.id ?? 0;
    return http<ResourceVersion>(`/resources/${id}/tasks`, {
      method: "POST",
      body: JSON.stringify({ task_id, version_id, expected_version_id }),
    });
  },
  async download(resource: number, version: number, path: string) {
    const response = await fetch(
      `/api/resources/${resource}/versions/${version}/download?${new URLSearchParams({ path })}`,
      { headers: { Authorization: `Bearer ${localStorage.getItem("artex_token") ?? ""}` } },
    );
    if (!response.ok) throw new Error(`下载失败 (${response.status})`);
    const url = URL.createObjectURL(await response.blob());
    const link = document.createElement("a");
    link.href = url;
    link.download = path.split("/").at(-1) ?? "resource";
    link.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  },
};
