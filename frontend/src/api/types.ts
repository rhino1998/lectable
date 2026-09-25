// Wire types, enums, the model catalog, live topics, and the route table
// are generated from the backend (see backend/cmd/apigen) into
// ./generated; this file re-exports them and adds the frontend-only
// pieces: UI copy keyed by the generated unions (so a new backend value is
// a compile error here until it gets a label), lookup tables derived from
// the catalog, and shapes TS can model more precisely than Go.
export * from './generated'
import {
  CLONE_MODELS,
  DESIGN_MODELS,
  type CharacterVoiceMode,
  type CloneModel,
  type DesignModel,
  type SFXEngine,
} from './generated'

// Live patch ops - backend live.Op (custom-marshaled, so not generated).
export type LivePath = (string | number)[]
export type LiveArrItem = [number, number] | { v: unknown }
export type LiveOp =
  | { op: 'set'; path: LivePath; value: unknown }
  | { op: 'del'; path: LivePath }
  | { op: 'arr'; path: LivePath; items: LiveArrItem[] }

export const SFX_ENGINE_LABELS: Record<SFXEngine, string> = {
  ace_step: 'ACE-Step (music)',
  stable_audio_sfx: 'Stable Audio 3 (sound effects)',
  stable_audio_music: 'Stable Audio 3 Small (music)',
  stable_audio_medium: 'Stable Audio 3 Medium (music)',
}

// Which of the three broad narration roles a paragraph segment plays in
// the annotations view - computed client-side from isQuote/
// describesCharacters (ReaderPage's annotationKind) rather than sent by
// the backend as its own field, mirroring how the existing speaker-hint
// badge is already derived from raw paragraph fields.
export type AnnotationKind = 'narrator' | 'speaker' | 'description'

// Shared between ReaderPage (per-segment title/legend) and PlayerBar (the
// bottom-bar toggle's own legend row), so the two can't drift.
export const ANNOTATION_LABELS: Record<AnnotationKind, string> = {
  narrator: 'Narrator',
  speaker: 'Speaker',
  description: 'Description',
}

export type ContentItem =
  | { kind: 'text'; paragraphIdx: number }
  | { kind: 'image'; paragraphIdx: number; imageUrl: string }
  // A scene/section break (backend epub.BlockBreak) - the source epub's own
  // <hr/>, or a paragraph that was nothing but scene-break glyphs
  // ("* * *", "***", a lone "⁂"). Carries no data of its own beyond its
  // position among the surrounding content.
  | { kind: 'break' }

export const CHARACTER_VOICE_MODE_LABELS: Record<CharacterVoiceMode, string> = {
  narrator: 'Narrator only',
  assigned: 'Custom character voices',
  instruct_unassigned: "Style unassigned characters with the narrator's voice",
  instruct_all: "Style all characters with the narrator's voice",
}

export const CHARACTER_VOICE_MODE_DESCRIPTIONS: Record<CharacterVoiceMode, string> = {
  narrator: 'Every paragraph narrates in this book\'s own single voice, regardless of attributed speaker.',
  assigned:
    'A character with a voice explicitly assigned below narrates in that voice; an unassigned character still narrates in the book\'s own.',
  instruct_unassigned:
    "A character with no voice explicitly assigned narrates in this book's own narrator voice, styled by their own characterization, instead of getting an entirely separate, independently-synthesized voice. A character can still be given their own distinct voice below regardless.",
  instruct_all:
    "Every character narrates in this book's own narrator voice, styled by their own characterization - even one with its own explicitly assigned voice below (that assignment is ignored while this is selected, not deleted).",
}

export const CLONE_MODEL_LABELS = Object.fromEntries(CLONE_MODELS.map((m) => [m.id, m.label])) as Record<CloneModel, string>

export const DESIGN_MODEL_LABELS = Object.fromEntries(DESIGN_MODELS.map((m) => [m.id, m.label])) as Record<DesignModel, string>

// Each design engine's default guidance_scale, for the ones that read one
// - the voice editor's placeholder; absent = the editor hides the control.
export const DESIGN_MODEL_GUIDANCE_DEFAULTS: Partial<Record<DesignModel, number>> = Object.fromEntries(
  DESIGN_MODELS.flatMap((m) => ('defaultGuidanceScale' in m ? [[m.id, m.defaultGuidanceScale]] : [])),
)

// Each model's default sampling temperature, for the ones that read one -
// the voice tester's placeholder; absent = the tester hides the control.
export const CLONE_MODEL_TEMPERATURE_DEFAULTS: Partial<Record<CloneModel, number>> = Object.fromEntries(
  CLONE_MODELS.flatMap((m) => ('defaultTemperature' in m ? [[m.id, m.defaultTemperature]] : [])),
)

export const DESIGN_MODEL_TEMPERATURE_DEFAULTS: Partial<Record<DesignModel, number>> = Object.fromEntries(
  DESIGN_MODELS.flatMap((m) => ('defaultTemperature' in m ? [[m.id, m.defaultTemperature]] : [])),
)
