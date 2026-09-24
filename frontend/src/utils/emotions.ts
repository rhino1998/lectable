// The fixed emotion set a dialogue line can carry - mirrors backend
// internal/emotions.All (ids are the stored/wire values; keep the two in
// the same order). An emotion isn't a TTS control token: each voice gets
// one extra reference clip per emotion its lines use, and an emotional
// line clones from that variant instead of the base clip.
export const EMOTIONS: { id: string; label: string }[] = [
  { id: 'warm', label: 'Warm' },
  { id: 'excited', label: 'Excited' },
  { id: 'sad', label: 'Sad' },
  { id: 'angry', label: 'Angry' },
  { id: 'afraid', label: 'Afraid' },
  { id: 'cold', label: 'Cold' },
  { id: 'whisper', label: 'Whisper' },
  { id: 'shout', label: 'Shout' },
  { id: 'weary', label: 'Weary' },
]

const LABELS = new Map(EMOTIONS.map((e) => [e.id, e.label]))

// Display label for an emotion id - the id itself for one this list
// doesn't know (a newer backend), rather than hiding it.
export function emotionLabel(id: string): string {
  return LABELS.get(id) ?? id
}
