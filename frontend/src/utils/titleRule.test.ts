import { describe, expect, it } from 'vitest'
import { applyTitleRule, cleanTitle, compileTitleRule, foldTitle, titleRuleBody } from './titleRule'

const run = (pattern: string, output: string, only: boolean, title: string) => {
  const rule = { pattern, output, only }
  return applyTitleRule(rule, compileTitleRule(rule), title)
}

describe('titleRule', () => {
  it('names a match from its capture groups', () => {
    expect(run('^TWI (\\d+\\.\\d+)', 'Chapter $1', true, 'TWI 10.71 (Pt. 1)')).toEqual({
      title: 'Chapter 10.71',
      leftOut: false,
    })
    expect(run('(?P<n>\\d+): (.*)', '${n} - $2 for $$5', false, 'Ch 3: Onward').title).toBe(
      '3 - Onward for $5',
    )
    // A group that didn't take part is empty, and the gap closes.
    expect(run('^(\\d+)(?: pt (\\d))?', 'Chapter $1 $2', false, '12').title).toBe('Chapter 12')
  })

  it('leaves out what does not match only when asked', () => {
    expect(run('^TWI', '', true, 'Side Stories Poll')).toEqual({
      title: 'Side Stories Poll',
      leftOut: true,
    })
    expect(run('^TWI', '', false, 'Side Stories Poll').leftOut).toBe(false)
    expect(run('(?i)^twi', '', true, 'TWI 1').leftOut).toBe(false)
  })

  it('cleans spacing, and folds look-alikes for the pattern', () => {
    expect(cleanTitle('Path of Dragons  18 -  The\nTrees\u200b ')).toBe(
      'Path of Dragons 18 - The Trees',
    )
    expect(foldTitle('\uff30ath\u00a0of \uff18\u2026')).toBe('Path of 8...')
    expect(
      run(
        '^Path of Dragons (\\d+) - Chapter (\\d+) - (.+)$',
        '$1-$2. $3',
        true,
        'Path of Dragons  18 - Chapter 6 -  The Secret Life of Trees',
      ),
    ).toEqual({ title: '18-6. The Secret Life of Trees', leftOut: false })
    // No rule still cleans.
    expect(run('', '', false, 'Weekly\tCheck-In ').title).toBe('Weekly Check-In')
  })

  it('does nothing without a usable pattern', () => {
    expect(compileTitleRule({ pattern: ' ', output: '', only: true })).toBeNull()
    expect(compileTitleRule({ pattern: '(', output: '', only: true })).toBe('invalid')
    expect(run('(', 'x', true, 'Title')).toEqual({ title: 'Title', leftOut: false })
    expect(titleRuleBody({ pattern: '', output: 'x', only: true })).toBeUndefined()
  })
})
