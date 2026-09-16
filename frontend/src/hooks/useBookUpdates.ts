import { useEffect } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import type { AudioStatus, ChapterDetail, WordTiming } from '../api/types'

interface ParagraphUpdate {
  chapterIdx: number
  paragraphIdx: number
  audioStatus: AudioStatus
  audioError?: string
  durationSeconds?: number
  audioUrl?: string
  // Mirrors backend wshub.ParagraphUpdate's own field of the same name -
  // see Paragraph.audioPointerSeconds (api/types.ts) for what it means.
  audioPointerSeconds?: number
  // Omitted (not sent as []) for a "generating"/"error" update - see
  // backend wshub.ParagraphUpdate's own doc comment - so the merge below
  // only touches words on an update that's actually about alignment.
  words?: WordTiming[]
}

const RECONNECT_DELAY_MS = 2000

// Subscribes to real-time paragraph status pushes for bookId over
// WebSocket (backend: wshub + GET /api/books/{id}/ws), patching the same
// chapter query cache entries useChapterRange reads from directly. This is
// what lets the reader see a paragraph go pending -> generating -> ready
// within about a second instead of waiting on chapterQueryOptions' slow
// safety-net poll, and without every loaded chapter re-fetching its whole
// paragraph list on an interval. One connection per mounted reader,
// covering the whole book (not just currently-loaded chapters) - updates
// for a chapter that isn't loaded are harmless no-ops (setQueryData bails
// out when there's no existing cache entry to patch).
export function useBookUpdates(bookId: string): void {
  const queryClient = useQueryClient()

  useEffect(() => {
    let socket: WebSocket | null = null
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null
    let stopped = false

    const connect = () => {
      const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
      socket = new WebSocket(`${proto}://${window.location.host}/api/books/${bookId}/ws`)

      socket.onmessage = (event) => {
        let update: ParagraphUpdate
        try {
          update = JSON.parse(event.data)
        } catch {
          return
        }
        queryClient.setQueryData<ChapterDetail>(['books', bookId, 'chapters', update.chapterIdx], (old) => {
          if (!old) return old
          return {
            ...old,
            paragraphs: old.paragraphs.map((p) =>
              p.idx === update.paragraphIdx
                ? {
                    ...p,
                    audioStatus: update.audioStatus,
                    audioError: update.audioError,
                    durationSeconds: update.durationSeconds,
                    audioUrl: update.audioUrl,
                    audioPointerSeconds: update.audioPointerSeconds,
                    words: update.words ?? p.words,
                  }
                : p,
            ),
          }
        })
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
  }, [bookId, queryClient])
}
