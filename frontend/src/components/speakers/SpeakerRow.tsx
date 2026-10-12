import { useState, type ReactNode } from 'react'
import {
  RiDeleteBinLine,
  RiEqualizerLine,
  RiForbidLine,
  RiGitMergeLine,
  RiGroupLine,
  RiScissorsCutLine,
  RiUserHeartLine,
  RiUserVoiceLine,
} from 'react-icons/ri'
import {
  useRenameCharacter,
  useSetCharacterAliases,
  useSplitCharacterAliases,
  useBookFacts,
  useSetCharacterPeople,
  useSetCharacterPronouns,
} from '../../api/queries'
import { ApiError } from '../../api/client'
import type { CustomVoicePreset, Speaker, SpeakerAppearance, VoicePreset } from '../../api/types'
import { AppearanceRow } from './SpeakerEvidence'
import { CharacterVoiceEditor } from './CharacterVoiceEditor'
import { AddStage, StageVoiceLine } from './SpeakerStages'
import { SpeakerVoiceLine } from './SpeakerVoices'

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
  seriesLines,
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
  onMerge: (targetName: string, noAlias: boolean) => void
  merging: boolean
  onToggleMerge: () => void
  // "Auto Split" - unlike every other action here, available for the
  // id-less "Unknown" row too (see isNarrator/canAutoSplit below), so it's
  // wired up outside the isNarrator-gated block the rest of these share.
  onAutoSplit: () => void
  autoSplitting: boolean
  // Their lines of dialogue across the whole series, in the whole-series
  // view (undefined otherwise, or when they have none).
  seriesLines?: number
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
  // A real character is split across the whole series, so the button is
  // there even when none of their lines are in this book.
  const canAutoSplit =
    speaker.name !== 'Narrator' && (speaker.paragraphCount > 0 || speaker.id !== '')

  return (
    <>
      <div className="speaker-row">
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
            {seriesLines !== undefined && ` · ${seriesLines} in the series`}
          </span>
        </div>

        {!isNarrator && (
          <>
            <SpeakerPronouns
              bookId={bookId}
              characterId={speaker.id}
              pronouns={speaker.pronouns ?? ''}
            >
              <SpeakerPeople
                bookId={bookId}
                characterId={speaker.id}
                race={speaker.race ?? ''}
                nationality={speaker.nationality ?? ''}
                age={speaker.ageYears ?? 0}
                ageNote={speaker.age ?? ''}
              />
            </SpeakerPronouns>
            <SpeakerAliases
              bookId={bookId}
              characterId={speaker.id}
              name={speaker.name}
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
        {/* Every voice their lines are spoken in, one line of samples each:
            their own, each numbered member of a role, each age. */}
        {speaker.voices.map((v) =>
          v.stage ? (
            <StageVoiceLine
              key={'stage:' + v.stage.id}
              bookId={bookId}
              characterId={speaker.id}
              speakerName={speaker.name}
              voice={v}
              stage={v.stage}
            />
          ) : (
            <SpeakerVoiceLine
              key={'number:' + (v.number ?? 0)}
              bookId={bookId}
              speakerName={speaker.name}
              voice={v}
              label={
                v.number ? (
                  <span
                    title={`One of several unnamed people sharing the role “${speaker.name}” - numbered within a conversation`}
                  >
                    {speaker.name} {v.number}
                  </span>
                ) : undefined
              }
            />
          ),
        )}
        {!isNarrator && !speaker.role && !speaker.invalid && (
          <AddStage
            bookId={bookId}
            characterId={speaker.id}
            hasStages={speaker.voices.some((v) => v.stage)}
            hasCasting={Boolean(speaker.summary)}
          />
        )}

        <div className="speaker-row-actions">
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
                      : "Auto Split — this character isn't a real, distinct individual: re-judge and reassign every line attributed to them, in every book of the series, to whichever real character, Narrator, or Unknown actually speaks it"
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
          <MergeCharacterPanel
            name={speaker.name}
            targets={mergeTargets}
            onMerge={onMerge}
            onCancel={onToggleMerge}
          />
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
      </div>
    </>
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
// so a save shows up without any local state beyond the draft. "Split
// off" is for names that are really somebody else (api.splitCharacterAliases):
// pick which, and each becomes a character of its own.
function SpeakerAliases({
  bookId,
  characterId,
  name,
  aliases,
}: {
  bookId: string
  characterId: string
  name: string
  aliases: string[]
}) {
  const setAliases = useSetCharacterAliases(bookId)
  const splitAlias = useSplitCharacterAliases(bookId)
  const [draft, setDraft] = useState<string | null>(null)
  const [splitting, setSplitting] = useState(false)
  const [picked, setPicked] = useState<string[]>([])
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const split = () => {
    // In the order they are listed, whatever order they were ticked in.
    const chosen = aliases.filter((a) => picked.includes(a))
    if (chosen.length === 0) return
    const quoted = chosen.map((a) => `"${a}"`).join(', ')
    const question =
      chosen.length === 1
        ? `Make ${quoted} a character of their own, apart from ${name}? ${name}'s lines in the chapters that mention ${quoted} are looked over again and moved where they belong.`
        : `Make ${quoted} ${chosen.length} characters of their own, apart from ${name} and from each other? ${name}'s lines in the chapters that mention any of them are looked over again and moved where they belong.`
    if (!confirm(question)) return
    setError(null)
    setNotice(null)
    splitAlias.mutate(
      { characterId, aliases: chosen },
      {
        onSuccess: ({ queued }) => {
          setSplitting(false)
          setPicked([])
          const list = chosen.join(', ')
          setNotice(
            queued > 0
              ? `Split off ${list} - checking ${queued} chapter${queued === 1 ? '' : 's'} for their lines`
              : `Split off ${list} - no lines to move`,
          )
        },
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not split'),
      },
    )
  }

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
  if (splitting && aliases.length > 0) {
    return (
      <div className="speaker-aliases muted">
        <span>
          {aliases.length > 1 ? 'Which names are someone else?' : 'Is this name someone else?'}
        </span>
        {aliases.map((alias) => (
          <label key={alias}>
            <input
              type="checkbox"
              checked={picked.includes(alias)}
              disabled={splitAlias.isPending}
              onChange={(e) =>
                setPicked((prev) =>
                  e.target.checked ? [...prev, alias] : prev.filter((a) => a !== alias),
                )
              }
            />{' '}
            {alias}
          </label>
        ))}
        <button
          onClick={split}
          disabled={splitAlias.isPending || !aliases.some((a) => picked.includes(a))}
          title="Each name ticked becomes a separate character"
        >
          Split off
        </button>
        <button
          className="link-button"
          onClick={() => {
            setSplitting(false)
            setPicked([])
          }}
          disabled={splitAlias.isPending}
        >
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
      {aliases.length > 0 && (
        <button
          className="link-button"
          onClick={() => {
            setError(null)
            setNotice(null)
            setSplitting(true)
          }}
          title="One of these names is a different person - make them a character of their own and move their lines"
        >
          Split off
        </button>
      )}
      {notice && <span>{notice}</span>}
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
  children,
}: {
  bookId: string
  characterId: string
  pronouns: string
  // More of the speaker's settings, on the same line after the pronouns
  // (SpeakerPeople).
  children?: ReactNode
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
      </label>{' '}
      {children}
      {error && <p className="error-text">{error}</p>}
    </div>
  )
}

// The races and nationalities the book's facts name, as the suggestions
// SpeakerPeople's inputs offer - rendered once for the whole roster.
export function SpeakerPeopleLists({ bookId }: { bookId: string }) {
  const facts = useBookFacts(bookId).data ?? []
  const keys = (c: 'race' | 'nationality') =>
    facts.filter((f) => f.class === c && !f.proposed && !f.skipped).map((f) => f.key)
  return (
    <>
      <datalist id={`people-races-${bookId}`}>
        {keys('race').map((k) => (
          <option key={k} value={k} />
        ))}
      </datalist>
      <datalist id={`people-nationalities-${bookId}`}>
        {keys('nationality').map((k) => (
          <option key={k} value={k} />
        ))}
      </datalist>
    </>
  )
}

// A character's race, nationality and age (Speaker.race, Speaker.nationality,
// Speaker.ageYears) -
// proposed by fact finding as character notes, or set here. Their casting
// is shown both, with the facts filed under those words (a race's
// lifespan and accent, how a people speaks), so a change only reaches the
// voice after "Regenerate". Saved on Enter or on leaving the field.
function SpeakerPeople({
  bookId,
  characterId,
  race,
  nationality,
  age,
  ageNote,
}: {
  bookId: string
  characterId: string
  race: string
  nationality: string
  // Their age in years (0 for none) and what the notes say of it ("12 as
  // of Chapter 3").
  age: number
  ageNote: string
}) {
  const setPeople = useSetCharacterPeople(bookId)
  const [draft, setDraft] = useState<{ race?: string; nationality?: string; age?: string }>({})
  const [error, setError] = useState<string | null>(null)
  const save = () => {
    const next = {
      race: (draft.race ?? race).trim(),
      nationality: (draft.nationality ?? nationality).trim(),
      age: draft.age === undefined ? age : Math.max(0, Math.floor(Number(draft.age) || 0)),
    }
    if (next.race === race && next.nationality === nationality && next.age === age)
      return setDraft({})
    setError(null)
    setPeople.mutate(
      { characterId, ...next },
      {
        onSuccess: () => setDraft({}),
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not save that'),
      },
    )
  }
  const field = (key: 'race' | 'nationality', label: string, list: string, value: string) => (
    <label>
      {label}{' '}
      <input
        className="speaker-people-input"
        list={list}
        value={draft[key] ?? value}
        placeholder="Not set"
        disabled={setPeople.isPending}
        title="Shown to the casting of this speaker's voice, with the facts about it - Regenerate the voice to apply a change"
        onChange={(e) => setDraft({ ...draft, [key]: e.target.value })}
        onBlur={save}
        onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
      />
    </label>
  )
  return (
    <>
      {field('race', 'Race', `people-races-${bookId}`, race)}{' '}
      {field('nationality', 'Nationality', `people-nationalities-${bookId}`, nationality)}{' '}
      <label>
        Age{' '}
        <input
          className="speaker-people-input speaker-age-input"
          type="number"
          min={0}
          value={draft.age ?? (age > 0 ? String(age) : '')}
          placeholder="Not set"
          disabled={setPeople.isPending}
          title={
            (ageNote ? `The notes say: ${ageNote}. ` : '') +
            'Their age in years, shown to the casting of their voice - Regenerate the voice to apply a change. Setting it replaces the ages found in the text (they stay under Skipped on the Facts tab)'
          }
          onChange={(e) => setDraft({ ...draft, age: e.target.value })}
          onBlur={save}
          onKeyDown={(e) => e.key === 'Enter' && e.currentTarget.blur()}
        />
      </label>
      {error && <span className="error-text"> {error}</span>}
    </>
  )
}

// A speaker's own "targetName" picker + confirm/cancel for
// handleMergeCharacter - targets is "Narrator" plus every other real
// character's name (see BookSettingsPage's own mergeTargets computation,
// which already excludes the row being merged).
function MergeCharacterPanel({
  name,
  targets,
  onMerge,
  onCancel,
}: {
  name: string
  targets: string[]
  onMerge: (targetName: string, noAlias: boolean) => void
  onCancel: () => void
}) {
  const [target, setTarget] = useState(targets[0] ?? 'Narrator')
  const [keepAlias, setKeepAlias] = useState(true)
  // Narrator and Unknown are nobody's names to add to.
  const aliasApplies = target !== 'Narrator' && target !== 'Unknown'

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
      {aliasApplies && (
        <label title="Off for a name that was never theirs - attribution folds any line answered with another name into the character">
          <input
            type="checkbox"
            checked={keepAlias}
            onChange={(e) => setKeepAlias(e.target.checked)}
          />{' '}
          Keep {name} as another name for them
        </label>
      )}
      <div className="voice-panel-actions">
        <button
          className="primary-button"
          onClick={() => onMerge(target, aliasApplies && !keepAlias)}
        >
          Merge
        </button>
        <button className="text-button" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  )
}
