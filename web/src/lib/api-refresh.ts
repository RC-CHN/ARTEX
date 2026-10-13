import { RefreshCoordinator, RequestCache, type RefreshOptions } from "./api-cache";

export const apiCache = new RequestCache();
const coordinator = new RefreshCoordinator();
let subscribers = 0;
let removeListeners: (() => void) | undefined;
let session: string | null | undefined;

export function syncApiSession(token: string | null): void {
  if (session === token) return;
  session = token;
  apiCache.clear();
}

export function clearApiCache(): void {
  session = undefined;
  apiCache.clear();
  coordinator.invalidate();
}

export function invalidateApiData(keys?: readonly string[]): void {
  apiCache.invalidate(keys);
  coordinator.invalidate(keys);
}

// Activity can arrive for every streamed token. Coalesce it into at most one
// invalidation per second while preserving the last batch during long streams.
let eventTimer: ReturnType<typeof setTimeout> | undefined;
const eventKeys = new Set<string>();
export function notifyApiEvent(keys: readonly string[]): void {
  for (const key of keys) eventKeys.add(key);
  if (eventTimer !== undefined) return;
  eventTimer = setTimeout(() => {
    eventTimer = undefined;
    const batch = [...eventKeys];
    eventKeys.clear();
    invalidateApiData(batch);
  }, 1_000);
}

function attachBrowserListeners(): () => void {
  if (typeof document === "undefined") return () => undefined;
  const visibility = () => {
    const visible = document.visibilityState !== "hidden";
    if (visible) apiCache.invalidate();
    coordinator.setVisible(visible);
  };
  const refresh = () => {
    if (document.visibilityState !== "hidden") invalidateApiData();
  };
  const storage = (event: StorageEvent) => {
    if (event.key === "artex_token" || event.key === null) clearApiCache();
  };
  visibility();
  document.addEventListener("visibilitychange", visibility);
  window.addEventListener("focus", refresh);
  window.addEventListener("online", refresh);
  window.addEventListener("storage", storage);
  return () => {
    document.removeEventListener("visibilitychange", visibility);
    window.removeEventListener("focus", refresh);
    window.removeEventListener("online", refresh);
    window.removeEventListener("storage", storage);
  };
}

export function subscribeRefresh(load: () => unknown | Promise<unknown>, options?: RefreshOptions): () => void {
  if (++subscribers === 1) removeListeners = attachBrowserListeners();
  const unsubscribe = coordinator.subscribe(load, options);
  let stopped = false;
  return () => {
    if (stopped) return;
    stopped = true;
    unsubscribe();
    if (--subscribers === 0) {
      removeListeners?.();
      removeListeners = undefined;
    }
  };
}
