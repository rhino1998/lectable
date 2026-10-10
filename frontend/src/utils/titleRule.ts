import type { SourceTitleRule } from '../api/types'

// A source's title rule (backend store.TitleRule): a regular expression
// over the titles a source lists its entries under, saying which are
// chapters and what each is called. The server applies it (Go's regexp);
// this mirrors it closely enough to show what a rule being typed would do.

export const NO_TITLE_RULE: SourceTitleRule = { pattern: '', output: '', only: false }

// The pattern as a JS expression: null for none, 'invalid' for one that
// doesn't parse. Go's "(?P<name>" and a leading "(?i)" are translated.
export function compileTitleRule(rule: SourceTitleRule): RegExp | null | 'invalid' {
  let pattern = rule.pattern.trim() === '' ? '' : rule.pattern
  if (!pattern) return null
  let flags = ''
  if (pattern.startsWith('(?i)')) {
    pattern = pattern.slice(4)
    flags = 'i'
  }
  try {
    return new RegExp(pattern.replaceAll('(?P<', '(?<'), flags)
  } catch {
    return 'invalid'
  }
}

// A title with its spacing put right, as the server stores it: line
// breaks, tabs and odd spaces become one ordinary space, and characters
// that aren't seen (zero-width ones, soft hyphens, controls) go.
export function cleanTitle(title: string): string {
  return title
    .replace(/\s+/gu, ' ')
    .replace(/[\p{Cf}\p{Cc}]/gu, '')
    .replace(/ +/g, ' ')
    .trim()
}

// A title as a pattern sees it: cleaned, with Unicode's compatibility
// forms folded to the plain ones (NFKC), so a pattern typed on a keyboard
// matches a title that only looks like what was typed.
export function foldTitle(title: string): string {
  return cleanTitle(title.normalize('NFKC'))
}

// What the rule makes of one title: its name under the rule (cleaned
// either way), and whether the rule leaves the entry out.
export function applyTitleRule(
  rule: SourceTitleRule,
  re: RegExp | null | 'invalid',
  title: string,
): { title: string; leftOut: boolean } {
  const folded = foldTitle(title)
  title = cleanTitle(title)
  if (!re || re === 'invalid') return { title, leftOut: false }
  const m = re.exec(folded)
  if (!m) return { title, leftOut: rule.only }
  if (!rule.output.trim()) return { title, leftOut: false }
  // Go's template syntax: $1, $name, ${name}, and $$ for a dollar sign.
  const made = rule.output
    .replace(/\$(\$|\{(\w+)\}|(\w+))/g, (_, whole: string, braced?: string, bare?: string) => {
      if (whole === '$') return '$'
      const name = braced ?? bare ?? ''
      return (/^\d+$/.test(name) ? m[Number(name)] : m.groups?.[name]) ?? ''
    })
    .split(/\s+/)
    .filter(Boolean)
    .join(' ')
  return { title: cleanTitle(made) || title, leftOut: false }
}

// The rule as a request sends it: undefined for none.
export function titleRuleBody(rule: SourceTitleRule): SourceTitleRule | undefined {
  return rule.pattern.trim() ? rule : undefined
}
