export interface BookSummary {
  id: string
  title: string
  author: string
  seriesName?: string
  seriesIndex?: number
  coverUrl?: string
  chapterCount: number
  posChapterIdx: number
  posParagraphIdx: number
  posSeconds: number
  progressPercent: number
  finished: boolean
  estimatedTotalSeconds: number
  estimateCalibrated: boolean
  generatedPercent: number
  // True while the backend's whole-book preprocess meta-task (attribution
  // -> characterization -> voice provisioning -> direction tagging) is
  // still running for this book - see backend httpapi.isPreprocessing.
  // useBooks polls while any book has this set so the library page's
  // spinner clears on its own once the run finishes.
  preprocessing: boolean
  // Reader-facing background-music toggle - book-wide (unlike
  // ChapterSummary.passes.music below, which is chapter-scoped scoring
  // progress), set via VoiceSettings.musicEnabled/api.updateVoice. Mirrored
  // here too purely as a read convenience, so pages already holding a
  // book (the reader, the Speakers page) don't need a separate voice-
  // settings fetch just to know whether background music is on.
  musicEnabled: boolean
}

export interface ChapterSummary {
  idx: number
  title: string
  paragraphCount: number
  readyCount: number
  // Persisted (not derived) - see backend store.Passes. Independent
  // per-pipeline-pass facts; direction is no longer keyed by clone_model
  // (only "audiocpp-higgs-4b" ever actually produced sentence/inline tags,
  // but pronunciation resolution - the third sub-pass - runs regardless
  // of clone model, so this is just one flat bool now). music is set once
  // background-music tone-region scoring (api.scoreChapterMusic) has
  // completed a full pass over this chapter - independent of the book-wide
  // BookSummary.musicEnabled toggle, the same way direction is independent
  // of attribution.
  passes: {
    attribution: boolean
    description: boolean
    scareQuote: boolean
    direction: boolean
    music: boolean
  }
}

export interface BookDetail extends BookSummary {
  chapters: ChapterSummary[]
}

export type AudioStatus = 'pending' | 'generating' | 'ready' | 'error'

// Forced word-level alignment from tts-service (backend: ttsclient.Align /
// POST /align) - start/end in seconds within the paragraph's own clip.
// No confidence field: the aligner hardcodes it to 0.0 upstream rather
// than computing anything, so tts-service doesn't send it.
export interface WordTiming {
  text: string
  start: number
  end: number
}

export interface Paragraph {
  idx: number
  text: string
  audioStatus: AudioStatus
  audioError?: string
  durationSeconds?: number
  audioUrl?: string
  // Set only when this paragraph is a scare-quote merge group pointer (see
  // backend store.AudioState.PointerOffset) - the time, in seconds within
  // audioUrl's own shared clip, where this paragraph's own content
  // actually begins. audioUrl itself already resolves to whichever
  // paragraph holds that shared recording, so two consecutive paragraphs
  // sharing a merge group report the identical audioUrl string -
  // usePlayback.ts relies on that exact match to know playback already
  // mid-flight through the shared clip should keep flowing rather than
  // reload/restart at the boundary, using this value only to know when
  // the "current" paragraph (for highlighting/position-reporting) has
  // advanced into this one. 0/absent for a paragraph with its own real
  // file, which always begins at the very start of audioUrl's clip.
  audioPointerSeconds?: number
  // "" (never attributed) or "Narrator" both narrate in the book's own
  // voice; anything else is a character name - see SpeakerRow and
  // VoicePanel/SpeakersPanel. Only actually changes this paragraph's
  // narration voice when the book's characterVoiceMode isn't "narrator"
  // and that character has a voice assigned (or the mode styles them via
  // the narrator anyway) - otherwise purely informational.
  speaker?: string
  // True when this paragraph is a split-out continuation of the previous
  // one (a quoted-dialogue segment split out of a mixed narration+
  // dialogue source paragraph), not the start of a new visual paragraph -
  // consecutive inline paragraphs should render joined, with no break
  // between them, while still being their own playback/highlight unit.
  inline?: boolean
  // True when this is an actual quoted-dialogue span, as opposed to
  // narration/description - see backend store.Paragraph.IsQuote. This,
  // not `speaker` being set, is what marks a segment as dialogue in the
  // annotations view: an unattributed quote still has speaker == ""/
  // undefined but is dialogue all the same.
  isQuote?: boolean
  // True when this IsQuote paragraph looks structurally like dialogue but
  // isn't actually spoken aloud (sarcasm, a quoted term, a written source
  // being read) - see backend store.Paragraph.ScareQuote. Always false
  // when isQuote is false.
  scareQuote?: boolean
  // Which characters (if any) this paragraph's narration describes - see
  // backend store.Paragraph.DescribesCharacters. Always empty when
  // isQuote is true.
  describesCharacters?: string[]
  // Empty until alignment finishes (arrives later via useBookUpdates'
  // WebSocket push, or already populated on a fresh chapter fetch) -
  // ParagraphText falls back to estimating word timing from character
  // position until then.
  words: WordTiming[]
  // DirectionMarks mirrors backend paragraphDTO.DirectionMarks - every
  // inline Higgs delivery tag (e.g. "<|emotion:anger|>", "<|sfx:laughter|>")
  // currently active on this paragraph, each pinned to where it was
  // actually inserted (offset is a JS-string-index-compatible code point
  // count into `text` - see the backend field's own doc comment), already
  // resolved for whichever clone model currently narrates it, in speaking
  // order. A paragraph can carry more than one (a mid-quote emotion shift,
  // or an emotion tag stacked with an inline sfx tag). Empty/absent for
  // the common case (no tags, or a clone model that doesn't understand
  // this vocabulary).
  directionMarks?: DirectionMark[]
  // PronunciationMarks mirrors backend paragraphDTO.PronunciationMarks -
  // every resolved pronunciation substitution ("Dr." -> "Doctor") active
  // on this paragraph, each pinned to the exact word it replaces (offset
  // is a JS-string-index-compatible code point count into `text`, same
  // convention as DirectionMark.offset). Unlike DirectionMarks, not
  // scoped to a clone model - a pronunciation fix reads the same
  // regardless of which TTS model narrates it. Empty/absent for the
  // common case (no ambiguous abbreviation found in this paragraph).
  pronunciationMarks?: PronunciationMark[]
  // sfx* are a sound-effect (Stable Audio SFX) test surface (see backend
  // store.Paragraph's own SFX* fields) - manual/plumbing-only for now:
  // sfxPrompt is a reader-entered text-to-audio prompt, sfxStatus is ''
  // (never generated) | 'generating' | 'ready' | 'error', and
  // sfxAudioUrl is set only once sfxStatus is 'ready'. Independent of
  // audioStatus/audioUrl above - a sound effect isn't narration and isn't
  // voice-scoped. sfxTriggerWord is which word (a 0-based index into
  // `words` above) playback should start the clip at, once ready.
  sfxPrompt?: string
  sfxStatus?: '' | 'generating' | 'ready' | 'error'
  sfxError?: string
  sfxAudioUrl?: string
  sfxDurationSeconds?: number
  sfxTriggerWord?: number
}

// Which generation-only "gen task" engine a standalone SFX test render
// uses (see SFXTestOptions.engine) - kept identical to the backend's own
// httpapi.generateSFXRequest.Engine values.
export const SFX_ENGINES = [
  'ace_step',
  'stable_audio_music',
  'stable_audio_medium',
  'stable_audio_sfx',
] as const
export type SFXEngine = (typeof SFX_ENGINES)[number]

export const SFX_ENGINE_LABELS: Record<SFXEngine, string> = {
  ace_step: 'ACE-Step (music)',
  stable_audio_sfx: 'Stable Audio 3 (sound effects)',
  stable_audio_music: 'Stable Audio 3 Small (music)',
  stable_audio_medium: 'Stable Audio 3 Medium (music)',
}

// SFXTestOptions is the standalone SFX/music test page's own request
// shape (see api.testSFX / httpapi.handleGenerateSFX) - one shared shape
// across every engine (engine selects which one actually runs); every
// field but prompt is optional and defers to that engine's own built-in
// default when omitted - see SFX_ENGINE_LABELS and backend
// audioworker.Worker.Music/StableAudioMusic/StableAudioSFX/
// StableAudioMedium's own doc comments for what each engine actually reads.
export interface SFXTestOptions {
  engine?: SFXEngine
  prompt: string
  lyrics?: string
  negativePrompt?: string
  durationSeconds?: number
  numInferenceSteps?: number
  guidanceScale?: number
  seed?: number
}

// LLMTestOptions is the standalone LLM prompt test page's own request
// shape (see api.testLLM / httpapi.handleTestLLM) - a raw system+user
// prompt pair against this app's own embedded speaker-attribution GGUF
// model, no speakerattr-specific framing. temp/maxTokens left at their
// zero value defer to the backend's own default (maxTokens 2048).
export interface LLMTestOptions {
  systemPrompt?: string
  userPrompt: string
  temp?: number
  maxTokens?: number
}

export interface DirectionMark {
  offset: number
  tag: string
}

export interface PronunciationMark {
  offset: number
  length: number
  original: string
  replacement: string
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

export interface ChapterDetail {
  idx: number
  title: string
  generating: boolean
  paragraphs: Paragraph[]
  content: ContentItem[]
}

// One chapter tone region for background music - see backend
// store.MusicRegion's own doc comment for the full generation pipeline.
export interface MusicRegion {
  id: string
  startIdx: number
  endIdx: number
  mood: string
  // The actual Stable Audio generation prompt for this region - see
  // backend musicRegionDTO.Prompt's own doc comment. Fuller than mood
  // (a short label for compact display); shown as the reader's
  // annotations-view boundary marker's own tooltip.
  prompt: string
  transition: 'cut' | 'continuation'
  status: AudioStatus
  error?: string
  durationSeconds: number
  // Set only once status is 'ready' - see musicRegionDTO's own doc comment.
  audioUrl?: string
}

// enabled mirrors BookSummary.musicEnabled (the book-wide toggle); scored
// mirrors ChapterSummary.passes.music (this one chapter's own scoring
// progress) - both duplicated here purely for convenience, since
// useBackgroundMusic fetches this endpoint regardless for the regions
// themselves and a caller may not have the book/chapter list in scope too.
export interface ChapterMusic {
  enabled: boolean
  scored: boolean
  regions: MusicRegion[]
}

export interface VoicePreset {
  id: string
  name: string
  instruct: string
  seed: number
  ref_text: string
  speed_multiplier: number
  cloneModel: string
  // Which VoiceDesign engine renders this preset's reference clip - ""
  // defers to the worker's own process-wide default (see DESIGN_MODELS
  // below).
  designModel: string
  audioUrl: string
  // Only ever populated in a regeneratePreset response - GET
  // /api/voices/presets doesn't render a preset's clip just to list it, so
  // this is always absent there.
  refError?: string
}

export interface VoicePresets {
  default: string
  presets: VoicePreset[]
}

// Keep in sync with the backend's store.CharacterVoiceMode - whether/how a
// character's own narration voice can override the book's own. Exactly
// four mutually exclusive states (a dropdown, not independent checkboxes,
// since combinations beyond these four either don't exist or collapse
// into one of them):
export const CHARACTER_VOICE_MODES = ['narrator', 'assigned', 'instruct_unassigned', 'instruct_all'] as const
export type CharacterVoiceMode = (typeof CHARACTER_VOICE_MODES)[number]

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

// Kept identical to the backend's store.DefaultCharacterVoiceMode - what a
// brand-new book starts with.
export const DEFAULT_CHARACTER_VOICE_MODE: CharacterVoiceMode = 'narrator'

export interface VoiceSettings {
  presetId: string
  instruct: string
  language: string
  seed?: number
  // See CharacterVoiceMode's own doc comment above. The two instruct_*
  // values only actually change anything when the book's resolved clone
  // model is audiocpp-breeze-tts (see INSTRUCTED_CLONE_MODEL below) - a
  // no-op (same as "assigned") otherwise.
  characterVoiceMode: CharacterVoiceMode
  // Opts this book into waiting for speech-direction tagging (a chapter's
  // passes.direction) before generating its audio - see backend
  // voiceSettingsDTO.SpeechDirection. Off by default: most books never
  // touch direction tagging, so nothing should pay an LLM-pass delay
  // before its first audio unless a reader explicitly opts in.
  speechDirection: boolean
  // Reader-facing background-music toggle - book-wide (unlike
  // ChapterSummary.passes.music, which is chapter-scoped scoring progress)
  // since scoring/generating a whole book's worth of chapters at once
  // isn't something turning this on should eagerly trigger - see backend
  // store.Book.MusicEnabled's own doc comment. Off by default.
  musicEnabled: boolean
}

// A user-created, reusable narrator voice, cloned by tts-service from a
// short reference line into a stable reference clip - distinct from the
// curated built-in presets (VoicePreset above), which ship with tts-service
// and are just proxied, never created/edited here.
export interface CustomVoicePreset {
  id: string
  name: string
  instruct: string
  refText: string
  seed: number
  speedMultiplier: number
  cloneModel: string
  // Which VoiceDesign engine renders this preset's reference clip - ""
  // defers to the worker's own process-wide default (see DESIGN_MODELS
  // below). A brand-new preset is always created with an explicit value
  // (DEFAULT_DESIGN_MODEL, "breeze_tts") regardless of what any similar
  // existing preset uses.
  designModel: string
  createdAt: number
  audioUrl: string
  refError?: string
  // Set only for a preset auto-created for a speaker (see SpeakersPage) -
  // the series name that speaker's character roster is shared across, or
  // that lone book's title when it isn't part of a series. Absent for a
  // reader's own general-purpose voice not tied to any character.
  groupLabel?: string
}

// Keep in sync with tts-service/app.py's CLONE_MODELS - one flat enum
// covering both the cloning backend/API and the checkpoint size, since
// every combination maps to a distinct loadable model anyway.
export const CLONE_MODELS = [
  "audiocpp-qwen3-0.6b",
  "audiocpp-higgs-4b",
  "audiocpp-breeze-tts",
  "audiocpp-omnivoice",
] as const
export type CloneModel = (typeof CLONE_MODELS)[number]

export const CLONE_MODEL_LABELS: Record<CloneModel, string> = {
  "audiocpp-qwen3-0.6b": "Qwen3-TTS (audio.cpp)",
  "audiocpp-higgs-4b": "Higgs Audio v3 TTS 4B (audio.cpp)",
  "audiocpp-breeze-tts": "BreezeTTS 2 (audio.cpp)",
  "audiocpp-omnivoice": "OmniVoice (audio.cpp)",
}

// Kept identical to the backend's voices.InstructedCloneModel - the one
// clone model whose family accepts a clone-time style instruction
// alongside its reference clip in the same call ("instructed voice
// cloning"). VoiceSettings.instructCharacterVoices and the voice/character
// editor's "test with another voice as a base" control are both no-ops for
// any other clone model.
export const INSTRUCTED_CLONE_MODEL: CloneModel = "audiocpp-breeze-tts"

// Keep in sync with backend/internal/audioworker's designEngines - which
// VoiceDesign engine renders a preset's reference clip (a separate concern
// from CLONE_MODELS above, which is what clones per-paragraph audio from
// that already-rendered clip).
export const DESIGN_MODELS = ["breeze_tts", "qwen3_tts", "omnivoice"] as const
export type DesignModel = (typeof DESIGN_MODELS)[number]

export const DESIGN_MODEL_LABELS: Record<DesignModel, string> = {
  breeze_tts: "BreezeTTS 2 (audio.cpp)",
  qwen3_tts: "Qwen3-TTS VoiceDesign (audio.cpp)",
  omnivoice: "OmniVoice (audio.cpp)",
}

// Kept identical to the backend's voices.DefaultDesignModel - what a
// brand-new custom voice preset is created with when the request doesn't
// specify one.
export const DEFAULT_DESIGN_MODEL: DesignModel = "breeze_tts"

export interface CustomVoicePresetInput {
  name: string
  instruct: string
  refText: string
  seed?: number
  speedMultiplier?: number
  cloneModel?: string
  designModel?: string
}

export interface Position {
  chapterIdx: number
  paragraphIdx: number
  seconds: number
}

export interface SearchResult {
  chapterIdx: number
  chapterTitle: string
  paragraphIdx: number
  text: string
}

export interface Bookmark {
  id: string
  chapterIdx: number
  chapterTitle: string
  paragraphIdx: number
  text: string
  note: string
  createdAt: number
}

// QueueTask is one task in the backend's shared job queue (see backend
// internal/jobs.Kind, .QueueTask) - voice-clone/design paragraph
// generation, a chapter's speaker-attribution run, a character's
// voice-characterization run, or one of a book's own four preprocessing
// phases (the "pipeline_*" kinds - see backend jobs.EnqueuePipeline), all
// sorted through the same tier-ordered priority queue (per-item kinds) or
// shown alongside it (pipeline kinds - see backend jobs.Manager.Snapshot's
// own doc comment on why the two aren't really one shared priority order).
export interface QueueTask {
  // Stable per-task identifier (backend jobs.QueueTask.ID) - what Cancel
  // takes and what a dashboard row should key on, rather than a composite
  // of the other, display-oriented fields.
  id: string
  kind:
    | 'voice_clone'
    | 'voice_design'
    | 'voice_design_preview'
    | 'voice_provision'
    | 'speaker_attribution'
    | 'speaker_characterization'
    // "Auto Split" - one per chapter containing a paragraph currently
    // credited to the speaker being eliminated (see backend
    // jobs.KindSpeakerReattribution/EnqueueReattribution). Chapter-scoped
    // like speaker_attribution (chapterId/chapterIdx meaningful), but
    // label carries the speaker's own name (a real character's name, or
    // literally "Unknown") the same way speaker_characterization's label
    // carries a character's name - see useReattributingSpeakers.
    | 'speaker-reattribute'
    | 'speech_direction'
    // Background-music tone-region scoring for one chapter (LLM,
    // chapter-scoped) and rendering one already-scored region's own clip
    // (Stable Audio, not chapter/paragraph-scoped the way the others
    // above are - see backend jobs.KindMusicGeneration) - see backend
    // jobs.KindMusicScoring and store.MusicRegion's own doc comment.
    | 'music_scoring'
    | 'music_generation'
    // Stable Audio SFX sound-effect generation - a paragraph's own
    // persisted clip (sfx_generation, chapter/paragraph-scoped,
    // ParagraphIdx below meaningful) - see backend jobs.KindSFXGeneration.
    | 'sfx_generation'
    // A standalone, one-off render from any of this app's generation-only
    // engines (ACE-Step, Stable Audio - Music/SFX/Medium - see
    // SFX_ENGINES), no chapter/paragraph of its own - the SFX test page's
    // own POST /api/sfx/generate (see backend jobs.KindSFXPreview). One
    // shared kind across every engine, not one per engine.
    | 'sfx_preview'
    // A standalone raw system+user prompt call against this app's own
    // embedded speaker-attribution GGUF model, no chapter/paragraph of its
    // own - the LLM test page's own POST /api/llm/test (see backend
    // jobs.KindLLMPreview).
    | 'llm_preview'
    // The five ordered phases of a book's own "Preprocess" run (POST
    // .../preprocess) - "pipeline_"-prefixed so none of these can ever
    // collide with the per-item kind sharing a phase's own bare name
    // (voice_provision above is a real, distinct per-character kind).
    // Never chapter/paragraph-scoped - see label below.
    | 'pipeline_attribution'
    | 'pipeline_characterization'
    | 'pipeline_voice_provision'
    | 'pipeline_direction'
    | 'pipeline_music'
    // The library page's "Generate audio" -> All/Remaining and the
    // reader's "Generate chapter audio" - moved off bare fire-and-forget
    // goroutines onto this same cancelable poolPipeline machinery (see
    // backend jobs.Manager.EnqueueChapter/EnqueueBookGenerate/
    // EnqueueRemaining). Unlike the four phases above, these carry no
    // dependency chain and never affect BookSummary.preprocessing.
    // pipeline_generate_chapter is chapter-scoped (see label below); the
    // other two cover the whole book, like every phase above.
    | 'pipeline_generate_chapter'
    | 'pipeline_generate_book'
    | 'pipeline_generate_remaining'
  // label is a human-readable identifier for a kind that isn't chapter/
  // paragraph-scoped - the character's name for "speaker_characterization"
  // and "voice_provision" (see jobs.EnqueueVoiceProvision), a "preview #N"
  // sequence tag for "voice_design_preview" (which has no chapter/character
  // identity at all - see jobs.RunVoiceDesignPreview), the fixed string
  // "Whole book" for every book-scoped "pipeline_*" kind (a phase/generate-
  // book/generate-remaining task covers every chapter in the book, not any
  // one of them) or "Chapter N" for "pipeline_generate_chapter" (see
  // ChapterIdx) - "" for every other kind, which already has a chapter/
  // paragraph to show.
  label?: string
  bookId: string
  bookTitle: string
  // chapterIdx/chapterTitle are meaningless (0/"") for
  // "speaker_characterization" and every "pipeline_*" kind, neither of
  // which is chapter-scoped - see label above.
  chapterIdx: number
  chapterTitle: string
  // paragraphIdx is meaningless (0) for the two LLM kinds
  // ("speaker_attribution"/"speaker_characterization") and every
  // "pipeline_*" kind, none of which are paragraph-scoped.
  paragraphIdx: number
  tier: 'urgent' | 'lookahead' | 'background'
  // presetId/instruct are "" for the two LLM kinds and every "pipeline_*"
  // kind, and for a "voice_design" task presetId alone is "" (a pure
  // custom instruct with no preset backing it) - resolve presetId against
  // built-in/custom preset lists for a friendly name, falling back to
  // instruct.
  presetId: string
  instruct: string
  // attempt counts genuine failures this exact unit of work has already
  // suffered (backend jobs.QueueTask.Attempt) - 0 on a task's first ever
  // dispatch, only bumped by a real failure-retry, never by a cooperative
  // pause (a long chapter's own attribution/direction-tagging run
  // pausing between batches to yield to something more urgent, then
  // picking back up - see backend/CLAUDE.md's "Speech-direction
  // tagging"). A row that keeps reappearing with attempt staying at 0 is
  // just slow, normal progress through a long chapter; one where it's
  // climbing is genuinely failing and retrying.
  attempt: number
}

export interface JobsSnapshot {
  inFlight: QueueTask[]
  queued: QueueTask[]
  // paused mirrors backend jobs.Manager.Paused() - see the Jobs page's
  // own pause/resume button.
  paused: boolean
}

// One row of a book's speaker table (GET .../speakers) - the synthesized
// Narrator row (id "") plus every character attributed so far, shared
// across the whole series a book belongs to (see backend
// store.SeriesScope). id is also "" for a speaker name a paragraph carries
// that was never registered as a character - shouldn't normally happen,
// but the row still displays rather than being hidden.
export interface Speaker {
  id: string
  name: string
  voicePresetId?: string
  // The LLM's characterization of how this speaker sounds - "" (never
  // characterized) for the Narrator row or a character not yet voiced.
  summary?: string
  // summary's paired reference passage - what this character's
  // auto-created voice preset's own reference clip is actually rendered
  // from, written to match summary's own pace/energy. "" alongside an
  // empty summary, same as summary itself.
  refLine?: string
  paragraphCount: number
  readyCount: number
  // The voice this speaker is currently resolving to's own reference clip
  // - available immediately after a voice is assigned/auto-assigned,
  // before any of this book's paragraphs have actually been generated in
  // it. Previewing real generated dialogue lives in each appearance's own
  // audioUrl (see SpeakerAppearance below) instead of a single row-level
  // sample.
  refAudioUrl?: string
}

// One paragraph a character speaks, from GET
// .../characters/{characterId}/appearances - spans every book in the
// character's series (store.SeriesScope), not just the book the request
// was made through.
export interface SpeakerAppearance {
  bookId: string
  bookTitle: string
  chapterIdx: number
  chapterTitle: string
  paragraphIdx: number
  text: string
  // Set only once this specific paragraph's audio is ready under
  // whatever voice it currently resolves to - "" (absent) means it needs
  // generating first (see AppearanceRow's own "Generate" fallback).
  audioUrl?: string
}
