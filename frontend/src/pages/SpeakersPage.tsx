import { useEffect, useMemo, useState } from 'react'
import { Link, useParams } from '@tanstack/react-router'
import {
  RiCheckLine,
  RiCloseLine,
  RiDeleteBinLine,
  RiDiscLine,
  RiDoubleQuotesL,
  RiEmotionLine,
  RiSpeakLine,
  RiEqualizerLine,
  RiForbidLine,
  RiGitMergeLine,
  RiLoader4Line,
  RiMusic2Line,
  RiPriceTag3Line,
  RiRefreshLine,
  RiScissorsCutLine,
  RiUserHeartLine,
  RiUserSearchLine,
  RiUserVoiceLine,
  RiVoiceprintLine,
} from 'react-icons/ri'
import {
  useAttributeSpeakers,
  useAttributingChapters,
  useBook,
  useCharacterAppearances,
  useCharacterDescriptions,
  useCharacterizeSpeaker,
  useCharacterizeSpeakers,
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
  useGenerateCharacterVoices,
  useGeneratingChapters,
  useGeneratingMusicChapters,
  useMergeCharacter,
  usePreprocessBook,
  useReattributeSpeaker,
  useReattributingSpeakers,
  useSetCharacterInvalid,
  useRegenerateCharacterVoices,
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
import {
  CHARACTER_VOICE_MODE_DESCRIPTIONS,
  CHARACTER_VOICE_MODE_LABELS,
  CHARACTER_VOICE_MODES,
  DEFAULT_CHARACTER_VOICE_MODE,
  DEFAULT_DESIGN_MODEL,
  DEFAULT_CLONE_MODEL,
  HIGGS_CLONE_MODEL,
  INSTRUCTED_CLONE_MODEL,
  type CharacterVoiceMode,
} from '../api/types'
import type { CustomVoicePreset, Speaker, SpeakerAppearance, VoicePreset } from '../api/types'

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
  const characterizeSpeakers = useCharacterizeSpeakers(bookId)
  const deleteSpeakerData = useDeleteSpeakerData(bookId)
  const deleteCharacter = useDeleteCharacter(bookId)
  const setCharacterInvalid = useSetCharacterInvalid(bookId)
  const mergeCharacter = useMergeCharacter(bookId)
  const reattributeSpeaker = useReattributeSpeaker(bookId)
  const generateVoice = useGenerateCharacterVoice(bookId)
  const generateVoices = useGenerateCharacterVoices(bookId)
  const regenerateVoices = useRegenerateCharacterVoices(bookId)

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
  // useCharacterizeSpeakers/useCharacterizingCharacters' own doc comments);
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
  // it's somehow unset. Direction tagging's own tag vocabulary is specific to Higgs's tokenizer (see
  // backend speakerattr.validDirectionTags), so this gates whether the
  // tag-directions buttons below do anything - the backend enforces the
  // same check server-side regardless (400 for any other clone model),
  // this is just so the UI doesn't offer an action that can't work.
  const effectiveCloneModel = voiceQuery.data?.cloneModel || DEFAULT_CLONE_MODEL
  const directionSupported = effectiveCloneModel === HIGGS_CLONE_MODEL

  // Same gating shape as directionSupported, for
  // instructCharacterVoices - only actually does anything when the book's
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

  // Fires every chapter's own attribute-speakers request at once, rather
  // than requiring N individual clicks - safe to do in a burst since the
  // backend queues/serializes them one at a time regardless (TierUrgent,
  // one shared LLM slot - see internal/jobs' KindSpeakerAttribution).
  // Re-attributing an already-attributed chapter is the same idempotent
  // re-run the single "Attribute speakers" button already does, so this
  // doesn't try to skip chapters that look already done.
  const runAttributeAll = () => {
    book.chapters.forEach((c) => runAttribute(c.idx))
  }

  // Same batch trigger as runAttributeAll, but skips any chapter already
  // marked attributed (see ChapterSummary.passes.attribution) - for
  // topping up a book after adding chapters or an interrupted run,
  // without re-running (and re-billing the LLM for) chapters already
  // done.
  const runAttributeUnattributed = () => {
    book.chapters.filter((c) => !c.passes.attribution).forEach((c) => runAttribute(c.idx))
  }
  const hasUnattributed = book.chapters.some((c) => !c.passes.attribution)

  // isDirected/hasUndirected mirror c.passes.attribution/hasUnattributed
  // exactly - passes.direction is a single flat bool, not keyed by clone
  // model (only Higgs ever produces sentence/inline tags - see
  // directionSupported). runDirectionAll/runDirectionUndirected fire every chapter's own
  // request at once, same batching rationale as runAttributeAll/
  // runAttributeUnattributed - the backend queues them one at a time
  // regardless (poolLLM, KindSpeechDirection).
  const isDirected = (c: (typeof book.chapters)[number]) => c.passes.direction
  const runDirectionAll = () => {
    book.chapters.forEach((c) => runDirection(c.idx))
  }
  const runDirectionUndirected = () => {
    book.chapters.filter((c) => !isDirected(c)).forEach((c) => runDirection(c.idx))
  }
  const hasUndirected = book.chapters.some((c) => !isDirected(c))

  // Same shape again, for pronunciation resolution (c.passes.pronunciation)
  // - available for every clone model, unlike direction tagging.
  const runPronunciationAll = () => {
    book.chapters.forEach((c) => runPronunciation(c.idx))
  }
  const runPronunciationUnresolved = () => {
    book.chapters.filter((c) => !c.passes.pronunciation).forEach((c) => runPronunciation(c.idx))
  }
  const hasUnresolvedPronunciation = book.chapters.some((c) => !c.passes.pronunciation)

  // isScoredMusic/hasUnscoredMusic/runScoreMusicAll/runScoreMusicUnscored
  // mirror isDirected/hasUndirected/runDirectionAll/runDirectionUndirected
  // exactly, for c.passes.music instead - see api.scoreChapterMusic's own
  // doc comment for why this is always allowed regardless of the book-wide
  // musicEnabled toggle.
  const isScoredMusic = (c: (typeof book.chapters)[number]) => c.passes.music
  const runScoreMusicAll = () => {
    book.chapters.forEach((c) => runScoreMusic(c.idx))
  }
  const runScoreMusicUnscored = () => {
    book.chapters.filter((c) => !isScoredMusic(c)).forEach((c) => runScoreMusic(c.idx))
  }
  const hasUnscoredMusic = book.chapters.some((c) => !isScoredMusic(c))

  // isGenerated/hasUngenerated/runGenerateAll/runGenerateUngenerated mirror
  // isScoredMusic/hasUnscoredMusic/runScoreMusicAll/runScoreMusicUnscored
  // exactly, for a chapter's own narration audio (readyCount/paragraphCount
  // - see PlayerBar's own identical "c.readyCount < c.paragraphCount" not-
  // fully-generated check) instead of a tagging pass. Each call goes
  // through the same backend jobs.Manager.EnqueueChapter every other
  // "generate this chapter" entry point (the reader, the library page's
  // "Generate audio") already uses, so it's idempotent per chapter and -
  // per maybeScoreChapterMusic - also kicks off background-music scoring
  // for a not-yet-scored chapter when the book has music turned on.
  const isGenerated = (c: (typeof book.chapters)[number]) => c.readyCount >= c.paragraphCount
  const runGenerateAll = () => {
    book.chapters.forEach((c) => runGenerate(c.idx))
  }
  const runGenerateUngenerated = () => {
    book.chapters.filter((c) => !isGenerated(c)).forEach((c) => runGenerate(c.idx))
  }
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
  const runGenerateMissingMusic = () => {
    missingMusicChapters.forEach((c) => runGenerateMusic(c.idx))
  }

  // One batch request (see useCharacterizeSpeakers/handleCharacterizeSpeakers)
  // rather than firing runCharacterize once per character - a real backend
  // enqueue, not a client-side loop, so it isn't at the mercy of the
  // browser's own per-origin connection cap or a page reload dropping
  // whichever characters' requests hadn't gone out yet. Includes characters
  // with no dialogue attributed yet too - characterizeVoice's own "nothing
  // to characterize from yet" case is a normal no-op there, not an error,
  // so there's nothing to gain from trying to pre-filter them out here.
  const runCharacterizeAll = () => {
    setCharacterizeError(null)
    setCharacterizeNote(null)
    const ids = speakers.map((s) => s.id).filter((id): id is string => !!id)
    characterizeSpeakers.mutate(ids, {
      onError: (err) =>
        setCharacterizeError(err instanceof ApiError ? err.message : 'Recharacterize all failed'),
    })
  }

  // One batch request (see useGenerateCharacterVoices/
  // handleGenerateCharacterVoices), same rationale as runCharacterizeAll.
  // Only characters with no voice yet - matching the per-row "Generate
  // voice" button, which is itself only ever shown for those (see
  // SpeakerRow below); a character that already has one has nothing for
  // this to do.
  const runGenerateVoicesAll = () => {
    setGenerateVoiceError(null)
    const ids = speakers.filter((s) => s.id && !s.voicePresetId).map((s) => s.id)
    generateVoices.mutate(ids, {
      onError: (err) =>
        setGenerateVoiceError(err instanceof ApiError ? err.message : 'Generate all voices failed'),
    })
  }

  // One batch request (see useRegenerateCharacterVoices/
  // handleRegenerateCharacterVoices) - every character with an id, not
  // just un-voiced ones (unlike runGenerateVoicesAll above): this forces a
  // fresh render regardless of whether a voice already exists.
  const runRegenerateVoicesAll = () => {
    setGenerateVoiceError(null)
    const ids = speakers.map((s) => s.id).filter((id): id is string => !!id)
    regenerateVoices.mutate(ids, {
      onError: (err) =>
        setGenerateVoiceError(err instanceof ApiError ? err.message : 'Regenerate all voices failed'),
    })
  }

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
            title="Preprocess: attribute speakers, characterize, provision voices, and tag directions for the whole book"
          >
            {book.preprocessing ? 'Preprocessing…' : 'Preprocess'}
          </button>
          <Link to="/books/$bookId" params={{ bookId }} className="text-button">
            ← Back to book
          </Link>
        </div>
      </div>

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
          {directionSupported
            ? 'Wait for direction tagging and pronunciation before generating audio'
            : 'Wait for pronunciation resolution before generating audio'}
        </label>
        <p className="muted">
          {directionSupported
            ? "When on, a chapter's audio waits for direction tagging and pronunciation resolution to finish first, so its narration picks up any delivery tags and resolved abbreviations right away instead of needing a later regenerate. Off by default."
            : `When on, a chapter's audio waits for pronunciation resolution to finish first, so ambiguous abbreviations are read correctly right away instead of needing a later regenerate. Off by default. (Direction tagging is only available for the "${HIGGS_CLONE_MODEL}" clone model - this book uses "${effectiveCloneModel}".)`}
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
          <div className="speakers-roster-actions">
            <button
              className="text-button"
              onClick={runAttributeUnattributed}
              disabled={attributingIdxs.size > 0 || !hasUnattributed}
              title="Attribute every chapter that isn't fully attributed yet, skipping ones already done"
            >
              Attribute unattributed
            </button>
            <button className="text-button" onClick={runAttributeAll} disabled={attributingIdxs.size > 0}>
              Attribute all
            </button>
            {directionSupported && (
              <>
                <button
                  className="text-button"
                  onClick={runDirectionUndirected}
                  disabled={directingIdxs.size > 0 || !hasUndirected}
                  title="Tag every chapter that isn't fully direction-tagged yet, skipping ones already done"
                >
                  Tag undirected
                </button>
                <button className="text-button" onClick={runDirectionAll} disabled={directingIdxs.size > 0}>
                  Tag all directions
                </button>
              </>
            )}
            <button
              className="text-button"
              onClick={runPronunciationUnresolved}
              disabled={pronouncingIdxs.size > 0 || !hasUnresolvedPronunciation}
              title="Resolve pronunciation for every chapter that isn't resolved yet, skipping ones already done"
            >
              Resolve unresolved pronunciation
            </button>
            <button className="text-button" onClick={runPronunciationAll} disabled={pronouncingIdxs.size > 0}>
              Resolve all pronunciation
            </button>
            <button
              className="text-button"
              onClick={runScoreMusicUnscored}
              disabled={scoringMusicIdxs.size > 0 || !hasUnscoredMusic}
              title="Score background music for every chapter that isn't scored yet, skipping ones already done"
            >
              Score unscored music
            </button>
            <button className="text-button" onClick={runScoreMusicAll} disabled={scoringMusicIdxs.size > 0}>
              Score all music
            </button>
            <button
              className="text-button"
              onClick={runGenerateUngenerated}
              disabled={generatingIdxs.size > 0 || !hasUngenerated}
              title="Generate audio for every chapter that isn't fully generated yet, skipping ones already done"
            >
              Generate ungenerated
            </button>
            <button className="text-button" onClick={runGenerateAll} disabled={generatingIdxs.size > 0}>
              Generate all audio
            </button>
            <button
              className="text-button"
              onClick={runGenerateMissingMusic}
              disabled={generatingMusicIdxs.size > 0 || missingMusicChapters.length === 0}
              title="Generate background music for every scored, fully-narrated chapter that's missing any, retrying failed regions"
            >
              Generate missing music
            </button>
          </div>
        </div>
        {!directionSupported && (
          <p className="muted">
            Speech-direction tagging (inline delivery tags like <code>&lt;|emotion:anger|&gt;</code>) is only
            available for the "{HIGGS_CLONE_MODEL}" clone model - this book uses "
            {effectiveCloneModel}".
          </p>
        )}
        {attributeError && <p className="error-text">{attributeError}</p>}
        {retagError && <p className="error-text">{retagError}</p>}
        {retagScareQuoteError && <p className="error-text">{retagScareQuoteError}</p>}
        {directionError && <p className="error-text">{directionError}</p>}
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
              {directionSupported && <th>Direction</th>}
              <th>Pronunciation</th>
              <th>Music</th>
              <th>Audio</th>
              <th>Music audio</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {book.chapters.map((c) => {
              const attributing = attributingIdxs.has(c.idx)
              const retagging = describingIdxs.has(c.idx)
              const retaggingScareQuotes = scareQuotingIdxs.has(c.idx)
              const directing = directingIdxs.has(c.idx)
              const pronouncing = pronouncingIdxs.has(c.idx)
              const scoringMusic = scoringMusicIdxs.has(c.idx)
              const generating = generatingIdxs.has(c.idx)
              const generatingMusic = generatingMusicIdxs.has(c.idx)
              return (
                <tr key={c.idx}>
                  <td>{c.title}</td>
                  <td>
                    <PassStatus done={c.passes.attribution} busy={attributing} label="Attribution" />
                  </td>
                  <td>
                    <PassStatus done={c.passes.description} busy={retagging} label="Description tagging" />
                  </td>
                  <td>
                    <PassStatus done={c.passes.scareQuote} busy={retaggingScareQuotes} label="Scare-quote tagging" />
                  </td>
                  {directionSupported && (
                    <td>
                      <PassStatus done={c.passes.direction} busy={directing} label="Direction tagging" />
                    </td>
                  )}
                  <td>
                    <PassStatus done={c.passes.pronunciation} busy={pronouncing} label="Pronunciation resolution" />
                  </td>
                  <td>
                    <PassStatus done={c.passes.music} busy={scoringMusic} label="Music scoring" />
                  </td>
                  <td>
                    <CountStatus
                      ready={c.readyCount}
                      total={c.paragraphCount}
                      busy={generating}
                      label="Narration audio"
                    />
                  </td>
                  <td>
                    {musicCounts(c).total === 0 ? (
                      <span className="chapter-attribute-status" title="Music audio: not scored yet">
                        —
                      </span>
                    ) : (
                      <CountStatus
                        ready={musicCounts(c).ready}
                        total={musicCounts(c).total}
                        errors={musicCounts(c).errors}
                        busy={generatingMusic}
                        label="Music audio"
                        unit="regions"
                      />
                    )}
                  </td>
                  <td>
                    <div className="chapter-attribute-actions">
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
                      {directionSupported && (
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
                      )}
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
                  disabled={characterizeSpeakers.isPending || characterizingNames.size > 0}
                  title={
                    characterizingNames.size > 0
                      ? 'Recharacterizing…'
                      : "Recharacterize all — re-describe every character's voice from their dialogue, and invalidate their assigned voices so they're rebuilt fresh next time they're needed"
                  }
                >
                  <RiUserHeartLine
                    className={characterizeSpeakers.isPending || characterizingNames.size > 0 ? 'spin' : undefined}
                  />
                </button>
              )}
              {speakers.some((s) => s.id && !s.voicePresetId) && (
                <button
                  className="icon-action-button"
                  onClick={runGenerateVoicesAll}
                  disabled={generateVoices.isPending}
                  title="Generate all voices — characterize and create a voice now for every character that doesn't have one yet"
                >
                  <RiUserVoiceLine className={generateVoices.isPending ? 'spin' : undefined} />
                </button>
              )}
              {speakers.some((s) => s.id) && (
                <button
                  className="icon-action-button"
                  onClick={runRegenerateVoicesAll}
                  disabled={regenerateVoices.isPending}
                  title="Regenerate all voices — force a fresh render for every character's voice, whether or not they already have one"
                >
                  <RiRefreshLine className={regenerateVoices.isPending ? 'spin' : undefined} />
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

      {speaker.summary && <p className="muted speaker-row-summary">{speaker.summary}</p>}
      {speaker.refLine && (
        <p className="muted speaker-row-refline">
          Reference line: <em>&ldquo;{speaker.refLine}&rdquo;</em>
        </p>
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
function PassStatus({ done, busy, label }: { done: boolean; busy: boolean; label: string }) {
  const text = `${label}: ${busy ? 'running…' : done ? 'done' : 'not done'}`
  return (
    <span
      className={'chapter-attribute-status' + (done && !busy ? ' chapter-attribute-status-done' : '')}
      title={text}
      aria-label={text}
    >
      {busy ? <RiLoader4Line className="spin" /> : done ? <RiCheckLine /> : <RiCloseLine />}
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
  busy,
  label,
  unit = 'paragraphs',
}: {
  ready: number
  total: number
  errors?: number
  busy: boolean
  label: string
  unit?: string
}) {
  const done = total > 0 && ready >= total
  const text =
    `${label}: ${ready}/${total} ${unit} ready` + (errors > 0 ? `, ${errors} failed` : '') + (busy ? ' — generating…' : '')
  return (
    <span
      className={
        'chapter-attribute-status' +
        (done && !busy ? ' chapter-attribute-status-done' : '') +
        (errors > 0 && !busy ? ' chapter-attribute-status-error' : '')
      }
      title={text}
      aria-label={text}
    >
      {busy ? <RiLoader4Line className="spin" /> : done ? <RiCheckLine /> : `${ready}/${total}`}
    </span>
  )
}
