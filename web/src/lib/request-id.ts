let fallbackSequence = 0;

// Private installations also use HTTP/IP origins: randomUUID is restricted to
// secure contexts, while getRandomValues remains available over HTTP.
export function newRequestId(): string {
  if (globalThis.crypto?.randomUUID) return globalThis.crypto.randomUUID();
  if (globalThis.crypto?.getRandomValues) {
    const bytes = globalThis.crypto.getRandomValues(new Uint8Array(16));
    return `req-${Array.from(bytes, (value) => value.toString(16).padStart(2, "0")).join("")}`;
  }
  // This is a correlation key, never an authentication credential.
  return `req-${Date.now().toString(36)}-${(fallbackSequence++).toString(36)}-${Math.random().toString(36).slice(2)}`;
}
