// This browser's reader id: random, made once and kept in localStorage. Sent
// with lookahead calls so the backend tracks each reader's window on its own
// (see the backend's lookaheadRequest.clientId).
const KEY = 'lectable.clientId'

let cached: string | null = null

export function clientId(): string {
  if (cached) return cached
  let id: string | null = null
  try {
    id = localStorage.getItem(KEY)
  } catch {
    // Storage unavailable: an id for this page load still works.
  }
  if (!id) {
    id = crypto.randomUUID()
    try {
      localStorage.setItem(KEY, id)
    } catch {
      // See above.
    }
  }
  cached = id
  return id
}
