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
  useSetParagraphDescription,
  useSetParagraphEmotion,
  useSetParagraphQuote,
  useSetParagraphScareQuote,
  useSetParagraphSpeaker,
} from '../api/queries'
import { ApiError } from '../api/client'
import type { ChapterDetail, Paragraph, Speaker } from '../api/types'
import { EMOTIONS } from '../utils/emotions'
import { useClickOutside } from '../hooks/useClickOutside'
import { chapterCharacterNames, pickerSections } from '../utils/speakerOrder'

export interface ParagraphContextMenuHandle {
  open: (chapterIdx: number, p: Paragraph, x: number, y: number) => void
}

// The reader's right-click menu (annotations view only): reassign a
// dialogue line's speaker, mark/unmark it as a scare quote, override its
// emotion, reassign a description paragraph, or mark plain narration as
// describing someone. Split out of ReaderPage
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
  const setParagraphDescription = useSetParagraphDescription(bookId)
  const setParagraphScareQuote = useSetParagraphScareQuote(bookId)
  const setParagraphQuote = useSetParagraphQuote(bookId)
  const setParagraphEmotion = useSetParagraphEmotion(bookId)
  const [contextMenu, setContextMenu] = useState<{
    chapterIdx: number
    paragraphIdx: number
    isQuote: boolean
    currentSpeaker: string
    describesCharacters: string[]
    scareQuote: boolean
    emotion: string
    x: number
    y: number
  } | null>(null)
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
        setContextMenu({
          chapterIdx,
          paragraphIdx: p.idx,
          isQuote: !!p.isQuote,
          currentSpeaker: p.speaker ?? '',
          describesCharacters: p.describesCharacters ?? [],
          scareQuote: !!p.scareQuote,
          emotion: p.emotion ?? '',
          x,
          y,
        })
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

  const reassignSpeaker = useCallback(
    (speaker: string) => {
      if (!contextMenu) return
      onError(null)
      setParagraphSpeaker.mutate(
        { chapterIdx: contextMenu.chapterIdx, paragraphIdx: contextMenu.paragraphIdx, speaker },
        {
          onError: (err) =>
            onError(err instanceof ApiError ? err.message : 'Could not reassign this paragraph'),
        },
      )
      setContextMenu(null)
    },
    [contextMenu, setParagraphSpeaker, onError],
  )

  // reassignSpeaker's own counterpart for a *description* paragraph - moves
  // it off `from`'s own describes-list and onto `to`'s instead ("" clears
  // it, describing no one), leaving any other character this same
  // paragraph also describes untouched (a description paragraph can name
  // more than one - see the menu's own per-name rows below).
  const reassignDescription = useCallback(
    (from: string, to: string) => {
      if (!contextMenu) return
      onError(null)
      setParagraphDescription.mutate(
        { chapterIdx: contextMenu.chapterIdx, paragraphIdx: contextMenu.paragraphIdx, from, to },
        {
          onError: (err) =>
            onError(err instanceof ApiError ? err.message : 'Could not reassign this description'),
        },
      )
      setContextMenu(null)
    },
    [contextMenu, setParagraphDescription, onError],
  )

  // Live "mark/unmark as scare quote" toggle - see
  // handleSetParagraphScareQuote's own doc comment for why this (unlike
  // reassignSpeaker/reassignDescription above) immediately invalidates the
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

  // Everyone who speaks or is described in the clicked paragraph's own
  // chapter - listed first in both reassign branches, since a misattributed
  // line or description usually belongs to someone already in the scene.
  // Mirrors android's SpeakerPickerSheet/DescriptionPickerSheet.
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
          <TargetSections
            sections={speakerTargets}
            keyPrefix="speaker"
            isCurrent={(name) => name === contextMenu.currentSpeaker}
            onPick={reassignSpeaker}
          />
        </>
      ) : contextMenu.describesCharacters.length > 0 ? (
        <>
          <button className="speaker-context-menu-item" onClick={toggleQuote}>
            Mark as dialogue
          </button>
          <div className="speaker-context-menu-title">Reassign description</div>
          {contextMenu.describesCharacters.map((from) => (
            <div key={from} className="speaker-context-menu-group">
              <div className="speaker-context-menu-current">
                Describes: <strong>{from}</strong>
              </div>
              <button
                className="speaker-context-menu-item"
                onClick={() => reassignDescription(from, '')}
              >
                Remove
              </button>
              <TargetSections
                sections={pickerSections(rosterNames, chapterNames, from)}
                keyPrefix={`describes-${from}`}
                isCurrent={() => false}
                onPick={(name) => reassignDescription(from, name)}
              />
            </div>
          ))}
        </>
      ) : (
        <>
          <button className="speaker-context-menu-item" onClick={toggleQuote}>
            Mark as dialogue
          </button>
          {/* Plain narration: tag it as describing someone (from '' just
              adds the name - see handleSetParagraphDescription). */}
          <div className="speaker-context-menu-title">Mark as description of</div>
          <TargetSections
            sections={speakerTargets}
            keyPrefix="describe-new"
            isCurrent={() => false}
            onPick={(name) => reassignDescription('', name)}
          />
        </>
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
