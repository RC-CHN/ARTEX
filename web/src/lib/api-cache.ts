// Browser-independent request caching and refresh scheduling. Keeping this core
// free of React/DOM dependencies lets CI exercise races with a deterministic clock.

type Entry = {
  path: string;
  revision: number;
  expires: number;
  value?: unknown;
  present: boolean;
  disposed: boolean;
  pending?: Promise<unknown>;
};

export function matchesPath(path: string, prefixes?: readonly string[]): boolean {
  return (
    !prefixes?.length ||
    prefixes.some((prefix) => path === prefix || path.startsWith(`${prefix}?`) || path.startsWith(`${prefix}/`))
  );
}

export class RequestCache {
  private entries = new Map<string, Entry>();
  private now: () => number;
  private ttlMs: number;
  private maxEntries: number;
  constructor(now = Date.now, ttlMs = 5_000, maxEntries = 256) {
    this.now = now;
    this.ttlMs = ttlMs;
    this.maxEntries = maxEntries;
  }

  read<T>(key: string, path: string, loader: () => Promise<T>): Promise<T> {
    let entry = this.entries.get(key);
    if (!entry) {
      entry = { path, revision: 0, expires: 0, present: false, disposed: false };
      this.entries.set(key, entry);
      for (const [oldKey, old] of this.entries) {
        if (this.entries.size <= this.maxEntries) break;
        if (oldKey !== key && !old.pending) this.entries.delete(oldKey);
      }
    }
    // Refresh insertion order for bounded LRU eviction.
    this.entries.delete(key);
    this.entries.set(key, entry);
    if (entry.pending) return entry.pending as Promise<T>;
    if (entry.present && entry.expires > this.now()) return Promise.resolve(entry.value as T);
    const current = entry;
    const pending = (async () => {
      // An invalidation while a read is running schedules exactly one subsequent
      // read. All callers await that new result; the stale response never escapes.
      for (;;) {
        const revision = current.revision;
        try {
          const value = await loader();
          if (current.disposed) throw new Error("请求会话已失效");
          if (current.revision !== revision) continue;
          current.value = value;
          current.present = true;
          current.expires = this.now() + this.ttlMs;
          return value;
        } catch (error) {
          if (!current.disposed && current.revision !== revision) continue;
          // Rejected promises and errors never receive a freshness window.
          current.expires = 0;
          throw error;
        }
      }
    })();
    current.pending = pending;
    void pending
      .finally(() => {
        if (current.pending === pending) current.pending = undefined;
      })
      .catch(() => {
        // The original promise retains the rejection for the caller.
      });
    return pending;
  }

  invalidate(prefixes?: readonly string[]): void {
    for (const entry of this.entries.values()) {
      if (!matchesPath(entry.path, prefixes)) continue;
      entry.revision++;
      entry.expires = 0;
    }
  }

  clear(): void {
    for (const entry of this.entries.values()) entry.disposed = true;
    this.entries.clear();
  }
}

export interface RefreshOptions {
  keys?: readonly string[];
  intervalMs?: number;
  immediate?: boolean;
}

type Clock = {
  now(): number;
  setTimeout(callback: () => void, delay: number): ReturnType<typeof setTimeout>;
  clearTimeout(timer: ReturnType<typeof setTimeout>): void;
};
type Job = {
  load: () => unknown | Promise<unknown>;
  keys?: readonly string[];
  interval: number;
  due: number;
  running: boolean;
  queued: boolean;
};

export class RefreshCoordinator {
  private jobs = new Set<Job>();
  private timer?: ReturnType<typeof setTimeout>;
  private visible = true;
  private clock: Clock;
  constructor(
    clock: Clock = {
      now: Date.now,
      // Calling a browser timer as clock.setTimeout changes its receiver and
      // throws "Illegal invocation". Keep native calls in the global scope.
      setTimeout: (callback, delay) => setTimeout(callback, delay),
      clearTimeout: (timer) => clearTimeout(timer),
    },
  ) {
    this.clock = clock;
  }

  subscribe(load: Job["load"], options: RefreshOptions = {}): () => void {
    const interval = Math.max(1_000, options.intervalMs ?? 30_000);
    const job: Job = {
      load,
      keys: options.keys,
      interval,
      running: false,
      queued: false,
      due: this.clock.now() + (options.immediate === false ? interval : 0),
    };
    this.jobs.add(job);
    this.schedule();
    return () => {
      this.jobs.delete(job);
      this.schedule();
    };
  }

  invalidate(prefixes?: readonly string[]): void {
    for (const job of this.jobs) {
      if (
        prefixes?.length &&
        job.keys?.length &&
        !job.keys.some((key) => matchesPath(key, prefixes) || prefixes.some((prefix) => matchesPath(prefix, [key])))
      )
        continue;
      if (job.running) job.queued = true;
      else job.due = Math.min(job.due, this.clock.now() + 100);
    }
    this.schedule();
  }

  setVisible(visible: boolean): void {
    if (this.visible === visible) return;
    this.visible = visible;
    if (visible) {
      for (const job of this.jobs) {
        if (job.running) job.queued = true;
        else job.due = this.clock.now();
      }
    }
    this.schedule();
  }

  private schedule(): void {
    if (this.timer !== undefined) this.clock.clearTimeout(this.timer);
    this.timer = undefined;
    if (!this.visible) return;
    let due = Infinity;
    for (const job of this.jobs) if (!job.running) due = Math.min(due, job.due);
    if (!Number.isFinite(due)) return;
    this.timer = this.clock.setTimeout(
      () => {
        this.timer = undefined;
        for (const job of this.jobs) {
          if (job.running || job.due > this.clock.now()) continue;
          job.running = true;
          void Promise.resolve()
            .then(job.load)
            .catch(() => {
              // The consumer owns its error UI. Keep the subscription alive so a
              // transient network error cannot permanently stop future refreshes.
            })
            .finally(() => {
              if (!this.jobs.has(job)) return;
              job.running = false;
              job.due = this.clock.now() + (job.queued ? 100 : job.interval);
              job.queued = false;
              this.schedule();
            });
        }
        this.schedule();
      },
      Math.max(0, due - this.clock.now()),
    );
  }
}
