import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from '@tanstack/react-router'
import {
  RiCheckLine,
  RiCloseLine,
  RiDeleteBinLine,
  RiDiscLine,
  RiFileHistoryLine,
  RiDoubleQuotesL,
  RiEmotionLine,
  RiSpeakLine,
  RiEqualizerLine,
  RiForbidLine,
  RiGitMergeLine,
  RiMusic2Line,
  RiPauseFill,
  RiPlayFill,
  RiPriceTag3Line,
  RiRefreshLine,
  RiScissorsCutLine,
  RiUserHeartLine,
  RiUserSearchLine,
  RiUserVoiceLine,
  RiVoiceprintLine,
} from 'react-icons/ri'
import type { IconType } from 'react-icons'
import {
  useAttributeSpeakers,
  useBulkAction,
  useBulkActionsRunning,
  useResetPass,
  useAttributingChapters,
  useBook,
  useCharacterAppearances,
  useCharacterDescriptions,
  useCharacterizeSpeaker,
  useCharacterizingCharacters,
  useCreateCustomVoicePreset,
  useCustomVoicePresets,
  useDeleteCharacter,
  useDeleteSpeakerData,
  useDescribingChapters,
  useDirectingChapters,
  usePronouncingChapters,
  useResolvePronunciation,
  useGenerateChapter,
  useGenerateChapterMusic,
  useGenerateCharacterVoice,
  useGeneratingChapters,
  useGeneratingMusicChapters,
  useMergeCharacter,
  usePreprocessBook,
  useReattributeSpeaker,
  useReattributingSpeakers,
  useRegenerateVariant,
  useSetCharacterInvalid,
  useSetCharacterAliases,
  useRegenerateParagraph,
  useRetagDescriptions,
  useRetagScareQuotes,
  useScareQuotingChapters,
  useScoreChapterMusic,
  useScoringMusicChapters,
  useSetCharacterVoice,
  useSetParagraphDescription,
  useSetParagraphSpeaker,
  useSpeakers,
  useTagDirections,
  useTestCloneInstruct,
  useTestCustomVoicePreset,
  useTestVoiceDesign,
  useUpdateCustomVoicePreset,
  useUpdateVoice,
  useVoice,
  useVoicePresets,
} from '../api/queries'
import { ApiError } from '../api/client'
import { DEFAULT_REF_TEXT, VoiceEditorForm, randomSeed } from '../components/VoiceEditorForm'
import { withCacheBust } from '../utils/cacheBust'
import { useReimportChapterAction } from '../hooks/useReimportChapterAction'
import {
  CHARACTER_VOICE_MODE_DESCRIPTIONS,
  CHARACTER_VOICE_MODE_LABELS,
  CHARACTER_VOICE_MODES,
  DEFAULT_CHARACTER_VOICE_MODE,
  DEFAULT_DESIGN_MODEL,
  DEFAULT_CLONE_MODEL,
  INSTRUCTED_CLONE_MODEL,
  type CharacterVoiceMode,
} from '../api/types'
import type { BulkAction, BulkScope, ResetPass, CustomVoicePreset, Speaker, SpeakerAppearance, SpeakerEmotion, VoicePreset } from '../api/types'

// Per-book speaker management: run LLM attribution chapter by chapter,
// review who's been identified so far and how much of their dialogue has
// generated audio, assign each character their own narration voice, and
// turn on book.MultiVoice once satisfied with the assignments. Voice
// *selection* for the book's own (narrator) voice stays in VoicePanel/the
// reader - this page is only about characters and the multi-voice switch.
export function SpeakersPage() {
  const { bookId } = useParams({ from: '/books/$bookId/speakers' })
  const bookQuery = useBook(bookId)
  const voiceQuery = useVoice(bookId)
  const updateVoice = useUpdateVoice(bookId)
  const speakersQuery = useSpeakers(bookId)
  const builtinsQuery = useVoicePresets()
  const customPresetsQuery = useCustomVoicePresets()
  const preprocessBook = usePreprocessBook()
  const attributeSpeakers = useAttributeSpeakers(bookId)
  const retagDescriptions = useRetagDescriptions(bookId)
  const retagScareQuotes = useRetagScareQuotes(bookId)
  const tagDirections = useTagDirections(bookId)
  const resolvePronunciation = useResolvePronunciation(bookId)
  const scoreChapterMusic = useScoreChapterMusic(bookId)
  const generateChapterMusic = useGenerateChapterMusic(bookId)
  const generateChapter = useGenerateChapter(bookId)
  const characterizeSpeaker = useCharacterizeSpeaker(bookId)
  const deleteSpeakerData = useDeleteSpeakerData(bookId)
  const deleteCharacter = useDeleteCharacter(bookId)
  const setCharacterInvalid = useSetCharacterInvalid(bookId)
  const mergeCharacter = useMergeCharacter(bookId)
  const reattributeSpeaker = useReattributeSpeaker(bookId)
  const generateVoice = useGenerateCharacterVoice(bookId)
  // Every whole-book button below goes through one bulk endpoint, queued
  // as a single cancelable Jobs row per click (see api.bulkAction).
  const bulkAction = useBulkAction(bookId)
  const bulkRunning = useBulkActionsRunning(bookId)
  const [bulkError, setBulkError] = useState<string | null>(null)
  const resetPass = useResetPass(bookId)

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
  // background-music tone-region scoring - see useScoringMusicChapters'
  // own doc comment.
  const scoringMusicIdxs = useScoringMusicChapters(bookId)
  const generatingMusicIdxs = useGeneratingMusicChapters(bookId)
  const [musicScoreError, setMusicScoreError] = useState<string | null>(null)
  const [musicGenerateError, setMusicGenerateError] = useState<string | null>(null)
  // Same job-queue-derived tracking as attributingIdxs/directingIdxs/
  // scoringMusicIdxs, for a chapter's own narration audio generation - see
  // useGeneratingChapters' own doc comment. Generating a chapter's voice
  // now also triggers background-music scoring for it automatically
  // (backend jobs.Manager.maybeScoreChapterMusic), so this button doubles
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
  // The single-row "Regenerate" button is still a genuinely blocking
  // request (useCharacterizeSpeaker), so it tracks its own in-flight
  // character id locally rather than through the job-queue topic above - a
  // Set, not a single id, purely so a rapid double-click or two rows
  // regenerating independently don't clobber each other's spinner.
  const [characterizingIds, setCharacterizingIds] = useState<Set<string>>(new Set())
  const [characterizeError, setCharacterizeError] = useState<string | null>(null)
  const [characterizeNote, setCharacterizeNote] = useState<string | null>(null)
  const [deleteSpeakerError, setDeleteSpeakerError] = useState<string | null>(null)
  const [deletingCharacterId, setDeletingCharacterId] = useState<string | null>(null)
  const [deleteCharacterError, setDeleteCharacterError] = useState<string | null>(null)
  const [invalidError, setInvalidError] = useState<string | null>(null)
  const [togglingInvalidId, setTogglingInvalidId] = useState<string | null>(null)
  const [mergingId, setMergingId] = useState<string | null>(null)
  const [mergeError, setMergeError] = useState<string | null>(null)
  // "Auto Split" is fire-and-forget across several per-chapter tasks (see
  // useReattributeSpeaker/useReattributingSpeakers' own doc comments), so
  // "is this speaker's split still running" is tracked through the shared
  // job-queue topic (by name - Auto Split works on "Unknown" too, which has
  // no character id to key a local Set on) rather than local mutation
  // state, the same split attributingIdxs/characterizingNames already use
  // for their own fire-and-forget actions.
  const [autoSplitError, setAutoSplitError] = useState<string | null>(null)
  const [autoSplitNote, setAutoSplitNote] = useState<string | null>(null)
  const reattributingNames = useReattributingSpeakers(bookId)
  const [rosterFilter, setRosterFilter] = useState('')
  const [generatingVoiceId, setGeneratingVoiceId] = useState<string | null>(null)
  const [generateVoiceError, setGenerateVoiceError] = useState<string | null>(null)
  const [appearancesFor, setAppearancesFor] = useState<string | null>(null)
  const [descriptionsFor, setDescriptionsFor] = useState<string | null>(null)
  const [customizingId, setCustomizingId] = useState<string | null>(null)
  // Bumped per character id after a successful recharacterize that
  // actually invalidated their voice, so its <audio> preview re-fetches
  // instead of showing/playing the pre-recharacterize clip once it's
  // lazily rebuilt - see withCacheBust's own doc comment.
  const [audioVersion, setAudioVersion] = useState<Record<string, number>>({})

  const appearancesQuery = useCharacterAppearances(bookId, appearancesFor)
  const descriptionsQuery = useCharacterDescriptions(bookId, descriptionsFor)

  const builtins = builtinsQuery.data?.presets ?? []
  const customs = customPresetsQuery.data ?? []
  const speakers = speakersQuery.data ?? []
  // Roster search - matches on name only. Filters just what's displayed;
  // merge/reassign targets still come from the full speakers list.
  const filteredSpeakers = useMemo(() => {
    const q = rosterFilter.trim().toLowerCase()
    return q === '' ? speakers : speakers.filter((s) => s.name.toLowerCase().includes(q))
  }, [speakers, rosterFilter])

  // Derived from the shared backend job queue (the live jobs topic), not
  // local mutation state - "Recharacterize all" is fire-and-forget (see
  // api.bulkAction/useCharacterizingCharacters' own doc comments);
  // tracked by character *name* since that's the only identifier a
  // speaker_characterization QueueTask carries. onFinished bumps
  // audioVersion for whichever characters just finished, the same
  // cache-busting the single-row "Regenerate" button's own onSuccess
  // already does - without it, an <audio> preview left open during a bulk
  // run would keep showing the pre-recharacterize clip under its
  // unchanged URL even though the backend already deleted and rebuilt it.
  const characterizingNames = useCharacterizingCharacters(bookId, (names) => {
    const ids = names
      .map((name) => speakers.find((s) => s.name === name)?.id)
      .filter((id): id is string => !!id)
    if (ids.length === 0) return
    setAudioVersion((v) => {
      const next = { ...v }
      for (const id of ids) next[id] = (next[id] ?? 0) + 1
      return next
    })
  })

  // The Narrator row always narrates in the book's own voice (see backend
  // handleListSpeakers' resolveFor) - named here so that row can say which
  // one, instead of duplicating a ref-audio preview that's already just a
  // click away in VoicePanel/the reader.
  const bookVoiceName =
    builtins.find((p) => p.id === voiceQuery.data?.presetId)?.name ??
    customs.find((p) => p.id === voiceQuery.data?.presetId)?.name

  // Mirrors backend narration.BookCloneModel: the book's own clone model
  // (a property of the book, not of any voice), or DEFAULT_CLONE_MODEL if
  // it's somehow unset.
  const effectiveCloneModel = voiceQuery.data?.cloneModel || DEFAULT_CLONE_MODEL

  // Gates instructCharacterVoices - only actually does anything when the book's
  // clone model is INSTRUCTED_CLONE_MODEL (the one family
  // that honors a clone-time style instruction alongside a reference
  // clip). Backend narration.Resolver already no-ops the setting itself
  // for any other clone model; this is just so the toggle's own copy
  // reflects that instead of silently doing nothing.
  const instructedCloneSupported = effectiveCloneModel === INSTRUCTED_CLONE_MODEL

  const runAttribute = (idx: number) => {
    setAttributeError(null)
    attributeSpeakers.mutate(idx, {
      onError: (err) =>
        setAttributeError(err instanceof ApiError ? err.message : 'Speaker attribution failed'),
    })
  }

  const runRetagDescriptions = (idx: number) => {
    setRetagError(null)
    retagDescriptions.mutate(idx, {
      onError: (err) => setRetagError(err instanceof ApiError ? err.message : 'Description tagging failed'),
    })
  }

  const runRetagScareQuotes = (idx: number) => {
    setRetagScareQuoteError(null)
    retagScareQuotes.mutate(idx, {
      onError: (err) => setRetagScareQuoteError(err instanceof ApiError ? err.message : 'Scare-quote tagging failed'),
    })
  }

  const runDirection = (idx: number) => {
    setDirectionError(null)
    tagDirections.mutate(idx, {
      onError: (err) => setDirectionError(err instanceof ApiError ? err.message : 'Speech-direction tagging failed'),
    })
  }

  const runPronunciation = (idx: number) => {
    setPronunciationError(null)
    resolvePronunciation.mutate(idx, {
      onError: (err) =>
        setPronunciationError(err instanceof ApiError ? err.message : 'Pronunciation resolution failed'),
    })
  }

  const runScoreMusic = (idx: number) => {
    setMusicScoreError(null)
    scoreChapterMusic.mutate(idx, {
      onError: (err) => setMusicScoreError(err instanceof ApiError ? err.message : 'Background-music scoring failed'),
    })
  }

  const runGenerateMusic = (idx: number) => {
    setMusicGenerateError(null)
    generateChapterMusic.mutate(idx, {
      onError: (err) =>
        setMusicGenerateError(err instanceof ApiError ? err.message : 'Background-music generation failed'),
    })
  }

  const runGenerate = (idx: number) => {
    setGenerateError(null)
    generateChapter.mutate(idx, {
      onError: (err) => setGenerateError(err instanceof ApiError ? err.message : 'Audio generation failed'),
    })
  }

  const runCharacterize = (characterId: string) => {
    setCharacterizeError(null)
    setCharacterizeNote(null)
    setCharacterizingIds((ids) => new Set(ids).add(characterId))
    characterizeSpeaker.mutate(characterId, {
      onSuccess: ({ voiceInvalidated }) => {
        setCharacterizeNote(
          voiceInvalidated
            ? "Characterization updated — voice will be rebuilt next time it's needed."
            : 'Characterization updated.',
        )
        if (voiceInvalidated) {
          setAudioVersion((v) => ({ ...v, [characterId]: (v[characterId] ?? 0) + 1 }))
        }
      },
      onError: (err) =>
        setCharacterizeError(err instanceof ApiError ? err.message : 'Characterization failed'),
      onSettled: () =>
        setCharacterizingIds((ids) => {
          const next = new Set(ids)
          next.delete(characterId)
          return next
        }),
    })
  }

  const runDeleteCharacter = (characterId: string, name: string) => {
    if (!confirm(`Delete "${name}"? Their lines in this book revert to Unknown, and their voice is removed.`)) {
      return
    }
    setDeleteCharacterError(null)
    setDeletingCharacterId(characterId)
    deleteCharacter.mutate(characterId, {
      onError: (err) =>
        setDeleteCharacterError(err instanceof ApiError ? err.message : 'Could not delete this character'),
      onSettled: () => setDeletingCharacterId(null),
    })
  }

  // Non-destructive toggle - see api.setCharacterInvalid. No confirm:
  // nothing is removed, and it's one click to undo.
  const runToggleInvalid = (characterId: string, invalid: boolean) => {
    setInvalidError(null)
    setTogglingInvalidId(characterId)
    setCharacterInvalid.mutate(
      { characterId, invalid },
      {
        onError: (err) => setInvalidError(err instanceof ApiError ? err.message : 'Could not update this character'),
        onSettled: () => setTogglingInvalidId(null),
      },
    )
  }

  const runMerge = (characterId: string, name: string, targetName: string) => {
    if (
      !confirm(
        `Merge "${name}" into "${targetName}"? Their lines in this book are reattributed to "${targetName}", and "${name}"'s own voice is removed.`,
      )
    ) {
      return
    }
    setMergeError(null)
    mergeCharacter.mutate(
      { characterId, targetName },
      {
        onSuccess: () => setMergingId(null),
        onError: (err) => setMergeError(err instanceof ApiError ? err.message : 'Could not merge this character'),
      },
    )
  }

  // "Auto Split" - for a speaker believed not to be a real, distinct
  // individual (a false-positive character, or literally "Unknown"): every
  // line currently attributed to them in this book gets re-judged and
  // redistributed to whichever real character/Narrator/Unknown actually
  // speaks it - see api.reattributeSpeaker's own doc comment. Unlike
  // runMerge/runDeleteCharacter, this never deletes anything itself
  // (backend deliberately leaves that to a follow-up manual Delete once a
  // reader sees the row is actually empty), so the confirmation is worded
  // as a redistribution, not a removal.
  const runAutoSplit = (name: string) => {
    if (
      !confirm(
        `Auto Split "${name}"? Every line currently attributed to them in this book will be re-judged and reassigned to whichever real character, Narrator, or Unknown actually speaks it.${name === 'Unknown' ? '' : ` "${name}" will also be marked invalid so it isn't assigned again.`}`,
      )
    ) {
      return
    }
    setAutoSplitError(null)
    setAutoSplitNote(null)
    reattributeSpeaker.mutate(name, {
      onSuccess: ({ queued }) =>
        setAutoSplitNote(
          queued > 0
            ? `Auto Split queued for ${queued} chapter${queued === 1 ? '' : 's'}.`
            : 'Nothing to split — no lines are currently attributed to this speaker.',
        ),
      onError: (err) => setAutoSplitError(err instanceof ApiError ? err.message : 'Auto Split failed'),
    })
  }

  const runGenerateVoice = (characterId: string) => {
    setGenerateVoiceError(null)
    setGeneratingVoiceId(characterId)
    generateVoice.mutate(characterId, {
      onError: (err) =>
        setGenerateVoiceError(
          err instanceof ApiError ? err.message : 'Could not generate a voice for this character',
        ),
      onSettled: () => setGeneratingVoiceId(null),
    })
  }

  const changeCharacterVoiceMode = (mode: CharacterVoiceMode) => {
    if (!voiceQuery.data) return
    updateVoice.mutate({ ...voiceQuery.data, characterVoiceMode: mode })
  }

  const toggleSpeechDirection = (checked: boolean) => {
    if (!voiceQuery.data) return
    updateVoice.mutate({ ...voiceQuery.data, speechDirection: checked })
  }

  const toggleMusicEnabled = (checked: boolean) => {
    if (!voiceQuery.data) return
    updateVoice.mutate({ ...voiceQuery.data, musicEnabled: checked })
  }

  if (bookQuery.isLoading) return <p>Loading…</p>
  if (bookQuery.isError || !bookQuery.data) return <p className="error-text">Could not load this book.</p>

  const book = bookQuery.data

  const runBulk = (action: BulkAction, scope: BulkScope) => {
    setBulkError(null)
    bulkAction.mutate(
      { action, scope },
      { onError: (err) => setBulkError(err instanceof ApiError ? err.message : 'Could not start this action') },
    )
  }

  // Per-pass "Reset" row: clears the pass for the whole book as though it
  // never ran, deleting any audio that used it - see api.resetPass. The
  // character roster is untouched ("Delete speaker data" covers that).
  const runReset = (pass: ResetPass, what: string) => {
    if (!confirm(`Reset ${what} for every chapter of "${book.title}"? This can't be undone.`)) return
    setBulkError(null)
    resetPass.mutate(pass, {
      onError: (err) => setBulkError(err instanceof ApiError ? err.message : 'Could not reset this pass'),
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
  const runPronunciationAll = () => runBulk('pronunciation', 'all')
  const runPronunciationUnresolved = () => runBulk('pronunciation', 'rest')
  const hasUnresolvedPronunciation = book.chapters.some((c) => !c.passes.pronunciation)
  // Always allowed regardless of the book-wide musicEnabled toggle - see
  // api.scoreChapterMusic.
  const runScoreMusicAll = () => runBulk('music_scoring', 'all')
  const runScoreMusicUnscored = () => runBulk('music_scoring', 'rest')
  const hasUnscoredMusic = book.chapters.some((c) => !c.passes.music)

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
  const canGenerateMusic = (c: (typeof book.chapters)[number]) => musicCounts(c).total > 0 && isGenerated(c)
  const missingMusicChapters = book.chapters.filter((c) => canGenerateMusic(c) && !isMusicGenerated(c))
  const runGenerateMissingMusic = () => runBulk('music_generation', 'rest')

  // Roster-wide counterparts, over the series roster (characters marked
  // invalid are skipped server-side). Characters with no dialogue yet are
  // included - characterizing from a name alone is a normal case.
  const runCharacterizeAll = () => runBulk('characterization', 'all')
  // Only characters with no voice yet, matching the per-row "Generate voice"
  // button.
  const runGenerateVoicesAll = () => runBulk('voices', 'rest')
  // Every character, forcing a fresh render whether or not they have a voice.
  const runRegenerateVoicesAll = () => runBulk('voices', 'all')

  // The character roster (names, characterizations, voice assignments,
  // and their auto-created voice presets) is shared across a book's whole
  // series - see backend store.SeriesScope - so deleting it from here
  // reaches every book in that series, not just this one; paragraph
  // attribution itself is cleared for this book only. The confirm text
  // makes that distinction explicit rather than surprising a reader with
  // a series-wide effect from a button on one book's page.
  const runDeleteSpeakerData = () => {
    const warning = book.seriesName
      ? `Delete all speaker data for "${book.title}"?\n\nThis clears every attributed speaker in this book, and deletes the entire character roster (names, characterizations, and voice assignments) for the whole "${book.seriesName}" series - including other books in it. This cannot be undone.`
      : `Delete all speaker data for "${book.title}"?\n\nThis clears every attributed speaker and deletes the entire character roster (names, characterizations, and voice assignments). This cannot be undone.`
    if (!confirm(warning)) return
    setDeleteSpeakerError(null)
    deleteSpeakerData.mutate(undefined, {
      onError: (err) => setDeleteSpeakerError(err instanceof ApiError ? err.message : 'Could not delete speaker data'),
    })
  }

  // Same "run everything" meta-task as the library page's own Preprocess
  // button (attribution -> characterization -> voice provisioning ->
  // direction tagging, POST .../preprocess) - offered here too since a
  // reader already on this page tuning character voices is a natural
  // place to kick it off, without having to go back to the library grid.
  const handlePreprocess = () => {
    preprocessBook.mutate(bookId, {
      onError: (err) =>
        alert(err instanceof ApiError ? err.message : 'Could not start preprocessing'),
    })
  }

  return (
    <div className="speakers-page">
      <div className="library-header">
        <div>
          <h1>Speakers</h1>
          <p className="muted">{book.title}</p>
        </div>
        <div className="speakers-roster-actions">
          <button
            className="text-button"
            onClick={handlePreprocess}
            disabled={book.preprocessing || preprocessBook.isPending}
            title="Preprocess: attribute speakers, characterize, provision voices, and label emotions for the whole book"
          >
            {book.preprocessing ? 'Preprocessing…' : 'Preprocess'}
          </button>
          <Link to="/books/$bookId" params={{ bookId }} className="text-button">
            ← Back to book
          </Link>
        </div>
      </div>

      {bulkError && <p className="error-text">{bulkError}</p>}

      <section className="speakers-multivoice">
        <label>
          Character voices
          <select
            value={voiceQuery.data?.characterVoiceMode ?? DEFAULT_CHARACTER_VOICE_MODE}
            disabled={!voiceQuery.data || updateVoice.isPending}
            onChange={(e) => changeCharacterVoiceMode(e.target.value as CharacterVoiceMode)}
          >
            {CHARACTER_VOICE_MODES.map((mode) => (
              <option key={mode} value={mode}>
                {CHARACTER_VOICE_MODE_LABELS[mode]}
              </option>
            ))}
          </select>
        </label>
        <p className="muted">
          {CHARACTER_VOICE_MODE_DESCRIPTIONS[voiceQuery.data?.characterVoiceMode ?? DEFAULT_CHARACTER_VOICE_MODE]}
        </p>
        {!instructedCloneSupported && (
          <p className="muted">
            The two "style" options above are only available for the "{INSTRUCTED_CLONE_MODEL}" clone
            model - this book uses "{effectiveCloneModel}".
          </p>
        )}

        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={voiceQuery.data?.speechDirection ?? false}
            disabled={!voiceQuery.data || updateVoice.isPending}
            onChange={(e) => toggleSpeechDirection(e.target.checked)}
          />
          Wait for emotion labeling and pronunciation before generating audio
        </label>
        <p className="muted">
          When on, a chapter's audio waits for emotion labeling and pronunciation resolution to finish first, so
          each line is voiced in its emotion and ambiguous abbreviations are read correctly right away, instead of
          needing a later regenerate. Off by default.
        </p>

        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={voiceQuery.data?.musicEnabled ?? false}
            disabled={!voiceQuery.data || updateVoice.isPending}
            onChange={(e) => toggleMusicEnabled(e.target.checked)}
          />
          Background music
        </label>
        <p className="muted">
          When on, each chapter's own background music is generated (from the chapter table below, or
          automatically as the reader reaches it) and mixed in under narration. Off by default - turning this off
          doesn't delete anything already scored or generated, it just stops new chapters from generating audio.
        </p>
      </section>

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
        {musicScoreError && <p className="error-text">{musicScoreError}</p>}
        {musicGenerateError && <p className="error-text">{musicGenerateError}</p>}
        {generateError && <p className="error-text">{generateError}</p>}
        <table className="chapter-attribute-table">
          <thead>
            <tr>
              <th>Chapter</th>
              <th>Attribution</th>
              <th>Description</th>
              <th>Scare quotes</th>
              <th>Emotion</th>
              <th>Pronunciation</th>
              <th>Music</th>
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
              <td className="muted" title="Every chapter that isn't done yet">Rest</td>
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
                  busy={bulkRunning.has('music_scoring')}
                  disabled={!hasUnscoredMusic}
                  onClick={runScoreMusicUnscored}
                  title="Score unscored music — every chapter that isn't scored yet, skipping ones already done"
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
              <td className="muted" title="Every chapter, including ones already done">All</td>
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
                  busy={bulkRunning.has('music_scoring')}
                  onClick={runScoreMusicAll}
                  title="Score all music — every chapter, including ones already done"
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
                  disabled={bulkRunning.has('music_scoring') || resetPass.isPending}
                  onClick={() => runReset('music_scoring', 'music scoring')}
                  title="Reset music scoring — delete every music region and its generated music"
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
              const scoringMusic = scoringMusicIdxs.has(c.idx)
              const generating = generatingIdxs.has(c.idx) || (bulkRunning.has('generate') && !isGenerated(c))
              const generatingMusic = generatingMusicIdxs.has(c.idx)
              return (
                <tr key={c.idx}>
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
                        <RiFileHistoryLine className={reimportingIdx === c.idx ? 'spin' : undefined} />
                      </button>
                      {c.title}
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
                        onClick={() => runScoreMusic(c.idx)}
                        title={
                          scoringMusic
                            ? 'Scoring…'
                            : c.passes.music
                              ? 'Re-score background music for this chapter'
                              : 'Score background music for this chapter'
                        }
                      >
                        <RiMusic2Line className={scoringMusic ? 'spin' : undefined} />
                      </button>
                      <PassStatus done={c.passes.music} label="Music scoring" />
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
                        <span className="chapter-attribute-status" title="Music audio: not scored yet">
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
              )
            })}
          </tbody>
        </table>
      </section>

      <section>
        <div className="library-header">
          <h2>
            Roster
            {speakers.length > 0 && (
              <span className="muted">
                {' '}
                ({filteredSpeakers.length === speakers.length
                  ? speakers.length
                  : `${filteredSpeakers.length} of ${speakers.length}`}
                )
              </span>
            )}
          </h2>
          {speakers.length > 0 && (
            <div className="speakers-roster-actions">
              {speakers.some((s) => s.id) && (
                <button
                  className="icon-action-button"
                  onClick={runCharacterizeAll}
                  disabled={bulkRunning.has('characterization') || characterizingNames.size > 0}
                  title={
                    characterizingNames.size > 0
                      ? 'Recharacterizing…'
                      : "Recharacterize all — re-describe every character's voice from their dialogue, and invalidate their assigned voices so they're rebuilt fresh next time they're needed"
                  }
                >
                  <RiUserHeartLine
                    className={bulkRunning.has('characterization') || characterizingNames.size > 0 ? 'spin' : undefined}
                  />
                </button>
              )}
              {speakers.some((s) => s.id && !s.voicePresetId) && (
                <button
                  className="icon-action-button"
                  onClick={runGenerateVoicesAll}
                  disabled={bulkRunning.has('voices')}
                  title="Generate all voices — characterize and create a voice now for every character that doesn't have one yet"
                >
                  <RiUserVoiceLine className={bulkRunning.has('voices') ? 'spin' : undefined} />
                </button>
              )}
              {speakers.some((s) => s.id) && (
                <button
                  className="icon-action-button"
                  onClick={runRegenerateVoicesAll}
                  disabled={bulkRunning.has('voices')}
                  title="Regenerate all voices — force a fresh render for every character's voice, whether or not they already have one"
                >
                  <RiRefreshLine className={bulkRunning.has('voices') ? 'spin' : undefined} />
                </button>
              )}
              <button
                className="icon-action-button"
                onClick={runDeleteSpeakerData}
                disabled={deleteSpeakerData.isPending}
                title="Delete all speaker data for this book"
              >
                <RiDeleteBinLine />
              </button>
            </div>
          )}
        </div>
        {speakersQuery.isLoading && <p>Loading…</p>}
        {speakersQuery.isError && <p className="error-text">Could not load speakers.</p>}
        {speakers.length === 0 && !speakersQuery.isLoading && (
          <p className="muted">
            No speakers yet — attribute a chapter above to find out who's talking in it.
          </p>
        )}
        {deleteSpeakerError && <p className="error-text">{deleteSpeakerError}</p>}
        {characterizeError && <p className="error-text">{characterizeError}</p>}
        {characterizeNote && <p className="muted">{characterizeNote}</p>}
        {deleteCharacterError && <p className="error-text">{deleteCharacterError}</p>}
        {invalidError && <p className="error-text">{invalidError}</p>}
        {mergeError && <p className="error-text">{mergeError}</p>}
        {autoSplitError && <p className="error-text">{autoSplitError}</p>}
        {autoSplitNote && <p className="muted">{autoSplitNote}</p>}
        {generateVoiceError && <p className="error-text">{generateVoiceError}</p>}
        {speakers.length > 0 && (
          <input
            className="speakers-roster-search"
            type="search"
            placeholder="Search speakers…"
            value={rosterFilter}
            onChange={(e) => setRosterFilter(e.target.value)}
          />
        )}
        {speakers.length > 0 && filteredSpeakers.length === 0 && <p className="muted">No matches</p>}
        <ul className="speaker-list">
          {filteredSpeakers.map((s) => {
            // "Someone other than this character" - shared by the
            // whole-character merge panel and per-line reassignment in
            // this row's own appearances list.
            const otherTargets = [
              'Narrator',
              ...speakers.filter((o) => o.id && o.id !== s.id && !o.invalid).map((o) => o.name),
            ]
            return (
              <SpeakerRow
                key={s.id || s.name}
                bookId={bookId}
                speaker={s.refAudioUrl ? { ...s, refAudioUrl: withCacheBust(s.refAudioUrl, audioVersion[s.id]) } : s}
                builtins={builtins}
                customs={customs}
                bookVoiceName={bookVoiceName}
                narratorPresetId={voiceQuery.data?.presetId}
                bookCloneModel={effectiveCloneModel}
                onCharacterize={() => s.id && runCharacterize(s.id)}
                characterizing={characterizingIds.has(s.id) || characterizingNames.has(s.name)}
                onGenerateVoice={() => s.id && runGenerateVoice(s.id)}
                generatingVoice={generatingVoiceId === s.id}
                onDelete={() => s.id && runDeleteCharacter(s.id, s.name)}
                deleting={deletingCharacterId === s.id}
                onToggleInvalid={() => s.id && runToggleInvalid(s.id, !s.invalid)}
                togglingInvalid={togglingInvalidId === s.id}
                mergeTargets={otherTargets}
                onMerge={(targetName) => s.id && runMerge(s.id, s.name, targetName)}
                merging={mergingId === s.id}
                onToggleMerge={() => setMergingId((cur) => (cur === s.id ? null : s.id))}
                onAutoSplit={() => runAutoSplit(s.name)}
                autoSplitting={reattributingNames.has(s.name)}
                customizing={customizingId === s.id && s.id !== ''}
                onToggleCustomize={() => setCustomizingId((cur) => (cur === s.id ? null : s.id))}
                expanded={appearancesFor === s.id && s.id !== ''}
                onToggleAppearances={() => setAppearancesFor((cur) => (cur === s.id ? null : s.id))}
                appearances={appearancesQuery.data}
                appearancesLoading={appearancesQuery.isLoading}
                reassignTargets={otherTargets}
                descriptionsExpanded={descriptionsFor === s.id && s.id !== ''}
                onToggleDescriptions={() => setDescriptionsFor((cur) => (cur === s.id ? null : s.id))}
                descriptions={descriptionsQuery.data}
                descriptionsLoading={descriptionsQuery.isLoading}
              />
            )
          })}
        </ul>
      </section>
    </div>
  )
}

// One speaker's emotion variants - every emotion their dialogue uses in
// this book (backend speakerRowDTO.Emotions), each a chip with its line
// count, a play button once the variant clip exists, and a regenerate
// button (re-renders the variant and resets those lines' audio - see
// api.regenerateVariant). A pending variant renders by itself the first
// time one of its lines generates.
// Other names a character goes by (Speaker.aliases), shown under their
// name with an inline comma-separated editor. The speakers list is live,
// so a save shows up without any local state beyond the draft.
function SpeakerAliases({ bookId, characterId, aliases }: { bookId: string; characterId: string; aliases: string[] }) {
  const setAliases = useSetCharacterAliases(bookId)
  const [draft, setDraft] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const save = () => {
    if (draft === null) return
    setError(null)
    const next = draft
      .split(',')
      .map((a) => a.trim())
      .filter(Boolean)
    setAliases.mutate(
      { characterId, aliases: next },
      {
        onSuccess: () => setDraft(null),
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not save aliases'),
      },
    )
  }

  if (draft !== null) {
    return (
      <div className="speaker-aliases">
        <input
          className="speaker-aliases-input"
          value={draft}
          autoFocus
          placeholder="Other names, comma-separated (e.g. Albert, Al)"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') save()
            if (e.key === 'Escape') setDraft(null)
          }}
        />
        <button onClick={save} disabled={setAliases.isPending}>
          Save
        </button>
        <button onClick={() => setDraft(null)} disabled={setAliases.isPending}>
          Cancel
        </button>
        {error && <p className="error-text">{error}</p>}
      </div>
    )
  }
  return (
    <div className="speaker-aliases muted">
      {aliases.length > 0 ? <span>Also called {aliases.join(', ')}</span> : null}
      <button
        className="link-button"
        onClick={() => setDraft(aliases.join(', '))}
        title="Other names this character goes by - attribution treats them as this character"
      >
        {aliases.length > 0 ? 'Edit' : 'Add other names'}
      </button>
    </div>
  )
}

function SpeakerEmotions({
  bookId,
  speakerName,
  emotions,
}: {
  bookId: string
  speakerName: string
  emotions: SpeakerEmotion[]
}) {
  const regenerate = useRegenerateVariant(bookId)
  const [error, setError] = useState<string | null>(null)
  // Bumped per emotion on regenerate, so the replaced clip's URL isn't
  // served from the browser cache.
  const [versions, setVersions] = useState<Record<string, number>>({})
  const [playing, setPlaying] = useState<string | null>(null)
  const audioRef = useRef<HTMLAudioElement | null>(null)
  useEffect(() => () => audioRef.current?.pause(), [])

  const togglePlay = (e: SpeakerEmotion) => {
    audioRef.current?.pause()
    if (playing === e.emotion || !e.audioUrl) {
      setPlaying(null)
      return
    }
    const audio = new Audio(withCacheBust(e.audioUrl, versions[e.emotion]))
    audio.onended = () => setPlaying(null)
    audioRef.current = audio
    setPlaying(e.emotion)
    audio.play().catch(() => setPlaying(null))
  }

  const runRegenerate = (emotion: string) => {
    setError(null)
    regenerate.mutate(
      { speaker: speakerName, emotion },
      {
        onSuccess: () => setVersions((v) => ({ ...v, [emotion]: Date.now() })),
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not regenerate this variant'),
      },
    )
  }

  return (
    <div className="speaker-emotions">
      <span className="muted">Emotions:</span>
      {emotions.map((e) => (
        <span
          key={e.emotion}
          className={'speaker-emotion-chip speaker-emotion-' + e.status}
          title={
            e.status === 'ready'
              ? `${e.label}: ${e.count} line(s), variant ready`
              : e.status === 'failed'
                ? `${e.label}: ${e.count} line(s) - the variant couldn't be rendered, so these lines use the base voice`
                : `${e.label}: ${e.count} line(s) - the variant renders the first time one of these lines generates`
          }
        >
          {e.status === 'ready' && (
            <button
              className="speaker-emotion-button"
              onClick={() => togglePlay(e)}
              aria-label={playing === e.emotion ? `Stop ${e.label} sample` : `Play ${e.label} sample`}
            >
              {playing === e.emotion ? <RiPauseFill /> : <RiPlayFill />}
            </button>
          )}
          {e.label} <span className="muted">{e.count}</span>
          <button
            className="speaker-emotion-button"
            onClick={() => runRegenerate(e.emotion)}
            disabled={regenerate.isPending}
            aria-label={`Regenerate ${e.label} variant`}
            title={`Regenerate the ${e.label} variant (and re-voice these lines)`}
          >
            <RiRefreshLine />
          </button>
        </span>
      ))}
      {error && <span className="error-text">{error}</span>}
    </div>
  )
}

function SpeakerRow({
  bookId,
  speaker,
  builtins,
  customs,
  bookVoiceName,
  narratorPresetId,
  bookCloneModel,
  onCharacterize,
  characterizing,
  onGenerateVoice,
  generatingVoice,
  onDelete,
  deleting,
  onToggleInvalid,
  togglingInvalid,
  mergeTargets,
  onMerge,
  merging,
  onToggleMerge,
  onAutoSplit,
  autoSplitting,
  customizing,
  onToggleCustomize,
  expanded,
  onToggleAppearances,
  appearances,
  appearancesLoading,
  reassignTargets,
  descriptionsExpanded,
  onToggleDescriptions,
  descriptions,
  descriptionsLoading,
}: {
  bookId: string
  speaker: Speaker
  builtins: VoicePreset[]
  customs: CustomVoicePreset[]
  bookVoiceName: string | undefined
  // This book's own currently-resolved narrator preset id, if any - passed
  // through to CharacterVoiceEditor so its "test with another voice as a
  // base" control can default to the narrator, the base a reader will
  // pick most often (see store.Book.InstructCharacterVoices).
  narratorPresetId: string | undefined
  // This book's own clone model - what CharacterVoiceEditor's test
  // previews through by default.
  bookCloneModel: string
  onCharacterize: () => void
  characterizing: boolean
  onGenerateVoice: () => void
  generatingVoice: boolean
  onDelete: () => void
  deleting: boolean
  onToggleInvalid: () => void
  togglingInvalid: boolean
  mergeTargets: string[]
  onMerge: (targetName: string) => void
  merging: boolean
  onToggleMerge: () => void
  // "Auto Split" - unlike every other action here, available for the
  // id-less "Unknown" row too (see isNarrator/canAutoSplit below), so it's
  // wired up outside the isNarrator-gated block the rest of these share.
  onAutoSplit: () => void
  autoSplitting: boolean
  customizing: boolean
  onToggleCustomize: () => void
  expanded: boolean
  onToggleAppearances: () => void
  appearances: SpeakerAppearance[] | undefined
  appearancesLoading: boolean
  reassignTargets: string[]
  descriptionsExpanded: boolean
  onToggleDescriptions: () => void
  descriptions: SpeakerAppearance[] | undefined
  descriptionsLoading: boolean
}) {
  const isNarrator = speaker.id === ''
  // Auto Split works by name, not id (see api.reattributeSpeaker), so it's
  // available for "Unknown" too - the one other id-less row isNarrator
  // above also matches (see speakerRowDTO's own doc comment: ID is "" for
  // both the Narrator row and any unregistered speaker name) - just never
  // for the literal Narrator sentinel itself, and never for an empty row
  // with nothing to split.
  const canAutoSplit = speaker.name !== 'Narrator' && speaker.paragraphCount > 0

  return (
    <li className="speaker-row">
      <div className="speaker-row-main">
        <span>
          {speaker.name}
          {speaker.invalid && (
            <span className="speaker-invalid-badge" title="Not a real speaker — attribution won't assign this name">
              invalid
            </span>
          )}
        </span>
        <span className="muted">
          {speaker.readyCount}/{speaker.paragraphCount} generated
        </span>
      </div>

      {!isNarrator && <SpeakerAliases bookId={bookId} characterId={speaker.id} aliases={speaker.aliases ?? []} />}
      {speaker.summary && <p className="muted speaker-row-summary">{speaker.summary}</p>}
      {speaker.refLine && (
        <p className="muted speaker-row-refline">
          Reference line: <em>&ldquo;{speaker.refLine}&rdquo;</em>
        </p>
      )}
      {speaker.emotions && speaker.emotions.length > 0 && (
        <SpeakerEmotions bookId={bookId} speakerName={speaker.name} emotions={speaker.emotions} />
      )}

      <div className="speaker-row-actions">
        {/* The Narrator always narrates in the book's own voice (see
            backend handleListSpeakers' resolveFor) - previewing that voice
            already lives in VoicePanel/the reader, so this ref-clip box is
            only for an actual character's own assigned voice, not a
            redundant copy of the book's. */}
        {!isNarrator && speaker.refAudioUrl && (
          <div className="speaker-row-audio">
            <audio controls preload="none" src={speaker.refAudioUrl} />
          </div>
        )}

        {isNarrator ? (
          speaker.name === 'Narrator' ? (
            <span className="muted">
              Narrates in the book's own voice{bookVoiceName ? ` (${bookVoiceName})` : ''}
            </span>
          ) : (
            // An unregistered speaker name (e.g. "Unknown") - no character
            // id, so none of the real-character actions below apply, but
            // Auto Split works by name and is still offered.
            canAutoSplit && (
              <button
                className="icon-action-button"
                onClick={onAutoSplit}
                disabled={autoSplitting}
                title={
                  autoSplitting
                    ? 'Auto Split in progress…'
                    : 'Auto Split — re-judge and reassign every line currently attributed to this speaker to whichever real character, Narrator, or Unknown actually speaks it'
                }
              >
                <RiScissorsCutLine className={autoSplitting ? 'spin' : undefined} />
              </button>
            )
          )
        ) : (
          <>
            <button
              className="icon-action-button"
              onClick={onCharacterize}
              disabled={characterizing}
              title={
                characterizing
                  ? 'Regenerating characterization…'
                  : 'Regenerate characterization — re-describe this character\'s voice from their dialogue, and invalidate their assigned voice so it\'s rebuilt fresh next time it\'s needed'
              }
            >
              <RiUserHeartLine className={characterizing ? 'spin' : undefined} />
            </button>
            {!speaker.voicePresetId && (
              <button
                className="icon-action-button"
                onClick={onGenerateVoice}
                disabled={generatingVoice}
                title={
                  generatingVoice
                    ? 'Generating…'
                    : "Generate voice — characterize and create this character's voice now, instead of waiting until their dialogue is next generated"
                }
              >
                <RiUserVoiceLine className={generatingVoice ? 'spin' : undefined} />
              </button>
            )}
            <button
              className={'icon-action-button' + (customizing ? ' icon-action-button-active' : '')}
              onClick={onToggleCustomize}
              title="Customize voice"
            >
              <RiEqualizerLine />
            </button>
            <button className="text-button" onClick={onToggleAppearances}>
              {expanded ? 'Hide appearances' : 'View appearances'}
            </button>
            <button className="text-button" onClick={onToggleDescriptions}>
              {descriptionsExpanded ? 'Hide descriptions' : 'View descriptions'}
            </button>
            {canAutoSplit && (
              <button
                className="icon-action-button"
                onClick={onAutoSplit}
                disabled={autoSplitting}
                title={
                  autoSplitting
                    ? 'Auto Split in progress…'
                    : 'Auto Split — this character isn\'t a real, distinct individual: re-judge and reassign every line currently attributed to them to whichever real character, Narrator, or Unknown actually speaks it'
                }
              >
                <RiScissorsCutLine className={autoSplitting ? 'spin' : undefined} />
              </button>
            )}
            <button
              className={'icon-action-button' + (merging ? ' icon-action-button-active' : '')}
              onClick={onToggleMerge}
              title="Merge into another speaker"
            >
              <RiGitMergeLine />
            </button>
            <button
              className={'icon-action-button' + (speaker.invalid ? ' icon-action-button-active' : '')}
              onClick={onToggleInvalid}
              disabled={togglingInvalid}
              title={
                speaker.invalid
                  ? 'Marked invalid — click to allow attribution to assign this name again'
                  : "Mark invalid — not a real speaker; attribution won't create or assign this name again (existing lines stay until Auto Split)"
              }
            >
              <RiForbidLine />
            </button>
            <button
              className="icon-action-button"
              onClick={onDelete}
              disabled={deleting}
              title="Delete this character — their lines revert to Unknown, and their voice is removed"
            >
              <RiDeleteBinLine />
            </button>
          </>
        )}
      </div>

      {merging && <MergeCharacterPanel targets={mergeTargets} onMerge={onMerge} onCancel={onToggleMerge} />}

      {customizing && (
        <CharacterVoiceEditor
          bookId={bookId}
          speaker={speaker}
          builtins={builtins}
          customs={customs}
          narratorPresetId={narratorPresetId}
          bookCloneModel={bookCloneModel}
          onDone={onToggleCustomize}
        />
      )}

      {expanded && (
        <div className="speaker-appearances">
          {appearancesLoading && <p className="muted">Loading…</p>}
          {appearances?.length === 0 && <p className="muted">No appearances found.</p>}
          {appearances?.map((a) => (
            <AppearanceRow
              key={`${a.bookId}:${a.chapterIdx}:${a.paragraphIdx}`}
              appearance={a}
              reassignTargets={reassignTargets}
            />
          ))}
        </div>
      )}

      {descriptionsExpanded && (
        <div className="speaker-appearances">
          {descriptionsLoading && <p className="muted">Loading…</p>}
          {descriptions?.length === 0 && (
            <p className="muted">
              No descriptions found yet — description-tagging runs automatically after a full,
              uninterrupted "Attribute speakers" pass, or can be re-run per chapter above.
            </p>
          )}
          {descriptions?.map((d, i) => (
            <DescriptionRow key={i} description={d} characterName={speaker.name} reassignTargets={reassignTargets} />
          ))}
        </div>
      )}
    </li>
  )
}

// A speaker's own "targetName" picker + confirm/cancel for
// handleMergeCharacter - targets is "Narrator" plus every other real
// character's name (see SpeakersPage's own mergeTargets computation,
// which already excludes the row being merged).
function MergeCharacterPanel({
  targets,
  onMerge,
  onCancel,
}: {
  targets: string[]
  onMerge: (targetName: string) => void
  onCancel: () => void
}) {
  const [target, setTarget] = useState(targets[0] ?? 'Narrator')

  return (
    <div className="speaker-merge-panel">
      <label>
        Merge into
        <select value={target} onChange={(e) => setTarget(e.target.value)}>
          {targets.map((name) => (
            <option key={name} value={name}>
              {name}
            </option>
          ))}
        </select>
      </label>
      <div className="voice-panel-actions">
        <button className="primary-button" onClick={() => onMerge(target)}>
          Merge
        </button>
        <button className="text-button" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  )
}

// One line from "View appearances", with its own "regenerate this line"
// and "reassign this line" actions - a character's appearances can span
// every book in their series (see backend Store.ParagraphsForSpeaker), so
// this owns its own useRegenerateParagraph(appearance.bookId)/
// useSetParagraphSpeaker(appearance.bookId) rather than sharing ones bound
// to whichever book the Speakers page itself is open on. reassignTargets
// is "Narrator" plus every other character's name (the row's own
// mergeTargets, reused - same set, since reassigning one line and merging
// a whole character both mean "someone other than this character").
function AppearanceRow({
  appearance,
  reassignTargets,
}: {
  appearance: SpeakerAppearance
  reassignTargets: string[]
}) {
  const regenerate = useRegenerateParagraph(appearance.bookId)
  const reassign = useSetParagraphSpeaker(appearance.bookId)
  const [error, setError] = useState<string | null>(null)
  const [queued, setQueued] = useState(false)
  const [reassigned, setReassigned] = useState(false)
  const [reassignError, setReassignError] = useState<string | null>(null)

  const run = () => {
    setError(null)
    regenerate.mutate(
      { chapterIdx: appearance.chapterIdx, paragraphIdx: appearance.paragraphIdx },
      {
        onSuccess: () => setQueued(true),
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not generate this line'),
      },
    )
  }

  const runReassign = (targetName: string) => {
    setReassignError(null)
    reassign.mutate(
      { chapterIdx: appearance.chapterIdx, paragraphIdx: appearance.paragraphIdx, speaker: targetName },
      {
        onSuccess: () => setReassigned(true),
        onError: (err) =>
          setReassignError(err instanceof ApiError ? err.message : 'Could not reassign this line'),
      },
    )
  }

  // Reassigned lines disappear from this list once the appearances topic
  // pushes the change (they're no longer this character's) - reassigned
  // stays true in the meantime so a still-visible stale row shows as done
  // rather than silently reappearing enabled, in the gap between the
  // mutation resolving and that update landing.
  return (
    <div className="speaker-appearance-item">
      <div className="speaker-appearance-item-main">
        <span className="book-search-result-chapter">
          {appearance.bookTitle} · {appearance.chapterTitle}
        </span>
        <span className="book-search-result-snippet">{appearance.text}</span>
      </div>
      {appearance.audioUrl && <audio controls preload="none" src={appearance.audioUrl} />}
      <button
        className="icon-action-button"
        onClick={run}
        disabled={regenerate.isPending}
        title={
          regenerate.isPending
            ? 'Generating…'
            : appearance.audioUrl
              ? 'Regenerate this line'
              : 'Generate this line for this speaker'
        }
      >
        <RiRefreshLine className={regenerate.isPending ? 'spin' : undefined} />
      </button>
      <select
        value=""
        disabled={reassign.isPending || reassigned}
        onChange={(e) => {
          if (e.target.value) runReassign(e.target.value)
        }}
        title="Reassign this line to a different speaker"
      >
        <option value="" disabled>
          {reassigned ? 'Reassigned' : 'Reassign to…'}
        </option>
        {reassignTargets.map((name) => (
          <option key={name} value={name}>
            {name}
          </option>
        ))}
      </select>
      {!appearance.audioUrl && queued && !error && <span className="muted">Queued</span>}
      {error && (
        <span className="error-text" title={error}>
          ⚠
        </span>
      )}
      {reassignError && (
        <span className="error-text" title={reassignError}>
          ⚠
        </span>
      )}
    </div>
  )
}

// One line from "View descriptions". Unlike AppearanceRow, this paragraph
// doesn't belong to characterName as its *speaker* (it's narration *about*
// them, almost always still attributed to Narrator as its speaker - see
// backend Store.ParagraphsDescribing's own doc comment) - so "reassign"
// here means moving this paragraph off characterName's own describes-list
// and onto a different character's instead (setParagraphDescription), not
// changing who speaks it. The audio preview, when present, plays whatever
// narrates that paragraph today (the book's own voice, per the same
// resolution).
function DescriptionRow({
  description,
  characterName,
  reassignTargets,
}: {
  description: SpeakerAppearance
  characterName: string
  reassignTargets: string[]
}) {
  const reassign = useSetParagraphDescription(description.bookId)
  const [reassigned, setReassigned] = useState(false)
  const [reassignError, setReassignError] = useState<string | null>(null)

  const runReassign = (targetName: string) => {
    setReassignError(null)
    reassign.mutate(
      { chapterIdx: description.chapterIdx, paragraphIdx: description.paragraphIdx, from: characterName, to: targetName },
      {
        onSuccess: () => setReassigned(true),
        onError: (err) =>
          setReassignError(err instanceof ApiError ? err.message : 'Could not reassign this description'),
      },
    )
  }

  // Same "stays visible as done rather than silently reappearing enabled"
  // reasoning as AppearanceRow's own reassigned flag - this row disappears
  // from the list once the descriptions topic pushes the change.
  return (
    <div className="speaker-appearance-item">
      <div className="speaker-appearance-item-main">
        <span className="book-search-result-chapter">
          {description.bookTitle} · {description.chapterTitle}
        </span>
        <span className="book-search-result-snippet">{description.text}</span>
      </div>
      {description.audioUrl && <audio controls preload="none" src={description.audioUrl} />}
      <select
        value=""
        disabled={reassign.isPending || reassigned}
        onChange={(e) => {
          if (e.target.value) runReassign(e.target.value)
        }}
        title="Reassign this description to a different character"
      >
        <option value="" disabled>
          {reassigned ? 'Reassigned' : 'Reassign to…'}
        </option>
        {reassignTargets.map((name) => (
          <option key={name} value={name}>
            {name}
          </option>
        ))}
      </select>
      {reassignError && (
        <span className="error-text" title={reassignError}>
          ⚠
        </span>
      )}
    </div>
  )
}

// Full voice editor for one character - the same recipe fields the Voices
// page uses (VoiceEditorForm), pre-filled from whichever voice this
// character currently resolves to (their own assigned preset if they have
// one, otherwise their characterization summary, if any) and, on save,
// either updating that preset in place (already a custom one of theirs) or
// creating a fresh custom preset and assigning it to this character in one
// step (currently unassigned, or currently resolving to a built-in, which
// can't be edited in place).
function CharacterVoiceEditor({
  bookId,
  speaker,
  builtins,
  customs,
  narratorPresetId,
  bookCloneModel,
  onDone,
}: {
  bookId: string
  speaker: Speaker
  builtins: VoicePreset[]
  customs: CustomVoicePreset[]
  narratorPresetId: string | undefined
  bookCloneModel: string
  onDone: () => void
}) {
  const createPreset = useCreateCustomVoicePreset()
  const updatePreset = useUpdateCustomVoicePreset()
  const setCharacterVoice = useSetCharacterVoice(bookId)
  const testPreset = useTestCustomVoicePreset()
  const testDesign = useTestVoiceDesign()
  const testCloneInstruct = useTestCloneInstruct()

  // The preset this character's current voice comes from, if any - a
  // custom one of theirs (editable in place) or a built-in (only usable
  // as a starting point for a new custom one, same as the Voices page's
  // own "Derive a new voice").
  const customSource = customs.find((p) => p.id === speaker.voicePresetId)
  const builtinSource = !customSource ? builtins.find((p) => p.id === speaker.voicePresetId) : undefined
  const isEditingCustom = !!customSource

  const [name, setName] = useState(customSource?.name ?? builtinSource?.name ?? speaker.name)
  const [instruct, setInstruct] = useState(customSource?.instruct ?? builtinSource?.instruct ?? speaker.summary ?? '')
  const [refText, setRefText] = useState(customSource?.refText ?? builtinSource?.ref_text ?? DEFAULT_REF_TEXT)
  const [speedMultiplier, setSpeedMultiplier] = useState(
    customSource?.speedMultiplier ?? builtinSource?.speed_multiplier ?? 1,
  )
  // Preview-only - which clone model "Play test" runs the saved voice
  // through. Not part of the voice (it has none); starts at this book's
  // own clone model, what the voice will actually narrate through here.
  const [previewCloneModel, setPreviewCloneModel] = useState(bookCloneModel)
  // Only ever inherited from an already-editable custom preset, never from
  // a builtinSource - same "a new voice never inherits another preset's
  // design model" reasoning as VoicesPage's own openDerive.
  const [designModel, setDesignModel] = useState(customSource?.designModel || DEFAULT_DESIGN_MODEL)
  // Pinned once (not left for the server to pick at save time) so a
  // preview and the eventual create-voice call agree on the same seed -
  // see backend voicerefs.DesignConfigHash. Irrelevant once isEditingCustom
  // (its own already-saved seed is reused untouched).
  const [seed] = useState(customSource?.seed ?? builtinSource?.seed ?? randomSeed())
  const [error, setError] = useState<string | null>(null)
  const [testText, setTestText] = useState(refText)
  const [testAudioUrl, setTestAudioUrl] = useState<string | null>(null)
  const [testError, setTestError] = useState<string | null>(null)

  // "Test with another voice as a base" - previews instructed voice
  // cloning (BreezeTTS 2's own ability to clone a reference clip *and*
  // apply a style instruction in one call) directly, without saving
  // anything: clones baseVoiceId's own already-rendered reference clip
  // while applying the instruct text above as a clone-time instruction -
  // the same thing store.Book.InstructCharacterVoices does automatically
  // for an unassigned character, previewable here per-character before
  // turning that book-wide setting on. Defaults to the book's own
  // narrator voice, the base a reader will pick most often. Kept as its
  // own audio/error state, independent of the "Test with text" box above,
  // so running one doesn't clobber the other's result.
  const [baseVoiceId, setBaseVoiceId] = useState(narratorPresetId ?? builtins[0]?.id ?? '')
  // "" defers to the backend's own configured default
  // (LECTABLE_AUDIOCPP_BREEZE_CLONE_GUIDANCE_SCALE) - only overrides it for
  // this one test call when a reader types a value, so experimenting here
  // never requires a server restart/env var change.
  const [guidanceScale, setGuidanceScale] = useState('')
  const [baseTestAudioUrl, setBaseTestAudioUrl] = useState<string | null>(null)
  const [baseTestError, setBaseTestError] = useState<string | null>(null)

  useEffect(() => {
    return () => {
      if (testAudioUrl) URL.revokeObjectURL(testAudioUrl)
    }
  }, [testAudioUrl])

  useEffect(() => {
    return () => {
      if (baseTestAudioUrl) URL.revokeObjectURL(baseTestAudioUrl)
    }
  }, [baseTestAudioUrl])

  const saving = createPreset.isPending || updatePreset.isPending || setCharacterVoice.isPending

  // Design-preview guidance_scale override - see VoiceEditorForm's
  // test.guidance. "" = the design engine's own default.
  const [designGuidanceScale, setDesignGuidanceScale] = useState('')
  // Sampling-temperature override for either test path - see
  // VoiceEditorForm's test.temperature. "" = the model's own default.
  const [testTemperature, setTestTemperature] = useState('')
  const temperatureOverride = testTemperature.trim() ? Number(testTemperature) : undefined

  const runTest = () => {
    setTestError(null)
    if (isEditingCustom) {
      testPreset.mutate(
        {
          id: customSource.id,
          text: testText,
          cloneModel: previewCloneModel,
          temperature: temperatureOverride,
        },
        {
          onSuccess: (blob) => setTestAudioUrl(URL.createObjectURL(blob)),
          onError: (err) => setTestError(err instanceof ApiError ? err.message : 'Could not synthesize test text'),
        },
      )
      return
    }
    testDesign.mutate(
      {
        instruct,
        text: refText,
        seed,
        designModel,
        guidanceScale: designGuidanceScale.trim() ? Number(designGuidanceScale) : undefined,
        temperature: temperatureOverride,
      },
      {
        onSuccess: (blob) => setTestAudioUrl(URL.createObjectURL(blob)),
        onError: (err) => setTestError(err instanceof ApiError ? err.message : 'Could not preview this voice'),
      },
    )
  }

  const runBaseTest = () => {
    if (!baseVoiceId) return
    setBaseTestError(null)
    testCloneInstruct.mutate(
      {
        baseId: baseVoiceId,
        instruct,
        text: testText,
        cloneModel: INSTRUCTED_CLONE_MODEL,
        guidanceScale: guidanceScale.trim() || undefined,
      },
      {
        onSuccess: (blob) => setBaseTestAudioUrl(URL.createObjectURL(blob)),
        onError: (err) =>
          setBaseTestError(err instanceof ApiError ? err.message : 'Could not synthesize this voice'),
      },
    )
  }

  const save = () => {
    setError(null)
    const input = { name, instruct, refText, speedMultiplier, seed, designModel }
    const onError = (err: unknown) => setError(err instanceof ApiError ? err.message : 'Could not save voice')
    if (isEditingCustom) {
      updatePreset.mutate({ id: customSource.id, input }, { onSuccess: onDone, onError })
      return
    }
    createPreset.mutate(input, {
      onSuccess: (created) =>
        setCharacterVoice.mutate({ characterId: speaker.id, voicePresetId: created.id }, { onSuccess: onDone, onError }),
      onError,
    })
  }

  return (
    <div className="speaker-voice-editor">
      <VoiceEditorForm
        name={name}
        onNameChange={setName}
        instruct={instruct}
        onInstructChange={setInstruct}
        refText={refText}
        onRefTextChange={setRefText}
        speedMultiplier={speedMultiplier}
        onSpeedChange={setSpeedMultiplier}
        designModel={designModel}
        onDesignModelChange={setDesignModel}
        currentAudioUrl={isEditingCustom ? speaker.refAudioUrl : undefined}
        error={error}
        saving={saving}
        saveLabel={isEditingCustom ? 'Save changes' : 'Create and assign voice'}
        saveDisabled={!name || !instruct || !refText}
        onSave={save}
        onCancel={onDone}
        test={{
          text: testText,
          onTextChange: setTestText,
          audioUrl: testAudioUrl,
          error: testError,
          pending: isEditingCustom ? testPreset.isPending : testDesign.isPending,
          disabled: isEditingCustom ? !testText.trim() : !instruct.trim() || !refText.trim(),
          buttonLabel: isEditingCustom ? 'Play test' : 'Preview',
          hint: isEditingCustom
            ? 'Uses the saved voice through the preview cloning model above - save any other changes first to hear them reflected here.'
            : 'Preview renders the reference line above via VoiceDesign directly - no preset saved yet. Saving right after, unchanged, reuses this exact clip instead of rendering again.',
          onRun: runTest,
          guidance: isEditingCustom ? undefined : { value: designGuidanceScale, onChange: setDesignGuidanceScale },
          temperature: { value: testTemperature, onChange: setTestTemperature },
          cloneModel: isEditingCustom ? { value: previewCloneModel, onChange: setPreviewCloneModel } : undefined,
        }}
      />

      <div className="voice-test-box">
        <label>
          Test with another voice as a base
          <select value={baseVoiceId} onChange={(e) => setBaseVoiceId(e.target.value)}>
            {builtins.map((p) => (
              <option key={p.id} value={p.id}>
                {p.id === narratorPresetId ? `${p.name} (this book's narrator)` : p.name}
              </option>
            ))}
            {customs.map((p) => (
              <option key={p.id} value={p.id}>
                {p.id === narratorPresetId ? `${p.name} (this book's narrator)` : p.name}
              </option>
            ))}
          </select>
        </label>
        <p className="muted">
          Clones the selected voice's own reference clip while applying the voice instruction above as a
          clone-time style direction, instead of designing a brand-new voice from scratch - previews
          exactly what "Style unassigned characters with the narrator's own voice" (Speakers page settings)
          does automatically. Only the "{INSTRUCTED_CLONE_MODEL}" clone model actually honors the
          instruction this way, so this always tests through that model regardless of this book's own
          cloning model.
        </p>
        <label>
          Guidance scale
          <input
            type="number"
            min={0}
            step={0.5}
            placeholder="server default"
            value={guidanceScale}
            onChange={(e) => setGuidanceScale(e.target.value)}
          />
        </label>
        <p className="muted">
          How strongly the model follows the instruction (and the reference clip/text) above - higher
          values push harder toward all of that conditioning at once, at some cost to generation speed.
          Blank uses the backend's own configured default. Overrides it for this test only, so you can
          compare values without a server restart.
        </p>
        {baseTestError && <p className="error-text">{baseTestError}</p>}
        <div className="voice-panel-actions">
          <button
            className="primary-button"
            onClick={runBaseTest}
            disabled={!baseVoiceId || !instruct.trim() || !testText.trim() || testCloneInstruct.isPending}
          >
            {testCloneInstruct.isPending ? 'Synthesizing…' : 'Test with base voice'}
          </button>
          {baseTestAudioUrl && <audio controls autoPlay src={baseTestAudioUrl} />}
        </div>
      </div>
    </div>
  )
}

// PassStatus is one chapter-table cell for a yes/no pipeline pass: a
// check once done, an X if not, a spinner while its task is running. The
// full wording lives in the tooltip/aria-label to keep the table narrow.
// A chapter-table bulk-action icon, column-aligned with the per-chapter
// icon of the same pass. With no onClick it renders an invisible spacer
// instead, for a per-chapter action that has no bulk variant.
function BulkActionButton({
  icon: Icon,
  onClick,
  disabled = false,
  busy = false,
  title,
}: {
  icon: IconType
  onClick?: () => void
  disabled?: boolean
  busy?: boolean
  title?: string
}) {
  if (!onClick) {
    return (
      <span className="icon-action-button icon-action-spacer" aria-hidden>
        <Icon />
      </span>
    )
  }
  return (
    <button className="icon-action-button" disabled={disabled || busy} onClick={onClick} title={title}>
      <Icon className={busy ? 'spin' : undefined} />
    </button>
  )
}

// PassStatus is a chapter pass's done/not-done mark. No running state of
// its own - the action button beside it spins while the pass runs.
function PassStatus({ done, label }: { done: boolean; label: string }) {
  const text = `${label}: ${done ? 'done' : 'not done'}`
  return (
    <span
      className={'chapter-attribute-status' + (done ? ' chapter-attribute-status-done' : '')}
      title={text}
      aria-label={text}
    >
      {done ? <RiCheckLine /> : <RiCloseLine />}
    </span>
  )
}

// CountStatus is PassStatus for a ready/total count (narration paragraphs,
// music regions): a check once everything is ready, otherwise the count
// itself - still short, and more useful than a bare X. errors, if any, is
// flagged in the error color.
function CountStatus({
  ready,
  total,
  errors = 0,
  label,
  unit = 'paragraphs',
}: {
  ready: number
  total: number
  errors?: number
  label: string
  unit?: string
}) {
  const done = total > 0 && ready >= total
  const text = `${label}: ${ready}/${total} ${unit} ready` + (errors > 0 ? `, ${errors} failed` : '')
  return (
    <span
      className={
        'chapter-attribute-status' +
        (done ? ' chapter-attribute-status-done' : '') +
        (errors > 0 ? ' chapter-attribute-status-error' : '')
      }
      title={text}
      aria-label={text}
    >
      {done ? <RiCheckLine /> : `${ready}/${total}`}
    </span>
  )
}
