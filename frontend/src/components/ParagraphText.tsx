import { useEffect, useMemo, useState } from 'react'
import type { CSSProperties, ReactNode } from 'react'
import { RiArrowDropUpLine } from 'react-icons/ri'
import type {
  DirectionMark,
  LineDirection,
  LineDirectionSpan,
  PronunciationMark,
  WordTiming,
} from '../api/types'
import { hasActiveTextSelection } from '../utils/selection'
import { directionTagCategory, formatDirectionTag } from '../utils/directionTags'

interface WordToken {
  word: string
  start: number
  end: number
}

// Splits text into words with their character offsets, purely for
// re-rendering the original text (whitespace/punctuation intact) as
// per-word spans - not itself a source of timing. See wordStartTimes for
// where each token's start time actually comes from.
//
// A table row's text is its cells joined with " | " (see TextCell): only
// the cells hold words. The separators aren't shown, and the aligner
// gives a bar on its own no timing, so counting them would put the word
// count off by one a cell.
function tokenize(text: string, cells?: TextCell[]): WordToken[] {
  const tokens: WordToken[] = []
  const regex = /\S+/g
  for (const range of cells ?? [{ start: 0, end: text.length }]) {
    const part = text.slice(range.start, range.end)
    regex.lastIndex = 0
    let match: RegExpExecArray | null
    while ((match = regex.exec(part)) !== null) {
      const start = range.start + match.index
      tokens.push({ word: match[0], start, end: start + match[0].length })
    }
  }
  return tokens
}

// One cell of a table row: its span of the row's text, and the element it
// is laid out in (ChapterSection places it on the table's grid).
export interface TextCell {
  start: number
  end: number
  className: string
  style: CSSProperties
}

// tts-service's forced aligner (words) is given the paragraph's exact
// transcript, so its word count should normally match tokenize's own
// whitespace-delimited split 1:1, in order - when it does, each token's
// real start time is used directly. If the counts disagree (different
// tokenization conventions - contractions, hyphenation, digits...) real
// per-word alignment can't be lined up reliably, so this falls back to the
// old estimate: allocating the paragraph's total duration proportionally
// to character position within the text. Approximate, but always
// available and good enough to track a karaoke-style highlight.
function wordStartTimes(
  tokens: WordToken[],
  words: WordTiming[],
  textLength: number,
  duration: number,
): number[] {
  if (words.length > 0 && words.length === tokens.length) {
    return words.map((w) => w.start)
  }
  if (textLength === 0 || duration <= 0) return tokens.map(() => 0)
  return tokens.map((t) => (t.start / textLength) * duration)
}

// Highlighting a word exactly at its measured/estimated start tends to
// read as a beat late (the eye already expects a word to be lighting up
// by the time it's heard) - nudging the lookup time forward compensates
// without touching the times themselves, so click-to-seek (which uses
// `times` directly) still lands on each word's real start.
const HIGHLIGHT_LOOKAHEAD_SECONDS = 0.1

function activeWordIndex(times: number[], currentTime: number): number {
  let idx = -1
  for (let i = 0; i < times.length; i++) {
    if (times[i] <= currentTime) idx = i
    else break
  }
  return idx
}

// One inline delivery-tag caret at an exact text position - a small
// colored up-arrow marking "something was inserted here", tinted per tag
// category (see index.css's .direction-caret-* rules) with the tag's own
// human-readable name as a hover tooltip. Zero-width in normal flow
// (.direction-caret-mark) - the icon itself is drawn just below the
// baseline instead of inline, so inserting one never shifts the
// surrounding text or line spacing.
function DirectionCaret({ tag }: { tag: string }) {
  return (
    <span
      className={`direction-caret direction-caret-mark direction-caret-${directionTagCategory(tag)}`}
      title={formatDirectionTag(tag)}
    >
      <RiArrowDropUpLine />
    </span>
  )
}

// A directed line's point tag ([sighs]) where it happens - a small muted
// pill reading the tag, so the event can be read at a glance.
function DeliveryPill({ tag }: { tag: string }) {
  return (
    <span className="delivery-pill" title={`Point tag: [${tag}]`}>
      {tag}
    </span>
  )
}

const SPAN_TITLES: Record<string, string> = {
  whisper: 'Whispered',
  slow: 'Slowly',
  emphasis: 'Emphasized',
}

// The words a directed line's scoped tag (<whisper>…</whisper>) covers -
// styled per tag (see index.css's .delivery-span-* rules).
function DeliverySpan({ tag, children }: { tag: string; children: ReactNode }) {
  return (
    <span
      className={`delivery-span delivery-span-${tag}`}
      title={`${SPAN_TITLES[tag] ?? tag} (<${tag}>)`}
    >
      {children}
    </span>
  )
}

// What a directed line adds to the annotations view (LineDirection, minus
// its style, which ChapterSection draws before the line): point-tag pills
// by offset, and the spans whose words are styled.
interface DeliveryRender {
  pillsByOffset: Map<number, string[]>
  spans: LineDirectionSpan[]
}

const NO_DELIVERY: DeliveryRender = { pillsByOffset: new Map(), spans: [] }

// The scoped tag covering [start, end) of the text, if any.
function spanTagAt(spans: LineDirectionSpan[], start: number, end: number): string | undefined {
  return spans.find((s) => start >= s.start && end <= s.end)?.tag
}

// An underline on the exact word a pronunciation fix applies to (it used to be a strikeout,
// hence the name) - marks "this specific word is read differently than written" right on the word itself, in the same
// --pronunciation-caret color as DirectionCaret's own before-word up-arrow (delivery-tag
// insertions), so the two still read as related even though this one isn't a caret shape.
function PronunciationStrike({ mark, children }: { mark: PronunciationMark; children: ReactNode }) {
  return (
    <span
      className="pronunciation-strike"
      title={`"${mark.original}" read as "${mark.replacement}"`}
    >
      {children}
    </span>
  )
}

// Interleaves marksByOffset/pronunciationByOffset into nodes built by
// walking tokens left to right - shared by both the actively-playing
// (word-highlighted) and idle (plain-text) render paths below, so a
// caret's position logic doesn't need to be duplicated for each. Every
// direction mark's own offset is expected to land exactly at some token's
// own start (or at text.length, for a tag trailing the very last word) -
// true by construction for every tag this app's own LLM passes ever
// produce (each is inserted immediately before a whole word - see backend
// speakerattr.direction.go/sfx.go's own prompts) - a mark that doesn't
// land on a boundary is simply dropped rather than rendered at the wrong
// spot, a display-only fallback with no effect on what's actually spoken.
// A pronunciation mark's own offset is expected to land exactly at some
// token's own start too (every pronunciationCandidates match is a whole
// abbreviation token - see backend speakerattr/pronunciation.go), and is
// dropped the same way if it doesn't (only ever the token it starts is
// wrapped, never a partial word).
function renderWithMarks(
  text: string,
  tokens: WordToken[],
  marksByOffset: Map<number, DirectionMark[]>,
  pronunciationByOffset: Map<number, PronunciationMark>,
  renderToken: (t: WordToken, i: number) => ReactNode,
  // The span of text to render - one table cell's, or all of it. A
  // token's index stays its place among every token either way.
  from = 0,
  to = text.length,
  delivery: DeliveryRender = NO_DELIVERY,
): ReactNode[] {
  const nodes: ReactNode[] = []
  let cursor = from
  const emitMarksAt = (offset: number) => {
    const marks = marksByOffset.get(offset)
    if (marks)
      for (const m of marks)
        nodes.push(<DirectionCaret key={`mark-${offset}-${m.tag}`} tag={m.tag} />)
    const pills = delivery.pillsByOffset.get(offset)
    if (pills)
      for (const tag of pills) nodes.push(<DeliveryPill key={`pill-${offset}-${tag}`} tag={tag} />)
  }
  tokens.forEach((t, i) => {
    if (t.start < from || t.end > to) return
    if (t.start > cursor) {
      // The gap between two words of one span is styled with them, so an
      // underline runs unbroken.
      const gap = text.slice(cursor, t.start)
      const gapTag = cursor > from ? spanTagAt(delivery.spans, cursor, t.start) : undefined
      nodes.push(
        gapTag ? (
          <DeliverySpan key={`gap-${cursor}`} tag={gapTag}>
            {gap}
          </DeliverySpan>
        ) : (
          gap
        ),
      )
    }
    emitMarksAt(t.start)
    let rendered = renderToken(t, i)
    const pron = pronunciationByOffset.get(t.start)
    if (pron)
      rendered = (
        <PronunciationStrike key={`pron-${t.start}`} mark={pron}>
          {rendered}
        </PronunciationStrike>
      )
    // A token's own trailing punctuation may run past a span's end
    // ("her</whisper>." is the token "her."), so a token is in a span when
    // its start is.
    const tag = spanTagAt(delivery.spans, t.start, t.start + 1)
    nodes.push(
      tag ? (
        <DeliverySpan key={`span-${t.start}`} tag={tag}>
          {rendered}
        </DeliverySpan>
      ) : (
        rendered
      ),
    )
    cursor = t.end
  })
  if (cursor < to) nodes.push(text.slice(cursor, to))
  if (to === text.length) emitMarksAt(text.length)
  return nodes
}

function renderWithHighlight(
  text: string,
  tokens: WordToken[],
  times: number[],
  activeIdx: number,
  onSeek: (seconds: number) => void,
  marksByOffset: Map<number, DirectionMark[]>,
  pronunciationByOffset: Map<number, PronunciationMark>,
  from?: number,
  to?: number,
  delivery?: DeliveryRender,
): ReactNode[] {
  const renderToken = (t: WordToken, i: number) => (
    <span
      key={i}
      className={'word' + (i === activeIdx ? ' word-active' : '')}
      onClick={(e) => {
        e.stopPropagation()
        if (hasActiveTextSelection()) return
        onSeek(times[i])
      }}
    >
      {t.word}
    </span>
  )
  return renderWithMarks(
    text,
    tokens,
    marksByOffset,
    pronunciationByOffset,
    renderToken,
    from,
    to,
    delivery,
  )
}

function renderPlainWithMarks(
  text: string,
  tokens: WordToken[],
  marksByOffset: Map<number, DirectionMark[]>,
  pronunciationByOffset: Map<number, PronunciationMark>,
  from?: number,
  to?: number,
  delivery?: DeliveryRender,
): ReactNode[] {
  return renderWithMarks(
    text,
    tokens,
    marksByOffset,
    pronunciationByOffset,
    (t) => t.word,
    from,
    to,
    delivery,
  )
}

export function ParagraphText({
  text,
  active,
  audioElement,
  duration,
  words,
  onSeek,
  marks = [],
  pronunciationMarks = [],
  cells,
  delivery,
}: {
  text: string
  active: boolean
  audioElement: HTMLAudioElement
  duration: number
  // Real per-word timing from tts-service's forced aligner, when it's
  // finished (see WordTiming) - empty until then, in which case timing is
  // estimated instead (see wordStartTimes).
  words: WordTiming[]
  // Word-click-to-seek - only meaningful (and only wired to clickable
  // spans below) while this paragraph is the one actually playing, since
  // that's the only audio currently loaded to seek within.
  onSeek: (seconds: number) => void
  // This paragraph's own inline delivery-tag positions (see
  // Paragraph.directionMarks) - rendered as small colored carets at their
  // exact offset into text, independent of active/duration (unlike word
  // highlighting, these annotate the paragraph's own static content, not
  // playback). Defaults to none - callers gate this on the reader's
  // annotations-view toggle, the same way annotation coloring is.
  marks?: DirectionMark[]
  // This paragraph's own resolved pronunciation substitutions (see
  // Paragraph.pronunciationMarks) - rendered as a strikeout through the
  // exact word each one applies to (PronunciationStrike). Defaults to
  // none, gated the same way marks is.
  pronunciationMarks?: PronunciationMark[]
  // Set for a table row: each cell's span of text is rendered in an
  // element of its own, and the " | " between them isn't rendered at all.
  cells?: TextCell[]
  // A directed line's point tags and spans (Paragraph.delivery) - pills
  // and styled words. Gated on the annotations view like marks; its style
  // is drawn by the caller.
  delivery?: LineDirection
}) {
  const tokens = useMemo(() => tokenize(text, cells), [text, cells])
  const times = useMemo(
    () => wordStartTimes(tokens, words, text.length, duration),
    [tokens, words, text.length, duration],
  )
  const marksByOffset = useMemo(() => {
    const map = new Map<number, DirectionMark[]>()
    for (const m of marks) {
      const list = map.get(m.offset)
      if (list) list.push(m)
      else map.set(m.offset, [m])
    }
    return map
  }, [marks])
  const deliveryRender = useMemo((): DeliveryRender => {
    if (!delivery) return NO_DELIVERY
    const pillsByOffset = new Map<number, string[]>()
    for (const m of delivery.marks ?? []) {
      const list = pillsByOffset.get(m.offset)
      if (list) list.push(m.tag)
      else pillsByOffset.set(m.offset, [m.tag])
    }
    return { pillsByOffset, spans: delivery.spans ?? [] }
  }, [delivery])
  const pronunciationByOffset = useMemo(() => {
    const map = new Map<number, PronunciationMark>()
    for (const m of pronunciationMarks) map.set(m.offset, m)
    return map
  }, [pronunciationMarks])

  // Polls the audio element directly via requestAnimationFrame rather than
  // relying on playback state (driven by the 'timeupdate' event, which
  // only fires a few times a second) - that cadence is fine for a progress
  // bar but makes per-word highlighting visibly jump between words instead
  // of tracking smoothly. Scoped to this one component (not shared
  // playback state) so the high-frequency updates don't re-render the rest
  // of the reader.
  const [currentTime, setCurrentTime] = useState(() => audioElement.currentTime)

  useEffect(() => {
    if (!active) return
    let frame: number
    const tick = () => {
      setCurrentTime(audioElement.currentTime)
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [active, audioElement])

  const highlighting = active && duration > 0
  const activeIdx = highlighting
    ? activeWordIndex(times, currentTime + HIGHLIGHT_LOOKAHEAD_SECONDS)
    : -1
  const renderRange = (from: number, to: number): ReactNode => {
    if (highlighting) {
      return renderWithHighlight(
        text,
        tokens,
        times,
        activeIdx,
        onSeek,
        marksByOffset,
        pronunciationByOffset,
        from,
        to,
        deliveryRender,
      )
    }
    if (
      marksByOffset.size === 0 &&
      pronunciationByOffset.size === 0 &&
      deliveryRender === NO_DELIVERY
    )
      return text.slice(from, to)
    return renderPlainWithMarks(
      text,
      tokens,
      marksByOffset,
      pronunciationByOffset,
      from,
      to,
      deliveryRender,
    )
  }

  if (cells) {
    return (
      <>
        {cells.map((c, i) => (
          <span key={i} className={c.className} style={c.style}>
            {renderRange(c.start, c.end)}
          </span>
        ))}
      </>
    )
  }
  return <>{renderRange(0, text.length)}</>
}
