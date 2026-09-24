import { useLayoutEffect, useMemo, useRef, useState } from 'react'
import {
  RiArrowDropUpLine,
  RiBook2Line,
  RiBookOpenLine,
  RiBookmarkLine,
  RiEmotionLine,
  RiFileTextLine,
  RiFontSize,
  RiGitMergeLine,
  RiIndentIncrease,
  RiPaletteLine,
  RiPauseFill,
  RiPlayFill,
  RiRefreshLine,
  RiSkipForwardMiniLine,
  RiUser3Line,
  RiUserHeartLine,
} from 'react-icons/ri'
import type { PlaybackController } from '../hooks/usePlayback'
import type { ReaderFontFamily } from '../hooks/useReaderTypography'
import { ANNOTATION_LABELS } from '../api/types'
import type { AnnotationKind, ChapterDetail, ChapterSummary, Speaker } from '../api/types'
import { DIRECTION_TAG_CATEGORY_LABELS, type DirectionTagCategory } from '../utils/directionTags'
import { VoicePanel } from './VoicePanel'
import { ReaderTypographyPanel } from './ReaderTypographyPanel'
import { BookSearch } from './BookSearch'
import { BookmarksPanel } from './BookmarksPanel'
import { SleepTimerButton } from './SleepTimerButton'
import { formatCountdown, useSleepTimer } from '../hooks/useSleepTimer'
import { formatDurationLong } from '../utils/time'
import { useClickOutside } from '../hooks/useClickOutside'
import { useCharacterizeSpeaker, useMergeCharacter, useRegenerateCharacterVoices } from '../api/queries'
import { ApiError } from '../api/client'

const RATES = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 4, 5]

function formatTime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return '0:00'
  const m = Math.floor(seconds / 60)
  const s = Math.floor(seconds % 60)
  return `${m}:${s.toString().padStart(2, '0')}`
}

function wordCount(text: string): number {
  const trimmed = text.trim()
  return trimmed === '' ? 0 : trimmed.split(/\s+/).length
}

export function PlayerBar({
  bookId,
  chapter,
  bookChapters,
  playback,
  autoFollow,
  onJumpToCurrent,
  onJumpToChapter,
  onJumpToParagraph,
  progressPercent,
  estimatedTotalSeconds,
  finished,
  annotationsView,
  onToggleAnnotationsView,
  speakers,
  selectedSpeaker,
  onSelectSpeaker,
  scopeChapter,
  onJumpToNextAppearance,
  readerFontSize,
  readerFontFamily,
  onReaderFontSizeChange,
  onReaderFontFamilyChange,
}: {
  bookId: string
  chapter: ChapterDetail | undefined
  bookChapters: ChapterSummary[]
  playback: PlaybackController
  autoFollow: boolean
  onJumpToCurrent: () => void
  onJumpToChapter: (idx: number) => void
  onJumpToParagraph: (chapterIdx: number, paragraphIdx: number) => void
  progressPercent: number
  estimatedTotalSeconds: number
  finished: boolean
  annotationsView: boolean
  onToggleAnnotationsView: () => void
  speakers: Speaker[]
  selectedSpeaker: string | null
  onSelectSpeaker: (name: string | null) => void
  scopeChapter: ChapterDetail | undefined
  onJumpToNextAppearance: (name: string) => void
  readerFontSize: number
  readerFontFamily: ReaderFontFamily
  onReaderFontSizeChange: (size: number) => void
  onReaderFontFamilyChange: (family: ReaderFontFamily) => void
}) {
  const [voiceOpen, setVoiceOpen] = useState(false)
  const voiceButtonRef = useRef<HTMLButtonElement>(null)
  const voicePopoverRef = useRef<HTMLDivElement>(null)
  useClickOutside([voiceButtonRef, voicePopoverRef], () => setVoiceOpen(false), voiceOpen)

  const [typographyOpen, setTypographyOpen] = useState(false)
  const typographyButtonRef = useRef<HTMLButtonElement>(null)
  const typographyPopoverRef = useRef<HTMLDivElement>(null)
  useClickOutside([typographyButtonRef, typographyPopoverRef], () => setTypographyOpen(false), typographyOpen)

  const sleepTimer = useSleepTimer(playback.chapterIdx, playback.pause)

  // "This chapter" scope toggle - chapterSpeakerNames is who actually
  // speaks/narrates in whichever chapter is scrolled into view
  // (scopeChapter, tracked in ReaderPage via IntersectionObserver -
  // deliberately not the `chapter` prop, which follows playback position
  // and can be scrolled off-screen while reading ahead/back), same
  // Narrator normalization matchesSelectedSpeaker/annotationKind use
  // elsewhere ("" or "Narrator" both mean the book's own voice).
  const [scopeToChapter, setScopeToChapter] = useState(false)
  const chapterSpeakerNames = useMemo(() => {
    const names = new Set<string>()
    for (const p of scopeChapter?.paragraphs ?? []) names.add(p.speaker || 'Narrator')
    return names
  }, [scopeChapter])

  const [speakerFilter, setSpeakerFilter] = useState('')
  const filteredSpeakers = useMemo(() => {
    const q = speakerFilter.trim().toLowerCase()
    let list = speakers
    if (scopeToChapter) list = list.filter((s) => chapterSpeakerNames.has(s.name))
    if (q !== '') list = list.filter((s) => s.name.toLowerCase().includes(q))
    return list
  }, [speakers, speakerFilter, scopeToChapter, chapterSpeakerNames])

  // Merge button on each speaker-list row - folds one character into
  // another (or into "Narrator") via the same endpoint SpeakersPage's own
  // MergeCharacterPanel uses. Only real characters (s.id truthy) get the
  // button - the synthesized Narrator row (id "") has no characterId to
  // merge from. mergingId tracks which single row has its inline
  // target-picker expanded, mirroring MergeCharacterPanel's own
  // select-then-confirm shape rather than AppearanceRow's fire-on-select
  // one (a character merge is series-wide and not easily undone, so it
  // gets the extra confirm step).
  const mergeCharacter = useMergeCharacter(bookId)
  const [mergingId, setMergingId] = useState<string | null>(null)
  const [mergeTarget, setMergeTarget] = useState('')
  const [mergeError, setMergeError] = useState<string | null>(null)

  // Recharacterize/regenerate-voice buttons on each speaker-list row,
  // mirroring SpeakersPage's own runCharacterize/useRegenerateCharacterVoices
  // - a Set (not a single id) for the same reason SpeakersPage's
  // characterizingIds is one: two rows firing independently shouldn't
  // clobber each other's spinner.
  const characterizeSpeaker = useCharacterizeSpeaker(bookId)
  const [characterizingIds, setCharacterizingIds] = useState<Set<string>>(new Set())
  const [characterizeError, setCharacterizeError] = useState<string | null>(null)
  const runCharacterize = (characterId: string) => {
    setCharacterizeError(null)
    setCharacterizingIds((ids) => new Set(ids).add(characterId))
    characterizeSpeaker.mutate(characterId, {
      onError: (err) => setCharacterizeError(err instanceof ApiError ? err.message : 'Characterization failed'),
      onSettled: () =>
        setCharacterizingIds((ids) => {
          const next = new Set(ids)
          next.delete(characterId)
          return next
        }),
    })
  }

  const regenerateVoices = useRegenerateCharacterVoices(bookId)
  const [regeneratingIds, setRegeneratingIds] = useState<Set<string>>(new Set())
  const [regenerateError, setRegenerateError] = useState<string | null>(null)
  const runRegenerateVoice = (characterId: string) => {
    setRegenerateError(null)
    setRegeneratingIds((ids) => new Set(ids).add(characterId))
    regenerateVoices.mutate([characterId], {
      onError: (err) => setRegenerateError(err instanceof ApiError ? err.message : 'Regenerate voice failed'),
      onSettled: () =>
        setRegeneratingIds((ids) => {
          const next = new Set(ids)
          next.delete(characterId)
          return next
        }),
    })
  }

  // Positions/sizes the floating legend to sit in the actual gap between
  // .app-sidebar and .app-main's own left edge, measured live rather than
  // assumed from a hardcoded sidebar width - .player-bar itself spans the
  // full viewport (left:0/right:0) under the sidebar, and .app-main is
  // centered within the remaining space up to its own max-width, so that
  // gap's real size varies with viewport width and can't be computed from
  // fixed CSS constants alone. Recomputed on resize while the panel's
  // open; before the first measurement, .annotation-legend-float's own
  // CSS gives a reasonable fallback position so there's nothing to
  // measure against yet.
  const [legendBounds, setLegendBounds] = useState<{ left: number; width: number } | null>(null)
  useLayoutEffect(() => {
    if (!annotationsView) return
    const compute = () => {
      const sidebar = document.querySelector('.app-sidebar')
      const main = document.querySelector('.app-main')
      const sidebarRight = sidebar ? sidebar.getBoundingClientRect().right : 0
      // .app-main's own getBoundingClientRect().left is its OUTER box
      // edge, before its own left padding - the actual reader text starts
      // padding-left further right than that. Subtracting that padding
      // here (rather than just a fixed few px, as an earlier version
      // did) matters much more now that width below is an exact fixed
      // value instead of a cap a narrower box could stay safely inside
      // of - any slack here is now fully exposed as real overlap with
      // the text instead of just unused space.
      let textLeft = window.innerWidth
      if (main) {
        const rect = main.getBoundingClientRect()
        const paddingLeft = parseFloat(getComputedStyle(main).paddingLeft) || 0
        textLeft = rect.left + paddingLeft
      }
      const margin = 24
      const buffer = 16
      const left = sidebarRight + margin
      // A fixed width (the whole measured gap), not a cap - see
      // .annotation-legend-float's own comment on why this stays
      // constant across "this chapter"/whole-book/search-filtered
      // content instead of shrink-wrapping it.
      setLegendBounds({ left, width: Math.max(160, textLeft - left - buffer) })
    }
    compute()
    window.addEventListener('resize', compute)
    return () => window.removeEventListener('resize', compute)
  }, [annotationsView])

  const paragraphs = chapter?.paragraphs ?? []
  const total = paragraphs.length
  const current = paragraphs[playback.paragraphIdx]
  // Progress through the whole chapter (paragraphs completed, plus how far
  // into the current one), not just the currently-playing clip - so the
  // bar reflects "how much of this chapter is left" rather than resetting
  // to empty at the start of every paragraph.
  const currentFraction = playback.duration ? playback.currentTime / playback.duration : 0
  const chapterProgress = total > 0 ? ((playback.paragraphIdx + currentFraction) / total) * 100 : 0

  // How much further playback could run right now without waiting on
  // generation - the run of already-ready paragraphs immediately ahead of
  // the current one. Stops at the first non-ready paragraph, since that's
  // where playback would actually stall, even if later ones happen to be
  // ready already (lookahead can finish out of order).
  let bufferedAheadCount = 0
  for (let i = playback.paragraphIdx + 1; i < paragraphs.length; i++) {
    if (paragraphs[i]?.audioStatus !== 'ready') break
    bufferedAheadCount++
  }
  // Also credit whatever's left of the *current* paragraph (it's already
  // playing, so it's available too) - otherwise the remainder between
  // currentTime and the end of the current clip is neither "played" nor
  // "buffered", leaving a permanent gap even when the whole chapter is
  // ready.
  const bufferedAheadUnits = bufferedAheadCount + (current?.audioStatus === 'ready' ? 1 - currentFraction : 0)
  const bufferedAheadPercent = total > 0 ? (bufferedAheadUnits / total) * 100 : 0

  // Elapsed/total time for the whole chapter, not just the current
  // paragraph's clip - sums known paragraph durations (ready ones only;
  // ungenerated paragraphs don't have a duration yet, so the total grows
  // as more of the chapter finishes generating, same spirit as the book's
  // own estimatedTotalSeconds recalibrating over time). These are all in
  // the audio clips' own native (1x) timescale - durationSeconds is a
  // fixed property of each rendered .wav, unaffected by playback.rate.
  const chapterElapsedSeconds =
    paragraphs.slice(0, playback.paragraphIdx).reduce((sum, p) => sum + (p.durationSeconds ?? 0), 0) +
    playback.currentTime
  const chapterTotalSeconds = paragraphs.reduce((sum, p) => sum + (p.durationSeconds ?? 0), 0)

  const chapterRemainingSeconds = Math.max(0, chapterTotalSeconds - chapterElapsedSeconds)

  // Words per minute the reader is actually hearing right now: this chapter's own narrated
  // pace (words already generated, over their audio's own duration - ready paragraphs only,
  // same set chapterTotalSeconds already sums), scaled by the current playback rate since a
  // speed change is applied to the clip directly rather than baked into durationSeconds. null
  // until at least one paragraph in this chapter has generated audio to measure a pace from.
  const chapterWordCount = paragraphs.reduce((sum, p) => (p.durationSeconds ? sum + wordCount(p.text) : sum), 0)
  const wordsPerMinute =
    chapterTotalSeconds > 0 ? Math.round((chapterWordCount / (chapterTotalSeconds / 60)) * playback.playbackRate) : null

  // Total narration time left in the *whole book*, not just this chapter -
  // extrapolated from the book's self-calibrating estimatedTotalSeconds
  // (see backend httpapi.estimateTotalSeconds) and how far through it
  // progressPercent already says the reader is.
  const bookRemainingSeconds = Math.max(0, estimatedTotalSeconds * (1 - progressPercent / 100))

  // Every clock reading in the player bar (elapsed/remaining-in-chapter,
  // chapter total, book remaining) displays how much real listening time
  // that represents at the *current* playback speed, not the underlying
  // clips' own native duration - same reasoning wordsPerMinute above
  // already applies to narration pace. Dividing (not multiplying) is
  // correct: at 2x, a native 10-minute chapter only takes 5 real minutes
  // to actually listen to. Progress-bar widths (chapterProgress/
  // bufferedAheadPercent, above) are unaffected - those are fractions of
  // paragraph count, not absolute durations.
  const chapterElapsedAtSpeed = chapterElapsedSeconds / playback.playbackRate
  const chapterTotalAtSpeed = chapterTotalSeconds / playback.playbackRate
  const chapterRemainingAtSpeed = chapterRemainingSeconds / playback.playbackRate
  const bookRemainingAtSpeed = bookRemainingSeconds / playback.playbackRate

  return (
    <div className="player-bar">
      {!autoFollow && (
        <button className="jump-to-current-button" title="Scroll to the paragraph currently playing" onClick={onJumpToCurrent}>
          <RiIndentIncrease />
        </button>
      )}

      {voiceOpen && (
        <div className="bottom-popover" ref={voicePopoverRef}>
          <VoicePanel bookId={bookId} />
        </div>
      )}

      {typographyOpen && (
        <div className="bottom-popover" ref={typographyPopoverRef}>
          <ReaderTypographyPanel
            fontSize={readerFontSize}
            fontFamily={readerFontFamily}
            onFontSizeChange={onReaderFontSizeChange}
            onFontFamilyChange={onReaderFontFamilyChange}
          />
        </div>
      )}

      {annotationsView && (
        <div
          className="annotation-legend-float"
          style={legendBounds ? { left: legendBounds.left, width: legendBounds.width } : undefined}
        >
          {speakers.length > 0 && (
            <>
              <div className="annotation-speaker-list">
                <div className="annotation-speaker-list-header">
                  <div className="annotation-legend-title">Speakers</div>
                  <button
                    className={'icon-action-button' + (scopeToChapter ? ' icon-action-button-active' : '')}
                    onClick={() => setScopeToChapter((v) => !v)}
                    title={scopeToChapter ? 'Showing this chapter only - click to show the whole book' : 'Show only this chapter’s speakers'}
                  >
                    {scopeToChapter ? <RiFileTextLine /> : <RiBook2Line />}
                  </button>
                </div>
                <input
                  className="annotation-speaker-search"
                  type="text"
                  placeholder="Search speakers…"
                  value={speakerFilter}
                  onChange={(e) => setSpeakerFilter(e.target.value)}
                />
                <div className="annotation-legend-divider" />
                <div className="annotation-speaker-list-names">
                  {filteredSpeakers.length === 0 && <span className="muted">No matches</span>}
                  {filteredSpeakers.map((s) => {
                    const mergeTargets = [
                      'Narrator',
                      ...speakers.filter((o) => o.id && o.id !== s.id && !o.invalid).map((o) => o.name),
                    ]
                    return (
                      <div key={s.id || s.name} className="annotation-speaker-row">
                        <div className="annotation-speaker-row-main">
                          <button
                            className={
                              'annotation-speaker-list-item' +
                              (selectedSpeaker === s.name ? ' annotation-speaker-list-item-active' : '')
                            }
                            onClick={() => onSelectSpeaker(selectedSpeaker === s.name ? null : s.name)}
                            title={`Highlight ${s.name}'s lines in the reader`}
                          >
                            {s.name}
                          </button>
                          <button
                            className="icon-action-button"
                            onClick={() => onJumpToNextAppearance(s.name)}
                            title={`Jump to ${s.name}'s next appearance`}
                          >
                            <RiSkipForwardMiniLine />
                          </button>
                          {s.id && (
                            <>
                              <button
                                className="icon-action-button"
                                onClick={() => runCharacterize(s.id)}
                                disabled={characterizingIds.has(s.id)}
                                title={
                                  characterizingIds.has(s.id)
                                    ? 'Regenerating characterization…'
                                    : `Recharacterize "${s.name}" — re-describe their voice from their dialogue, and invalidate their assigned voice so it's rebuilt fresh next time it's needed`
                                }
                              >
                                <RiUserHeartLine className={characterizingIds.has(s.id) ? 'spin' : undefined} />
                              </button>
                              <button
                                className="icon-action-button"
                                onClick={() => runRegenerateVoice(s.id)}
                                disabled={regeneratingIds.has(s.id)}
                                title={
                                  regeneratingIds.has(s.id)
                                    ? 'Regenerating voice…'
                                    : `Regenerate "${s.name}"'s voice — force a fresh render, whether or not they already have one`
                                }
                              >
                                <RiRefreshLine className={regeneratingIds.has(s.id) ? 'spin' : undefined} />
                              </button>
                              <button
                                className="icon-action-button"
                                onClick={() => {
                                  setMergeError(null)
                                  setMergeTarget('')
                                  setMergingId((cur) => (cur === s.id ? null : s.id))
                                }}
                                title={`Merge "${s.name}" into another speaker`}
                              >
                                <RiGitMergeLine />
                              </button>
                            </>
                          )}
                        </div>
                        {mergingId === s.id && (
                          <div className="annotation-speaker-merge">
                            <select value={mergeTarget} onChange={(e) => setMergeTarget(e.target.value)}>
                              <option value="" disabled>
                                Merge into…
                              </option>
                              {mergeTargets.map((name) => (
                                <option key={name} value={name}>
                                  {name}
                                </option>
                              ))}
                            </select>
                            <button
                              className="text-button"
                              disabled={!mergeTarget || mergeCharacter.isPending}
                              onClick={() =>
                                mergeCharacter.mutate(
                                  { characterId: s.id, targetName: mergeTarget },
                                  {
                                    onSuccess: () => setMergingId(null),
                                    onError: (err) =>
                                      setMergeError(err instanceof ApiError ? err.message : 'Could not merge'),
                                  },
                                )
                              }
                            >
                              Merge
                            </button>
                            <button className="text-button" onClick={() => setMergingId(null)}>
                              Cancel
                            </button>
                          </div>
                        )}
                      </div>
                    )
                  })}
                </div>
                {mergeError && <p className="error-text">{mergeError}</p>}
                {characterizeError && <p className="error-text">{characterizeError}</p>}
                {regenerateError && <p className="error-text">{regenerateError}</p>}
              </div>
              <div className="annotation-legend-divider" />
            </>
          )}
          {(Object.keys(ANNOTATION_LABELS) as AnnotationKind[]).map((kind) => (
            <span key={kind} className="annotation-legend-item">
              <span className={`annotation-legend-swatch annotation-${kind}`} />
              {ANNOTATION_LABELS[kind]}
            </span>
          ))}
          {(Object.keys(DIRECTION_TAG_CATEGORY_LABELS) as Exclude<DirectionTagCategory, 'other'>[]).map((category) => (
            <span key={category} className="annotation-legend-item">
              <span className={`direction-caret direction-caret-${category}`} aria-hidden="true">
                <RiArrowDropUpLine />
              </span>
              {DIRECTION_TAG_CATEGORY_LABELS[category]}
            </span>
          ))}
          <span className="annotation-legend-item">
            <span className="emotion-icon" aria-hidden="true">
              <RiEmotionLine />
            </span>
            Emotion
          </span>
          <span className="annotation-legend-item">
            <span className="pronunciation-legend-swatch" aria-hidden="true" />
            Pronunciation
          </span>
          <span className="annotation-legend-item">
            <span className="scare-quote-legend-swatch" aria-hidden="true" />
            Scare quote
          </span>
        </div>
      )}

      <div className="player-bar-row">
        <button
          className="play-button"
          onClick={() => (playback.isPlaying ? playback.pause() : playback.play())}
          disabled={!current}
        >
          {playback.isPlaying ? <RiPauseFill /> : <RiPlayFill />}
        </button>

        <div className="player-progress">
          <div className="player-status">
            {chapter ? `${chapter.title} — paragraph ${playback.paragraphIdx + 1} / ${total}` : ''}
            {playback.isWaitingForAudio && <span className="muted"> — generating…</span>}
          </div>
          <div className="player-track">
            <div className="player-track-buffered" style={{ left: `${chapterProgress}%`, width: `${bufferedAheadPercent}%` }} />
            <div className="player-track-fill" style={{ width: `${chapterProgress}%` }} />
          </div>
          <div className="player-times">
            <span>
              {formatTime(chapterElapsedAtSpeed)}
              {wordsPerMinute != null && (
                <span className="player-wpm" title="Narration pace at the current playback speed">
                  {' '}
                  · {wordsPerMinute} wpm
                </span>
              )}
            </span>
            {sleepTimer.option !== 'off' && (
              <span className="player-sleep-remaining">
                {sleepTimer.option === 'end-of-chapter'
                  ? 'Sleeping at end of chapter'
                  : `Sleeping in ${formatCountdown(sleepTimer.remainingSeconds)}`}
              </span>
            )}
            <span className="player-times-right">
              <span className="player-remaining" title="Time left in this chapter, at the current playback speed">
                {!finished && `-${formatTime(chapterRemainingAtSpeed)} · `}
                {formatTime(chapterTotalAtSpeed)} <RiBookmarkLine />
              </span>
              {!finished && (
                <span className="player-remaining" title="Time left in the book, at the current playback speed">
                  {formatDurationLong(bookRemainingAtSpeed)} ({Math.round(100 - progressPercent)}%) <RiBookOpenLine />
                </span>
              )}
            </span>
          </div>
        </div>

        <label className="player-speed">
          Speed
          <select value={playback.playbackRate} onChange={(e) => playback.setPlaybackRate(Number(e.target.value))}>
            {RATES.map((r) => (
              <option key={r} value={r}>
                {r}×
              </option>
            ))}
          </select>
        </label>
      </div>

      <div className="player-bar-row player-bar-row-meta">
        <select
          className="player-chapter-select"
          value={playback.chapterIdx}
          onChange={(e) => onJumpToChapter(Number(e.target.value))}
        >
          {bookChapters.map((c) => (
            <option key={c.idx} value={c.idx}>
              {c.title} {c.readyCount < c.paragraphCount ? `(${c.readyCount}/${c.paragraphCount})` : ''}
            </option>
          ))}
        </select>

        <button
          ref={voiceButtonRef}
          className={'text-button-icon' + (voiceOpen ? ' text-button-icon-active' : '')}
          onClick={() => setVoiceOpen((v) => !v)}
        >
          <RiUser3Line /> Voice
        </button>

        <button
          ref={typographyButtonRef}
          className={'text-button-icon' + (typographyOpen ? ' text-button-icon-active' : '')}
          onClick={() => setTypographyOpen((v) => !v)}
          title="Reader text size and font"
        >
          <RiFontSize /> Text size
        </button>

        <button
          className={'text-button-icon' + (annotationsView ? ' text-button-icon-active' : '')}
          onClick={onToggleAnnotationsView}
          title="Highlight each paragraph as narrator, speaker, or description"
        >
          <RiPaletteLine /> Annotations
        </button>

        <BookSearch bookId={bookId} onJump={onJumpToParagraph} align="above" />

        <BookmarksPanel bookId={bookId} onJump={onJumpToParagraph} align="above" />

        <SleepTimerButton
          option={sleepTimer.option}
          remainingSeconds={sleepTimer.remainingSeconds}
          onChange={sleepTimer.setOption}
        />

        {finished && <span className="player-percent muted">Finished</span>}
      </div>
    </div>
  )
}
