// Voice-recipe defaults shared by VoiceEditorForm's callers (VoicesPage,
// CharacterVoiceEditor) - kept out of the component file so it only exports
// components (fast refresh).

// Kept identical to tts-service's own DEFAULT_REF_TEXT (voices.py) - the
// same phonetically-balanced pangram-style line it clones its built-in
// presets from, so a custom voice's default reference clip samples English
// phonemes just as broadly instead of whatever a short throwaway line
// happens to contain.
export const DEFAULT_REF_TEXT =
  'The beige hue on the waters of the loch impressed all, including the French queen, ' +
  'before she heard that symphony again, just as young Arthur wanted.'

// A positive 31-bit seed, same shape as the backend's own store.RandomSeed
// (doesn't need to match bit-for-bit, just "some positive int"). A brand
// new voice pins one of these up front rather than leaving it for the
// server to pick at save time, specifically so a design preview and the
// eventual create-voice call agree on the same seed - otherwise they'd be
// unrelated random samples of the same instruct, and saving could never
// reuse what testing just rendered (see backend voicerefs.DesignConfigHash).
export function randomSeed(): number {
  return Math.floor(Math.random() * 0x7fffffff)
}
