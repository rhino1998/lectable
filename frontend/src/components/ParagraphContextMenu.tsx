import {
  forwardRef,
  useCallback,
  useImperativeHandle,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from 'react'
import { RiEmotionNormalLine } from 'react-icons/ri'
import {
  useSetParagraphDirection,
  useSetParagraphEmotion,
  useSetParagraphQuote,
  useSetParagraphScareQuote,
  useSetParagraphSpeaker,
  useVoice,
} from '../api/queries'
import { ApiError } from '../api/client'
import { CLONE_MODELS, type CloneModelInfo } from '../api/types'
import type { ChapterDetail, Paragraph, Speaker } from '../api/types'
import { EMOTIONS } from '../utils/emotions'
import { useClickOutside } from '../hooks/useClickOutside'
import { chapterCharacterNames, pickerSections } from '../utils/speakerOrder'

export interface ParagraphContextMenuHandle {
  open: (chapterIdx: number, p: Paragraph, x: number, y: number) => void
}

// The reader's right-click menu (annotations view only): reassign a
// dialogue line's speaker, mark/unmark it as a scare quote, override its
// emotion, or mark plain narration as dialogue. Split out of ReaderPage
// with an imperative open() (the same shape as AnnotationTooltip) so
// opening and positioning the menu re-renders only this component, not the
// reader's whole loaded paragraph list. `speakers` backs the "reassign
// to…" list - a live topic, so a brand-new speaker shows up next time the
// menu opens without a separate fetch. `chapters` backs the "This chapter"
// section. Failures report through onError into the reader's shared
// chapter-action error.
export const ParagraphContextMenu = forwardRef<
  ParagraphContextMenuHandle,
  {
    bookId: string
    chapters: Map<number, ChapterDetail>
    speakers: Speaker[]
    onError: (message: string | null) => void
  }
>(({ bookId, chapters, speakers, onError }, ref) => {
  const setParagraphSpeaker = useSetParagraphSpeaker(bookId)
  const setParagraphScareQuote = useSetParagraphScareQuote(bookId)
  const setParagraphQuote = useSetParagraphQuote(bookId)
  const setParagraphEmotion = useSetParagraphEmotion(bookId)
  const setParagraphDirection = useSetParagraphDirection(bookId)
  // A clone model that takes direction as text (Fish Drama 3) gets the
  // free-form direction editor beside the emotion labels.
  const voice = useVoice(bookId)
  const directed =
    (CLONE_MODELS as readonly CloneModelInfo[]).find((m) => m.id === voice.data?.cloneModel)
      ?.delivery === 'directed'
  // The direction being edited: the line written back with its direction.
  const [directionDraft, setDirectionDraft] = useState('')
  const [contextMenu, setContextMenu] = useState<{
    chapterIdx: number
    paragraphIdx: number
    isQuote: boolean
    currentSpeaker: string
    scareQuote: boolean
    emotion: string
    text: string
    direction: string
    x: number
    y: number
  } | null>(null)
  // The "New speaker…" entry's name while it is being typed; null while
  // the entry is still a button.
  const [newSpeaker, setNewSpeaker] = useState<string | null>(null)
  const contextMenuRef = useRef<HTMLDivElement>(null)
  useClickOutside([contextMenuRef], () => setContextMenu(null), contextMenu !== null)

  // See the useLayoutEffect below.
  const [menuPos, setMenuPos] = useState<{ left: number; top: number } | null>(null)

  useImperativeHandle(
    ref,
    () => ({
      open: (chapterIdx, p, x, y) => {
        // Cleared here rather than on close: the menu isn't rendered while
        // closed, and this keeps a reopen from flashing the old position.
        setMenuPos(null)
        setNewSpeaker(null)
        setContextMenu({
          chapterIdx,
          paragraphIdx: p.idx,
          isQuote: !!p.isQuote,
          currentSpeaker: p.speaker ?? '',
          scareQuote: !!p.scareQuote,
          emotion: p.emotion ?? '',
          text: p.text,
          direction: p.direction ?? '',
          x,
          y,
        })
        setDirectionDraft(p.direction ?? p.text)
      },
    }),
    [],
  )

  // Corrects the menu's raw click-point position (contextMenu.x/y) to keep
  // it fully on screen - clamped against both the viewport edges and
  // .player-bar's own top edge, since the raw position alone can otherwise
  // render the menu (or its lower rows) behind the fixed bottom bar or off
  // the right/bottom of the window when the paragraph clicked sits near
  // either. Runs via useLayoutEffect (not useEffect) so the corrected
  // position is committed before the browser ever paints the menu at its
  // raw, unclamped spot - no visible jump. null until measured, so the
  // menu's own style falls back to the raw click point for that first,
  // pre-measurement render.
  useLayoutEffect(() => {
    if (!contextMenu) return
    const el = contextMenuRef.current
    if (!el) return
    const margin = 8
    const rect = el.getBoundingClientRect()
    const playerBar = document.querySelector('.player-bar')
    const bottomBoundary = playerBar ? playerBar.getBoundingClientRect().top : window.innerHeight
    const left = Math.min(contextMenu.x, window.innerWidth - rect.width - margin)
    const top = Math.min(contextMenu.y, bottomBoundary - rect.height - margin)
    setMenuPos({ left: Math.max(margin, left), top: Math.max(margin, top) })
  }, [contextMenu])

  // Every valid character's name - both reassign branches split it into a
  // "This chapter" section and the full alphabetical roster (pickerSections).
  const rosterNames = useMemo(
    () => speakers.filter((s) => !s.invalid).map((s) => s.name),
    [speakers],
  )

  // isNew: the typed name of "New speaker…" - a person of their own even
  // if the roster knows the name as someone's alias.
  const reassignSpeaker = useCallback(
    (speaker: string, isNew = false) => {
      if (!contextMenu) return
      onError(null)
      setParagraphSpeaker.mutate(
        {
          chapterIdx: contextMenu.chapterIdx,
          paragraphIdx: contextMenu.paragraphIdx,
          speaker,
          isNew,
        },
        {
          onError: (err) =>
            onError(err instanceof ApiError ? err.message : 'Could not reassign this paragraph'),
        },
      )
      setContextMenu(null)
    },
    [contextMenu, setParagraphSpeaker, onError],
  )

  // Live "mark/unmark as scare quote" toggle - see
  // handleSetParagraphScareQuote's own doc comment for why this (unlike
  // reassignSpeaker above) immediately invalidates the
  // paragraph's own audio server-side rather than requiring a separate
  // explicit regenerate.
  const toggleScareQuote = useCallback(() => {
    if (!contextMenu) return
    onError(null)
    setParagraphScareQuote.mutate(
      {
        chapterIdx: contextMenu.chapterIdx,
        paragraphIdx: contextMenu.paragraphIdx,
        scareQuote: !contextMenu.scareQuote,
      },
      {
        onError: (err) =>
          onError(err instanceof ApiError ? err.message : 'Could not update scare-quote status'),
      },
    )
    setContextMenu(null)
  }, [contextMenu, setParagraphScareQuote, onError])

  // Flips the paragraph between dialogue and narration - for a line the
  // parser misread (see handleSetParagraphQuote); regenerates server-side.
  const toggleQuote = useCallback(() => {
    if (!contextMenu) return
    onError(null)
    setParagraphQuote.mutate(
      {
        chapterIdx: contextMenu.chapterIdx,
        paragraphIdx: contextMenu.paragraphIdx,
        quote: !contextMenu.isQuote,
      },
      {
        onError: (err) =>
          onError(
            err instanceof ApiError
              ? err.message
              : 'Could not change this line to ' + (contextMenu.isQuote ? 'narration' : 'dialogue'),
          ),
      },
    )
    setContextMenu(null)
  }, [contextMenu, setParagraphQuote, onError])

  // Manual emotion override for a dialogue line ('' = neutral) - like
  // toggleScareQuote, the backend invalidates and regenerates the line's
  // audio itself when its effective emotion changes.
  const setEmotion = useCallback(
    (emotion: string) => {
      if (!contextMenu) return
      onError(null)
      setParagraphEmotion.mutate(
        { chapterIdx: contextMenu.chapterIdx, paragraphIdx: contextMenu.paragraphIdx, emotion },
        {
          onError: (err) =>
            onError(err instanceof ApiError ? err.message : "Could not set this line's emotion"),
        },
      )
      setContextMenu(null)
    },
    [contextMenu, setParagraphEmotion, onError],
  )

  // Manual direction override ('' clears) - the backend reads it with the
  // same parser as the direction pass and regenerates the line when its
  // effective direction changes.
  const saveDirection = useCallback(
    (direction: string) => {
      if (!contextMenu) return
      onError(null)
      setParagraphDirection.mutate(
        { chapterIdx: contextMenu.chapterIdx, paragraphIdx: contextMenu.paragraphIdx, direction },
        {
          onError: (err) =>
            onError(err instanceof ApiError ? err.message : "Could not set this line's direction"),
        },
      )
      setContextMenu(null)
    },
    [contextMenu, setParagraphDirection, onError],
  )

  // Everyone who speaks in the clicked paragraph's own chapter - listed
  // first, since a misattributed line usually belongs to someone already
  // in the scene. Mirrors android's SpeakerPickerSheet.
  const chapterNames = useMemo(
    () => chapterCharacterNames(contextMenu ? chapters.get(contextMenu.chapterIdx) : undefined),
    [contextMenu, chapters],
  )
  const speakerTargets = useMemo(
    () => pickerSections(rosterNames, chapterNames),
    [rosterNames, chapterNames],
  )

  if (!contextMenu) return null
  return (
    <div
      ref={contextMenuRef}
      className="speaker-context-menu"
      style={
        menuPos
          ? { top: menuPos.top, left: menuPos.left }
          : { top: contextMenu.y, left: contextMenu.x }
      }
    >
      {contextMenu.isQuote ? (
        <>
          <div className="speaker-context-menu-title">Reassign speaker</div>
          <div className="speaker-context-menu-current">
            Currently: <strong>{contextMenu.currentSpeaker || 'Narrator'}</strong>
          </div>
          <button
            className="speaker-context-menu-item"
            disabled={contextMenu.currentSpeaker === ''}
            onClick={() => reassignSpeaker('')}
          >
            Narrator
          </button>
          {/* Attribution's own "can't tell who says this" value - not a
              roster character, so a fixed entry like Narrator. */}
          <button
            className="speaker-context-menu-item"
            disabled={contextMenu.currentSpeaker === 'Unknown'}
            onClick={() => reassignSpeaker('Unknown')}
          >
            Unknown
          </button>
          {/* For a line attribution gave to the wrong person because it
              took two people for one: a name typed here becomes a
              character of its own. */}
          {newSpeaker === null ? (
            <button className="speaker-context-menu-item" onClick={() => setNewSpeaker('')}>
              New speaker…
            </button>
          ) : (
            <form
              className="speaker-context-menu-new"
              onSubmit={(e) => {
                e.preventDefault()
                const name = newSpeaker.trim()
                if (name) reassignSpeaker(name, true)
              }}
            >
              <input
                autoFocus
                value={newSpeaker}
                placeholder="New speaker's name"
                aria-label="New speaker's name"
                onChange={(e) => setNewSpeaker(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === 'Escape') setNewSpeaker(null)
                }}
              />
              <button type="submit" disabled={newSpeaker.trim() === ''}>
                Add
              </button>
            </form>
          )}
          <div className="speaker-context-menu-section">Scare quote</div>
          <button className="speaker-context-menu-item" onClick={toggleScareQuote}>
            {contextMenu.scareQuote ? 'Unmark as scare quote' : 'Mark as scare quote'}
          </button>
          <button className="speaker-context-menu-item" onClick={toggleQuote}>
            Mark as narration
          </button>
          {!contextMenu.scareQuote && (
            <>
              <div className="speaker-context-menu-section">Emotion</div>
              <div className="speaker-context-menu-chips">
                {[{ id: '', label: 'Neutral', icon: RiEmotionNormalLine }, ...EMOTIONS].map((e) => (
                  <button
                    key={e.id}
                    className={
                      'speaker-context-menu-chip' +
                      (e.id === contextMenu.emotion ? ' speaker-context-menu-chip-active' : '')
                    }
                    disabled={e.id === contextMenu.emotion}
                    onClick={() => setEmotion(e.id)}
                  >
                    <e.icon /> {e.label}
                  </button>
                ))}
              </div>
            </>
          )}
          {directed && !contextMenu.scareQuote && (
            <>
              <div className="speaker-context-menu-section">Direction</div>
              <form
                className="speaker-context-menu-direction"
                onSubmit={(e) => {
                  e.preventDefault()
                  saveDirection(
                    directionDraft.trim() === contextMenu.text.trim() ? '' : directionDraft,
                  )
                }}
              >
                <textarea
                  value={directionDraft}
                  rows={3}
                  aria-label="The line with its direction"
                  placeholder="[dry, barely holding back a laugh] the line, with [sighs] or <whisper>…</whisper>"
                  onChange={(e) => setDirectionDraft(e.target.value)}
                />
                <div className="speaker-context-menu-chips">
                  {EMOTIONS.map((e) => (
                    <button
                      key={e.id}
                      type="button"
                      className="speaker-context-menu-chip"
                      title={e.direction}
                      onClick={() =>
                        setDirectionDraft(
                          `[${e.direction}] ` + directionDraft.replace(/^\s*\[[^\]]*\]\s*/, ''),
                        )
                      }
                    >
                      <e.icon /> {e.label}
                    </button>
                  ))}
                </div>
                <div className="speaker-context-menu-direction-actions">
                  <button type="submit">Save</button>
                  {contextMenu.direction && (
                    <button type="button" onClick={() => saveDirection('')}>
                      Clear
                    </button>
                  )}
                </div>
              </form>
            </>
          )}
          <TargetSections
            sections={speakerTargets}
            keyPrefix="speaker"
            isCurrent={(name) => name === contextMenu.currentSpeaker}
            onPick={(name) => reassignSpeaker(name)}
          />
        </>
      ) : (
        <button className="speaker-context-menu-item" onClick={toggleQuote}>
          Mark as dialogue
        </button>
      )}
    </div>
  )
})
ParagraphContextMenu.displayName = 'ParagraphContextMenu'

// A reassign branch's target list: "This chapter" names first (when there
// are any), then every character - see pickerSections.
function TargetSections({
  sections,
  keyPrefix,
  isCurrent,
  onPick,
}: {
  sections: { local: string[]; all: string[] }
  keyPrefix: string
  isCurrent: (name: string) => boolean
  onPick: (name: string) => void
}) {
  const button = (name: string, section: string) => (
    <button
      key={`${keyPrefix}-${section}-${name}`}
      className="speaker-context-menu-item"
      disabled={isCurrent(name)}
      onClick={() => onPick(name)}
    >
      {name}
    </button>
  )
  return (
    <>
      {sections.local.length > 0 && (
        <>
          <div className="speaker-context-menu-section">This chapter</div>
          {sections.local.map((name) => button(name, 'local'))}
          <div className="speaker-context-menu-section">All characters</div>
        </>
      )}
      {sections.all.map((name) => button(name, 'all'))}
    </>
  )
}
