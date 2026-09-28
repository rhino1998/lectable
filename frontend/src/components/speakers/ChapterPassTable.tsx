import { Fragment, useState } from 'react'
import {
  RiDiscLine,
  RiFileHistoryLine,
  RiDoubleQuotesL,
  RiEmotionLine,
  RiEyeLine,
  RiFileTextLine,
  RiSpeakLine,
  RiLightbulbLine,
  RiMusic2Line,
  RiPriceTag3Line,
  RiUserSearchLine,
  RiVoiceprintLine,
} from 'react-icons/ri'
import {
  useAttributeSpeakers,
  useResetPass,
  useAttributingChapters,
  useDescribingChapters,
  useDirectingChapters,
  usePronouncingChapters,
  useResolvePronunciation,
  useGenerateChapter,
  useGenerateChapterMusic,
  useGeneratingChapters,
  useGeneratingMusicChapters,
  useRetagDescriptions,
  useRetagScareQuotes,
  useScareQuotingChapters,
  useRunChapterAmbience,
  useAmbienceChapters,
  useTagDirections,
  useSummarizeChapter,
  useSummarizingChapters,
  useChapterNotes,
} from '../../api/queries'
import { ApiError } from '../../api/client'
import { useReimportChapterAction } from '../../hooks/useReimportChapterAction'
import type { BookDetail, BulkAction, BulkScope, ResetPass } from '../../api/types'
import { BulkActionButton, CountStatus, PassStatus } from './PassStatus'

// The Speakers page's per-chapter pass table: one row per chapter with each
// pass's run button and status (attribution, descriptions, scare quotes,
// emotions, pronunciation, ambience, narration audio, music audio), topped
// by whole-book Rest/All/Reset rows. Whole-book actions go through the
// page's own runBulk (one bulk endpoint, shared with the roster's buttons);
// bulk and reset errors surface through onBulkError at the top of the page.
export function ChapterPassTable({
  bookId,
  book,
  bulkRunning,
  runBulk,
  onBulkError,
}: {
  bookId: string
  book: BookDetail
  bulkRunning: Set<BulkAction>
  runBulk: (action: BulkAction, scope: BulkScope) => void
  onBulkError: (message: string | null) => void
}) {
  const attributeSpeakers = useAttributeSpeakers(bookId)
  const retagDescriptions = useRetagDescriptions(bookId)
  const retagScareQuotes = useRetagScareQuotes(bookId)
  const tagDirections = useTagDirections(bookId)
  const resolvePronunciation = useResolvePronunciation(bookId)
  const runChapterAmbience = useRunChapterAmbience(bookId)
  const generateChapterMusic = useGenerateChapterMusic(bookId)
  const generateChapter = useGenerateChapter(bookId)
  const resetPass = useResetPass(bookId)
  const summarizeChapter = useSummarizeChapter(bookId)
  const summarizingIdxs = useSummarizingChapters(bookId)
  const [summaryError, setSummaryError] = useState<string | null>(null)
  // The chapter whose summary notes are shown in an expanded row, if any.
  const [notesFor, setNotesFor] = useState<number | null>(null)

  // Derived from the shared backend job queue (the live jobs topic), not
  // local mutation state - attribution is fire-and-forget now (see useAttributingChapters'
  // own doc comment), so "is this chapter still attributing" isn't
  // something a mutation's own lifecycle can answer any more.
  const attributingIdxs = useAttributingChapters(bookId)
  const [attributeError, setAttributeError] = useState<string | null>(null)
  // Same job-queue-derived tracking as attributingIdxs, for the speech-
  // direction-tagging pass - see useDirectingChapters' own doc comment.
  const directingIdxs = useDirectingChapters(bookId)
  const [directionError, setDirectionError] = useState<string | null>(null)
  const pronouncingIdxs = usePronouncingChapters(bookId)
  const [pronunciationError, setPronunciationError] = useState<string | null>(null)
  const [reimportError, setReimportError] = useState<string | null>(null)
  const { run: runReimport, reimportingIdx } = useReimportChapterAction(bookId, setReimportError)
  // Same job-queue-derived tracking as attributingIdxs/directingIdxs, for
  // background-music tone-region scoring - see useAmbienceChapters'
  // own doc comment.
  const ambienceIdxs = useAmbienceChapters(bookId)
  const generatingMusicIdxs = useGeneratingMusicChapters(bookId)
  const [musicScoreError, setMusicScoreError] = useState<string | null>(null)
  const [musicGenerateError, setMusicGenerateError] = useState<string | null>(null)
  // Same job-queue-derived tracking as attributingIdxs/directingIdxs/
  // ambienceIdxs, for a chapter's own narration audio generation - see
  // useGeneratingChapters' own doc comment. Generating a chapter's voice
  // now also triggers background-music scoring for it automatically
  // (backend jobs.Manager.maybeRunChapterAmbience), so this button doubles
  // as "generate everything for this chapter" once music is turned on.
  const generatingIdxs = useGeneratingChapters(bookId)
  const [generateError, setGenerateError] = useState<string | null>(null)
  // Same job-queue-derived tracking as attributingIdxs, for description
  // and scare-quote tagging - both their own queued jobs now (attribution
  // and description tagging each wait on a chapter's scare-quote job).
  const describingIdxs = useDescribingChapters(bookId)
  const [retagError, setRetagError] = useState<string | null>(null)
  const scareQuotingIdxs = useScareQuotingChapters(bookId)
  const [retagScareQuoteError, setRetagScareQuoteError] = useState<string | null>(null)

  const runAttribute = (idx: number) => {
    setAttributeError(null)
    attributeSpeakers.mutate(idx, {
      onError: (err) =>
        setAttributeError(err instanceof ApiError ? err.message : 'Speaker attribution failed'),
    })
  }

  const runSummarize = (idx: number) => {
    setSummaryError(null)
    summarizeChapter.mutate(idx, {
      onError: (err) => setSummaryError(err instanceof ApiError ? err.message : 'Summary failed'),
    })
  }

  const runRetagDescriptions = (idx: number) => {
    setRetagError(null)
    retagDescriptions.mutate(idx, {
      onError: (err) =>
        setRetagError(err instanceof ApiError ? err.message : 'Description tagging failed'),
    })
  }

  const runRetagScareQuotes = (idx: number) => {
    setRetagScareQuoteError(null)
    retagScareQuotes.mutate(idx, {
      onError: (err) =>
        setRetagScareQuoteError(
          err instanceof ApiError ? err.message : 'Scare-quote tagging failed',
        ),
    })
  }

  const runDirection = (idx: number) => {
    setDirectionError(null)
    tagDirections.mutate(idx, {
      onError: (err) =>
        setDirectionError(
          err instanceof ApiError ? err.message : 'Speech-direction tagging failed',
        ),
    })
  }

  const runPronunciation = (idx: number) => {
    setPronunciationError(null)
    resolvePronunciation.mutate(idx, {
      onError: (err) =>
        setPronunciationError(
          err instanceof ApiError ? err.message : 'Pronunciation resolution failed',
        ),
    })
  }

  const runAmbience = (idx: number) => {
    setMusicScoreError(null)
    runChapterAmbience.mutate(idx, {
      onError: (err) =>
        setMusicScoreError(err instanceof ApiError ? err.message : 'Ambience pass failed'),
    })
  }

  const runGenerateMusic = (idx: number) => {
    setMusicGenerateError(null)
    generateChapterMusic.mutate(idx, {
      onError: (err) =>
        setMusicGenerateError(
          err instanceof ApiError ? err.message : 'Background-music generation failed',
        ),
    })
  }

  const runGenerate = (idx: number) => {
    setGenerateError(null)
    generateChapter.mutate(idx, {
      onError: (err) =>
        setGenerateError(err instanceof ApiError ? err.message : 'Audio generation failed'),
    })
  }

  // Per-pass "Reset" row: clears the pass for the whole book as though it
  // never ran, deleting any audio that used it - see api.resetPass. The
  // character roster is untouched ("Delete speaker data" covers that).
  const runReset = (pass: ResetPass, what: string) => {
    if (!confirm(`Reset ${what} for every chapter of "${book.title}"? This can't be undone.`))
      return
    onBulkError(null)
    resetPass.mutate(pass, {
      onError: (err) =>
        onBulkError(err instanceof ApiError ? err.message : 'Could not reset this pass'),
    })
  }

  // "Rest" skips chapters whose pass is already done (ChapterSummary.passes);
  // "All" re-runs every chapter. Both are one backend bulk task each.
  const runAttributeAll = () => runBulk('attribution', 'all')
  const runAttributeUnattributed = () => runBulk('attribution', 'rest')
  const hasUnattributed = book.chapters.some((c) => !c.passes.attribution)
  const runRetagDescriptionsAll = () => runBulk('description', 'all')
  const runRetagDescriptionsUndescribed = () => runBulk('description', 'rest')
  const hasUndescribed = book.chapters.some((c) => !c.passes.description)
  const runRetagScareQuotesAll = () => runBulk('scare_quote', 'all')
  const runRetagScareQuotesUntagged = () => runBulk('scare_quote', 'rest')
  const hasUnscareQuoted = book.chapters.some((c) => !c.passes.scareQuote)
  const runDirectionAll = () => runBulk('direction', 'all')
  const runDirectionUndirected = () => runBulk('direction', 'rest')
  const hasUndirected = book.chapters.some((c) => !c.passes.direction)
  const runSummaryAll = () => runBulk('summary', 'all')
  const runSummaryRest = () => runBulk('summary', 'rest')
  const hasUnsummarized = book.chapters.some((c) => !c.passes.summary)
  const runPronunciationAll = () => runBulk('pronunciation', 'all')
  const runPronunciationUnresolved = () => runBulk('pronunciation', 'rest')
  const hasUnresolvedPronunciation = book.chapters.some((c) => !c.passes.pronunciation)
  // Always allowed regardless of the book-wide musicEnabled toggle - see
  // api.runChapterAmbience.
  const runAmbienceAll = () => runBulk('ambience', 'all')
  const runAmbienceRest = () => runBulk('ambience', 'rest')
  const hasAmbienceRest = book.chapters.some((c) => !(c.passes.ambience && c.passes.lighting))

  // A chapter's narration audio is done once every paragraph is ready - see
  // PlayerBar's identical "c.readyCount < c.paragraphCount" check.
  const isGenerated = (c: (typeof book.chapters)[number]) => c.readyCount >= c.paragraphCount
  // Generation only ever renders paragraphs that aren't ready yet, so
  // "All" and "Rest" are the same one whole-book generate task.
  const runGenerateAll = () => runBulk('generate', 'all')
  const runGenerateUngenerated = runGenerateAll
  const hasUngenerated = book.chapters.some((c) => !isGenerated(c))

  // A chapter's music can only generate once it's scored and its narration
  // is fully generated (each region's clip is sized to its own narration -
  // see api.generateChapterMusic); "missing" is any region without a ready
  // clip, failed ones included.
  // `?? 0`: an older backend doesn't send the music counts at all.
  const musicCounts = (c: (typeof book.chapters)[number]) => ({
    total: c.musicRegionCount ?? 0,
    ready: c.musicReadyCount ?? 0,
    errors: c.musicErrorCount ?? 0,
  })
  const isMusicGenerated = (c: (typeof book.chapters)[number]) =>
    musicCounts(c).total > 0 && musicCounts(c).ready >= musicCounts(c).total
  const canGenerateMusic = (c: (typeof book.chapters)[number]) =>
    musicCounts(c).total > 0 && isGenerated(c)
  const missingMusicChapters = book.chapters.filter(
    (c) => canGenerateMusic(c) && !isMusicGenerated(c),
  )
  const runGenerateMissingMusic = () => runBulk('music_generation', 'rest')

  return (
    <section>
      <div className="library-header">
        <h2>Attribute speakers</h2>
      </div>
      {attributeError && <p className="error-text">{attributeError}</p>}
      {retagError && <p className="error-text">{retagError}</p>}
      {retagScareQuoteError && <p className="error-text">{retagScareQuoteError}</p>}
      {directionError && <p className="error-text">{directionError}</p>}
      {reimportError && <p className="error-text">{reimportError}</p>}
      {pronunciationError && <p className="error-text">{pronunciationError}</p>}
      {summaryError && <p className="error-text">{summaryError}</p>}
      {musicScoreError && <p className="error-text">{musicScoreError}</p>}
      {musicGenerateError && <p className="error-text">{musicGenerateError}</p>}
      {generateError && <p className="error-text">{generateError}</p>}
      <table className="chapter-attribute-table">
        <thead>
          <tr>
            <th>Chapter</th>
            <th title="Per-chapter notes - a short summary, its scenes (where and when), and its cast - that attribution, ambience and lighting read">
              Summary
            </th>
            <th>Attribution</th>
            <th>Description</th>
            <th>Scare quotes</th>
            <th>Emotion</th>
            <th>Pronunciation</th>
            <th>Ambience</th>
            <th>Audio</th>
            <th>Music audio</th>
          </tr>
        </thead>
        <tbody>
          {/* Whole-book counterparts of each chapter row's own action
            icons, each in its pass's column: the first row only touches
            chapters that aren't done yet, the second re-runs every
            chapter, the third resets each pass. */}
          <tr className="chapter-attribute-bulk-row">
            <td className="muted" title="Every chapter that isn't done yet">
              Rest
            </td>
            <td>
              <BulkActionButton
                icon={RiFileTextLine}
                busy={bulkRunning.has('summary')}
                disabled={!hasUnsummarized}
                onClick={runSummaryRest}
                title="Summarize unsummarized chapters — every chapter without a summary yet"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiUserSearchLine}
                busy={bulkRunning.has('attribution')}
                disabled={!hasUnattributed}
                onClick={runAttributeUnattributed}
                title="Attribute unattributed — every chapter that isn't fully attributed yet, skipping ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiPriceTag3Line}
                busy={bulkRunning.has('description')}
                disabled={!hasUndescribed}
                onClick={runRetagDescriptionsUndescribed}
                title="Tag untagged descriptions — every chapter that isn't description-tagged yet, skipping ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiDoubleQuotesL}
                busy={bulkRunning.has('scare_quote')}
                disabled={!hasUnscareQuoted}
                onClick={runRetagScareQuotesUntagged}
                title="Tag untagged scare quotes — every chapter that isn't scare-quote-tagged yet, skipping ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiEmotionLine}
                busy={bulkRunning.has('direction')}
                disabled={!hasUndirected}
                onClick={runDirectionUndirected}
                title="Label unlabeled emotions — every chapter that isn't emotion-labeled yet, skipping ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiSpeakLine}
                busy={bulkRunning.has('pronunciation')}
                disabled={!hasUnresolvedPronunciation}
                onClick={runPronunciationUnresolved}
                title="Resolve unresolved pronunciation — every chapter that isn't resolved yet, skipping ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiMusic2Line}
                busy={bulkRunning.has('ambience')}
                disabled={!hasAmbienceRest}
                onClick={runAmbienceRest}
                title="Run ambience where missing — music scoring and lighting for every chapter that doesn't have both yet (a scored chapter only gets lit)"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiVoiceprintLine}
                busy={bulkRunning.has('generate')}
                disabled={!hasUngenerated}
                onClick={runGenerateUngenerated}
                title="Generate ungenerated — audio for every chapter that isn't fully generated yet, skipping ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiDiscLine}
                busy={bulkRunning.has('music_generation')}
                disabled={missingMusicChapters.length === 0}
                onClick={runGenerateMissingMusic}
                title="Generate missing music — every scored, fully-narrated chapter that's missing any, retrying failed regions"
              />
            </td>
          </tr>
          <tr className="chapter-attribute-bulk-row">
            <td className="muted" title="Every chapter, including ones already done">
              All
            </td>
            <td>
              <BulkActionButton
                icon={RiFileTextLine}
                busy={bulkRunning.has('summary')}
                onClick={runSummaryAll}
                title="Re-summarize every chapter, including ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiUserSearchLine}
                busy={bulkRunning.has('attribution')}
                onClick={runAttributeAll}
                title="Attribute all — every chapter, including ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiPriceTag3Line}
                busy={bulkRunning.has('description')}
                onClick={runRetagDescriptionsAll}
                title="Retag all descriptions — every chapter, including ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiDoubleQuotesL}
                busy={bulkRunning.has('scare_quote')}
                onClick={runRetagScareQuotesAll}
                title="Retag all scare quotes — every chapter, including ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiEmotionLine}
                busy={bulkRunning.has('direction')}
                onClick={runDirectionAll}
                title="Label all emotions — every chapter, including ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiSpeakLine}
                busy={bulkRunning.has('pronunciation')}
                onClick={runPronunciationAll}
                title="Resolve all pronunciation — every chapter, including ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiMusic2Line}
                busy={bulkRunning.has('ambience')}
                onClick={runAmbienceAll}
                title="Re-run all ambience — re-score music and re-light every chapter, including ones already done"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiVoiceprintLine}
                busy={bulkRunning.has('generate')}
                onClick={runGenerateAll}
                title="Generate all audio — every chapter"
              />
            </td>
            <td>
              {/* No force variant: chapter music generation only ever
                fills in missing regions (see api.generateChapterMusic). */}
            </td>
          </tr>
          <tr className="chapter-attribute-bulk-row chapter-attribute-bulk-row-last">
            <td className="muted" title="Clear a pass for every chapter, as though it never ran">
              Reset
            </td>
            <td>
              <BulkActionButton
                icon={RiFileTextLine}
                disabled={bulkRunning.has('summary') || resetPass.isPending}
                onClick={() => runReset('summary', 'chapter summaries')}
                title="Reset summaries — delete every chapter's summary notes (no audio changes)"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiUserSearchLine}
                disabled={bulkRunning.has('attribution') || resetPass.isPending}
                onClick={() => runReset('attribution', 'speaker attribution')}
                title="Reset attribution — clear every line's speaker (scare quotes stay Narrator) and delete audio voiced by a character"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiPriceTag3Line}
                disabled={bulkRunning.has('description') || resetPass.isPending}
                onClick={() => runReset('description', 'description tags')}
                title="Reset descriptions — clear every description tag (no audio is affected)"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiDoubleQuotesL}
                disabled={bulkRunning.has('scare_quote') || resetPass.isPending}
                onClick={() => runReset('scare_quote', 'scare-quote tags')}
                title="Reset scare quotes — unflag every scare quote, including manual ones, and delete their audio"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiEmotionLine}
                disabled={bulkRunning.has('direction') || resetPass.isPending}
                onClick={() => runReset('direction', 'emotion labels')}
                title="Reset emotions — clear every emotion label, including manual overrides, and delete audio voiced with one"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiSpeakLine}
                disabled={bulkRunning.has('pronunciation') || resetPass.isPending}
                onClick={() => runReset('pronunciation', 'pronunciation fixes')}
                title="Reset pronunciation — clear every pronunciation fix and delete audio that used one"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiMusic2Line}
                disabled={bulkRunning.has('ambience') || resetPass.isPending}
                onClick={() => runReset('ambience', 'ambience')}
                title="Reset ambience — delete every music region, its generated music, and its lighting"
              />
              <BulkActionButton
                icon={RiLightbulbLine}
                disabled={bulkRunning.has('ambience') || resetPass.isPending}
                onClick={() => runReset('lighting', 'lighting')}
                title="Reset lighting — clear every region's lighting cue, keeping the music (run ambience again to re-light)"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiVoiceprintLine}
                disabled={bulkRunning.has('generate') || resetPass.isPending}
                onClick={() => runReset('generate', 'all generated narration audio')}
                title="Reset audio — delete every generated narration clip (music regions go back to pending too)"
              />
            </td>
            <td>
              <BulkActionButton
                icon={RiDiscLine}
                disabled={bulkRunning.has('music_generation') || resetPass.isPending}
                onClick={() => runReset('music_generation', 'all generated music')}
                title="Reset music audio — delete every generated music clip, keeping the scored regions"
              />
            </td>
          </tr>
          {book.chapters.map((c) => {
            const attributing = attributingIdxs.has(c.idx)
            const retagging = describingIdxs.has(c.idx)
            const retaggingScareQuotes = scareQuotingIdxs.has(c.idx)
            const directing = directingIdxs.has(c.idx)
            const pronouncing = pronouncingIdxs.has(c.idx)
            const scoringMusic = ambienceIdxs.has(c.idx)
            const generating =
              generatingIdxs.has(c.idx) || (bulkRunning.has('generate') && !isGenerated(c))
            const generatingMusic = generatingMusicIdxs.has(c.idx)
            const summarizing = summarizingIdxs.has(c.idx)
            return (
              <Fragment key={c.idx}>
                <tr>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={reimportingIdx !== null}
                        onClick={() => runReimport(c.idx, c.title)}
                        title={
                          reimportingIdx === c.idx
                            ? 'Re-importing…'
                            : "Re-import this chapter from the book's epub — re-parses its paragraphs and resets its passes and audio"
                        }
                      >
                        <RiFileHistoryLine
                          className={reimportingIdx === c.idx ? 'spin' : undefined}
                        />
                      </button>
                      {c.title}
                    </div>
                  </td>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={summarizing}
                        onClick={() => runSummarize(c.idx)}
                        title={
                          summarizing
                            ? 'Summarizing…'
                            : c.passes.summary
                              ? 'Re-summarize this chapter'
                              : 'Summarize this chapter — its summary, scenes and cast'
                        }
                      >
                        <RiFileTextLine className={summarizing ? 'spin' : undefined} />
                      </button>
                      <PassStatus done={c.passes.summary} label="Summary" />
                      {c.passes.summary && (
                        <button
                          className="icon-action-button"
                          onClick={() => setNotesFor((cur) => (cur === c.idx ? null : c.idx))}
                          title={
                            notesFor === c.idx ? 'Hide summary' : 'Show summary, scenes and cast'
                          }
                        >
                          <RiEyeLine />
                        </button>
                      )}
                    </div>
                  </td>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={attributing}
                        onClick={() => runAttribute(c.idx)}
                        title={
                          attributing
                            ? 'Attributing…'
                            : c.passes.attribution
                              ? 'Re-attribute speakers for this chapter'
                              : 'Attribute speakers for this chapter'
                        }
                      >
                        <RiUserSearchLine className={attributing ? 'spin' : undefined} />
                      </button>
                      <PassStatus done={c.passes.attribution} label="Attribution" />
                    </div>
                  </td>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={retagging}
                        onClick={() => runRetagDescriptions(c.idx)}
                        title={
                          retagging
                            ? 'Retagging…'
                            : "Retag descriptions — re-run description-tagging for this chapter (which narration paragraphs describe a character's appearance or personality)"
                        }
                      >
                        <RiPriceTag3Line className={retagging ? 'spin' : undefined} />
                      </button>
                      <PassStatus done={c.passes.description} label="Description tagging" />
                    </div>
                  </td>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={retaggingScareQuotes}
                        onClick={() => runRetagScareQuotes(c.idx)}
                        title={
                          retaggingScareQuotes
                            ? 'Retagging…'
                            : "Retag scare quotes — re-run scare-quote tagging for this chapter (quoted spans that look like dialogue but aren't actually spoken aloud)"
                        }
                      >
                        <RiDoubleQuotesL className={retaggingScareQuotes ? 'spin' : undefined} />
                      </button>
                      <PassStatus done={c.passes.scareQuote} label="Scare-quote tagging" />
                    </div>
                  </td>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={directing}
                        onClick={() => runDirection(c.idx)}
                        title={
                          directing
                            ? 'Tagging…'
                            : c.passes.direction
                              ? 'Re-tag speech direction for this chapter'
                              : 'Tag speech direction for this chapter'
                        }
                      >
                        <RiEmotionLine className={directing ? 'spin' : undefined} />
                      </button>
                      <PassStatus done={c.passes.direction} label="Emotion labeling" />
                    </div>
                  </td>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={pronouncing}
                        onClick={() => runPronunciation(c.idx)}
                        title={
                          pronouncing
                            ? 'Resolving…'
                            : c.passes.pronunciation
                              ? 'Re-resolve pronunciation for this chapter (ambiguous abbreviations like "Dr." or "St.")'
                              : 'Resolve pronunciation for this chapter (ambiguous abbreviations like "Dr." or "St.")'
                        }
                      >
                        <RiSpeakLine className={pronouncing ? 'spin' : undefined} />
                      </button>
                      <PassStatus done={c.passes.pronunciation} label="Pronunciation resolution" />
                    </div>
                  </td>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={scoringMusic}
                        onClick={() => runAmbience(c.idx)}
                        title={
                          scoringMusic
                            ? 'Running ambience…'
                            : c.passes.ambience && c.passes.lighting
                              ? 'Re-run ambience (re-score music and re-light) for this chapter'
                              : c.passes.ambience
                                ? 'Light this chapter (its music is already scored)'
                                : 'Run ambience (music + lighting) for this chapter'
                        }
                      >
                        <RiMusic2Line className={scoringMusic ? 'spin' : undefined} />
                      </button>
                      <PassStatus
                        done={c.passes.ambience && c.passes.lighting}
                        label={
                          c.passes.ambience && !c.passes.lighting
                            ? 'Ambience (music scored, not lit yet)'
                            : 'Ambience'
                        }
                      />
                    </div>
                  </td>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={generating}
                        onClick={() => runGenerate(c.idx)}
                        title={
                          generating
                            ? 'Generating…'
                            : isGenerated(c)
                              ? 'Already fully generated — click to check for background music if it was turned on since'
                              : 'Generate audio for this chapter'
                        }
                      >
                        <RiVoiceprintLine className={generating ? 'spin' : undefined} />
                      </button>
                      <CountStatus
                        ready={c.readyCount}
                        total={c.paragraphCount}
                        label="Narration audio"
                      />
                    </div>
                  </td>
                  <td>
                    <div className="chapter-attribute-cell">
                      <button
                        className="icon-action-button"
                        disabled={generatingMusic || !canGenerateMusic(c)}
                        onClick={() => runGenerateMusic(c.idx)}
                        title={
                          generatingMusic
                            ? 'Generating music…'
                            : musicCounts(c).total === 0
                              ? 'Generate background music — score this chapter first'
                              : !isGenerated(c)
                                ? "Generate background music — this chapter's narration needs to be fully generated first"
                                : isMusicGenerated(c)
                                  ? 'Background music is fully generated for this chapter'
                                  : 'Generate background music for this chapter (retries failed regions)'
                        }
                      >
                        <RiDiscLine className={generatingMusic ? 'spin' : undefined} />
                      </button>
                      {musicCounts(c).total === 0 ? (
                        <span
                          className="chapter-attribute-status"
                          title="Music audio: not scored yet"
                        >
                          —
                        </span>
                      ) : (
                        <CountStatus
                          ready={musicCounts(c).ready}
                          total={musicCounts(c).total}
                          errors={musicCounts(c).errors}
                          label="Music audio"
                          unit="regions"
                        />
                      )}
                    </div>
                  </td>
                </tr>
                {notesFor === c.idx && (
                  <tr className="chapter-notes-row">
                    <td colSpan={10}>
                      <ChapterNotesView bookId={bookId} chapterIdx={c.idx} />
                    </td>
                  </tr>
                )}
              </Fragment>
            )
          })}
        </tbody>
      </table>
    </section>
  )
}

// ChapterNotesView shows one chapter's summary-pass notes: the summary, each
// scene (from its paragraph, where, when, who's there), and the cast with
// what the chapter calls them.
function ChapterNotesView({ bookId, chapterIdx }: { bookId: string; chapterIdx: number }) {
  const notes = useChapterNotes(bookId, chapterIdx).data?.notes
  if (!notes) return <p className="muted">No summary yet.</p>
  return (
    <div className="chapter-notes">
      <p>{notes.summary}</p>
      {notes.scenes.length > 0 && (
        <ol className="chapter-notes-scenes">
          {notes.scenes.map((sc) => (
            <li key={sc.startIdx}>
              <span className="muted">¶{sc.startIdx}</span> {sc.location || 'Unknown place'}
              {sc.time && <span className="muted"> · {sc.time}</span>}
              {sc.present.length > 0 && <span className="muted"> — {sc.present.join(', ')}</span>}
            </li>
          ))}
        </ol>
      )}
      {notes.cast.length > 0 && (
        <p className="chapter-notes-cast">
          {notes.cast.map((m, i) => (
            <span key={m.name}>
              {i > 0 && ', '}
              {m.name}
              {m.role && <span className="muted"> (role)</span>}
              {m.refs.length > 0 && <span className="muted"> ({m.refs.join('; ')})</span>}
            </span>
          ))}
        </p>
      )}
    </div>
  )
}
