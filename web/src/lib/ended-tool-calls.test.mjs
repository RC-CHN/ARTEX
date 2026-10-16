import { endedToolCalls } from "./ended-tool-calls.ts";
import assert from "node:assert/strict";
import test from "node:test";

const step = (seq, kind, extra = {}) => ({ seq, kind, worker: "mainagent", main_seg: 0, ...extra });

test("a cancelled historical reply stops displaying as running", () => {
  assert.deepEqual([...endedToolCalls([step(1, "tool_use"), step(2, "result", { is_error: true })])], [1]);
});

test("normal consultation settlement also closes outstanding calls", () => {
  assert.deepEqual([...endedToolCalls([step(1, "tool_use"), step(2, "result", { is_error: false })])], [1]);
});

test("later turns and other worker, intent or main-session lanes stay live", () => {
  const steps = [
    step(1, "tool_use"),
    step(2, "tool_use", { worker: "worker", intent_id: "10" }),
    step(3, "tool_use", { worker: "worker", intent_id: "11" }),
    step(4, "tool_use", { main_seg: 1 }),
    step(5, "result", { worker: "worker", intent_id: "10" }),
    step(6, "result"),
    step(7, "tool_use"),
  ];
  assert.deepEqual([...endedToolCalls(steps)], [1, 2]);
  assert.deepEqual([...endedToolCalls([...steps].reverse())].sort(), [1, 2]);
});

test("tool results and text do not end a model turn", () => {
  assert.equal(endedToolCalls([step(1, "tool_use"), step(2, "tool_result"), step(3, "text")]).size, 0);
});

test("legacy main-segment omission matches segment zero", () => {
  assert.deepEqual([...endedToolCalls([step(1, "tool_use", { main_seg: undefined }), step(2, "result")])], [1]);
});
