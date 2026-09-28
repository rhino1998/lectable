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
  paused: false,
  ...over,
})

describe('narrationEta', () => {
  it('is null without a measured rate or narration work', () => {
    expect(narrationEta(undefined)).toBeNull()
    expect(narrationEta(snap({ queued: [task({ chars: 100 })] }))).toBeNull()
    expect(
      narrationEta(
        snap({ narrationCharsPerSecond: 10, queued: [task({ kind: 'speaker_attribution' })] }),
      ),
    ).toBeNull()
  })

  it('sums in-flight and queued narration per book at the measured rate', () => {
    const eta = narrationEta(
      snap({
        narrationCharsPerSecond: 20,
        inFlight: [task({ chars: 200 })],
        queued: [
          task({ chars: 400 }),
          task({ bookId: 'b', bookTitle: 'B', chars: 1200 }),
          task({ kind: 'ambience' }),
        ],
      }),
    )
    expect(eta).toEqual({
      charsPerSecond: 20,
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
