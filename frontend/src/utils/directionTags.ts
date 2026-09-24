// Shared helpers for displaying a raw Higgs inline delivery tag (today
// only "<|prosody:pause|>" - see backend deliverytags.PauseInsertions) -
// used by annotations (tooltips) and ParagraphText (inline carets), so
// they stay in sync rather than each parsing the tag string its own way.
// Emotion isn't a tag any more - see utils/emotions.ts.

// Turns a raw tag into a short human-readable label ("Pause") - the exact
// <|category:value|> syntax matters to audio.cpp's tokenizer, not to a
// reader, so this is purely a display concern. Falls back to the raw tag
// string unchanged if it doesn't match the expected shape, rather than
// hiding it - still better than nothing for a value this component didn't
// invent.
export function formatDirectionTag(tag: string): string {
  const m = /^<\|\w+:(\w+)\|>$/.exec(tag)
  if (!m) return tag
  const value = m[1].replace(/_/g, ' ')
  return value.charAt(0).toUpperCase() + value.slice(1)
}

export type DirectionTagCategory = 'emotion' | 'style' | 'prosody' | 'sfx' | 'other'

// The tag categories the backend still produces - used by PlayerBar's
// annotation legend to show what each caret color means.
export const DIRECTION_TAG_CATEGORY_LABELS: Partial<Record<Exclude<DirectionTagCategory, 'other'>, string>> = {
  prosody: 'Pause',
}

// Extracts a tag's own category ("emotion", "style", "prosody", "sfx") for
// per-category caret coloring - see index.css's .direction-caret-* rules.
export function directionTagCategory(tag: string): DirectionTagCategory {
  const m = /^<\|(\w+):/.exec(tag)
  switch (m?.[1]) {
    case 'emotion':
    case 'style':
    case 'prosody':
    case 'sfx':
      return m[1]
    default:
      return 'other'
  }
}
