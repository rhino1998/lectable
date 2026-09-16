// Appends a cache-busting query param to url when version is set (>0).
// Needed because a rendered voice clip's URL is a stable per-preset/
// per-character path that never itself changes even when the underlying
// file on disk does (regenerate, recharacterize) - so React re-rendering
// an <audio src={url}> with that same string gives the browser no reason
// to re-fetch it, and it can keep playing/serving stale cached bytes.
// Callers keep a small Record<id, number> of their own, bumped on a
// successful regenerate/recharacterize, and pass the relevant entry here
// when building that resource's src.
export function withCacheBust(url: string, version: number | undefined): string {
  if (!version) return url
  return `${url}${url.includes('?') ? '&' : '?'}v=${version}`
}
