import { useMemo, useState } from 'react'
import { Link, useParams, useSearch } from '@tanstack/react-router'
import {
  RiDeleteBinLine,
  RiGitMergeLine,
  RiHeadphoneLine,
  RiRefreshLine,
  RiSearchEyeLine,
  RiUserHeartLine,
  RiUserVoiceLine,
  RiVoiceprintLine,
} from 'react-icons/ri'
import {
  useBulkAction,
  useBulkActionsRunning,
  useBook,
  useBookJobsSnapshot,
  useCancelBookJobs,
  useCharacterAppearances,
  useCharacterizeSpeaker,
  useCharacterizingCharacters,
  useCustomVoicePresets,
  useDeleteCharacter,
  useDeleteSpeakerData,
  useGenerateCharacterVoice,
  useMergeCharacter,
  usePreprocessBook,
  useReattributeSpeaker,
  useReattributingSpeakers,
  useSetCharacterInvalid,
  useSetCharacterRole,
  useDedupeCharacters,
  useRosterReviewRunning,
  useRunRosterReview,
  useRunVoiceCheck,
  useVoiceCheckRunning,
  useSpeakers,
  useVoice,
  useVoicePresets,
  useSeriesSpeakerLines,
} from '../api/queries'
import { ApiError } from '../api/client'
import { withCacheBust } from '../utils/cacheBust'
import { JobLists } from './JobsPage'
import { LazyRow } from '../components/speakers/LazyRow'
import { SpeakerPeopleLists, SpeakerRow } from '../components/speakers/SpeakerRow'
import { ChapterPassTable } from '../components/speakers/ChapterPassTable'
import { SpeakerSettings } from '../components/speakers/SpeakerSettings'
import { BookDetailsForm } from '../components/BookDetailsForm'
import { BookSourcesPanel } from '../components/BookSourcesPanel'
import { EpubMatchPanel } from '../components/EpubMatchPanel'
import { LexiconPanel } from '../components/speakers/LexiconPanel'
import { BookFactsPanel } from '../components/speakers/BookFactsPanel'
import { SettingPanel } from '../components/speakers/SettingPanel'
import { RosterReviewPanel } from '../components/speakers/RosterReviewPanel'
import { VoiceCheckPanel } from '../components/speakers/VoiceCheckPanel'
import { TimeJumpsPanel } from '../components/speakers/TimeJumpsPanel'
import { sortedRoster } from '../utils/speakerOrder'
import { BOOK_SETTINGS_TABS, type BookSettingsTab } from '../utils/bookSettingsTabs'
import { DEFAULT_CLONE_MODEL } from '../api/types'
import type { BulkAction, BulkScope, Speaker } from '../api/types'

const TAB_LABELS: Record<BookSettingsTab, string> = {
  general: 'General',
  chapters: 'Chapters',
  characters: 'Characters',
  facts: 'Facts',
  pronunciations: 'Pronunciations',
  jobs: 'Jobs',
}

// A stable empty roster while speakers load, so memos keyed on it hold.
const NO_SPEAKERS: Speaker[] = []

// The Jobs tab: this book's part of the queue, with one button to cancel
// all of it.
function BookJobsTab({ bookId, bookTitle }: { bookId: string; bookTitle: string }) {
  const jobsQuery = useBookJobsSnapshot(bookId)
  const cancelJobs = useCancelBookJobs()
  const [cancelError, setCancelError] = useState<string | null>(null)
  const runCancelAll = () => {
    if (!confirm(`Cancel every queued and running job for "${bookTitle}"?`)) return
    setCancelError(null)
    cancelJobs.mutate(bookId, {
      onError: (err) =>
        setCancelError(err instanceof ApiError ? err.message : 'Could not cancel jobs'),
    })
  }
  return (
    <section>
      <div className="library-header">
        <p className="muted">
          What's running and waiting for this book. Narration tasks (one per paragraph) aren't
          listed here - see the <Link to="/jobs">Jobs page</Link>.
        </p>
        <button
          className="text-button"
          onClick={runCancelAll}
          disabled={cancelJobs.isPending}
          title="Cancel every queued and running job for this book, narration included"
        >
          Cancel all
        </button>
      </div>
      {cancelError && <p className="error-text">{cancelError}</p>}
      {jobsQuery.isLoading && <p>Loading…</p>}
      {jobsQuery.isError && <p className="error-text">Could not load this book's jobs.</p>}
      {jobsQuery.data && <JobLists snapshot={jobsQuery.data} />}
    </section>
  )
}

// Per-book settings, in tabs: General (the book-wide switches, character
// voice mode and setting profile), Chapters (run the LLM passes chapter by
// chapter), Characters (review who's been identified so far and how much
// of their dialogue has generated audio, and assign each their own
// narration voice), Facts (the series' world facts), Pronunciations (the
// series lexicon) and Jobs (this
// book's part of the queue). Voice
// *selection* for the book's own (narrator) voice stays in VoicePanel/the
// reader.
// How the Speakers page orders its roster: as first heard (the server's
// order), by lines of dialogue, by name, or by age (youngest first, those
// with no age last).
type RosterSort = 'heard' | 'lines' | 'name' | 'age'

// The roster's view settings, remembered per browser.
function readRosterPref(key: string): string | null {
  try {
    return window.localStorage.getItem(`lectable.roster.${key}`)
  } catch {
    return null
  }
}
function writeRosterPref(key: string, value: string) {
  try {
    window.localStorage.setItem(`lectable.roster.${key}`, value)
  } catch {
    // Private mode: the setting just isn't remembered.
  }
}

// The race/nationality filters' value for speakers with none set.
const NOT_SET = '\u0000not-set'

export function BookSettingsPage() {
  const { bookId } = useParams({ from: '/books/$bookId/settings' })
  const { tab = 'general' } = useSearch({ from: '/books/$bookId/settings' })
  const bookQuery = useBook(bookId)
  const voiceQuery = useVoice(bookId)
  const speakersQuery = useSpeakers(bookId)
  const builtinsQuery = useVoicePresets()
  const customPresetsQuery = useCustomVoicePresets()
  const preprocessBook = usePreprocessBook()
  const characterizeSpeaker = useCharacterizeSpeaker(bookId)
  const deleteSpeakerData = useDeleteSpeakerData(bookId)
  const deleteCharacter = useDeleteCharacter(bookId)
  const setCharacterInvalid = useSetCharacterInvalid(bookId)
  const setCharacterRole = useSetCharacterRole(bookId)
  const dedupeCharacters = useDedupeCharacters(bookId)
  const [dedupeNote, setDedupeNote] = useState<string | null>(null)
  const runRosterReview = useRunRosterReview(bookId)
  const rosterReviewRunning = useRosterReviewRunning(bookId)
  const runVoiceCheck = useRunVoiceCheck(bookId)
  const voiceCheckRunning = useVoiceCheckRunning(bookId)
  const [togglingRoleId, setTogglingRoleId] = useState<string | null>(null)
  const mergeCharacter = useMergeCharacter(bookId)
  const reattributeSpeaker = useReattributeSpeaker(bookId)
  const generateVoice = useGenerateCharacterVoice(bookId)
  // Every whole-book button below goes through one bulk endpoint, queued
  // as a single cancelable Jobs row per click (see api.bulkAction).
  const bulkAction = useBulkAction(bookId)
  const bulkRunning = useBulkActionsRunning(bookId)
  const [bulkError, setBulkError] = useState<string | null>(null)

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
  // Roster filters by race and nationality: '' is everyone, NOT_SET the
  // speakers with none.
  const [raceFilter, setRaceFilter] = useState('')
  const [nationalityFilter, setNationalityFilter] = useState('')
  // Whole series: every speaker of the series with their lines across all
  // of its books; off, only who speaks in this book. Remembered per
  // browser, like the sort order.
  const [wholeSeries, setWholeSeries] = useState(() => readRosterPref('series') === '1')
  const [rosterSort, setRosterSort] = useState<RosterSort>(
    () => (readRosterPref('sort') as RosterSort | null) ?? 'heard',
  )
  const seriesLinesQuery = useSeriesSpeakerLines(bookId, wholeSeries)
  const seriesLines = useMemo(
    () => new Map((seriesLinesQuery.data?.speakers ?? []).map((l) => [l.name, l.lines])),
    [seriesLinesQuery.data],
  )
  const [generatingVoiceId, setGeneratingVoiceId] = useState<string | null>(null)
  const [generateVoiceError, setGenerateVoiceError] = useState<string | null>(null)
  const [appearancesFor, setAppearancesFor] = useState<string | null>(null)
  const [customizingId, setCustomizingId] = useState<string | null>(null)
  // Bumped per character id after a successful recharacterize that
  // actually invalidated their voice, so its <audio> preview re-fetches
  // instead of showing/playing the pre-recharacterize clip once it's
  // lazily rebuilt - see withCacheBust's own doc comment.
  const [audioVersion, setAudioVersion] = useState<Record<string, number>>({})

  const appearancesQuery = useCharacterAppearances(bookId, appearancesFor)

  const builtins = builtinsQuery.data?.presets ?? []
  const customs = customPresetsQuery.data ?? []
  // Narrator first, then alphabetical - see sortedRoster. Everything below
  // (the filtered list, merge/reassign targets) inherits the order.
  const speakers = useMemo(
    () => (speakersQuery.data ? sortedRoster(speakersQuery.data) : NO_SPEAKERS),
    [speakersQuery.data],
  )
  // Roster search - matches on name only. Filters just what's displayed;
  // merge/reassign targets still come from the full speakers list.
  // Who a row's lines can be given to instead (minus the row itself).
  const validNames = useMemo(
    () => speakers.filter((o) => o.id && !o.invalid).map((o) => o.name),
    [speakers],
  )
  const filteredSpeakers = useMemo(() => {
    const q = rosterFilter.trim().toLowerCase()
    const of = (filter: string, value?: string) =>
      filter === '' ||
      (filter === NOT_SET ? !value : (value ?? '').toLowerCase() === filter.toLowerCase())
    // Lines to count and sort by: across the series in the whole-series
    // view, in this book otherwise.
    const lines = (s: Speaker) =>
      wholeSeries ? (seriesLines.get(s.name) ?? s.paragraphCount) : s.paragraphCount
    const shown = speakers.filter(
      (s) =>
        (wholeSeries || s.paragraphCount > 0 || s.name === 'Narrator') &&
        (q === '' || s.name.toLowerCase().includes(q)) &&
        of(raceFilter, s.race) &&
        of(nationalityFilter, s.nationality),
    )
    if (rosterSort === 'lines') return [...shown].sort((a, b) => lines(b) - lines(a))
    if (rosterSort === 'name') return [...shown].sort((a, b) => a.name.localeCompare(b.name))
    if (rosterSort === 'age')
      return [...shown].sort((a, b) => (a.ageYears || Infinity) - (b.ageYears || Infinity))
    return shown
  }, [speakers, rosterFilter, raceFilter, nationalityFilter, wholeSeries, rosterSort, seriesLines])
  // The races and nationalities the roster's speakers have, each with how
  // many, most first - what the filters offer.
  const peopleCounts = useMemo(() => {
    const count = (values: (string | undefined)[]) => {
      const n = new Map<string, number>()
      for (const v of values) if (v) n.set(v.toLowerCase(), (n.get(v.toLowerCase()) ?? 0) + 1)
      return [...n.entries()].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    }
    return {
      races: count(speakers.map((s) => s.race)),
      nationalities: count(speakers.map((s) => s.nationality)),
    }
  }, [speakers])

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
    if (
      !confirm(
        `Delete "${name}"? Their lines in this book revert to Unknown, and their voice is removed.`,
      )
    ) {
      return
    }
    setDeleteCharacterError(null)
    setDeletingCharacterId(characterId)
    deleteCharacter.mutate(characterId, {
      onError: (err) =>
        setDeleteCharacterError(
          err instanceof ApiError ? err.message : 'Could not delete this character',
        ),
      onSettled: () => setDeletingCharacterId(null),
    })
  }

  // "Merge duplicate names" - see api.dedupeCharacters. Merges can't be
  // undone in one click (each merged-away character is deleted), hence the
  // confirm.
  const runDedupe = () => {
    if (
      !confirm(
        'Merge duplicate names? Folds "Protagonist"-style placeholders into the first-person narrator, case variants ("woman"/"Woman") together, descriptions like "bearded man" into their role, and first names into the one full name containing them ("Lars" into "Lars Machmüller"). Numbered names like "Neighbor 1" are kept.',
      )
    ) {
      return
    }
    setDedupeNote(null)
    dedupeCharacters.mutate(undefined, {
      onSuccess: ({ merges }) =>
        setDedupeNote(
          merges.length === 0
            ? 'No duplicate names found.'
            : `Merged ${merges.length}: ` + merges.map((m) => `${m.from} → ${m.to}`).join(', '),
        ),
      onError: (err) =>
        setDedupeNote(err instanceof ApiError ? err.message : 'Could not merge duplicate names'),
    })
  }

  // Non-destructive toggle - see api.setCharacterRole.
  const runToggleRole = (characterId: string, role: boolean) => {
    setInvalidError(null)
    setTogglingRoleId(characterId)
    setCharacterRole.mutate(
      { characterId, role },
      {
        onError: (err) =>
          setInvalidError(
            err instanceof ApiError ? err.message : 'Could not update this character',
          ),
        onSettled: () => setTogglingRoleId(null),
      },
    )
  }

  // Non-destructive toggle - see api.setCharacterInvalid. No confirm:
  // nothing is removed, and it's one click to undo.
  const runToggleInvalid = (characterId: string, invalid: boolean) => {
    setInvalidError(null)
    setTogglingInvalidId(characterId)
    setCharacterInvalid.mutate(
      { characterId, invalid },
      {
        onError: (err) =>
          setInvalidError(
            err instanceof ApiError ? err.message : 'Could not update this character',
          ),
        onSettled: () => setTogglingInvalidId(null),
      },
    )
  }

  const runMerge = (characterId: string, name: string, targetName: string, noAlias: boolean) => {
    if (
      !confirm(
        `Merge "${name}" into "${targetName}"? Their lines in this book are reattributed to "${targetName}", and "${name}"'s own voice is removed.`,
      )
    ) {
      return
    }
    setMergeError(null)
    mergeCharacter.mutate(
      { characterId, targetName, noAlias },
      {
        onSuccess: () => setMergingId(null),
        onError: (err) =>
          setMergeError(err instanceof ApiError ? err.message : 'Could not merge this character'),
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
        `Auto Split "${name}"? Every line currently attributed to them in ${name === 'Unknown' ? 'this book' : 'every book of the series'} will be re-judged and reassigned to whichever real character, Narrator, or Unknown actually speaks it.${name === 'Unknown' ? '' : ` "${name}" will also be marked invalid so it isn't assigned again.`}`,
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
      onError: (err) =>
        setAutoSplitError(err instanceof ApiError ? err.message : 'Auto Split failed'),
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

  if (bookQuery.isLoading) return <p>Loading…</p>
  if (bookQuery.isError || !bookQuery.data)
    return <p className="error-text">Could not load this book.</p>

  const book = bookQuery.data

  const runBulk = (action: BulkAction, scope: BulkScope) => {
    setBulkError(null)
    bulkAction.mutate(
      { action, scope },
      {
        onError: (err) =>
          setBulkError(err instanceof ApiError ? err.message : 'Could not start this action'),
      },
    )
  }

  // Roster-wide counterparts, over the series roster (characters marked
  // invalid are skipped server-side). Characters with no dialogue yet are
  // included - characterizing from a name alone is a normal case.
  const runCharacterizeAll = () => runBulk('characterization', 'all')
  // Only characters with no voice yet, matching the per-row "Generate voice"
  // button.
  const runGenerateVoicesAll = () => runBulk('voices', 'rest')
  // Every character, forcing a fresh render whether or not they have a voice.
  const runRegenerateVoicesAll = () => runBulk('voices', 'all')
  // Only the voices this book's own lines use, but everything each still
  // lacks: the voice itself, its reference clip, its emotion variants.
  const runProvisionUsedVoices = () => runBulk('used_voices', 'rest')

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
      onError: (err) =>
        setDeleteSpeakerError(
          err instanceof ApiError ? err.message : 'Could not delete speaker data',
        ),
    })
  }

  // Same "run everything" meta-task as the library page's own Preprocess
  // button (attribution -> characterization ->
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
          <h1>Book settings</h1>
          <p className="muted">{book.title}</p>
        </div>
        <div className="speakers-roster-actions">
          <button
            className="text-button"
            onClick={handlePreprocess}
            disabled={book.preprocessing || preprocessBook.isPending}
            title="Preprocess: summarize, attribute speakers, characterize, label emotions, score ambience, and resolve pronunciation for the whole book. Voices are made later, when a character's lines are first generated"
          >
            {book.preprocessing ? 'Preprocessing…' : 'Preprocess'}
          </button>
          <Link to="/books/$bookId" params={{ bookId }} className="text-button">
            ← Back to book
          </Link>
        </div>
      </div>

      <nav className="page-tabs">
        {BOOK_SETTINGS_TABS.map((t) => (
          <Link
            key={t}
            to="/books/$bookId/settings"
            params={{ bookId }}
            search={{ tab: t }}
            replace
            className={t === tab ? 'page-tab page-tab-active' : 'page-tab'}
          >
            {TAB_LABELS[t]}
          </Link>
        ))}
      </nav>

      {bulkError && <p className="error-text">{bulkError}</p>}

      {tab === 'general' && (
        <>
          <BookDetailsForm bookId={bookId} title={book.title} author={book.author} />
          <SettingPanel bookId={bookId} />
          <SpeakerSettings bookId={bookId} cloneModel={effectiveCloneModel} />
        </>
      )}

      {tab === 'facts' && <BookFactsPanel bookId={bookId} seriesName={book.seriesName} />}

      {tab === 'pronunciations' && <LexiconPanel bookId={bookId} seriesName={book.seriesName} />}

      {tab === 'jobs' && <BookJobsTab bookId={bookId} bookTitle={book.title} />}

      {tab === 'chapters' && (
        <>
          <BookSourcesPanel book={book} />
          <EpubMatchPanel bookId={bookId} chapters={book.chapters} source={book.source} />
          <ChapterPassTable
            bookId={bookId}
            book={book}
            bulkRunning={bulkRunning}
            runBulk={runBulk}
            onBulkError={setBulkError}
          />
        </>
      )}

      {tab === 'characters' && (
        <section>
          <div className="library-header">
            <h2>
              Roster
              {speakers.length > 0 && (
                <span className="muted">
                  {' '}
                  (
                  {filteredSpeakers.length === speakers.length
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
                      className={
                        bulkRunning.has('characterization') || characterizingNames.size > 0
                          ? 'spin'
                          : undefined
                      }
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
                  onClick={runProvisionUsedVoices}
                  disabled={bulkRunning.has('used_voices')}
                  title="Provision used voices — for every voice this book's lines use, queue whatever is still missing (a voice for a character without one, its reference clip, and the emotion variants its lines need) so generation doesn't wait on them"
                >
                  <RiVoiceprintLine
                    className={bulkRunning.has('used_voices') ? 'spin' : undefined}
                  />
                </button>
                <button
                  className="icon-action-button"
                  onClick={runDedupe}
                  disabled={dedupeCharacters.isPending}
                  title="Merge duplicate names — placeholders like “Protagonist” into the first-person narrator, case variants, descriptions into their role, first names into full names"
                >
                  <RiGitMergeLine className={dedupeCharacters.isPending ? 'spin' : undefined} />
                </button>
                <button
                  className="icon-action-button"
                  onClick={() => runRosterReview.mutate()}
                  disabled={runRosterReview.isPending || rosterReviewRunning}
                  title="Review the roster — look for one person under two names, aliases that are someone else's name and descriptions not marked as roles. Suggestions appear below for you to accept or skip; nothing changes on its own"
                >
                  <RiSearchEyeLine className={rosterReviewRunning ? 'spin' : undefined} />
                </button>
                <button
                  className="icon-action-button"
                  onClick={() => runVoiceCheck.mutate()}
                  disabled={runVoiceCheck.isPending || voiceCheckRunning}
                  title="Check the voices — an audio model listens to each character's voice, and each of their ages, and lists below the ones that don't sound like their casting (a man cast and a woman heard, a child heard as an adult). It is a rough judge: nothing is re-rendered unless you ask"
                >
                  <RiHeadphoneLine className={voiceCheckRunning ? 'spin' : undefined} />
                </button>
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
          <RosterReviewPanel bookId={bookId} />
          <VoiceCheckPanel bookId={bookId} />
          <TimeJumpsPanel bookId={bookId} />
          {speakersQuery.isLoading && <p>Loading…</p>}
          {speakersQuery.isError && <p className="error-text">Could not load speakers.</p>}
          {speakers.length === 0 && !speakersQuery.isLoading && (
            <p className="muted">
              No speakers yet — attribute a chapter on the Chapters tab to find out who's talking in
              it.
            </p>
          )}
          {deleteSpeakerError && <p className="error-text">{deleteSpeakerError}</p>}
          {characterizeError && <p className="error-text">{characterizeError}</p>}
          {characterizeNote && <p className="muted">{characterizeNote}</p>}
          {dedupeNote && <p className="muted">{dedupeNote}</p>}
          {deleteCharacterError && <p className="error-text">{deleteCharacterError}</p>}
          {invalidError && <p className="error-text">{invalidError}</p>}
          {mergeError && <p className="error-text">{mergeError}</p>}
          {autoSplitError && <p className="error-text">{autoSplitError}</p>}
          {autoSplitNote && <p className="muted">{autoSplitNote}</p>}
          {generateVoiceError && <p className="error-text">{generateVoiceError}</p>}
          {speakers.length > 0 && (
            <div className="speakers-roster-filters">
              <input
                className="speakers-roster-search"
                type="search"
                placeholder="Search speakers…"
                value={rosterFilter}
                onChange={(e) => setRosterFilter(e.target.value)}
              />
              <label className="muted">
                Race{' '}
                <select value={raceFilter} onChange={(e) => setRaceFilter(e.target.value)}>
                  <option value="">Any</option>
                  <option value={NOT_SET}>Not set</option>
                  {peopleCounts.races.map(([v, n]) => (
                    <option key={v} value={v}>
                      {v} ({n})
                    </option>
                  ))}
                </select>
              </label>
              <label className="muted">
                Nationality{' '}
                <select
                  value={nationalityFilter}
                  onChange={(e) => setNationalityFilter(e.target.value)}
                >
                  <option value="">Any</option>
                  <option value={NOT_SET}>Not set</option>
                  {peopleCounts.nationalities.map(([v, n]) => (
                    <option key={v} value={v}>
                      {v} ({n})
                    </option>
                  ))}
                </select>
              </label>
              <label className="muted">
                Sort{' '}
                <select
                  value={rosterSort}
                  onChange={(e) => {
                    setRosterSort(e.target.value as RosterSort)
                    writeRosterPref('sort', e.target.value)
                  }}
                >
                  <option value="heard">First heard</option>
                  <option value="lines">Most lines</option>
                  <option value="name">Name</option>
                  <option value="age">Age</option>
                </select>
              </label>
              <label
                className="checkbox-label muted"
                title="Show every speaker of the series, counting and sorting by their lines across all of its books. Off, only who speaks in this book"
              >
                <input
                  type="checkbox"
                  checked={wholeSeries}
                  onChange={(e) => {
                    setWholeSeries(e.target.checked)
                    writeRosterPref('series', e.target.checked ? '1' : '0')
                  }}
                />
                Whole series
              </label>
            </div>
          )}
          {speakers.length > 0 && filteredSpeakers.length === 0 && (
            <p className="muted">No matches</p>
          )}
          <SpeakerPeopleLists bookId={bookId} />
          <ul className="speaker-list">
            {filteredSpeakers.map((s, i) => (
              // Only the rows near the screen are rendered (LazyRow): a
              // series roster is hundreds of speakers, each with its own
              // controls and voice samples.
              <LazyRow
                key={s.id || s.name}
                initiallyNear={i < 12}
                estimatedHeight={170}
                pinned={
                  s.id !== '' &&
                  (mergingId === s.id || customizingId === s.id || appearancesFor === s.id)
                }
              >
                {() => {
                  // "Someone other than this character" - shared by the
                  // whole-character merge panel and per-line reassignment
                  // in this row's own appearances list.
                  const otherTargets = ['Narrator', ...validNames.filter((n) => n !== s.name)]
                  return (
                    <SpeakerRow
                      bookId={bookId}
                      seriesLines={wholeSeries ? seriesLines.get(s.name) : undefined}
                      speaker={
                        s.refAudioUrl
                          ? { ...s, refAudioUrl: withCacheBust(s.refAudioUrl, audioVersion[s.id]) }
                          : s
                      }
                      builtins={builtins}
                      customs={customs}
                      bookVoiceName={bookVoiceName}
                      narratorPresetId={voiceQuery.data?.presetId}
                      bookCloneModel={effectiveCloneModel}
                      onCharacterize={() => s.id && runCharacterize(s.id)}
                      characterizing={
                        characterizingIds.has(s.id) || characterizingNames.has(s.name)
                      }
                      onGenerateVoice={() => s.id && runGenerateVoice(s.id)}
                      generatingVoice={generatingVoiceId === s.id}
                      onDelete={() => s.id && runDeleteCharacter(s.id, s.name)}
                      deleting={deletingCharacterId === s.id}
                      onToggleInvalid={() => s.id && runToggleInvalid(s.id, !s.invalid)}
                      togglingInvalid={togglingInvalidId === s.id}
                      onToggleRole={() => s.id && runToggleRole(s.id, !s.role)}
                      togglingRole={togglingRoleId === s.id}
                      mergeTargets={otherTargets}
                      onMerge={(targetName, noAlias) =>
                        s.id && runMerge(s.id, s.name, targetName, noAlias)
                      }
                      merging={mergingId === s.id}
                      onToggleMerge={() => setMergingId((cur) => (cur === s.id ? null : s.id))}
                      onAutoSplit={() => runAutoSplit(s.name)}
                      autoSplitting={reattributingNames.has(s.name)}
                      customizing={customizingId === s.id && s.id !== ''}
                      onToggleCustomize={() =>
                        setCustomizingId((cur) => (cur === s.id ? null : s.id))
                      }
                      expanded={appearancesFor === s.id && s.id !== ''}
                      onToggleAppearances={() =>
                        setAppearancesFor((cur) => (cur === s.id ? null : s.id))
                      }
                      appearances={appearancesQuery.data}
                      appearancesLoading={appearancesQuery.isLoading}
                      reassignTargets={otherTargets}
                    />
                  )
                }}
              </LazyRow>
            ))}
          </ul>
        </section>
      )}
    </div>
  )
}
