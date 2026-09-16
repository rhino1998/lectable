import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import type { JobsSnapshot } from '../api/types'

const RECONNECT_DELAY_MS = 2000

// Subscribes to the job queue's live state over WebSocket (backend:
// jobs.Manager.SubscribeChanges + GET /api/jobs/ws), replacing the ['jobs']
// query cache entry useJobsSnapshot reads from wholesale on every message -
// unlike useBookUpdates' own per-paragraph patch, each message here already
// *is* the complete current state (every task queued/in-flight), not a
// partial diff, so there's nothing to merge. Mounted once at the app root
// (routes.tsx) rather than per-page, since ['jobs'] has multiple consumers
// beyond the Jobs page itself (ReaderPage's useAttributingChapters/
// useCharacterizingSpeakers) that all benefit from staying live regardless
// of which page is currently open.
export function useJobsUpdates(): void {
  const queryClient = useQueryClient()

  useEffect(() => {
    let socket: WebSocket | null = null
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null
    let stopped = false

    const connect = () => {
      const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
      socket = new WebSocket(`${proto}://${window.location.host}/api/jobs/ws`)

      socket.onmessage = (event) => {
        let snapshot: JobsSnapshot
        try {
          snapshot = JSON.parse(event.data)
        } catch {
          return
        }
        queryClient.setQueryData<JobsSnapshot>(['jobs'], snapshot)
      }

      socket.onclose = () => {
        if (stopped) return
        reconnectTimer = setTimeout(connect, RECONNECT_DELAY_MS)
      }
      socket.onerror = () => {
        socket?.close()
      }
    }

    connect()

    return () => {
      stopped = true
      if (reconnectTimer) clearTimeout(reconnectTimer)
      socket?.close()
    }
  }, [queryClient])
}
