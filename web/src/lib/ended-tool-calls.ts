import type { Activity } from "./types";

const lane = (step: Activity) => JSON.stringify([step.worker, step.intent_id ?? "", step.main_seg ?? 0]);

// A terminal event ends only its own execution lane. Older records can lack a
// tool result after cancellation; do not keep rendering those calls as running.
// This does not infer whether the tool ran or succeeded.
export function endedToolCalls(steps: Activity[]): Set<number> {
  const lastTerminal = new Map<string, number>();
  for (const step of steps) {
    if (step.kind === "result") {
      const key = lane(step);
      lastTerminal.set(key, Math.max(lastTerminal.get(key) ?? 0, step.seq));
    }
  }
  return new Set(
    steps
      .filter((step) => step.kind === "tool_use" && step.seq < (lastTerminal.get(lane(step)) ?? 0))
      .map((step) => step.seq),
  );
}
