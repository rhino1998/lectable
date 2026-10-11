import { describe, expect, it } from 'vitest'
import { presetDisplayName } from './presetName'

const names = new Map([
  ['abc123', 'Will Oh'],
  ['def456', 'Guard'],
])

describe('presetDisplayName', () => {
  it('names a stored preset', () => {
    expect(presetDisplayName('abc123', names)).toBe('Will Oh')
  })
  it('names an age stage after its character', () => {
    expect(presetDisplayName('abc123~a20-0f3e9a1b', names)).toBe('Will Oh, about 20')
  })
  it('names a numbered role member after its role', () => {
    expect(presetDisplayName('def456~2-9c0d11aa', names)).toBe('Guard 2')
  })
  it('leaves an unknown id alone', () => {
    expect(presetDisplayName('zzz', names)).toBe('zzz')
    expect(presetDisplayName('velvet-narrator', names)).toBe('velvet-narrator')
  })
})
