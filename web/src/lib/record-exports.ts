import { http } from "@/lib/api";
export type RecordExport = {
  id: number;
  task_ids: number[];
  redacted: boolean;
  state: string;
  phase: string;
  error: string;
  files: number;
  bytes: number;
  sha256?: string;
  cutoff_at?: string;
  expires_at: string;
  attempt: number;
};
export const recordExportAPI = {
  list: (task: string, before = 0) =>
    http<RecordExport[]>(`/task-record-exports?task_id=${task}&before=${before}`, { cache: "no-store" }),
  create: (task_ids: number[], redacted: boolean, request_id: string) =>
    http<RecordExport>("/task-record-exports", {
      method: "POST",
      body: JSON.stringify({ task_ids, redacted, request_id }),
    }),
  action: (id: number, action: string) =>
    http<RecordExport>(`/task-record-exports/${id}${action === "delete" ? "" : `/${action}`}`, {
      method: action === "delete" ? "DELETE" : "POST",
    }),
};
