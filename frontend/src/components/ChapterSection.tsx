import { Fragment, memo, useEffect, useRef, useState } from 'react'
import {
  RiBookmarkFill,
  RiBookmarkLine,
  RiChatVoiceLine,
  RiCodeSSlashLine,
  RiDeleteBinLine,
  RiDoubleQuotesL,
  RiEmotionLine,
  RiMusic2Line,
  RiPauseFill,
  RiPlayFill,
  RiPriceTag3Line,
  RiRefreshLine,
  RiUser3Line,
  RiVoiceprintLine,
  RiVolumeUpLine,
} from 'react-icons/ri'
import type { ChapterDetail, ContentItem, MusicRegion, Paragraph } from '../api/types'
import { hasActiveTextSelection } from '../utils/selection'
import { formatDirectionTag } from '../utils/directionTags'
import { annotationKind, annotationTitle, matchesSelectedSpeaker } from '../utils/annotations'
import { resolveGenerationText } from '../utils/resolveGenerationText'
import { ParagraphText } from './ParagraphText'

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
  onSetSFXPrompt: (chapterIdx: number, paragraphIdx: number, prompt: string, triggerWord?: number) => void
  onGenerateSFX: (chapterIdx: number, paragraphIdx: number, prompt: string) => Promise<void>
}

// One visual paragraph (image, or one-or-more joined text segments) -
// memoized so that a state change elsewhere in the reader (a mouse move
// over an unrelated paragraph, a playback tick, a scroll-visibility
// update) doesn't force every other paragraph in every loaded chapter to
// re-render along with it. Only re-renders when its own group data,
// active/annotation state, or bookmark status actually changes.
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
  // reflected by segments[0].sfxStatus once the chapter query's poll
  // catches up, not by this promise settling.
  const [sfxOpen, setSFXOpen] = useState(false)
  const [sfxDraft, setSFXDraft] = useState<string | undefined>(undefined)
  const [sfxBusy, setSFXBusy] = useState(false)
  const [sfxLocalError, setSFXLocalError] = useState('')

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
  const directionTags = [
    ...new Set(segments.flatMap((p) => (p.directionMarks ?? []).map((m) => m.tag))),
  ].map(formatDirectionTag)
  const regenerating = segments.some((p) => p.audioStatus === 'generating')

  return (
    <>
      <p
        className={
          'paragraph' +
          (groupActive ? ' paragraph-active' : '') +
          (groupError ? ' paragraph-error' : '')
        }
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
              ref={(el) => registerParagraphRef(`${chapterIdx}:${p.idx}`, el)}
              className={
                (segmentPending ? 'segment-pending ' : '') +
                (annotationsView
                  ? `annotation-${annotationKind(p)}` +
                    (selectedSpeaker && matchesSelectedSpeaker(p, selectedSpeaker) ? ' annotation-selected-speaker' : '') +
                    (p.scareQuote ? ' annotation-scare-quote' : '')
                  : '')
              }
              onMouseEnter={annotationsView ? (e) => onShowTooltip(e, annotationTitle(p)) : undefined}
              onMouseMove={annotationsView ? (e) => onShowTooltip(e, annotationTitle(p)) : undefined}
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
              <ParagraphText
                text={p.text}
                active={isActive}
                audioElement={audioElement}
                duration={p.durationSeconds ?? 0}
                words={p.words}
                onSeek={onSeek}
                marks={annotationsView ? (p.directionMarks ?? []) : []}
                pronunciationMarks={annotationsView ? (p.pronunciationMarks ?? []) : []}
              />
              {p.audioStatus === 'error' && <span className="error-text"> (audio failed: {p.audioError})</span>}
            </span>
          )
        })}
        {speakers.length > 0 && (
          <span
            className={'paragraph-speaker-hint' + (annotationsView ? ' paragraph-speaker-hint-visible' : '')}
            title={`Speaker: ${speakers.join(', ')}`}
          >
            <RiUser3Line /> {speakers.join(', ')}
          </span>
        )}
        {directionTags.length > 0 && (
          <span
            className={'paragraph-direction-hint' + (annotationsView ? ' paragraph-direction-hint-visible' : '')}
            title={`Delivery: ${directionTags.join(', ')}`}
          >
            <RiEmotionLine /> {directionTags.join(', ')}
          </span>
        )}
        <button
          className={'paragraph-bookmark-toggle' + (bookmarked ? ' paragraph-bookmark-toggle-active' : '')}
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
              'paragraph-generation-toggle' + (showGenerationText ? ' paragraph-generation-toggle-active' : '')
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
          className={'paragraph-sfx-toggle' + (sfxOpen ? ' paragraph-sfx-toggle-active' : '') + (segments[0].sfxStatus === 'ready' ? ' paragraph-sfx-toggle-ready' : '')}
          onClick={(e) => {
            e.stopPropagation()
            setSFXOpen((v) => !v)
          }}
          title={segments[0].sfxStatus === 'ready' ? 'SFX test: sound effect ready' : 'SFX test: generate a sound effect for this paragraph'}
        >
          <RiVolumeUpLine />
        </button>
      </p>
      {annotationsView && showGenerationText && (
        <pre className="paragraph-generation-text">
          {segments.map((p) => (
            <div key={p.idx} className="paragraph-generation-text-segment">
              {resolveGenerationText(p.text, p.directionMarks, p.pronunciationMarks)}
            </div>
          ))}
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
                    className={'paragraph-sfx-trigger-word' + (i === (segments[0].sfxTriggerWord ?? 0) ? ' paragraph-sfx-trigger-word-active' : '')}
                    onClick={() => onSetSFXPrompt(chapterIdx, firstIdx, sfxDraft ?? segments[0].sfxPrompt ?? '', i)}
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
                  .catch((err) => setSFXLocalError(err instanceof Error ? err.message : String(err)))
                  .finally(() => setSFXBusy(false))
              }}
            >
              {sfxBusy ? 'Queuing…' : segments[0].sfxStatus === 'generating' ? 'Generating…' : segments[0].sfxStatus === 'ready' ? 'Regenerate' : 'Generate'}
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
})

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
  const durationLabel = region.status === 'ready' && region.durationSeconds > 0 ? formatMusicDuration(region.durationSeconds) : null
  // Appended to the hover tooltip alongside the full prompt, not just
  // shown inline - the inline chip stays short (mood + transition/status),
  // so duration goes wherever a reader is already looking to read the rest
  // of this region's own "score" anyway.
  const tooltipText = durationLabel ? `${region.prompt} (${durationLabel})` : region.prompt
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
      {region.status === 'ready' && region.audioUrl && <MusicRegionPreviewButton audioUrl={region.audioUrl} />}
      <button
        className="chapter-music-boundary-regenerate"
        disabled={busy}
        onClick={(e) => {
          e.stopPropagation()
          onRegenerate(region.id)
        }}
        title={busy ? 'Generating…' : region.status === 'ready' ? 'Regenerate this region’s music' : 'Generate this region’s music'}
      >
        <RiRefreshLine className={busy ? 'spin' : undefined} />
      </button>
    </div>
  )
}

// A standalone, isolated preview of one region's own generated clip - its
// own plain <audio> (not tied to usePlayback's narration element or
// useBackgroundMusic's own live-mixing Web Audio graph at all), the same
// "quick audition, nothing else on the page needs to know about it" shape
// the SFX panel's own <audio controls> preview already has, just a single
// icon toggle instead of the browser's own full control bar (this sits
// inside a small pill, not a whole panel). Playing a preview does not
// pause/duck narration or the live background-music mix - both keep
// running underneath, exactly like clicking the SFX panel's preview does.
function MusicRegionPreviewButton({ audioUrl }: { audioUrl: string }) {
  const audioRef = useRef<HTMLAudioElement | null>(null)
  const [playing, setPlaying] = useState(false)

  if (!audioRef.current) {
    const audio = new Audio(audioUrl)
    audio.addEventListener('ended', () => setPlaying(false))
    audio.addEventListener('pause', () => setPlaying(false))
    audioRef.current = audio
  }

  // Stops playback if this marker's own chapter scrolls out of the render
  // window (ChapterSection's renderFull/placeholderHeight) and unmounts
  // mid-preview - otherwise the clip would keep playing invisibly with no
  // way left on screen to stop it.
  useEffect(() => {
    return () => {
      audioRef.current?.pause()
    }
  }, [])

  return (
    <button
      className="chapter-music-boundary-preview"
      onClick={(e) => {
        e.stopPropagation()
        const audio = audioRef.current!
        if (playing) {
          audio.pause()
        } else {
          void audio.play()
          setPlaying(true)
        }
      }}
      title={playing ? 'Pause preview' : 'Preview this region’s generated music'}
    >
      {playing ? <RiPauseFill /> : <RiPlayFill />}
    </button>
  )
}

interface ChapterSectionProps {
  idx: number
  chapter: ChapterDetail | undefined
  chapterTitle: string
  chapterAttributing: boolean
  chapterClearing: boolean
  hasAttribution: boolean
  // The rest of the chapter-header action buttons - SpeakersPage's own
  // per-chapter row, mirrored here so a reader can run any of these
  // without leaving the reader for the Speakers page. chapterRetagging*
  // are locally-tracked (blocking mutations, like SpeakersPage's own
  // retaggingIdx/retaggingScareQuoteIdx), chapterDirecting/
  // chapterScoringMusic/chapterGenerating are job-queue-derived (see
  // useDirectingChapters/useScoringMusicChapters/useGeneratingChapters) -
  // same split SpeakersPage's own per-row booleans use, for the same
  // reasons documented on each hook.
  chapterRetaggingDescriptions: boolean
  chapterRetaggingScareQuotes: boolean
  chapterDirecting: boolean
  chapterScoringMusic: boolean
  chapterGenerating: boolean
  // Passes.direction/Passes.music - only used for the direction/music
  // buttons' own "Tag"/"Re-tag" and "Score"/"Re-score" title wording, the
  // same way hasAttribution already does for the attribute button.
  hasDirection: boolean
  hasMusic: boolean
  // c.readyCount >= c.paragraphCount (SpeakersPage's own isGenerated) -
  // only used for the generate button's own title wording.
  isGenerated: boolean
  // Mirrors SpeakersPage's own directionSupported: the book's resolved
  // clone model isn't Higgs, so the direction-tagging button (whose tag
  // vocabulary is Higgs-specific) is left out entirely rather than shown
  // disabled - same "don't offer an action that can't work" reasoning.
  directionSupported: boolean
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
  onAttribute: (idx: number) => void
  onRetagDescriptions: (idx: number) => void
  onRetagScareQuotes: (idx: number) => void
  onTagDirections: (idx: number) => void
  onScoreMusic: (idx: number) => void
  onGenerate: (idx: number) => void
  // This chapter's own background-music regions (see backend
  // store.MusicRegion) - annotations view uses these to mark each region's
  // own start boundary and show its mood/score, the same way it already
  // surfaces speaker/direction info. Empty for a chapter that hasn't been
  // scored, or while annotations view is off (ReaderPage only fetches
  // music regions at all while annotationsView is true - see its own
  // useChapterMusicRange call).
  musicRegions: MusicRegion[]
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
  onSetSFXPrompt: (chapterIdx: number, paragraphIdx: number, prompt: string, triggerWord?: number) => void
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
  hasAttribution,
  chapterRetaggingDescriptions,
  chapterRetaggingScareQuotes,
  chapterDirecting,
  chapterScoringMusic,
  chapterGenerating,
  hasDirection,
  hasMusic,
  isGenerated,
  directionSupported,
  renderFull,
  placeholderHeight,
  annotationsView,
  selectedSpeaker,
  isActiveChapter,
  activeParagraphIdx,
  audioElement,
  bookmarkByKey,
  onClearAudio,
  onAttribute,
  onRetagDescriptions,
  onRetagScareQuotes,
  onTagDirections,
  onScoreMusic,
  onGenerate,
  musicRegions,
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
          <button
            className="icon-action-button"
            onClick={() => onRetagDescriptions(idx)}
            disabled={chapterRetaggingDescriptions}
            title={
              chapterRetaggingDescriptions
                ? 'Retagging…'
                : "Retag descriptions — re-run description-tagging for this chapter (which narration paragraphs describe a character's appearance or personality)"
            }
          >
            <RiPriceTag3Line className={chapterRetaggingDescriptions ? 'spin' : undefined} />
          </button>
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
          {directionSupported && (
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
          )}
          <button
            className="icon-action-button"
            onClick={() => onScoreMusic(idx)}
            disabled={chapterScoringMusic}
            title={
              chapterScoringMusic
                ? 'Scoring…'
                : hasMusic
                  ? 'Re-score background music for this chapter'
                  : 'Score background music for this chapter'
            }
          >
            <RiMusic2Line className={chapterScoringMusic ? 'spin' : undefined} />
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
      {!chapter && <p className="muted">Loading chapter…</p>}
      <div className="paragraph-list">
        {groups.map((group) => {
          const startingRegion = group.kind === 'text' ? musicRegionsByStartIdx.get(group.paragraphs[0].idx) : undefined
          return (
            <Fragment key={group.key}>
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
                activeParagraphIdx={activeParagraphIdx}
                audioElement={audioElement}
                bookmarked={group.kind === 'text' ? bookmarkByKey.has(`${idx}:${group.paragraphs[0].idx}`) : false}
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
        })}
      </div>
    </section>
  )
})
