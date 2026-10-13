"use client";

import { useEffect, useRef, type DependencyList } from "react";
import { subscribeRefresh } from "@/lib/api-refresh";
import type { RefreshOptions } from "@/lib/api-cache";

// Fetch and commit are separated so a response from an old route/filter cannot
// write into a new view. Existing components can also use subscribeRefresh when
// they already maintain their own request-generation guards.
export function useApiRefresh<T>(
  fetcher: () => Promise<T>,
  commit: (data: T) => void,
  dependencies: DependencyList,
  options: RefreshOptions = {},
): void {
  const fetchRef = useRef(fetcher);
  fetchRef.current = fetcher;
  const commitRef = useRef(commit);
  commitRef.current = commit;
  const keys = JSON.stringify(options.keys ?? []);
  const { intervalMs, immediate } = options;
  useEffect(() => {
    let current = true;
    const query = fetchRef.current;
    const stop = subscribeRefresh(
      async () => {
        const data = await query();
        if (current) commitRef.current(data);
      },
      { intervalMs, immediate, keys: JSON.parse(keys) as string[] },
    );
    return () => {
      current = false;
      stop();
    };
  }, [...dependencies, keys, intervalMs, immediate]);
}
