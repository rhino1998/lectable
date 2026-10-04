import { useEffect, useRef, useState } from 'react'
import {
  RiDeleteBinLine,
  RiEqualizerLine,
  RiForbidLine,
  RiGitMergeLine,
  RiGroupLine,
  RiPauseFill,
  RiPlayFill,
  RiRefreshLine,
  RiScissorsCutLine,
  RiUserHeartLine,
  RiUserVoiceLine,
} from 'react-icons/ri'
import {
  useRegenerateVariant,
  useRenameCharacter,
  useSetCharacterAliases,
  useSetCharacterPronouns,
} from '../../api/queries'
import { ApiError } from '../../api/client'
import { withCacheBust } from '../../utils/cacheBust'
import type {
  CustomVoicePreset,
  Speaker,
  SpeakerAppearance,
  SpeakerEmotion,
  SpeakerVariant,
  VoicePreset,
} from '../../api/types'
import { AppearanceRow } from './SpeakerEvidence'
import { CharacterVoiceEditor } from './CharacterVoiceEditor'

export function SpeakerRow({
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
  onToggleRole,
  togglingRole,
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
  onToggleRole: () => void
  togglingRole: boolean
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
    <>
      <li className="speaker-row">
        <div className="speaker-row-main">
          <span>
            {isNarrator ? (
              speaker.name
            ) : (
              <SpeakerName bookId={bookId} characterId={speaker.id} name={speaker.name} />
            )}
            {speaker.role && (
              <span
                className="speaker-role-badge"
                title="A role label — several unnamed people of one kind, not one individual"
              >
                role
              </span>
            )}
            {speaker.invalid && (
              <span
                className="speaker-invalid-badge"
                title="Not a real speaker — attribution won't assign this name"
              >
                invalid
              </span>
            )}
          </span>
          <span className="muted">
            {speaker.readyCount}/{speaker.paragraphCount} generated
          </span>
        </div>

        {!isNarrator && (
          <>
            <SpeakerPronouns
              bookId={bookId}
              characterId={speaker.id}
              pronouns={speaker.pronouns ?? ''}
            />
            <SpeakerAliases
              bookId={bookId}
              characterId={speaker.id}
              aliases={speaker.aliases ?? []}
            />
          </>
        )}
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
                    : "Regenerate characterization — re-describe this character's voice from their dialogue, and invalidate their assigned voice so it's rebuilt fresh next time it's needed"
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
              {canAutoSplit && (
                <button
                  className="icon-action-button"
                  onClick={onAutoSplit}
                  disabled={autoSplitting}
                  title={
                    autoSplitting
                      ? 'Auto Split in progress…'
                      : "Auto Split — this character isn't a real, distinct individual: re-judge and reassign every line currently attributed to them to whichever real character, Narrator, or Unknown actually speaks it"
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
                className={
                  'icon-action-button' + (speaker.role ? ' icon-action-button-active' : '')
                }
                onClick={onToggleRole}
                disabled={togglingRole}
                title={
                  speaker.role
                    ? 'Marked as a role — click to treat this name as one individual again'
                    : 'Mark as a role — a label for several unnamed people of one kind ("Guard", "Woman"), not one individual'
                }
              >
                <RiGroupLine />
              </button>
              <button
                className={
                  'icon-action-button' + (speaker.invalid ? ' icon-action-button-active' : '')
                }
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

        {merging && (
          <MergeCharacterPanel targets={mergeTargets} onMerge={onMerge} onCancel={onToggleMerge} />
        )}

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
      </li>
      {speaker.variants?.map((v) => (
        <SpeakerVariantRow key={v.number} bookId={bookId} speakerName={speaker.name} variant={v} />
      ))}
    </>
  )
}

// One numbered member of a role ("Guard 2" - backend
// speakerRowDTO.Variants), listed in the roster as a speaker of its own
// right after the role: its line count, the reference clip its lines clone
// from, and its emotion variants. The role's row above carries the casting
// they all share and the role's unnumbered lines.
function SpeakerVariantRow({
  bookId,
  speakerName,
  variant,
}: {
  bookId: string
  speakerName: string
  variant: SpeakerVariant
}) {
  return (
    <li className="speaker-row">
      <div className="speaker-row-main">
        <span>
          {speakerName} {variant.number}
          <span
            className="speaker-role-badge"
            title={`One of several unnamed people sharing the role “${speakerName}” - numbered within a conversation`}
          >
            role
          </span>
        </span>
        <span className="muted">
          {variant.paragraphCount} {variant.paragraphCount === 1 ? 'line' : 'lines'}
        </span>
      </div>
      {variant.emotions && variant.emotions.length > 0 && (
        <SpeakerEmotions
          bookId={bookId}
          speakerName={speakerName}
          speakerIndex={variant.number}
          emotions={variant.emotions}
        />
      )}
      <div className="speaker-row-actions">
        {variant.refAudioUrl ? (
          <div className="speaker-row-audio">
            <audio controls preload="none" src={variant.refAudioUrl} />
          </div>
        ) : (
          <span className="muted">
            Their voice renders the first time one of these lines generates
          </span>
        )}
      </div>
    </li>
  )
}

// A character's name with an inline rename (api.renameCharacter): their
// lines, corrections and voice follow, and the old name becomes an alias.
// The speakers list is live, so a save shows up on its own.
function SpeakerName({
  bookId,
  characterId,
  name,
}: {
  bookId: string
  characterId: string
  name: string
}) {
  const rename = useRenameCharacter(bookId)
  const [draft, setDraft] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const save = () => {
    if (draft === null) return
    const next = draft.trim()
    if (next === '' || next === name) {
      setDraft(null)
      return
    }
    setError(null)
    rename.mutate(
      { characterId, name: next },
      {
        onSuccess: () => setDraft(null),
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not rename'),
      },
    )
  }

  if (draft !== null) {
    return (
      <span className="speaker-rename">
        <input
          className="speaker-rename-input"
          value={draft}
          autoFocus
          aria-label="New name"
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') save()
            if (e.key === 'Escape') setDraft(null)
          }}
        />
        <button onClick={save} disabled={rename.isPending}>
          Save
        </button>
        <button onClick={() => setDraft(null)} disabled={rename.isPending}>
          Cancel
        </button>
        {error && <span className="error-text">{error}</span>}
      </span>
    )
  }
  return (
    <>
      {name}
      <button
        className="link-button speaker-rename-button"
        onClick={() => setDraft(name)}
        title="Rename this character everywhere in the series - the old name stays on as another name for them"
      >
        Rename
      </button>
    </>
  )
}

// Other names a character goes by (Speaker.aliases), shown under their
// name with an inline comma-separated editor. The speakers list is live,
// so a save shows up without any local state beyond the draft.
function SpeakerAliases({
  bookId,
  characterId,
  aliases,
}: {
  bookId: string
  characterId: string
  aliases: string[]
}) {
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
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not save aliases'),
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

const PRONOUN_OPTIONS = ['he/him', 'she/her', 'they/them', 'it/its']

// A character's pronouns (Speaker.pronouns) - filled in by attribution,
// correctable here. Characterization reads them as the voice's gender, so
// a change only reaches the voice after "Regenerate".
function SpeakerPronouns({
  bookId,
  characterId,
  pronouns,
}: {
  bookId: string
  characterId: string
  pronouns: string
}) {
  const setPronouns = useSetCharacterPronouns(bookId)
  const [error, setError] = useState<string | null>(null)
  return (
    <div className="speaker-aliases muted">
      <label>
        Pronouns{' '}
        <select
          value={pronouns}
          disabled={setPronouns.isPending}
          title="Used by speaker attribution and voice characterization - Regenerate the voice to apply a change"
          onChange={(e) => {
            setError(null)
            setPronouns.mutate(
              { characterId, pronouns: e.target.value },
              {
                onError: (err) =>
                  setError(err instanceof ApiError ? err.message : 'Could not save pronouns'),
              },
            )
          }}
        >
          <option value="">Not set</option>
          {PRONOUN_OPTIONS.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </select>
      </label>
      {error && <p className="error-text">{error}</p>}
    </div>
  )
}

// One speaker's emotion variants - every emotion their dialogue uses in
// this book (backend speakerRowDTO.Emotions), each a chip with its line
// count, a play button once the variant clip exists, and a regenerate
// button (re-renders the variant and resets those lines' audio - see
// api.regenerateVariant). A pending variant renders by itself the first
// time one of its lines generates. With speakerIndex, the list is one
// numbered member of a role's (SpeakerVariantRow).
function SpeakerEmotions({
  bookId,
  speakerName,
  speakerIndex,
  emotions,
}: {
  bookId: string
  speakerName: string
  speakerIndex?: number
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
      { speaker: speakerName, emotion, speakerIndex },
      {
        onSuccess: () => setVersions((v) => ({ ...v, [emotion]: Date.now() })),
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not regenerate this variant'),
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
              aria-label={
                playing === e.emotion ? `Stop ${e.label} sample` : `Play ${e.label} sample`
              }
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
