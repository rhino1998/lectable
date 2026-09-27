// A minimal Philips Hue bridge client over the bridge's local v1 REST API
// (https://<bridge>/api/...). The web reader talks to the bridge directly
// from the browser - the backend never touches a light (see backend
// store.Lighting). HTTPS because current bridge firmware 301-redirects
// every plain-HTTP request there (seen on a BSB003 at API 1.78); CORS is
// permissive (`*`). Direct from the browser on purpose: the bridge is on the
// listener's own network, which only their browser can reach wherever the
// app itself is served from. Two browser rules stand in the way, each a
// one-time step per browser, and both surface in fetch as the same bare
// network error (HUE_UNREACHABLE) - explainUnreachable tells them apart as
// far as a page can:
//   - the certificate is issued by Philips' own "root-bridge" CA, which no
//     browser trusts and page code can't pin, so the listener opens
//     bridgeCertUrl once and accepts the warning;
//   - Chrome's Local Network Access (142+) makes a page served from the
//     internet ask permission before reaching a private IP. The request
//     waits on that prompt, so calls a listener may be answering it during
//     (probe, pair) get SETUP_TIMEOUT_MS instead of the usual 4s.
//
// Android's playback/lighting/HueBridgeClient.kt is the same client in
// Kotlin.

// HueState is the subset of a light's state this app reads and writes.
export interface HueState {
  on?: boolean
  bri?: number
  xy?: [number, number]
  ct?: number
  colormode?: string
  // Deciseconds - Hue's own unit.
  transitiontime?: number
}

export interface HueGroup {
  id: string
  name: string
  type: string
  lights: string[]
}

export interface HueLight {
  id: string
  name: string
  state: HueState
  // Whether the light can show color (anything with an xy gamut) - white-only
  // bulbs still take brightness.
  color: boolean
}

export interface DiscoveredBridge {
  id: string
  ip: string
}

export class HueError extends Error {
  readonly type: number
  constructor(type: number, message: string) {
    super(message)
    this.type = type
  }
}

// HUE_LINK_BUTTON_NOT_PRESSED is the bridge's error type while pairing
// waits on the physical link button.
export const HUE_LINK_BUTTON_NOT_PRESSED = 101

// HUE_UNREACHABLE is this client's own error type for a bridge fetch that
// failed outright - see explainUnreachable.
export const HUE_UNREACHABLE = -2

// bridgeCertUrl is where the listener accepts the bridge's certificate.
export function bridgeCertUrl(ip: string): string {
  return `https://${ip}/api/config`
}

const REQUEST_TIMEOUT_MS = 4000
// Long enough to answer a Local Network Access permission prompt.
const SETUP_TIMEOUT_MS = 60000

async function hueFetch<T>(
  url: string,
  init?: RequestInit,
  timeoutMs = REQUEST_TIMEOUT_MS,
): Promise<T> {
  const ctrl = new AbortController()
  const timer = setTimeout(() => ctrl.abort(), timeoutMs)
  try {
    let res: Response
    try {
      res = await fetch(url, { ...init, signal: ctrl.signal })
    } catch (err) {
      if (ctrl.signal.aborted || !url.startsWith('https://discovery.')) {
        throw new HueError(HUE_UNREACHABLE, "couldn't reach the bridge")
      }
      throw err
    }
    if (!res.ok) throw new HueError(-1, `bridge answered ${res.status}`)
    const body = (await res.json()) as unknown
    // v1 reports failures as 200 [{"error": {...}}].
    if (Array.isArray(body)) {
      const err = body.find((e) => e && typeof e === 'object' && 'error' in e) as
        { error: { type: number; description: string } } | undefined
      if (err) throw new HueError(err.error.type, err.error.description)
    }
    return body as T
  } finally {
    clearTimeout(timer)
  }
}

// localNetworkPermission is Chrome's Local Network Access permission state
// for this page, or null where the browser has no such permission (other
// browsers, older Chrome) or the page isn't subject to it.
export async function localNetworkPermission(): Promise<PermissionState | null> {
  try {
    const status = await navigator.permissions.query({
      name: 'local-network-access' as PermissionName,
    })
    return status.state
  } catch {
    return null
  }
}

// explainUnreachable turns a HUE_UNREACHABLE failure into the fix to try.
// A page can read the network permission but not why a TLS handshake
// failed, so once the permission is ruled out it points at the
// certificate (or the bridge being off / on another network).
export async function explainUnreachable(ip: string): Promise<string> {
  switch (await localNetworkPermission()) {
    case 'denied':
      return 'This site is blocked from your local network. Allow "Local network access" in the browser\'s site settings for this page, then try again.'
    case 'prompt':
      return 'The browser needs your permission to reach devices on your local network - try again and allow it when asked.'
  }
  return `The browser doesn't trust the bridge's certificate yet (or the bridge isn't reachable from this device). Open ${bridgeCertUrl(ip)}, accept the warning, then try again.`
}

// describeError is an error's message, with HUE_UNREACHABLE explained.
export async function describeError(err: unknown, ip: string): Promise<string> {
  if (err instanceof HueError && err.type === HUE_UNREACHABLE) return explainUnreachable(ip)
  return err instanceof Error ? err.message : String(err)
}

// probeBridge checks the bridge answers this browser (its unauthenticated
// /api/config), waiting long enough for a permission prompt; null when it
// does, otherwise what to fix.
export async function probeBridge(ip: string): Promise<string | null> {
  try {
    await hueFetch(bridgeCertUrl(ip), undefined, SETUP_TIMEOUT_MS)
    return null
  } catch (err) {
    return describeError(err, ip)
  }
}

// discoverBridges asks Philips' cloud discovery endpoint for bridges on
// this network (it matches by public IP). Rate-limited upstream, so call
// it only on an explicit click; manual IP entry is the fallback.
export async function discoverBridges(): Promise<DiscoveredBridge[]> {
  const list = await hueFetch<{ id: string; internalipaddress: string }[]>(
    'https://discovery.meethue.com/',
  )
  return list.map((b) => ({ id: b.id, ip: b.internalipaddress }))
}

// pairBridge registers this app with the bridge and returns the username
// (the v1 API key) to keep. Throws a HueError of type
// HUE_LINK_BUTTON_NOT_PRESSED until the bridge's link button has been
// pressed within the last 30 seconds - callers poll.
export async function pairBridge(ip: string, deviceName: string): Promise<string> {
  const res = await hueFetch<{ success?: { username: string } }[]>(
    `https://${ip}/api`,
    {
      method: 'POST',
      body: JSON.stringify({ devicetype: `lectable#${deviceName}`.slice(0, 40) }),
    },
    SETUP_TIMEOUT_MS,
  )
  const username = res[0]?.success?.username
  if (!username) throw new HueError(-1, 'bridge returned no username')
  return username
}

export class HueBridge {
  readonly ip: string
  readonly username: string

  constructor(ip: string, username: string) {
    this.ip = ip
    this.username = username
  }

  private url(path: string): string {
    return `https://${this.ip}/api/${this.username}${path}`
  }

  // groups lists rooms and zones (v1 "Room"/"Zone" groups) - the units a
  // listener picks to light.
  async groups(): Promise<HueGroup[]> {
    const raw = await hueFetch<Record<string, { name: string; type: string; lights: string[] }>>(
      this.url('/groups'),
    )
    return Object.entries(raw)
      .filter(([, g]) => g.type === 'Room' || g.type === 'Zone')
      .map(([id, g]) => ({ id, name: g.name, type: g.type, lights: g.lights }))
  }

  async lights(): Promise<HueLight[]> {
    const raw = await hueFetch<
      Record<
        string,
        { name: string; state: HueState; capabilities?: { control?: { colorgamut?: unknown } } }
      >
    >(this.url('/lights'))
    return Object.entries(raw).map(([id, l]) => ({
      id,
      name: l.name,
      state: l.state,
      color: !!l.capabilities?.control?.colorgamut || l.state.xy !== undefined,
    }))
  }

  // setLight is one light's state change. The bridge handles ~10 of these
  // a second across all lights - LightingEngine budgets for that.
  async setLight(id: string, state: HueState): Promise<void> {
    await hueFetch(this.url(`/lights/${id}/state`), {
      method: 'PUT',
      body: JSON.stringify(state),
    })
  }

  // setGroup changes every light in a group at once. The bridge handles
  // only ~1 of these a second.
  async setGroup(id: string, state: HueState): Promise<void> {
    await hueFetch(this.url(`/groups/${id}/action`), {
      method: 'PUT',
      body: JSON.stringify(state),
    })
  }
}
