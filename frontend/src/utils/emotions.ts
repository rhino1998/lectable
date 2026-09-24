import type { IconType } from 'react-icons'
import {
  RiChatPrivateLine,
  RiEmotionLaughLine,
  RiEmotionLine,
  RiEmotionSadLine,
  RiFireLine,
  RiGhostLine,
  RiHeart3Line,
  RiMegaphoneLine,
  RiSnowflakeLine,
  RiZzzLine,
} from 'react-icons/ri'

// The fixed emotion set a dialogue line can carry - mirrors backend
// internal/emotions.All (ids are the stored/wire values; keep the two in
// the same order). An emotion isn't a TTS control token: each voice gets
// one extra reference clip per emotion its lines use, and an emotional
// line clones from that variant instead of the base clip.
// icon is the annotations view's inline per-line marker (ChapterSection's
// EmotionIcon) and the reader's emotion override menu.
export const EMOTIONS: { id: string; label: string; icon: IconType }[] = [
  { id: 'warm', label: 'Warm', icon: RiHeart3Line },
  { id: 'excited', label: 'Excited', icon: RiEmotionLaughLine },
  { id: 'sad', label: 'Sad', icon: RiEmotionSadLine },
  { id: 'angry', label: 'Angry', icon: RiFireLine },
  { id: 'afraid', label: 'Afraid', icon: RiGhostLine },
  { id: 'cold', label: 'Cold', icon: RiSnowflakeLine },
  { id: 'whisper', label: 'Whisper', icon: RiChatPrivateLine },
  { id: 'shout', label: 'Shout', icon: RiMegaphoneLine },
  { id: 'weary', label: 'Weary', icon: RiZzzLine },
]

const LABELS = new Map(EMOTIONS.map((e) => [e.id, e.label]))
const ICONS = new Map(EMOTIONS.map((e) => [e.id, e.icon]))

// Display label for an emotion id - the id itself for one this list
// doesn't know (a newer backend), rather than hiding it.
export function emotionLabel(id: string): string {
  return LABELS.get(id) ?? id
}

// Icon for an emotion id - a generic face for one this list doesn't know,
// same don't-hide-it fallback as emotionLabel.
export function emotionIcon(id: string): IconType {
  return ICONS.get(id) ?? RiEmotionLine
}
