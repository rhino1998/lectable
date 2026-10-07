import { Fragment, createElement, memo, useMemo, useState } from 'react'
import type { CSSProperties, ReactNode } from 'react'
import {
  RiBookmarkFill,
  RiBookmarkLine,
  RiChatCheckLine,
  RiChatVoiceLine,
  RiCodeSSlashLine,
  RiDeleteBinLine,
  RiFileHistoryLine,
  RiDoubleQuotesL,
  RiEmotionLine,
  RiMusic2Line,
  RiRefreshLine,
  RiSpeakLine,
  RiUser3Line,
  RiVoiceprintLine,
  RiVolumeUpLine,
  RiWindyLine,
  RiLightbulbLine,
} from 'react-icons/ri'
import type {
  ChapterDetail,
  ChapterNotes,
  ContentItem,
  Lighting,
  MusicRegion,
  Paragraph,
  SummaryScene,
} from '../api/types'
import { LIGHT_EFFECT_LABELS } from '../api/types'
import { hasActiveTextSelection } from '../utils/selection'
import { emotionIcon, emotionLabel } from '../utils/emotions'
import { annotationKind, annotationTitle, matchesSelectedSpeaker } from '../utils/annotations'
import { resolveGenerationText } from '../utils/resolveGenerationText'
import { cellRanges, tableColumnEnd, tableGenerationMarks } from '../utils/table'
import { ParagraphText } from './ParagraphText'
import type { TextCell } from './ParagraphText'
import { ChapterNotesBlock } from './ChapterNotesBlock'
import { LightingSwatch } from './LightingSwatch'
import { MusicRegionPreviewButton } from './MusicRegionPreviewButton'

// One rendered visual block: either a single image, or one or more
// paragraphs joined with no break between them - a non-inline paragraph
// starts a new text group, and each following Inline paragraph (a
// split-out continuation, e.g. a quoted-dialogue segment pulled out of a
// mixed narration+dialogue source paragraph - see store.Paragraph.Inline)
// is appended to the current one instead of starting its own. Each
// paragraph inside a text group still gets its own click-to-play/
// highlight/word-seek (see the per-segment <span> below); only the
// bookmark/regenerate actions and the visual block itself are shared
// across the whole group - see groupContent's callers.
type ContentGroup =
  | { kind: 'image'; key: string; imageUrl: string }
  | { kind: 'break'; key: string }
  | { kind: 'text'; key: string; paragraphs: Paragraph[] }

function groupContent(content: ContentItem[], paragraphs: Paragraph[]): ContentGroup[] {
  const groups: ContentGroup[] = []
  content.forEach((item, i) => {
    if (item.kind === 'image') {
      groups.push({ kind: 'image', key: `img-${i}`, imageUrl: item.imageUrl })
      return
    }
    if (item.kind === 'break') {
      groups.push({ kind: 'break', key: `break-${i}` })
      return
    }
    const p = paragraphs[item.paragraphIdx]
    if (!p) return
    const last = groups[groups.length - 1]
    if (p.inline && last?.kind === 'text') {
      last.paragraphs.push(p)
    } else {
      groups.push({ kind: 'text', key: `p-${p.idx}`, paragraphs: [p] })
    }
  })
  return groups
}

// The table a group's paragraphs belong to, if any.
function groupTable(group: ContentGroup) {
  return group.kind === 'text' ? group.paragraphs[0].table : undefined
}

interface ParagraphGroupProps {
  group: ContentGroup
  chapterIdx: number
  annotationsView: boolean
  selectedSpeaker: string | null
  isActiveChapter: boolean
  activeParagraphIdx: number | null
  audioElement: HTMLAudioElement
  bookmarked: boolean
  onSeek: (seconds: number) => void
  onPlayAt: (chapterIdx: number, paragraphIdx: number) => void
  onToggleBookmark: (chapterIdx: number, paragraphIdx: number) => void
  onRegenerate: (chapterIdx: number, paragraphIdx: number) => void
  onOpenSpeakerMenu: (e: React.MouseEvent, chapterIdx: number, p: Paragraph) => void
  onShowTooltip: (e: React.MouseEvent<HTMLElement>, text: string) => void
  onHideTooltip: () => void
  registerParagraphRef: (key: string, el: HTMLElement | null) => void
  // Sound-effect (Stable Audio SFX) test surface (see backend/CLAUDE.md's
  // "SFX sound effects" section) - onGenerateSFX's returned promise
  // settling (resolve or reject) is what drives this panel's own local
  // busy/error state below, rather than a mutation object threaded all
  // the way down through ChapterSection. onSetSFXPrompt's triggerWord is
  // omitted for a plain prompt edit (see the word-picker below, the only
  // caller that passes one).
  onSetSFXPrompt: (
    chapterIdx: number,
    paragraphIdx: number,
    prompt: string,
    triggerWord?: number,
  ) => void
  onGenerateSFX: (chapterIdx: number, paragraphIdx: number, prompt: string) => Promise<void>
}

// A dialogue segment's emotion, as a small inline icon at the start of
// the line it applies to (annotations view only) - per segment, since a
// paragraph with several dialogue segments can carry a different emotion
// on each. Its own hover shows just the emotion name through the reader's
// shared AnnotationTooltip; stopPropagation keeps the enclosing segment's
// onMouseMove from immediately replacing that with the segment's full
// annotationTitle.
function EmotionIcon({
  emotion,
  onShowTooltip,
}: {
  emotion: string
  onShowTooltip: (e: React.MouseEvent<HTMLElement>, text: string) => void
}) {
  const label = `Emotion: ${emotionLabel(emotion)}`
  const show = (e: React.MouseEvent<HTMLElement>) => {
    e.stopPropagation()
    onShowTooltip(e, label)
  }
  return (
    <span className="emotion-icon" aria-label={label} onMouseEnter={show} onMouseMove={show}>
      {createElement(emotionIcon(emotion))}
    </span>
  )
}

// One visual paragraph (image, or one-or-more joined text segments) -
// memoized so that a state change elsewhere in the reader (a mouse move
// over an unrelated paragraph, a playback tick, a scroll-visibility
// update) doesn't force every other paragraph in every loaded chapter to
// re-render along with it. Only re-renders when its own group data,
// active/annotation state, or bookmark status actually changes - see
// paragraphGroupPropsEqual for what "its own" means for group and
// audioElement.
const ParagraphGroup = memo(function ParagraphGroup({
  group,
  chapterIdx,
  annotationsView,
  selectedSpeaker,
  isActiveChapter,
  activeParagraphIdx,
  audioElement,
  bookmarked,
  onSeek,
  onPlayAt,
  onToggleBookmark,
  onRegenerate,
  onOpenSpeakerMenu,
  onShowTooltip,
  onHideTooltip,
  registerParagraphRef,
  onSetSFXPrompt,
  onGenerateSFX,
}: ParagraphGroupProps) {
  // Local, not lifted to ReaderPage - purely this one paragraph's own
  // expand/collapse UI state, the same reasoning AnnotationTooltip's
  // imperative-ref design avoids lifting hover state: keeping it here
  // means toggling it only re-renders this one memoized ParagraphGroup,
  // not the whole loaded chapter list.
  const [showGenerationText, setShowGenerationText] = useState(false)
  // SFX test panel - sfxDraft starts undefined so the textarea shows this
  // paragraph's own already-saved prompt (segments[0].sfxPrompt) until the
  // reader actually edits it; sfxBusy/sfxLocalError track onGenerateSFX's
  // own promise, which only covers the enqueue request itself (see
  // useGenerateParagraphSFX's own doc comment) - actual completion is
  // reflected by segments[0].sfxStatus once the chapter topic pushes it,
  // not by this promise settling.
  const [sfxOpen, setSFXOpen] = useState(false)
  const [sfxDraft, setSFXDraft] = useState<string | undefined>(undefined)
  const [sfxBusy, setSFXBusy] = useState(false)
  const [sfxLocalError, setSFXLocalError] = useState('')

  // A paragraph from a table in the book (Paragraph.table) is laid out on
  // the grid ChapterSection wraps the table's paragraphs in (see
  // .chapter-table): it spans its cells' columns, and a row of several
  // cells puts each one's text in its own column (tableCells) rather
  // than showing the " | " they're joined with.
  const first = group.kind === 'text' ? group.paragraphs[0] : undefined
  const table = first?.table
  const rowOfCells = group.kind === 'text' && group.paragraphs.length === 1 && !!table
  const tableText = first?.text ?? ''
  const tableCells = useMemo((): TextCell[] | undefined => {
    if (!rowOfCells || !table || table.cells.length < 2) return undefined
    const firstCol = table.cells[0].col
    return cellRanges(tableText, table).map((c) => ({
      start: c.start,
      end: c.end,
      className: 'table-cell' + (c.header ? ' table-cell-header' : ''),
      style: { gridColumn: `${c.col - firstCol + 1} / span ${c.colSpan}` },
    }))
  }, [rowOfCells, table, tableText])
  const tableStyle: CSSProperties | undefined = table
    ? { gridColumn: `${table.cells[0].col + 1} / ${tableColumnEnd(table) + 1}` }
    : undefined

  if (group.kind === 'image') {
    return <img className="chapter-image" src={group.imageUrl} alt="" loading="lazy" />
  }

  if (group.kind === 'break') {
    return <div className="chapter-scene-break" aria-hidden="true" />
  }

  const segments = group.paragraphs
  const groupActive = isActiveChapter && segments.some((p) => p.idx === activeParagraphIdx)
  const groupError = segments.some((p) => p.audioStatus === 'error')
  // Bookmarking/regenerating apply to the whole visual paragraph, not one
  // segment - bookmark keys off the first segment (a group's own natural
  // identity), while regenerate re-renders every segment in it, since a
  // reader thinking "this paragraph sounds off" means the whole thing
  // they're looking at, inline dialogue included.
  const firstIdx = segments[0].idx
  const speakers = [...new Set(segments.map((p) => p.speaker).filter((s): s is string => !!s))]
  const emotionLabels = [
    ...new Set(segments.map((p) => p.emotion).filter((e): e is string => !!e)),
  ].map(emotionLabel)
  const regenerating = segments.some((p) => p.audioStatus === 'generating')

  // A row of cells is a grid itself, so what trails a paragraph's text
  // (badges, the hover buttons) would each land in a cell: it sits over
  // the row's end instead.
  const trailing = (children: ReactNode) =>
    tableCells ? <span className="table-row-actions">{children}</span> : children

  return (
    <>
      <p
        className={
          'paragraph' +
          (groupActive ? ' paragraph-active' : '') +
          (groupError ? ' paragraph-error' : '') +
          (table ? ' table-paragraph' : '') +
          (tableCells ? ' table-row' : '') +
          (table && !tableCells && table.cells[0].header ? ' table-cell-header' : '')
        }
        style={tableStyle}
        // The row's one segment has no box of its own to scroll to (see
        // .table-row-segment), so the row stands in for it.
        ref={tableCells ? (el) => registerParagraphRef(`${chapterIdx}:${firstIdx}`, el) : undefined}
      >
        {segments.map((p, segIdx) => {
          const isActive = isActiveChapter && p.idx === activeParagraphIdx
          // Per-segment, not per-group: a merged/inline paragraph can have
          // one segment ready and another still pending (e.g. a scare
          // quote's own merge group generating as one clip while an
          // adjacent, differently-voiced segment hasn't started yet) - see
          // ChapterSection's own segments.map. Dimming the whole combined
          // <p> for one not-yet-ready segment would grey out sibling text
          // that's already narrated.
          const segmentPending = p.audioStatus === 'pending' || p.audioStatus === 'generating'
          return (
            <span
              key={p.idx}
              ref={
                tableCells ? undefined : (el) => registerParagraphRef(`${chapterIdx}:${p.idx}`, el)
              }
              className={
                (tableCells ? 'table-row-segment ' : '') +
                (segmentPending ? 'segment-pending ' : '') +
                (annotationsView
                  ? `annotation-${annotationKind(p)}` +
                    (selectedSpeaker && matchesSelectedSpeaker(p, selectedSpeaker)
                      ? ' annotation-selected-speaker'
                      : '') +
                    (p.scareQuote ? ' annotation-scare-quote' : '')
                  : '')
              }
              onMouseEnter={
                annotationsView ? (e) => onShowTooltip(e, annotationTitle(p)) : undefined
              }
              onMouseMove={
                annotationsView ? (e) => onShowTooltip(e, annotationTitle(p)) : undefined
              }
              onMouseLeave={annotationsView ? onHideTooltip : undefined}
              onClick={() => {
                if (hasActiveTextSelection()) return
                onPlayAt(chapterIdx, p.idx)
              }}
              onContextMenu={(e) => onOpenSpeakerMenu(e, chapterIdx, p)}
            >
              {/* Each Inline segment's text is trimmed at parse time
                  (epub.splitQuoteSegments), so joining segments back into
                  one visual paragraph needs its own space reinserted
                  between them - otherwise "fire." and the next segment's
                  opening quote run together with no gap. */}
              {segIdx > 0 && ' '}
              {/* Per-segment, unlike the trailing emotion badge below: a
                  paragraph with several dialogue segments can carry a
                  different emotion on each, and the chip sits right on the
                  line it applies to. */}
              {annotationsView && p.emotion && (
                <EmotionIcon emotion={p.emotion} onShowTooltip={onShowTooltip} />
              )}
              {/* A play's speaker label ("THESEUS."), with any direction on
                  how the line is delivered: shown, never spoken. */}
              {p.label && <span className="paragraph-label">{p.label} </span>}
              <ParagraphText
                text={p.text}
                active={isActive}
                audioElement={audioElement}
                duration={p.durationSeconds ?? 0}
                words={p.words}
                onSeek={onSeek}
                marks={annotationsView ? (p.directionMarks ?? []) : []}
                pronunciationMarks={annotationsView ? (p.pronunciationMarks ?? []) : []}
                cells={tableCells}
              />
              {p.audioStatus === 'error' && (
                <span className="error-text"> (audio failed: {p.audioError})</span>
              )}
            </span>
          )
        })}
        {trailing(
          <>
            {speakers.length > 0 && (
              <span
                className={
                  'paragraph-speaker-hint' +
                  (annotationsView ? ' paragraph-speaker-hint-visible' : '')
                }
                title={`Speaker: ${speakers.join(', ')}`}
              >
                <RiUser3Line /> {speakers.join(', ')}
              </span>
            )}
            {/* Hover-only summary outside annotations view - annotations view
                shows each segment's own EmotionIcon instead. */}
            {!annotationsView && emotionLabels.length > 0 && (
              <span
                className="paragraph-direction-hint"
                title={`Emotion: ${emotionLabels.join(', ')}`}
              >
                <RiEmotionLine /> {emotionLabels.join(', ')}
              </span>
            )}
            <button
              className={
                'paragraph-bookmark-toggle' +
                (bookmarked ? ' paragraph-bookmark-toggle-active' : '')
              }
              onClick={(e) => {
                e.stopPropagation()
                onToggleBookmark(chapterIdx, firstIdx)
              }}
              title={bookmarked ? 'Remove bookmark' : 'Bookmark this paragraph'}
            >
              {bookmarked ? <RiBookmarkFill /> : <RiBookmarkLine />}
            </button>
            <button
              className="paragraph-regenerate-toggle"
              disabled={regenerating}
              onClick={(e) => {
                e.stopPropagation()
                for (const p of segments) onRegenerate(chapterIdx, p.idx)
              }}
              title={regenerating ? 'Already generating…' : 'Regenerate this paragraph'}
            >
              <RiRefreshLine />
            </button>
            {annotationsView && (
              <button
                className={
                  'paragraph-generation-toggle' +
                  (showGenerationText ? ' paragraph-generation-toggle-active' : '')
                }
                onClick={(e) => {
                  e.stopPropagation()
                  setShowGenerationText((v) => !v)
                }}
                title={
                  showGenerationText
                    ? 'Hide the exact text sent to the voice clone'
                    : 'Show the exact text sent to the voice clone'
                }
              >
                <RiCodeSSlashLine />
              </button>
            )}
            <button
              className={
                'paragraph-sfx-toggle' +
                (sfxOpen ? ' paragraph-sfx-toggle-active' : '') +
                (segments[0].sfxStatus === 'ready' ? ' paragraph-sfx-toggle-ready' : '')
              }
              onClick={(e) => {
                e.stopPropagation()
                setSFXOpen((v) => !v)
              }}
              title={
                segments[0].sfxStatus === 'ready'
                  ? 'SFX test: sound effect ready'
                  : 'SFX test: generate a sound effect for this paragraph'
              }
            >
              <RiVolumeUpLine />
            </button>
          </>,
        )}
      </p>
      {annotationsView && showGenerationText && (
        <pre className="paragraph-generation-text">
          {segments.map((p) => {
            const marks = tableGenerationMarks(p)
            return (
              <div key={p.idx} className="paragraph-generation-text-segment">
                {p.table?.heading
                  ? '(silent: these headings are read with each row under them)'
                  : resolveGenerationText(p.text, marks.directionMarks, marks.pronunciationMarks)}
              </div>
            )
          })}
        </pre>
      )}
      {sfxOpen && (
        <div className="paragraph-sfx-panel" onClick={(e) => e.stopPropagation()}>
          <textarea
            className="paragraph-sfx-prompt"
            placeholder="Describe a sound effect, e.g. 'a door slamming shut'"
            value={sfxDraft ?? segments[0].sfxPrompt ?? ''}
            onChange={(e) => setSFXDraft(e.target.value)}
            onBlur={() => {
              const prompt = (sfxDraft ?? segments[0].sfxPrompt ?? '').trim()
              if (sfxDraft !== undefined && prompt !== (segments[0].sfxPrompt ?? '')) {
                onSetSFXPrompt(chapterIdx, firstIdx, prompt)
              }
            }}
            rows={2}
          />
          {segments[0].words.length > 0 && (
            <div className="paragraph-sfx-trigger-row">
              <span className="muted">Plays at word:</span>
              <div className="paragraph-sfx-trigger-words">
                {segments[0].words.map((w, i) => (
                  <button
                    key={i}
                    className={
                      'paragraph-sfx-trigger-word' +
                      (i === (segments[0].sfxTriggerWord ?? 0)
                        ? ' paragraph-sfx-trigger-word-active'
                        : '')
                    }
                    onClick={() =>
                      onSetSFXPrompt(
                        chapterIdx,
                        firstIdx,
                        sfxDraft ?? segments[0].sfxPrompt ?? '',
                        i,
                      )
                    }
                    title={`Start the sound effect at "${w.text}"`}
                  >
                    {w.text}
                  </button>
                ))}
              </div>
            </div>
          )}
          <div className="paragraph-sfx-panel-row">
            <button
              className="paragraph-sfx-generate"
              disabled={sfxBusy || !(sfxDraft ?? segments[0].sfxPrompt ?? '').trim()}
              onClick={() => {
                const prompt = (sfxDraft ?? segments[0].sfxPrompt ?? '').trim()
                setSFXBusy(true)
                setSFXLocalError('')
                onGenerateSFX(chapterIdx, firstIdx, prompt)
                  .catch((err) =>
                    setSFXLocalError(err instanceof Error ? err.message : String(err)),
                  )
                  .finally(() => setSFXBusy(false))
              }}
            >
              {sfxBusy
                ? 'Queuing…'
                : segments[0].sfxStatus === 'generating'
                  ? 'Generating…'
                  : segments[0].sfxStatus === 'ready'
                    ? 'Regenerate'
                    : 'Generate'}
            </button>
            {segments[0].sfxStatus === 'ready' && segments[0].sfxAudioUrl && (
              <audio className="paragraph-sfx-preview" controls src={segments[0].sfxAudioUrl} />
            )}
          </div>
          {(sfxLocalError || segments[0].sfxStatus === 'error') && (
            <p className="error-text">{sfxLocalError || segments[0].sfxError}</p>
          )}
        </div>
      )}
    </>
  )
}, paragraphGroupPropsEqual)

// groupContent rebuilds every group object on each ChapterSection render
// (any live patch to the chapter, e.g. one paragraph's audio finishing),
// so compare groups by what they hold instead: live patches keep an
// unchanged paragraph's own object identity (see api/live.ts applyOps),
// so a group whose paragraphs are all the same objects is unchanged.
// Without this every paragraph in the chapter - a couple hundred - re-
// rendered on each such patch, a several-hundred-ms stall mid-scroll.
function groupsEqual(a: ContentGroup, b: ContentGroup): boolean {
  if (a === b) return true
  if (a.kind !== b.kind || a.key !== b.key) return false
  if (a.kind === 'image') return a.imageUrl === (b as typeof a).imageUrl
  if (a.kind === 'break') return true
  const bp = (b as typeof a).paragraphs
  return a.paragraphs.length === bp.length && a.paragraphs.every((p, i) => p === bp[i])
}

// audioElement is the real, per-paragraph-swapping playback element only
// for the group that's actually playing (ParagraphText ignores it
// otherwise), and ChapterSection passes activeParagraphIdx to that group
// alone (null for the rest) - so a paragraph advance re-renders just the
// outgoing and incoming groups, not the whole playing chapter.
function paragraphGroupPropsEqual(prev: ParagraphGroupProps, next: ParagraphGroupProps): boolean {
  for (const key of Object.keys(next) as (keyof ParagraphGroupProps)[]) {
    if (key === 'group') {
      if (!groupsEqual(prev.group, next.group)) return false
    } else if (key === 'audioElement') {
      if (
        (prev.activeParagraphIdx !== null || next.activeParagraphIdx !== null) &&
        prev.audioElement !== next.audioElement
      ) {
        return false
      }
    } else if (prev[key] !== next[key]) {
      return false
    }
  }
  return true
}

// SceneBoundary marks where one of the chapter summary's scenes starts
// (annotations view only): where, when, and who's there - what ambience
// and lighting are told about the stretch.
function SceneBoundary({ scene }: { scene: SummaryScene }) {
  return (
    <div className="chapter-scene-boundary">
      <span className="chapter-scene-boundary-place">{scene.location || 'Scene'}</span>
      {scene.time && <span className="muted"> · {scene.time}</span>}
      {scene.present.length > 0 && <span className="muted"> — {scene.present.join(', ')}</span>}
    </div>
  )
}

// formatMusicStatus is MusicRegionBoundary's own status label for a region
// that hasn't finished generating yet (or failed) - a region that's
// already 'ready' shows no status suffix at all, since "this region's
// music is playing under these paragraphs" is the whole point and needs
// no further comment.
function formatMusicStatus(status: MusicRegion['status']): string | null {
  switch (status) {
    case 'pending':
      return 'not generated yet'
    case 'generating':
      return 'generating…'
    case 'error':
      return 'generation failed'
    default:
      return null
  }
}

// "m:ss" - PlayerBar's own formatTime, duplicated rather than imported
// since that one isn't exported and this is the only other place a clip
// duration (as opposed to formatDurationLong's coarser "Xh Ym" whole-book
// estimate) needs formatting.
function formatMusicDuration(seconds: number): string {
  const m = Math.floor(seconds / 60)
  const s = Math.floor(seconds % 60)
  return `${m}:${s.toString().padStart(2, '0')}`
}

// MusicRegionBoundary marks where a chapter background-music region starts
// (annotations view only - see ChapterSection's own musicRegionsByStartIdx)
// - the mood/prompt "score" a reader can otherwise only see indirectly by
// hearing it, plus whether this region continues smoothly out of the
// previous one or is a hard cut (store.MusicTransition), and its own
// generation status. A labeled divider, not a per-paragraph badge like
// paragraph-direction-hint/paragraph-speaker-hint - a region's mood
// applies to every paragraph until the next boundary, not just one, so
// repeating it per paragraph would be noise.
function MusicRegionBoundary({
  region,
  onShowTooltip,
  onHideTooltip,
  onRegenerate,
}: {
  region: MusicRegion
  onShowTooltip: (e: React.MouseEvent<HTMLElement>, text: string) => void
  onHideTooltip: () => void
  onRegenerate: (regionId: string) => void
}) {
  const status = formatMusicStatus(region.status)
  const durationLabel =
    region.status === 'ready' && region.durationSeconds > 0
      ? formatMusicDuration(region.durationSeconds)
      : null
  // Appended to the hover tooltip alongside the full prompt, not just
  // shown inline - the inline chip stays short (mood + transition/status),
  // so duration goes wherever a reader is already looking to read the rest
  // of this region's own "score" anyway.
  // The ambience prompt goes on its own line (the tooltip is pre-line) so
  // it doesn't read as a continuation of the music prompt itself.
  const musicText = durationLabel ? `${region.prompt} (${durationLabel})` : region.prompt
  const lighting = region.lighting
  const tooltipText = [
    musicText,
    region.ambience && `Ambience: ${region.ambience}`,
    lighting && `Lighting: ${lightingSummary(lighting)}`,
  ]
    .filter(Boolean)
    .join('\n')
  const busy = region.status === 'generating'
  return (
    <div
      className="chapter-music-boundary"
      // The app's own annotations-view hover tooltip (AnnotationTooltip,
      // via ReaderPage's showAnnotationTooltip/hideAnnotationTooltip) -
      // same mechanism the paragraph segments below use for their own
      // speaker/description text, rather than a native title attribute,
      // for the same reason: styled consistently and appears immediately
      // instead of the browser's own slow default tooltip delay.
      onMouseEnter={(e) => onShowTooltip(e, tooltipText)}
      onMouseMove={(e) => onShowTooltip(e, tooltipText)}
      onMouseLeave={onHideTooltip}
    >
      <RiMusic2Line />
      <span className="chapter-music-boundary-mood">{region.mood || 'Untitled region'}</span>
      <span className="chapter-music-boundary-meta">
        {region.transition === 'continuation' ? 'continues' : 'new cue'}
        {status && ` · ${status}`}
        {durationLabel && ` · ${durationLabel}`}
      </span>
      {/* The ambient soundscape layered under this region's music - just
          the start of its prompt inline (truncated like the mood label),
          the full text in the hover tooltip above. */}
      {region.ambience && (
        <span className="chapter-music-boundary-ambience">
          <RiWindyLine />
          <span className="chapter-music-boundary-ambience-text">{region.ambience}</span>
        </span>
      )}
      {/* The region's room-lighting cue (the ambience pass's lighting
          sub-pass): a swatch animated like the effect the lights play,
          plus the start of its description; the rest is in the tooltip. */}
      {lighting && (
        <span className="chapter-music-boundary-lighting">
          <RiLightbulbLine />
          <LightingSwatch lighting={lighting} />
          <span className="chapter-music-boundary-ambience-text">
            {lighting.description || LIGHT_EFFECT_LABELS[lighting.effect]}
          </span>
        </span>
      )}
      {region.status === 'ready' && region.audioUrl && (
        <MusicRegionPreviewButton audioUrl={region.audioUrl} />
      )}
      <button
        className="chapter-music-boundary-regenerate"
        disabled={busy}
        onClick={(e) => {
          e.stopPropagation()
          onRegenerate(region.id)
        }}
        title={
          busy
            ? 'Generating…'
            : region.status === 'ready'
              ? 'Regenerate this region’s music'
              : 'Generate this region’s music'
        }
      >
        <RiRefreshLine className={busy ? 'spin' : undefined} />
      </button>
    </div>
  )
}

// lightingSummary is a lighting cue as one tooltip line: "dim candlelight
// in a cellar - flickering, 30% (#ff9a3c -> #ffd27a)".
function lightingSummary(l: Lighting): string {
  const colors = l.accent ? `${l.color} -> ${l.accent}` : l.color
  const what = l.description ? `${l.description} - ` : ''
  return `${what}${LIGHT_EFFECT_LABELS[l.effect]}, ${l.brightness}% (${colors})`
}

interface ChapterSectionProps {
  idx: number
  chapter: ChapterDetail | undefined
  chapterTitle: string
  chapterAttributing: boolean
  chapterClearing: boolean
  chapterReimporting: boolean
  hasAttribution: boolean
  // The rest of the chapter-header action buttons - BookSettingsPage's own
  // per-chapter row, mirrored here so a reader can run any of these
  // without leaving the reader for the Book settings page. All job-queue-
  // derived (see useScareQuotingChapters/
  // useDirectingChapters/useAmbienceChapters/useGeneratingChapters),
  // the same as BookSettingsPage's own per-row booleans.
  chapterRetaggingScareQuotes: boolean
  chapterDirecting: boolean
  chapterPronouncing: boolean
  chapterRunningAmbience: boolean
  chapterGenerating: boolean
  // Passes.direction/Passes.pronunciation/Passes.ambience - only used for the
  // direction/pronunciation/music buttons' own "Tag"/"Re-tag"-style title
  // wording, the same way hasAttribution already does for the attribute
  // button.
  hasDirection: boolean
  hasPronunciation: boolean
  hasMusic: boolean
  // c.readyCount >= c.paragraphCount (BookSettingsPage's own isGenerated) -
  // only used for the generate button's own title wording.
  isGenerated: boolean
  // Windowing (see ReaderPage's chapterHeights/registerHeight): false once
  // this chapter has been measured at least once and sits outside the
  // render window around whatever chapter is actually on screen - instead
  // of its heading + full paragraph list, this renders as a single sized
  // spacer (placeholderHeight, its own last-measured height) so the
  // scrollbar and surrounding layout stay exactly where they were, without
  // this chapter's DOM actually existing. A book scrolled through for a
  // long session can have hundreds of loaded chapters; without this, every
  // one of them stays mounted forever (see groupContent's callers) and
  // every unrelated re-render (ChapterSection's own memo aside) still pays
  // layout/paint cost proportional to how much has ever been scrolled
  // through, not to what's actually on screen.
  renderFull: boolean
  placeholderHeight: number | undefined
  annotationsView: boolean
  selectedSpeaker: string | null
  isActiveChapter: boolean
  activeParagraphIdx: number | null
  audioElement: HTMLAudioElement
  bookmarkByKey: Map<string, string>
  onClearAudio: (idx: number, title: string) => void
  onReimport: (idx: number, title: string) => void
  onAttribute: (idx: number) => void
  onAttributeAroundEdits: (idx: number) => void
  onSaveNotes: (idx: number, notes: ChapterNotes) => Promise<void>
  onRegenerateNotes: (idx: number) => void
  onRetagScareQuotes: (idx: number) => void
  onTagDirections: (idx: number) => void
  onResolvePronunciation: (idx: number) => void
  onRunAmbience: (idx: number) => void
  onGenerate: (idx: number) => void
  // This chapter's own background-music regions (see backend
  // store.MusicRegion) - annotations view uses these to mark each region's
  // own start boundary and show its mood/score, the same way it already
  // surfaces speaker/direction info. Empty for a chapter that hasn't been
  // scored, or while annotations view is off (ReaderPage only fetches
  // music regions at all while annotationsView is true - see its own
  // useChapterMusicRange call).
  musicRegions: MusicRegion[]
  // The chapter's summary notes (annotations view only), null until
  // summarized or while annotations view is off.
  notes: ChapterNotes | null
  // The paragraph the open "previously on" recap sits above (RecapPanel
  // renders into the element handed to registerRecapSlot), null when it
  // isn't open in this chapter.
  recapSlotIdx: number | null
  registerRecapSlot: (el: HTMLElement | null) => void
  onRegenerateMusic: (chapterIdx: number, regionId: string) => void
  registerSectionRef: (idx: number, el: HTMLElement | null) => void
  onSeek: (seconds: number) => void
  onPlayAt: (chapterIdx: number, paragraphIdx: number) => void
  onToggleBookmark: (chapterIdx: number, paragraphIdx: number) => void
  onRegenerate: (chapterIdx: number, paragraphIdx: number) => void
  onOpenSpeakerMenu: (e: React.MouseEvent, chapterIdx: number, p: Paragraph) => void
  onShowTooltip: (e: React.MouseEvent<HTMLElement>, text: string) => void
  onHideTooltip: () => void
  registerParagraphRef: (key: string, el: HTMLElement | null) => void
  onSetSFXPrompt: (
    chapterIdx: number,
    paragraphIdx: number,
    prompt: string,
    triggerWord?: number,
  ) => void
  onGenerateSFX: (chapterIdx: number, paragraphIdx: number, prompt: string) => Promise<void>
}

// One loaded chapter's worth of the infinite-scroll reader - memoized so
// that, of everything ReaderPage might re-render for (a mousemove
// elsewhere, an audio timeupdate tick, an unrelated bookmark toggle), only
// the chapter(s) whose own props actually changed re-render. This is what
// keeps the reader's per-frame cost bounded by "what changed" rather than
// "how many chapters have been scrolled through so far" - without it,
// every loaded chapter's full paragraph list gets reconciled on every one
// of those frequent, unrelated updates.
export const ChapterSection = memo(function ChapterSection({
  idx,
  chapter,
  chapterTitle,
  chapterAttributing,
  chapterClearing,
  chapterReimporting,
  hasAttribution,
  chapterRetaggingScareQuotes,
  chapterDirecting,
  chapterPronouncing,
  chapterRunningAmbience,
  chapterGenerating,
  hasDirection,
  hasPronunciation,
  hasMusic,
  isGenerated,
  renderFull,
  placeholderHeight,
  annotationsView,
  selectedSpeaker,
  isActiveChapter,
  activeParagraphIdx,
  audioElement,
  bookmarkByKey,
  onClearAudio,
  onReimport,
  onAttribute,
  onAttributeAroundEdits,
  onSaveNotes,
  onRegenerateNotes,
  onRetagScareQuotes,
  onTagDirections,
  onResolvePronunciation,
  onRunAmbience,
  onGenerate,
  musicRegions,
  notes,
  onRegenerateMusic,
  registerSectionRef,
  onSeek,
  onPlayAt,
  onToggleBookmark,
  onRegenerate,
  onOpenSpeakerMenu,
  onShowTooltip,
  onHideTooltip,
  registerParagraphRef,
  onSetSFXPrompt,
  onGenerateSFX,
  recapSlotIdx,
  registerRecapSlot,
}: ChapterSectionProps) {
  if (!renderFull) {
    return (
      <section
        className="chapter-block chapter-block-placeholder"
        data-chapter-idx={idx}
        ref={(el) => registerSectionRef(idx, el)}
        style={{ height: placeholderHeight }}
      />
    )
  }

  const groups = chapter ? groupContent(chapter.content, chapter.paragraphs) : []
  // Keyed by MusicRegion.startIdx so each text group can cheaply check
  // "does a region start exactly here" as we walk groups in order below -
  // regions are contiguous/non-overlapping (see store.MusicRegion's own
  // doc comment), so a group's first paragraph idx matching a region's
  // startIdx is exactly "annotations view should show that region's own
  // boundary marker right before this group".
  const musicRegionsByStartIdx = new Map(musicRegions.map((r) => [r.startIdx, r]))
  // Scene starts, keyed the same way - a scene always starts at a real
  // paragraph (see backend speakerattr.sceneStarts), so at a group's first.
  const scenesByStartIdx = new Map((notes?.scenes ?? []).map((sc) => [sc.startIdx, sc]))

  // One group with whatever annotations start at it.
  const renderGroup = (group: ContentGroup) => {
    const startingRegion =
      group.kind === 'text' ? musicRegionsByStartIdx.get(group.paragraphs[0].idx) : undefined
    const startingScene =
      group.kind === 'text' ? scenesByStartIdx.get(group.paragraphs[0].idx) : undefined
    return (
      <Fragment key={group.key}>
        {annotationsView && startingScene && <SceneBoundary scene={startingScene} />}
        {annotationsView && startingRegion && (
          <MusicRegionBoundary
            region={startingRegion}
            onShowTooltip={onShowTooltip}
            onHideTooltip={onHideTooltip}
            onRegenerate={(regionId) => onRegenerateMusic(idx, regionId)}
          />
        )}
        <ParagraphGroup
          group={group}
          chapterIdx={idx}
          annotationsView={annotationsView}
          selectedSpeaker={selectedSpeaker}
          isActiveChapter={isActiveChapter}
          activeParagraphIdx={
            isActiveChapter &&
            group.kind === 'text' &&
            group.paragraphs.some((p) => p.idx === activeParagraphIdx)
              ? activeParagraphIdx
              : null
          }
          audioElement={audioElement}
          bookmarked={
            group.kind === 'text' ? bookmarkByKey.has(`${idx}:${group.paragraphs[0].idx}`) : false
          }
          onSeek={onSeek}
          onPlayAt={onPlayAt}
          onToggleBookmark={onToggleBookmark}
          onRegenerate={onRegenerate}
          onOpenSpeakerMenu={onOpenSpeakerMenu}
          onShowTooltip={onShowTooltip}
          onHideTooltip={onHideTooltip}
          registerParagraphRef={registerParagraphRef}
          onSetSFXPrompt={onSetSFXPrompt}
          onGenerateSFX={onGenerateSFX}
        />
      </Fragment>
    )
  }
  // A table's paragraphs go in one grid, a rule between its rows. Placing
  // is left to the grid: each paragraph names only its columns, so a
  // cell's second paragraph drops under its first while the next cell
  // along sits beside it, and the rule (full width) starts each new row.
  const rendered: ReactNode[] = []
  // Where the recap goes: above the paragraph it was opened at (above the
  // whole table, for one inside a table).
  const holdsRecapSlot = (g: ContentGroup) =>
    recapSlotIdx !== null && g.kind === 'text' && g.paragraphs.some((p) => p.idx === recapSlotIdx)
  const recapSlot = <div key="recap-slot" className="recap-slot" ref={registerRecapSlot} />
  for (let i = 0; i < groups.length;) {
    const table = groupTable(groups[i])
    if (!table) {
      if (holdsRecapSlot(groups[i])) rendered.push(recapSlot)
      rendered.push(renderGroup(groups[i]))
      i++
      continue
    }
    const items: ReactNode[] = []
    let columns = 0
    let row = table.row
    const start = i
    for (; i < groups.length; i++) {
      const t = groupTable(groups[i])
      if (!t || t.table !== table.table) break
      if (holdsRecapSlot(groups[i])) rendered.push(recapSlot)
      columns = Math.max(columns, tableColumnEnd(t))
      if (t.row !== row) {
        items.push(<span key={`rule-${groups[i].key}`} className="chapter-table-rule" />)
        row = t.row
      }
      items.push(renderGroup(groups[i]))
    }
    rendered.push(
      <div
        key={`table-${groups[start].key}`}
        className="chapter-table"
        style={{ gridTemplateColumns: `repeat(${columns}, auto)` }}
      >
        {items}
      </div>,
    )
  }

  return (
    <section
      className="chapter-block"
      data-chapter-idx={idx}
      // Set only once this chapter's real data has arrived - the
      // ResizeObserver in ReaderPage ignores a resize on a section without
      // this, so a chapter that falls out of the render window while still
      // showing "Loading chapter…" can never get its placeholder stuck at
      // that skeleton's own tiny height instead of its real, eventual one.
      data-loaded={chapter ? '1' : undefined}
      ref={(el) => registerSectionRef(idx, el)}
    >
      <div className="chapter-heading-row">
        <h2 className="chapter-heading">{chapterTitle}</h2>
        <span className="chapter-heading-actions">
          <button
            className="icon-action-button"
            onClick={() => onClearAudio(idx, chapterTitle)}
            disabled={chapterClearing}
            title="Clear generated audio for this chapter"
          >
            <RiDeleteBinLine className={chapterClearing ? 'spin' : undefined} />
          </button>
          <button
            className="icon-action-button"
            onClick={() => onReimport(idx, chapterTitle)}
            disabled={chapterReimporting}
            title="Re-import this chapter from the book's epub (resets its speakers, emotions and audio)"
          >
            <RiFileHistoryLine className={chapterReimporting ? 'spin' : undefined} />
          </button>
          <button
            className="icon-action-button"
            onClick={() => onAttribute(idx)}
            disabled={chapterAttributing}
            title={
              chapterAttributing
                ? 'Attributing…'
                : hasAttribution
                  ? 'Re-run speaker attribution for this chapter'
                  : 'Run speaker attribution for this chapter'
            }
          >
            <RiChatVoiceLine className={chapterAttributing ? 'spin' : undefined} />
          </button>
          {hasAttribution && (
            <button
              className="icon-action-button"
              onClick={() => onAttributeAroundEdits(idx)}
              disabled={chapterAttributing}
              title="Re-attribute around my fixes — re-ask only the lines near speakers you corrected in this chapter, with your corrections shown to the model"
            >
              <RiChatCheckLine className={chapterAttributing ? 'spin' : undefined} />
            </button>
          )}
          <button
            className="icon-action-button"
            onClick={() => onRetagScareQuotes(idx)}
            disabled={chapterRetaggingScareQuotes}
            title={
              chapterRetaggingScareQuotes
                ? 'Retagging…'
                : "Retag scare quotes — re-run scare-quote tagging for this chapter (quoted spans that look like dialogue but aren't actually spoken aloud)"
            }
          >
            <RiDoubleQuotesL className={chapterRetaggingScareQuotes ? 'spin' : undefined} />
          </button>
          <button
            className="icon-action-button"
            onClick={() => onTagDirections(idx)}
            disabled={chapterDirecting}
            title={
              chapterDirecting
                ? 'Tagging…'
                : hasDirection
                  ? 'Re-tag speech direction for this chapter'
                  : 'Tag speech direction for this chapter'
            }
          >
            <RiEmotionLine className={chapterDirecting ? 'spin' : undefined} />
          </button>
          <button
            className="icon-action-button"
            onClick={() => onResolvePronunciation(idx)}
            disabled={chapterPronouncing}
            title={
              chapterPronouncing
                ? 'Resolving…'
                : hasPronunciation
                  ? 'Re-resolve pronunciation for this chapter (ambiguous abbreviations like "Dr." or "St.")'
                  : 'Resolve pronunciation for this chapter (ambiguous abbreviations like "Dr." or "St.")'
            }
          >
            <RiSpeakLine className={chapterPronouncing ? 'spin' : undefined} />
          </button>
          <button
            className="icon-action-button"
            onClick={() => onRunAmbience(idx)}
            disabled={chapterRunningAmbience}
            title={
              chapterRunningAmbience
                ? 'Running ambience…'
                : hasMusic
                  ? 'Re-run ambience (music + lighting) for this chapter'
                  : 'Run ambience (music + lighting) for this chapter'
            }
          >
            <RiMusic2Line className={chapterRunningAmbience ? 'spin' : undefined} />
          </button>
          <button
            className="icon-action-button"
            onClick={() => onGenerate(idx)}
            disabled={chapterGenerating}
            title={
              chapterGenerating
                ? 'Generating…'
                : isGenerated
                  ? 'Already fully generated — click to check for background music if it was turned on since'
                  : 'Generate audio for this chapter'
            }
          >
            <RiVoiceprintLine className={chapterGenerating ? 'spin' : undefined} />
          </button>
        </span>
      </div>
      {annotationsView && notes && (
        <ChapterNotesBlock
          chapterIdx={idx}
          notes={notes}
          onSave={onSaveNotes}
          onRegenerate={onRegenerateNotes}
          onAttribute={onAttribute}
        />
      )}
      {!chapter && <p className="muted">Loading chapter…</p>}
      <div className="paragraph-list">{rendered}</div>
    </section>
  )
})
