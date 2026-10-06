import { describe, expect, it } from 'vitest'
import type { JobsSnapshot, QueueTask } from '../api/types'
import { formatEta, narrationEta, narrationRate } from './narrationEta'

const task = (over: Partial<QueueTask>): QueueTask =>
  ({
    id: Math.random().toString(),
    kind: 'voice_clone',
    bookId: 'a',
    bookTitle: 'A',
    ...over,
  }) as QueueTask

const snap = (over: Partial<JobsSnapshot>): JobsSnapshot => ({
  inFlight: [],
  queued: [],
  queuedTotal: 0,
  drained: false,
  paused: false,
  ...over,
})

describe('narrationEta', () => {
  it('is null without a measured rate or narration work', () => {
    expect(narrationEta(undefined)).toBeNull()
    expect(
      narrationEta(snap({ narrationLeft: [{ bookId: 'a', bookTitle: 'A', chars: 100 }] })),
    ).toBeNull()
    expect(
      narrationEta(
        snap({ narrationCharsPerSecond: 10, queued: [task({ kind: 'speaker_attribution' })] }),
      ),
    ).toBeNull()
  })

  // The task list is capped, so the estimate reads the backend's sums.
  it("turns the backend's per-book narration sums into time at the measured rate", () => {
    const eta = narrationEta(
      snap({
        narrationCharsPerSecond: 20,
        narrationRealtimeFactor: 1.5,
        queued: [task({ chars: 400 })],
        queuedTotal: 30000,
        narrationLeft: [
          { bookId: 'a', bookTitle: 'A', chars: 600 },
          { bookId: 'b', bookTitle: 'B', chars: 1200 },
        ],
      }),
    )
    expect(eta).toEqual({
      charsPerSecond: 20,
      realtimeFactor: 1.5,
      totalSeconds: 90,
      books: [
        { bookId: 'b', bookTitle: 'B', chars: 1200, seconds: 60 },
        { bookId: 'a', bookTitle: 'A', chars: 600, seconds: 30 },
      ],
    })
  })
})

describe('narrationRate', () => {
  it('prefers the live rate, then the last run, else 0', () => {
    expect(narrationRate(undefined)).toBe(0)
    expect(narrationRate(snap({}))).toBe(0)
    expect(narrationRate(snap({ lastNarrationCharsPerSecond: 12 }))).toBe(12)
    expect(
      narrationRate(snap({ narrationCharsPerSecond: 20, lastNarrationCharsPerSecond: 12 })),
    ).toBe(20)
  })
})

describe('formatEta', () => {
  it('floors to <1m and formats hours', () => {
    expect(formatEta(20)).toBe('<1m')
    expect(formatEta(90)).toBe('2m')
    expect(formatEta(3600)).toBe('1h')
    expect(formatEta(3600 + 25 * 60)).toBe('1h 25m')
  })
})
