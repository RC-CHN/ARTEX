import assert from "node:assert/strict";
import test from "node:test";
import { RefreshCoordinator, RequestCache } from "./api-cache.ts";

function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
}
async function flush() {
  for (let i = 0; i < 12; i++) await Promise.resolve();
}

class FakeClock {
  time = 0;
  next = 0;
  timers = new Map();
  now = () => this.time;
  setTimeout = (callback, delay) => {
    const id = ++this.next;
    this.timers.set(id, { at: this.time + delay, callback });
    return id;
  };
  clearTimeout = (id) => {
    this.timers.delete(id);
  };
  async advance(duration) {
    const target = this.time + duration;
    for (let guard = 0; guard < 10_000; guard++) {
      await flush();
      const next = [...this.timers].sort((a, b) => a[1].at - b[1].at)[0];
      if (!next || next[1].at > target) {
        this.time = target;
        await flush();
        return;
      }
      this.time = next[1].at;
      this.timers.delete(next[0]);
      next[1].callback();
    }
    throw new Error("Scheduler did not settle");
  }
}

test("identical GETs share one pending request and reuse only fresh results", async () => {
  const clock = new FakeClock();
  const cache = new RequestCache(clock.now);
  const result = deferred();
  let calls = 0;
  const load = () => {
    calls++;
    return result.promise;
  };
  const first = cache.read("user:/tasks?page=1", "/tasks?page=1", load);
  const second = cache.read("user:/tasks?page=1", "/tasks?page=1", load);
  assert.equal(first, second);
  result.resolve({ version: 1 });
  assert.deepEqual(await first, { version: 1 });
  await cache.read("user:/tasks?page=1", "/tasks?page=1", load);
  assert.equal(calls, 1);
  await clock.advance(5_001);
  await cache.read("user:/tasks?page=1", "/tasks?page=1", load);
  assert.equal(calls, 2);
});

test("query parameters and auth identities do not share cached data", async () => {
  const cache = new RequestCache();
  const reads = [
    ["alice:/tasks?page=1", "/tasks?page=1", "first"],
    ["alice:/tasks?page=2", "/tasks?page=2", "second"],
    ["bob:/tasks?page=1", "/tasks?page=1", "other user"],
  ];
  for (const [key, path, value] of reads) assert.equal(await cache.read(key, path, async () => value), value);
  for (const [key, path, value] of reads) assert.equal(await cache.read(key, path, async () => "wrong"), value);
});

test("mutation during a GET waits for one fresh follow-up without overlap or stale writes", async () => {
  const cache = new RequestCache();
  const old = deferred(),
    fresh = deferred();
  let calls = 0;
  const load = () => (++calls === 1 ? old.promise : fresh.promise);
  const first = cache.read("task", "/tasks/one", load);
  cache.invalidate(["/tasks"]);
  cache.invalidate(["/tasks"]);
  const afterMutation = cache.read("task", "/tasks/one", load);
  assert.equal(calls, 1, "invalidation must not overlap the existing request");
  old.resolve("obsolete");
  await flush();
  assert.equal(calls, 2);
  fresh.resolve("current");
  assert.deepEqual(await Promise.all([first, afterMutation]), ["current", "current"]);
  assert.equal(await cache.read("task", "/tasks/one", async () => "wrong"), "current");
});

test("errors are not cached, and invalidation does not match unrelated prefixes", async () => {
  const cache = new RequestCache();
  let calls = 0;
  const load = async () => {
    if (++calls === 1) throw new Error("offline");
    return "online";
  };
  await assert.rejects(cache.read("task", "/tasks", load), /offline/);
  assert.equal(await cache.read("task", "/tasks", load), "online");
  cache.invalidate(["/task"]);
  assert.equal(await cache.read("task", "/tasks", load), "online");
  assert.equal(calls, 2);
  cache.invalidate(["/tasks"]);
  await cache.read("task", "/tasks", load);
  assert.equal(calls, 3);
});

test("an invalidated failed request retries current data instead of surfacing an obsolete error", async () => {
  const cache = new RequestCache();
  const old = deferred();
  let calls = 0;
  const read = cache.read("task", "/tasks/one", () => (++calls === 1 ? old.promise : Promise.resolve("new")));
  cache.invalidate(["/tasks"]);
  old.reject(new Error("obsolete error"));
  assert.equal(await read, "new");
  assert.equal(calls, 2);
});

test("logout clears successful values and rejects old in-flight responses", async () => {
  const cache = new RequestCache();
  const old = deferred();
  const request = cache.read("alice", "/tasks", () => old.promise);
  const rejected = assert.rejects(request, /会话已失效/);
  cache.clear();
  assert.equal(await cache.read("bob", "/tasks", async () => "bob data"), "bob data");
  old.resolve("alice data");
  await rejected;
  assert.equal(await cache.read("alice", "/tasks", async () => "new login"), "new login");
});

test("cache evicts least recently used completed entries", async () => {
  const cache = new RequestCache(Date.now, 5_000, 2);
  await cache.read("one", "/one", async () => 1);
  await cache.read("two", "/two", async () => 2);
  await cache.read("one", "/one", async () => 9);
  await cache.read("three", "/three", async () => 3);
  assert.equal(await cache.read("one", "/one", async () => 9), 1);
  assert.equal(await cache.read("two", "/two", async () => 22), 22);
});

test("two mounted consumers make one request per fallback cycle", async () => {
  const clock = new FakeClock();
  const cache = new RequestCache(clock.now);
  const coordinator = new RefreshCoordinator(clock);
  let network = 0,
    commits = 0;
  const load = async () => {
    await cache.read("tasks", "/tasks", async () => ++network);
    commits++;
  };
  const stop1 = coordinator.subscribe(load, { keys: ["/tasks"] });
  const stop2 = coordinator.subscribe(load, { keys: ["/tasks"] });
  await clock.advance(0);
  assert.equal(network, 1);
  assert.equal(commits, 2);
  await clock.advance(29_999);
  assert.equal(network, 1);
  await clock.advance(1);
  assert.equal(network, 2);
  stop1();
  stop2();
  assert.equal(clock.timers.size, 0);
});

test("slow refreshes never overlap and event bursts schedule only one follow-up", async () => {
  const clock = new FakeClock();
  const coordinator = new RefreshCoordinator(clock);
  const first = deferred();
  let calls = 0;
  const stop = coordinator.subscribe(() => (++calls === 1 ? first.promise : Promise.resolve()), { keys: ["/tasks"] });
  await clock.advance(0);
  for (let i = 0; i < 50; i++) coordinator.invalidate(["/tasks/one"]);
  await clock.advance(90_000);
  assert.equal(calls, 1);
  first.resolve();
  await flush();
  await clock.advance(100);
  assert.equal(calls, 2);
  stop();
});

test("hidden views pause initial/fallback/event loads and refresh once on return", async () => {
  const clock = new FakeClock();
  const coordinator = new RefreshCoordinator(clock);
  let calls = 0;
  coordinator.setVisible(false);
  const stop = coordinator.subscribe(async () => {
    calls++;
  });
  coordinator.invalidate();
  await clock.advance(120_000);
  assert.equal(calls, 0);
  coordinator.setVisible(true);
  await clock.advance(0);
  assert.equal(calls, 1);
  coordinator.setVisible(false);
  await clock.advance(120_000);
  assert.equal(calls, 1);
  coordinator.setVisible(true);
  await clock.advance(0);
  assert.equal(calls, 2);
  stop();
});

test("failed refreshes retry, unrelated events stay quiet, and unsubscribe cancels queued work", async () => {
  const clock = new FakeClock();
  const coordinator = new RefreshCoordinator(clock);
  let calls = 0;
  const stop = coordinator.subscribe(
    async () => {
      calls++;
      if (calls === 1) throw new Error("offline");
    },
    { keys: ["/tasks"] },
  );
  await clock.advance(0);
  coordinator.invalidate(["/assets"]);
  await clock.advance(1_000);
  assert.equal(calls, 1);
  await clock.advance(29_000);
  assert.equal(calls, 2);
  coordinator.invalidate(["/tasks"]);
  stop();
  await clock.advance(60_000);
  assert.equal(calls, 2);
});

test("unmounting during a request cannot resurrect its timer or queued invalidation", async () => {
  const clock = new FakeClock();
  const coordinator = new RefreshCoordinator(clock);
  const pending = deferred();
  let calls = 0;
  const stop = coordinator.subscribe(() => {
    calls++;
    return pending.promise;
  });
  await clock.advance(0);
  coordinator.invalidate();
  stop();
  pending.resolve();
  await clock.advance(120_000);
  assert.equal(calls, 1);
  assert.equal(clock.timers.size, 0);
});

test("default timer adapter preserves the native browser timer receiver", () => {
  const originalSet = globalThis.setTimeout;
  const originalClear = globalThis.clearTimeout;
  let scheduled = 0;
  let cleared = 0;
  try {
    globalThis.setTimeout = function (callback, delay) {
      assert(this == null || this === globalThis, "native browser setTimeout rejects an arbitrary receiver");
      scheduled++;
      return originalSet(callback, delay);
    };
    globalThis.clearTimeout = function (timer) {
      assert(this == null || this === globalThis, "native browser clearTimeout rejects an arbitrary receiver");
      cleared++;
      return originalClear(timer);
    };
    const coordinator = new RefreshCoordinator();
    const stop = coordinator.subscribe(async () => undefined);
    stop();
    assert.equal(scheduled, 1);
    assert.equal(cleared, 1);
  } finally {
    globalThis.setTimeout = originalSet;
    globalThis.clearTimeout = originalClear;
  }
});
