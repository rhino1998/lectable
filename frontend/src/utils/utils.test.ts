import { describe, expect, it } from 'vitest'
import type { Paragraph } from '../api/types'
import { annotationKind, matchesSelectedSpeaker } from './annotations'
import { withCacheBust } from './cacheBust'
import { resolveGenerationText } from './resolveGenerationText'
import { formatDurationLong } from './time'

const para = (over: Partial<Paragraph>): Paragraph => ({ idx: 0, text: '', ...over }) as Paragraph

describe('annotationKind', () => {
  it('treats real dialogue as speaker, scare quotes as narrator', () => {
    expect(annotationKind(para({ isQuote: true }))).toBe('speaker')
    expect(annotationKind(para({ isQuote: true, scareQuote: true }))).toBe('narrator')
  })
  it('treats everything else as narrator', () => {
    expect(annotationKind(para({}))).toBe('narrator')
  })
})

describe('matchesSelectedSpeaker', () => {
  it('treats Narrator as matching unattributed and literal Narrator', () => {
    expect(matchesSelectedSpeaker(para({ speaker: '' }), 'Narrator')).toBe(true)
    expect(matchesSelectedSpeaker(para({ speaker: 'Narrator' }), 'Narrator')).toBe(true)
    expect(matchesSelectedSpeaker(para({ speaker: 'Jake' }), 'Narrator')).toBe(false)
  })
  it('matches a named speaker exactly', () => {
    expect(matchesSelectedSpeaker(para({ speaker: 'Jake' }), 'Jake')).toBe(true)
    expect(matchesSelectedSpeaker(para({ speaker: 'jake' }), 'Jake')).toBe(false)
  })
})

describe('withCacheBust', () => {
  it('leaves the url alone without a version', () => {
    expect(withCacheBust('/a.wav', undefined)).toBe('/a.wav')
    expect(withCacheBust('/a.wav', 0)).toBe('/a.wav')
  })
  it('appends with ? or &', () => {
    expect(withCacheBust('/a.wav', 2)).toBe('/a.wav?v=2')
    expect(withCacheBust('/a.wav?x=1', 3)).toBe('/a.wav?x=1&v=3')
  })
})

describe('formatDurationLong', () => {
  it('formats hours and minutes', () => {
    expect(formatDurationLong(38 * 60)).toBe('38m')
    expect(formatDurationLong(2 * 3600)).toBe('2h')
    expect(formatDurationLong(4 * 3600 + 12 * 60 + 20)).toBe('4h 12m')
  })
  it('shows a dash for nothing or nonsense', () => {
    expect(formatDurationLong(0)).toBe('—')
    expect(formatDurationLong(Number.NaN)).toBe('—')
  })
})

describe('resolveGenerationText', () => {
  const text = 'Dr. Who... waits.'
  it('returns the text untouched with nothing to apply', () => {
    expect(resolveGenerationText(text, undefined, [])).toBe(text)
  })
  it('puts tags before a substitution at the same offset', () => {
    const out = resolveGenerationText(
      text,
      [
        { offset: 0, tag: '<a>' },
        { offset: 7, tag: '<p>' },
      ],
      [{ offset: 0, length: 3, original: 'Dr.', replacement: 'Doctor' }],
    )
    expect(out).toBe('<a>Doctor Who<p>... waits.')
  })
})
