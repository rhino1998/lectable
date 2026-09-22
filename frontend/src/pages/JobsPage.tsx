import { useMemo, useRef, useState } from 'react'
import { RiCloseCircleLine, RiPauseCircleLine, RiPlayCircleLine, RiRestartLine, RiStopCircleLine } from 'react-icons/ri'
import {
  useCancelAllJobs,
  useCancelJob,
  useJobsSnapshot,
  usePauseJobs,
  useResumeJobs,
  useRestartWorker,
  useSetJobTier,
  useVoicePresets,
  useCustomVoicePresets,
} from '../api/queries'
import { ApiError } from '../api/client'
import { useClickOutside } from '../hooks/useClickOutside'
import type { QueueTask } from '../api/types'

const QUEUED_DISPLAY_LIMIT = 200
// A raw instruct string can be a full sentence or two - too long for a
// table cell, so it's shown as a short preview rather than the whole thing.
const INSTRUCT_PREVIEW_LENGTH = 40

const KIND_LABELS: Record<QueueTask['kind'], string> = {
  voice_clone: 'Clone',
  voice_design: 'Design',
  voice_design_preview: 'Design Preview',
  voice_provision: 'Voice Provision',
  speaker_attribution: 'Attribution',
  speaker_characterization: 'Characterization',
  'speaker-reattribute': 'Auto Split',
  speech_direction: 'Direction Tagging',
  scare_quote_tagging: 'Scare Quote Tagging',
  description_tagging: 'Description Tagging',
  // The phases of a book's own "Preprocess" run (see LibraryPage's
  // own Preprocess button) - each fans out real per-chapter/per-character
  // work into the ordinary kinds above (a "pipeline_attribution" row is a
  // meta-task, not itself an LLM call) - see backend jobs.EnqueuePipeline.
  pipeline_scare_quote: 'Preprocess: Scare Quote Tagging',
  pipeline_attribution: 'Preprocess: Attribution',
  pipeline_description: 'Preprocess: Description Tagging',
  pipeline_characterization: 'Preprocess: Characterization',
  pipeline_voice_provision: 'Preprocess: Voice Provision',
  pipeline_direction: 'Preprocess: Direction Tagging',
  pipeline_music: 'Preprocess: Music Scoring',
  // "Generate audio" (library page's All/Remaining, and the reader's
  // per-chapter action) - see backend jobs.Manager.EnqueueChapter/
  // EnqueueBookGenerate/EnqueueRemaining.
  pipeline_generate_chapter: 'Generate: Chapter',
  pipeline_generate_book: 'Generate: All',
  pipeline_generate_remaining: 'Generate: Remaining',
  // One whole "Auto Split" click, wrapping its per-chapter
  // 'speaker-reattribute' rows - see backend jobs.Manager.EnqueueAutoSplit.
  pipeline_auto_split: 'Auto Split: All Chapters',
  sfx_generation: 'SFX',
  sfx_preview: 'SFX/Music Preview',
  llm_preview: 'LLM Preview',
  music_scoring: 'Background Music: Scoring',
  music_generation: 'Background Music: Generation',
  music_live_generation: 'Background Music: Live',
}

// Only these kinds ever carry a real paragraph index - every other
// kind (the two LLM kinds, the design-preview/provision kinds, the SFX
// preview kind, and every "pipeline_*" meta-task) has a meaningless zero
// paragraphIdx (see QueueTask.paragraphIdx). An allowlist, not a "kinds to
// exclude" list, so a future kind added here without updating this
// defaults to the safe '—' rather than silently displaying a misleading
// "Paragraph 1". "music_generation" is NOT in this list even though it's
// chapter-scoped work with a real chapterIdx - it dispatches as one batch
// task covering every currently-eligible region in the chapter at once
// (see backend jobs.EnqueueMusicGeneration's own doc comment), not one
// task per region, so there's no single paragraph/region position left to
// show for it any more than there is for a "pipeline_*" row.
const PARAGRAPH_SCOPED_KINDS = new Set<QueueTask['kind']>(['voice_clone', 'voice_design', 'sfx_generation'])

// Ordered most urgent first, matching backend jobs.TierUrgent/TierLookahead/
// TierBackground's own numeric ordering - used both to compute which tiers
// a row can still be promoted to (upgradeOptions) and to label them.
const TIER_ORDER: QueueTask['tier'][] = ['urgent', 'lookahead', 'normal', 'background']

const TIER_LABELS: Record<QueueTask['tier'], string> = {
  urgent: 'Urgent',
  lookahead: 'Lookahead',
  normal: 'Normal',
  background: 'Background',
}

// The tiers strictly more urgent than `tier` - the only ones PUT
// /api/jobs/{id}/tier will actually accept (see backend jobs.Manager.
// PromoteTier's own doc comment: upgrade only, never a downgrade or a
// same-tier no-op), so the priority menu only ever offers a choice that
// can actually succeed. Empty for "urgent" - already the top, nothing
// left to offer.
function upgradeOptions(tier: QueueTask['tier']): QueueTask['tier'][] {
  return TIER_ORDER.slice(0, TIER_ORDER.indexOf(tier))
}

// A dashboard row's Tier badge, promoted to a click target: clicking it
// opens a small menu of strictly-more-urgent tiers (upgradeOptions) to
// bump the task to - see backend jobs.Manager.PromoteTier. Renders as a
// plain, non-interactive badge (no menu, disabled) for a task already at
// "urgent", since there's nowhere left to promote it to.
function TierBadge({
  task,
  onPromote,
  promoting,
}: {
  task: QueueTask
  onPromote: (task: QueueTask, tier: QueueTask['tier']) => void
  promoting: boolean
}) {
  const [open, setOpen] = useState(false)
  const containerRef = useRef<HTMLSpanElement>(null)
  useClickOutside([containerRef], () => setOpen(false), open)

  const options = upgradeOptions(task.tier)
  const tierModifier = task.tier !== 'background' ? ` job-tier-badge-${task.tier}` : ''

  if (options.length === 0) {
    // Not a button at all - nothing to click, so it shouldn't pick up the
    // button variant's hover/cursor treatment either.
    return (
      <span className={'job-tier-badge' + tierModifier} title="Already at the highest priority">
        {task.tier}
      </span>
    )
  }

  return (
    <span className="job-tier-menu-container" ref={containerRef}>
      <button
        type="button"
        className={'job-tier-badge job-tier-badge-button' + tierModifier}
        onClick={() => setOpen((v) => !v)}
        disabled={promoting}
        title="Raise this task's priority"
      >
        {task.tier}
      </button>
      {open && (
        <div className="popover-panel job-tier-menu">
          {options.map((tier) => (
            <button
              key={tier}
              type="button"
              className={'job-tier-badge job-tier-badge-button job-tier-menu-option' + (tier !== 'background' ? ` job-tier-badge-${tier}` : '')}
              title={`Raise to ${TIER_LABELS[tier]}`}
              onClick={() => {
                onPromote(task, tier)
                setOpen(false)
              }}
            >
              {tier}
            </button>
          ))}
        </div>
      )}
    </span>
  )
}

// "Chapter" column: the chapter title/number for chapter-scoped kinds, or
// the task's own label for a kind that isn't chapter-scoped at all - a
// character's name for speaker_characterization/voice_provision, or the
// fixed "Whole book" for every "pipeline_*" kind (see QueueTask.label).
function targetLabel(t: QueueTask): string {
  if (t.label) return t.label
  return t.chapterTitle || `Chapter ${t.chapterIdx + 1}`
}

// Cancelling an in-flight speaker_attribution/speaker_characterization task
// can't interrupt its current LLM call mid-decode (see backend
// jobs.task.cancel's own doc comment) - it still takes effect, just once
// that call finishes on its own, unlike an in-flight clone/design task's
// genuinely-abortable HTTP call. Surfaced as a tooltip rather than
// disabling the button outright, since it's still a real, useful action
// (it stops the task from doing anything *after* its current step).
//
// An in-flight "pipeline_*" row has a related but distinct caveat:
// canceling it stops that phase from fanning out any *more*
// chapters/characters, but whatever it already dispatched into the
// ordinary attribution/characterization/provision/direction rows above
// keeps running independently and isn't itself touched (see backend
// jobs.CancelPipeline's own doc comment) - so cancelling here never
// interrupts a chapter/character already in progress, only ones this
// phase hadn't reached yet.
function cancelTitle(t: QueueTask, inFlight: boolean): string {
  if (!inFlight) return 'Cancel'
  if (
    t.kind === 'speaker_attribution' ||
    t.kind === 'speaker_characterization' ||
    t.kind === 'speech_direction' ||
    t.kind === 'scare_quote_tagging' ||
    t.kind === 'description_tagging'
  ) {
    return "Cancel — won't take effect until its current LLM call finishes"
  }
  if (t.kind.startsWith('pipeline_generate_')) {
    return "Cancel — drops this generation's still-queued paragraphs; ones already rendering finish"
  }
  if (t.kind.startsWith('pipeline_')) {
    return "Cancel — stops this phase from starting more chapters/characters; anything already in progress keeps running"
  }
  return 'Cancel'
}

function TaskTable({
  tasks,
  voiceName,
  inFlight,
  onCancel,
  cancelingId,
  onPromote,
  promotingId,
}: {
  tasks: QueueTask[]
  voiceName: (t: QueueTask) => string
  inFlight: boolean
  onCancel: (t: QueueTask) => void
  cancelingId: string | null
  onPromote: (t: QueueTask, tier: QueueTask['tier']) => void
  promotingId: string | null
}) {
  return (
    <table className="jobs-table">
      <thead>
        <tr>
          <th>Kind</th>
          <th>Book</th>
          <th>Chapter</th>
          <th>Paragraph</th>
          <th>Voice</th>
          <th>Tier</th>
          <th title="Genuine failure-retries so far, 0 on a task's first ever dispatch - never bumped by a cooperative pause (a long chapter's own attribution/direction-tagging run yielding between batches, then picking back up under this same row). Climbing here means it's actually failing and retrying, not just slowly working through a long chapter.">
            Attempt
          </th>
          <th />
        </tr>
      </thead>
      <tbody>
        {tasks.map((t) => (
          <tr key={t.id}>
            <td>{KIND_LABELS[t.kind]}</td>
            <td>{t.bookTitle}</td>
            <td>{targetLabel(t)}</td>
            <td>{PARAGRAPH_SCOPED_KINDS.has(t.kind) ? t.paragraphIdx + 1 : '—'}</td>
            <td>{voiceName(t)}</td>
            <td>
              <TierBadge task={t} onPromote={onPromote} promoting={promotingId === t.id} />
            </td>
            <td>
              {t.attempt > 0 ? <span className="job-tier-badge job-tier-badge-urgent">retry {t.attempt}</span> : t.attempt}
            </td>
            <td>
              <button
                className="icon-action-button icon-action-button-danger"
                onClick={() => onCancel(t)}
                disabled={cancelingId === t.id}
                title={cancelTitle(t, inFlight)}
              >
                <RiCloseCircleLine />
              </button>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

// Live view of the backend's shared job queue (jobs.Manager) - voice
// clone/design generation and speaker attribution alike, all sorted
// through one tier-ordered priority queue (see internal/jobs.Kind) - what's
// actually dispatched to a worker slot right now (at most maxInFlight, see
// manager.go) vs. everything still waiting, pushed live as it changes
// (the jobs topic - see api/live.ts). Useful for
// seeing whether a big "generate this chapter" job is actually moving, or
// to check what a burst of lookahead work looks like as you jump around a
// book.
export function JobsPage() {
  const jobsQuery = useJobsSnapshot()
  const builtinsQuery = useVoicePresets()
  const customPresetsQuery = useCustomVoicePresets()
  const cancelJob = useCancelJob()
  const setJobTier = useSetJobTier()
  const cancelAllJobs = useCancelAllJobs()
  const pauseJobs = usePauseJobs()
  const resumeJobs = useResumeJobs()
  const restartWorker = useRestartWorker()
  const snapshot = jobsQuery.data
  const queued = snapshot?.queued ?? []
  const totalCount = (snapshot?.inFlight.length ?? 0) + queued.length
  const paused = snapshot?.paused ?? false

  // Tracked locally (not derived from the mutation's own isPending, which
  // only ever reflects the single most recent call) so two different rows'
  // cancel buttons don't show each other's spinner/disabled state if
  // clicked in quick succession.
  const [cancelingId, setCancelingId] = useState<string | null>(null)
  const [cancelError, setCancelError] = useState<string | null>(null)
  const [restartError, setRestartError] = useState<string | null>(null)
  // Same "track locally, not off the mutation's own isPending" reasoning
  // as cancelingId - two different rows' priority menus shouldn't show
  // each other's pending state.
  const [promotingId, setPromotingId] = useState<string | null>(null)
  const [promoteError, setPromoteError] = useState<string | null>(null)

  const runCancel = (t: QueueTask) => {
    setCancelError(null)
    setCancelingId(t.id)
    cancelJob.mutate(t.id, {
      onError: (err) => setCancelError(err instanceof ApiError ? err.message : `Could not cancel "${targetLabel(t)}"`),
      onSettled: () => setCancelingId(null),
    })
  }

  const runPromote = (t: QueueTask, tier: QueueTask['tier']) => {
    setPromoteError(null)
    setPromotingId(t.id)
    setJobTier.mutate(
      { id: t.id, tier },
      {
        onError: (err) =>
          setPromoteError(err instanceof ApiError ? err.message : `Could not raise priority for "${targetLabel(t)}"`),
        onSettled: () => setPromotingId(null),
      },
    )
  }

  const runCancelAll = () => {
    if (!confirm(`Cancel all ${totalCount} queued/in-flight jobs? Anything already generating will stop as soon as it notices.`)) {
      return
    }
    setCancelError(null)
    cancelAllJobs.mutate(undefined, {
      onError: (err) => setCancelError(err instanceof ApiError ? err.message : 'Could not cancel all jobs'),
    })
  }

  // Not destructive (unlike Cancel/Cancel all) - nothing already in
  // flight is touched, so no confirm() prompt.
  const runTogglePause = () => {
    setCancelError(null)
    if (paused) {
      resumeJobs.mutate(undefined, {
        onError: (err) => setCancelError(err instanceof ApiError ? err.message : 'Could not resume the queue'),
      })
    } else {
      pauseJobs.mutate(undefined, {
        onError: (err) => setCancelError(err instanceof ApiError ? err.message : 'Could not pause the queue'),
      })
    }
  }

  // Kills and respawns ttsworker right now - the same thing the backend's
  // own watchdog does automatically on an RSS breach or crash, just
  // triggered manually. Disruptive to whatever's in flight (the restart
  // blocks every proxied TTS/LLM call until the new worker is healthy), so
  // this confirms first, same as Cancel all.
  const runRestartWorker = () => {
    if (!confirm('Restart ttsworker now? Any TTS/LLM call in flight will be interrupted until the new worker is healthy again.')) {
      return
    }
    setRestartError(null)
    restartWorker.mutate(undefined, {
      onError: (err) => setRestartError(err instanceof ApiError ? err.message : 'Could not restart ttsworker'),
    })
  }

  // presetId -> friendly name, built from the same built-in/custom preset
  // lists the Voices page already fetches (cached, so this adds no extra
  // requests). A task with no presetId is a pure custom instruct with
  // nothing to resolve (or a speaker_attribution task, which has neither),
  // so it falls back to a preview of the instruct text.
  const presetNames = useMemo(() => {
    const map = new Map<string, string>()
    for (const p of builtinsQuery.data?.presets ?? []) map.set(p.id, p.name)
    for (const p of customPresetsQuery.data ?? []) map.set(p.id, p.name)
    return map
  }, [builtinsQuery.data, customPresetsQuery.data])

  const voiceName = (t: QueueTask): string => {
    if (t.presetId) return presetNames.get(t.presetId) ?? t.presetId
    if (!t.instruct) return '—'
    return t.instruct.length > INSTRUCT_PREVIEW_LENGTH
      ? `${t.instruct.slice(0, INSTRUCT_PREVIEW_LENGTH)}…`
      : t.instruct
  }

  return (
    <div className="jobs-page">
      <div className="library-header">
        <h1>Job queue</h1>
        <div className="speakers-roster-actions">
          <button
            className="icon-action-button"
            onClick={runTogglePause}
            disabled={pauseJobs.isPending || resumeJobs.isPending}
            title={
              paused
                ? 'Resume dispatching new jobs'
                : "Pause dispatching new jobs - anything already in flight keeps running to completion"
            }
          >
            {paused ? <RiPlayCircleLine /> : <RiPauseCircleLine />}
          </button>
          <button
            className="icon-action-button icon-action-button-danger"
            onClick={runCancelAll}
            disabled={cancelAllJobs.isPending || totalCount === 0}
            title="Cancel every queued and in-flight job"
          >
            <RiStopCircleLine />
          </button>
          <button
            className="icon-action-button icon-action-button-danger"
            onClick={runRestartWorker}
            disabled={restartWorker.isPending}
            title="Restart ttsworker - recovers a stuck or visibly-leaking worker without waiting for the automatic RSS-threshold watchdog"
          >
            <RiRestartLine className={restartWorker.isPending ? 'spin' : undefined} />
          </button>
        </div>
      </div>
      <p className="muted">
        Live view of the backend's job queue - voice-clone/design generation, speaker attribution, and
        speaker characterization runs sharing one priority queue, plus each book's own "Preprocess" run
        shown as its four ordered phases - what's actually running right now, and what's waiting.
        Updates live as the queue changes.
      </p>
      {paused && (
        <p className="jobs-paused-banner">Paused — no new jobs will be dispatched. Anything already in flight is still running.</p>
      )}
      {cancelError && <p className="error-text">{cancelError}</p>}
      {promoteError && <p className="error-text">{promoteError}</p>}
      {restartError && <p className="error-text">{restartError}</p>}

      {jobsQuery.isLoading && <p>Loading…</p>}
      {jobsQuery.isError && <p className="error-text">Could not load the job queue.</p>}

      {snapshot && (
        <>
          <section className="jobs-section">
            <h2>In flight ({snapshot.inFlight.length})</h2>
            {snapshot.inFlight.length === 0 ? (
              <p className="muted">Nothing generating right now.</p>
            ) : (
              <TaskTable
                tasks={snapshot.inFlight}
                voiceName={voiceName}
                inFlight
                onCancel={runCancel}
                cancelingId={cancelingId}
                onPromote={runPromote}
                promotingId={promotingId}
              />
            )}
          </section>

          <section className="jobs-section">
            <h2>Queued ({queued.length})</h2>
            {queued.length === 0 ? (
              <p className="muted">Nothing waiting.</p>
            ) : (
              <>
                <TaskTable
                  tasks={queued.slice(0, QUEUED_DISPLAY_LIMIT)}
                  voiceName={voiceName}
                  inFlight={false}
                  onCancel={runCancel}
                  cancelingId={cancelingId}
                  onPromote={runPromote}
                  promotingId={promotingId}
                />
                {queued.length > QUEUED_DISPLAY_LIMIT && (
                  <p className="muted jobs-truncated">
                    Showing first {QUEUED_DISPLAY_LIMIT} of {queued.length} queued.
                  </p>
                )}
              </>
            )}
          </section>
        </>
      )}
    </div>
  )
}
