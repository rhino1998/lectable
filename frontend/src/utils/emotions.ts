import type { IconType } from 'react-icons'
import {
  RiChatPrivateLine,
  RiEmotion2Line,
  RiEmotionLaughLine,
  RiEmotionLine,
  RiEmotionSadLine,
  RiEmotionUnhappyLine,
  RiFireLine,
  RiFirstAidKitLine,
  RiGhostLine,
  RiHandHeartLine,
  RiHeart3Line,
  RiMegaphoneLine,
  RiSnowflakeLine,
  RiZzzLine,
} from 'react-icons/ri'
import { EMOTIONS as GENERATED_EMOTIONS, type Emotion } from '../api/types'

// The fixed emotion set a dialogue line can carry comes from the backend
// (generated EMOTIONS - ids are the stored/wire values, in display order).
// An emotion isn't a TTS control token: each voice gets one extra
// reference clip per emotion its lines use, and an emotional line clones
// from that variant instead of the base clip.
// The icon is the annotations view's inline per-line marker
// (ChapterSection's EmotionIcon) and the reader's emotion override menu.
// Keyed by the generated Emotion union, so a new backend emotion fails to
// compile here until it gets an icon.
const EMOTION_ICONS: Record<Emotion, IconType> = {
  warm: RiHeart3Line,
  excited: RiEmotionLaughLine,
  teasing: RiEmotion2Line,
  sad: RiEmotionSadLine,
  pleading: RiHandHeartLine,
  angry: RiFireLine,
  afraid: RiGhostLine,
  nervous: RiEmotionUnhappyLine,
  cold: RiSnowflakeLine,
  whisper: RiChatPrivateLine,
  shout: RiMegaphoneLine,
  weary: RiZzzLine,
  pained: RiFirstAidKitLine,
}

export const EMOTIONS: { id: Emotion; label: string; icon: IconType }[] = GENERATED_EMOTIONS.map((e) => ({
  id: e.id,
  label: e.label,
  icon: EMOTION_ICONS[e.id],
}))

const LABELS = new Map<string, string>(EMOTIONS.map((e) => [e.id, e.label]))
const ICONS = new Map<string, IconType>(EMOTIONS.map((e) => [e.id, e.icon]))

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
