package com.lectable.app.data.remote.dto

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

@Serializable
enum class AudioStatus {
    @SerialName("pending") PENDING,
    @SerialName("generating") GENERATING,
    @SerialName("ready") READY,
    @SerialName("error") ERROR,
}

/** One word's forced-alignment timing from tts-service's POST /align (seconds, matching the
 *  Python/Go/TS pipeline's units exactly - no ms anywhere). See ttsclient.Align on the backend.
 *  No confidence field: the aligner hardcodes it to 0.0 upstream, so it was dropped everywhere. */
@Serializable
data class WordTimingDto(
    val text: String,
    val start: Double,
    val end: Double,
)

/** One inline delivery tag pinned to where it was actually inserted in a paragraph's own text -
 *  see backend's directionMarkDTO. [offset] is a rune (Unicode code point) index into the
 *  paragraph's `text`, which lines up 1:1 with a plain Kotlin String index for anything actually
 *  found in book prose (both are UTF-16-code-unit-based, same as the JS index the frontend reads
 *  this against - see the backend field's own doc comment) - it would only drift for a character
 *  needing a UTF-16 surrogate pair, which no delivery tag is ever anchored beside in practice. */
@Serializable
data class DirectionMarkDto(
    val offset: Int,
    val tag: String,
)

/** One resolved pronunciation substitution pinned to where it applies in a paragraph's own
 *  `text` - see backend's pronunciationMarkDTO. [offset]/[length] are rune (Unicode code point)
 *  counts, same convention as [DirectionMarkDto.offset]. [original] is the exact text this mark
 *  covers (e.g. "Dr."), sent alongside [replacement] so a hover/long-press label can show both
 *  without re-slicing `text` itself. */
@Serializable
data class PronunciationMarkDto(
    val offset: Int,
    val length: Int,
    val original: String,
    val replacement: String,
)

@Serializable
data class ParagraphDto(
    val idx: Int,
    val text: String,
    val audioStatus: AudioStatus,
    val audioError: String? = null,
    val durationSeconds: Double? = null,
    val audioUrl: String? = null,
    // "" (never attributed) / "Narrator" / a character name - see backend's
    // paragraphDTO.Speaker. Purely informational here: audioUrl already points at whichever
    // voice this paragraph actually resolved to (book's own, or this speaker's assigned voice
    // when the book's multiVoice is on) - playback needs no client-side voice logic at all.
    val speaker: String? = null,
    // True when this is an actual quoted-dialogue span, as opposed to narration/description -
    // see backend's paragraphDTO.IsQuote. This, not [speaker] being set, is what marks a
    // segment as dialogue for ReaderScreen.kt's annotations mode: an unattributed quote still
    // has speaker == "" but is dialogue all the same.
    val isQuote: Boolean = false,
    // Marks an isQuote paragraph as NOT actually spoken dialogue despite looking like it
    // structurally (a sarcastic/scare-quoted phrase inside otherwise-ordinary narration) - see
    // backend's paragraphDTO.ScareQuote/store.Paragraph.ScareQuote. Always false for a
    // non-isQuote paragraph. Drives the long-press "Mark/unmark as scare quote" action
    // (ReaderViewModel.setScareQuote) - purely a display/annotation flag otherwise, same as
    // [speaker]; the backend already resolves a scare quote's own audio into whichever
    // narration-merge-group clip it belongs to (see [audioPointerSeconds]).
    val scareQuote: Boolean = false,
    // Which characters (if any) this paragraph's narration describes - see backend's
    // paragraphDTO.DescribesCharacters. Always empty when isQuote is true. Drives ReaderScreen
    // .kt's annotations mode alongside isQuote.
    val describesCharacters: List<String> = emptyList(),
    // Marks this paragraph as a split-out continuation of the previous one (a quote/narration
    // segment split out of one mixed source paragraph by backend's epub.splitQuoteSegments),
    // not the start of a new visual paragraph - see backend's paragraphDTO.Inline. Consecutive
    // Inline paragraphs should render joined into one visual block with no break between them
    // (see ReaderScreen.kt's InfiniteChapterContent grouping), same as
    // frontend/src/pages/ReaderPage.tsx's groupContent, while each still keeps its own
    // playback/highlight unit.
    val inline: Boolean = false,
    // Every inline Higgs delivery tag active on this paragraph (e.g. "<|emotion:anger|>",
    // "<|sfx:laughter|>"), each pinned to the exact rune offset into [text] it was inserted at -
    // see backend's paragraphDTO.DirectionMarks/directionMarkDTO. Already resolved for whichever
    // clone model currently narrates it; empty for the common case (no tags, or a clone model
    // that doesn't understand this vocabulary). Purely informational, same as [speaker]: the
    // tags are baked into audioUrl's own generated audio server-side, so playback needs no
    // client-side handling of them at all - see ReaderScreen.kt's formatDirectionTag/
    // directionTagCategory for display.
    val directionMarks: List<DirectionMarkDto> = emptyList(),
    // Every resolved pronunciation substitution active on this paragraph (e.g. "Dr." -> "Doctor"),
    // each pinned to the exact word it replaces - see backend's paragraphDTO.PronunciationMarks/
    // pronunciationMarkDTO. Unlike [directionMarks], never clone-model-scoped: a pronunciation fix
    // reads the same regardless of which TTS model narrates it. Purely informational, same
    // reasoning as [directionMarks] - the substitution is already baked into audioUrl's own
    // generated audio server-side (see backend internal/pronounce), so playback needs no
    // client-side handling of it at all; only ReaderScreen.kt's annotations mode displays it.
    val pronunciationMarks: List<PronunciationMarkDto> = emptyList(),
    // Always present on a full paragraph snapshot (REST) - "[]" before alignment has run,
    // same as backend's paragraphDTO.Words. Not every word necessarily has a 1:1 match with
    // our own whitespace tokenization (contractions, hyphenation, etc.) - see
    // ui/reader/WordHighlight.kt's wordStartTimes for the fallback this drives.
    val words: List<WordTimingDto> = emptyList(),
    // Set only when this paragraph is a scare-quote merge group's own non-anchor member (see
    // backend's paragraphDTO.AudioPointerSeconds/store.AudioState.PointerOffset): [audioUrl]
    // already resolves to the group's one shared clip (the anchor paragraph's own file), and
    // this is where THIS paragraph's own content actually starts within it, in seconds. 0.0
    // (the default) for a paragraph with its own real file, which always begins at that file's
    // own start - see ParagraphPlayer.playParagraph's own seek-target computation and
    // WordHighlight.kt's wordStartTimes, both of which treat every "seconds" value tied to a
    // paragraph as an absolute position within *its own* audioUrl - already correct for an
    // ordinary paragraph (pointer 0) without any special-casing, and exactly this offset for a
    // merge-group member. Never seek/reload across a merge group's own internal boundary
    // (a same-audioUrl transition, see ParagraphPlayer's loadedAudioUrl) - the underlying
    // recording is one continuous, never-cut file, so there's nothing to actually transition.
    val audioPointerSeconds: Double = 0.0,
)

/**
 * Mirrors the TS discriminated union `ContentItem` (kind: 'text' | 'image' | 'break').
 * Modeled as one flat class rather than a sealed hierarchy so it round-trips
 * through kotlinx.serialization without a custom polymorphic serializer -
 * callers switch on [kind]. A "break" item (a scene/section break - the
 * source epub's own <hr/>, or a text-only marker like "* * *") carries no
 * data of its own beyond its position among the surrounding content -
 * [paragraphIdx] is present but meaningless for it (always 0, the backend's
 * own zero value for an unused int field - never dereferenced against
 * [ChapterDetailDto.paragraphs] the way a "text" item's is).
 */
@Serializable
data class ContentItemDto(
    val kind: String,
    val paragraphIdx: Int,
    val imageUrl: String? = null,
)

@Serializable
data class ChapterDetailDto(
    val idx: Int,
    val title: String,
    val generating: Boolean,
    val paragraphs: List<ParagraphDto>,
    val content: List<ContentItemDto>,
)

@Serializable
data class QueuedResponseDto(val queued: Boolean)

@Serializable
data class LookaheadRequestDto(val chapterIdx: Int, val paragraphIdx: Int)
