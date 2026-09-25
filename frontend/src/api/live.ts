import { useCallback, useRef, useSyncExternalStore } from 'react'
import type {
  ErrorMessage,
  LiveOp,
  LiveTopics,
  PatchMessage,
  SnapshotMessage,
  SubscribeMessage,
  UnsubscribeMessage,
} from './types'

// Client for the backend's live-state WebSocket (GET /api/events - see
// backend internal/live and httpapi.registerLiveTopics). Every piece of
// server state the UI displays is a *topic* ("books", "chapter", "jobs",
// ...) plus params picking one instance ({bookId, chapterIdx}); subscribing
// gets the topic's full value once (a "snapshot"), then a "patch" every
// time it changes server-side, applied here into an immutable copy. This
// replaces every refetchInterval poll and per-mutation cache invalidation
// the app used to rely on: a write from anywhere - this tab, another tab,
// a background job - shows up everywhere it's displayed.
//
// One connection per page, shared by every hook. Subscriptions are
// ref-counted per (topic, params) and linger briefly after their last
// user unmounts so a quick remount (route change and back, StrictMode's
// double effect) reuses the value instead of refetching. On reconnect
// every active subscription is resent; the last known value stays visible
// until its fresh snapshot replaces it.

export interface LiveError {
  status: number
  message: string
}

// The subset of TanStack Query's result shape pages already read, so
// swapping a polled useQuery for a live topic doesn't ripple into every
// consumer.
export interface LiveResult<T> {
  data: T | undefined
  error: LiveError | undefined
  isLoading: boolean
  isError: boolean
}

type LiveParams = Record<string, string | number>

type Op = LiveOp
type ServerMessage = SnapshotMessage | PatchMessage | ErrorMessage
type ClientMessage = SubscribeMessage | UnsubscribeMessage

interface Entry {
  topic: string
  params: LiveParams
  refs: number
  listeners: Set<() => void>
  state: LiveResult<unknown>
  // Only true between a (re)subscribe being sent and its snapshot/error
  // arriving - a patch can't apply before then.
  awaitingSnapshot: boolean
  lingerTimer: ReturnType<typeof setTimeout> | null
}

const LINGER_MS = 5000
const RECONNECT_DELAY_MS = 1000
const MAX_RECONNECT_DELAY_MS = 10000

const LOADING: LiveResult<never> = { data: undefined, error: undefined, isLoading: true, isError: false }
const DISABLED: LiveResult<never> = { data: undefined, error: undefined, isLoading: false, isError: false }

// Applies patch ops to doc, copying only the containers along each changed
// path - untouched subtrees (e.g. every paragraph but the one whose audio
// just finished) keep their identity, so memoized components below them
// don't re-render.
export function applyOps(doc: unknown, ops: Op[]): unknown {
  for (const op of ops) doc = applyAt(doc, op, 0)
  return doc
}

function applyAt(node: unknown, op: Op, depth: number): unknown {
  const { path } = op
  if (depth === path.length) {
    if (op.op === 'set') return op.value
    if (op.op === 'arr') {
      const prev = node as unknown[]
      const out: unknown[] = []
      for (const item of op.items) {
        if (Array.isArray(item)) {
          for (let i = item[0]; i < item[1]; i++) out.push(prev[i])
        } else {
          out.push(item.v)
        }
      }
      return out
    }
    throw new Error('live: del op with empty path')
  }
  const key = path[depth]
  if (Array.isArray(node)) {
    const copy = node.slice()
    copy[key as number] = applyAt(node[key as number], op, depth + 1)
    return copy
  }
  const obj = node as Record<string, unknown>
  if (op.op === 'del' && depth === path.length - 1) {
    const copy = { ...obj }
    delete copy[key as string]
    return copy
  }
  return { ...obj, [key]: applyAt(obj[key as string], op, depth + 1) }
}

function keyOf(topic: string, params: LiveParams): string {
  const sorted = Object.keys(params)
    .sort()
    .map((k) => [k, params[k]])
  return `${topic}:${JSON.stringify(sorted)}`
}

class LiveClient {
  private entries = new Map<string, Entry>()
  private socket: WebSocket | null = null
  private open = false
  private reconnectDelay = RECONNECT_DELAY_MS
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null

  // Registers listener on (topic, params)'s value, subscribing over the
  // wire if nobody else already is. Returns the matching release.
  retain(topic: string, params: LiveParams, listener: () => void): () => void {
    const key = keyOf(topic, params)
    let entry = this.entries.get(key)
    if (!entry) {
      entry = {
        topic,
        params,
        refs: 0,
        listeners: new Set(),
        state: LOADING,
        awaitingSnapshot: true,
        lingerTimer: null,
      }
      this.entries.set(key, entry)
      this.sendSubscribe(key, entry)
    }
    if (entry.lingerTimer) {
      clearTimeout(entry.lingerTimer)
      entry.lingerTimer = null
    }
    entry.refs++
    entry.listeners.add(listener)
    this.ensureSocket()

    const e = entry
    let released = false
    return () => {
      if (released) return
      released = true
      e.listeners.delete(listener)
      e.refs--
      if (e.refs > 0) return
      e.lingerTimer = setTimeout(() => {
        e.lingerTimer = null
        if (e.refs > 0) return
        this.entries.delete(key)
        this.send({ type: 'unsubscribe', id: key })
      }, LINGER_MS)
    }
  }

  get(topic: string, params: LiveParams): LiveResult<unknown> {
    return this.entries.get(keyOf(topic, params))?.state ?? LOADING
  }

  private ensureSocket() {
    if (this.socket || this.reconnectTimer) return
    const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
    const socket = new WebSocket(`${proto}://${window.location.host}/api/events`)
    this.socket = socket

    socket.onopen = () => {
      this.open = true
      this.reconnectDelay = RECONNECT_DELAY_MS
      for (const [key, entry] of this.entries) {
        entry.awaitingSnapshot = true
        this.sendSubscribe(key, entry)
      }
    }
    socket.onmessage = (event) => {
      let msg: ServerMessage
      try {
        msg = JSON.parse(event.data)
      } catch {
        return
      }
      this.handle(msg)
    }
    socket.onclose = () => {
      this.socket = null
      this.open = false
      if (this.entries.size === 0) return
      this.reconnectTimer = setTimeout(() => {
        this.reconnectTimer = null
        this.ensureSocket()
      }, this.reconnectDelay)
      this.reconnectDelay = Math.min(this.reconnectDelay * 2, MAX_RECONNECT_DELAY_MS)
    }
    socket.onerror = () => socket.close()
  }

  private handle(msg: ServerMessage) {
    const entry = this.entries.get(msg.id)
    if (!entry) return
    switch (msg.type) {
      case 'snapshot':
        entry.awaitingSnapshot = false
        this.setState(entry, { data: msg.data, error: undefined, isLoading: false, isError: false })
        return
      case 'error':
        entry.awaitingSnapshot = false
        this.setState(entry, {
          data: undefined,
          error: { status: msg.status, message: msg.error },
          isLoading: false,
          isError: true,
        })
        return
      case 'patch': {
        if (entry.awaitingSnapshot || entry.state.data === undefined) return
        let next: unknown
        try {
          next = applyOps(entry.state.data, msg.ops)
        } catch (err) {
          // Our copy has diverged from the server's somehow - resubscribing
          // gets a fresh snapshot rather than compounding the error.
          console.error('live: failed to apply patch, resubscribing', msg.id, err)
          entry.awaitingSnapshot = true
          this.sendSubscribe(msg.id, entry)
          return
        }
        this.setState(entry, { ...entry.state, data: next })
        return
      }
    }
  }

  private setState(entry: Entry, state: LiveResult<unknown>) {
    entry.state = state
    for (const l of entry.listeners) l()
  }

  private sendSubscribe(key: string, entry: Entry) {
    this.send({ type: 'subscribe', id: key, topic: entry.topic, params: entry.params })
  }

  private send(msg: ClientMessage) {
    if (this.open && this.socket) this.socket.send(JSON.stringify(msg))
    // Otherwise onopen (re)sends every entry's subscribe itself.
  }
}

export const liveClient = new LiveClient()

// Subscribes to one topic for as long as the calling component is mounted.
// params === null disables the subscription (TanStack's `enabled: false`) -
// the result is then empty and not loading.
// Typed by the backend's topic table (generated LiveTopics): the topic
// name fixes both the params it takes and the value it yields.
export function useLive<K extends keyof LiveTopics>(
  topic: K,
  params: LiveTopics[K]['params'] | null,
): LiveResult<LiveTopics[K]['data']> {
  const key = params ? keyOf(topic, params) : null
  const subscribe = useCallback(
    (listener: () => void) => (params ? liveClient.retain(topic, params, listener) : () => {}),
    // key captures params by value - a fresh-but-equal params object each
    // render mustn't resubscribe.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key],
  )
  const getSnapshot = useCallback(
    () => (params ? liveClient.get(topic, params) : DISABLED),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [key],
  )
  return useSyncExternalStore(subscribe, getSnapshot) as LiveResult<LiveTopics[K]['data']>
}

// useLive over a list of instances of one topic (e.g. every chapter in the
// reader's scroll window). The returned array keeps its identity until one
// of its elements actually changes, so it's safe as a memo dependency.
export function useLiveMany<K extends keyof LiveTopics>(
  topic: K,
  paramsList: LiveTopics[K]['params'][],
): LiveResult<LiveTopics[K]['data']>[] {
  const keys = paramsList.map((p) => keyOf(topic, p)).join('\n')
  const cache = useRef<LiveResult<unknown>[]>([])
  const subscribe = useCallback(
    (listener: () => void) => {
      const releases = paramsList.map((p) => liveClient.retain(topic, p, listener))
      return () => releases.forEach((r) => r())
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [keys],
  )
  const getSnapshot = useCallback(
    () => {
      const next = paramsList.map((p) => liveClient.get(topic, p))
      const prev = cache.current
      if (prev.length === next.length && prev.every((s, i) => s === next[i])) return prev
      cache.current = next
      return next
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [keys],
  )
  return useSyncExternalStore(subscribe, getSnapshot) as LiveResult<LiveTopics[K]['data']>[]
}
