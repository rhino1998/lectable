package com.lectable.app.ui.reader

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.VolumeDown
import androidx.compose.material.icons.outlined.AcUnit
import androidx.compose.material.icons.outlined.Bedtime
import androidx.compose.material.icons.outlined.Campaign
import androidx.compose.material.icons.outlined.EmojiEmotions
import androidx.compose.material.icons.outlined.FavoriteBorder
import androidx.compose.material.icons.outlined.Healing
import androidx.compose.material.icons.outlined.LocalFireDepartment
import androidx.compose.material.icons.outlined.Mood
import androidx.compose.material.icons.outlined.MoodBad
import androidx.compose.material.icons.outlined.SentimentDissatisfied
import androidx.compose.material.icons.outlined.SentimentNeutral
import androidx.compose.material.icons.outlined.SentimentVeryDissatisfied
import androidx.compose.material.icons.outlined.SentimentVerySatisfied
import androidx.compose.material.icons.outlined.VolunteerActivism
import androidx.compose.material.icons.filled.Bedtime
import androidx.compose.material.icons.filled.FormatStrikethrough
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material.icons.filled.Mood
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.unit.dp
import com.lectable.app.data.remote.dto.ParagraphDto
import com.lectable.app.data.remote.dto.PronunciationMarkDto

// Every emotion id a dialogue line can be set to, in display order - mirrors backend
// internal/emotions.All and frontend's utils/emotions.ts EMOTIONS.
internal val EMOTION_IDS = listOf(
    "warm", "excited", "teasing", "sad", "pleading", "angry", "afraid", "nervous", "cold", "whisper", "shout",
    "weary", "pained",
)

// An emotion id ("angry", "whisper") as a display label ("Angry") - mirrors frontend's
// utils/emotions.ts emotionLabel, where every label is just the capitalized id.
internal fun formatEmotion(id: String): String = id.replaceFirstChar { it.uppercase() }

// An emotion id's icon - annotations mode's inline per-line marker (see ParagraphBodyText) and
// the "Set emotion" menu. Closest Material counterparts of frontend's utils/emotions.ts icons
// (there's no ghost for "afraid", hence the dismayed face); "" is Neutral, and an id this list
// doesn't know (a newer backend) gets a generic face rather than being hidden - same fallback
// as frontend's emotionIcon.
internal fun emotionIcon(id: String): ImageVector = when (id) {
    "" -> Icons.Outlined.SentimentNeutral
    "warm" -> Icons.Outlined.FavoriteBorder
    "excited" -> Icons.Outlined.SentimentVerySatisfied
    "teasing" -> Icons.Outlined.EmojiEmotions
    "sad" -> Icons.Outlined.SentimentDissatisfied
    "pleading" -> Icons.Outlined.VolunteerActivism
    "angry" -> Icons.Outlined.LocalFireDepartment
    "afraid" -> Icons.Outlined.SentimentVeryDissatisfied
    "nervous" -> Icons.Outlined.MoodBad
    "cold" -> Icons.Outlined.AcUnit
    "whisper" -> Icons.AutoMirrored.Outlined.VolumeDown
    "shout" -> Icons.Outlined.Campaign
    "weary" -> Icons.Outlined.Bedtime
    "pained" -> Icons.Outlined.Healing
    else -> Icons.Outlined.Mood
}

// ParagraphBodyText's inlineContent key for one emotion's inline icon.
internal fun emotionInlineId(emotion: String): String = "emotion:$emotion"

// Exact hex match to frontend's --direction-emotion, which its .emotion-icon uses - reuses
// DirectionEmotionColor* since emotion is no longer a delivery-tag caret category.
internal fun emotionColor(isDarkTheme: Boolean): Color =
    if (isDarkTheme) DirectionEmotionColorDark else DirectionEmotionColorLight

// Turns a raw Higgs inline delivery tag (today only "<|prosody:pause|>") into a
// short human-readable label ("Anger", "Whispering") - mirrors frontend's ReaderPage.tsx
// formatDirectionTag exactly, including its fallback: the exact <|category:value|> syntax
// matters to audio.cpp's tokenizer (see backend speakerattr.validDirectionTags), not to a
// reader, so this is purely a display concern - falls back to the raw tag string unchanged if
// it doesn't match the expected shape, rather than hiding it.
private val DIRECTION_TAG_REGEX = Regex("^<\\|\\w+:(\\w+)\\|>$")

internal fun formatDirectionTag(tag: String): String {
    val match = DIRECTION_TAG_REGEX.find(tag) ?: return tag
    val value = match.groupValues[1].replace('_', ' ')
    return value.replaceFirstChar { it.uppercaseChar() }
}

/** Mirrors frontend's PronunciationCaret hover tooltip text exactly. */
internal fun formatPronunciationMark(mark: PronunciationMarkDto): String =
    "\"${mark.original}\" read as \"${mark.replacement}\""

// A delivery tag's own category ("emotion", "style", "prosody", "sfx") for per-category caret
// coloring - mirrors frontend's utils/directionTags.ts directionTagCategory exactly.
internal enum class DirectionTagCategory { EMOTION, STYLE, PROSODY, SFX, OTHER }

private val DIRECTION_TAG_CATEGORY_REGEX = Regex("^<\\|(\\w+):")

internal fun directionTagCategory(tag: String): DirectionTagCategory =
    when (DIRECTION_TAG_CATEGORY_REGEX.find(tag)?.groupValues?.get(1)) {
        "emotion" -> DirectionTagCategory.EMOTION
        "style" -> DirectionTagCategory.STYLE
        "prosody" -> DirectionTagCategory.PROSODY
        "sfx" -> DirectionTagCategory.SFX
        else -> DirectionTagCategory.OTHER
    }

/** [otherColor] backs "other" - a display-only fallback for a tag [directionTagCategory] doesn't
 *  recognize, never something this app's own LLM passes actually produce, so it has no real
 *  brand color of its own (mirrors frontend's own .direction-caret-other, which reuses
 *  --annotation-narrator for the same reason). */
internal fun directionTagColor(tag: String, isDarkTheme: Boolean, otherColor: Color): Color =
    when (directionTagCategory(tag)) {
        DirectionTagCategory.EMOTION -> if (isDarkTheme) DirectionEmotionColorDark else DirectionEmotionColorLight
        DirectionTagCategory.STYLE -> if (isDarkTheme) DirectionStyleColorDark else DirectionStyleColorLight
        DirectionTagCategory.PROSODY -> if (isDarkTheme) DirectionProsodyColorDark else DirectionProsodyColorLight
        DirectionTagCategory.SFX -> if (isDarkTheme) DirectionSfxColorDark else DirectionSfxColorLight
        DirectionTagCategory.OTHER -> otherColor
    }

// Both long-press menu leading-icon composables below sit in a box of this same size, centered,
// so a colored-swatch row (speaker/description) and a caret-icon row (delivery tags) line up
// identically regardless of which one a given entry uses - matches DropdownMenuItem's own
// default leading-icon slot width.
private val AnnotationMenuIconSize = 24.dp

/** A small colored square identifying which annotation color a long-press menu entry refers to
 *  (speaker/description) - the menu's own analogue of the inline underline it's describing. */
@Composable
internal fun AnnotationSwatch(color: Color) {
    Box(modifier = Modifier.size(AnnotationMenuIconSize), contentAlignment = Alignment.Center) {
        Box(
            modifier = Modifier
                .size(16.dp)
                .clip(RoundedCornerShape(3.dp))
                .background(color),
        )
    }
}

/** The direction-tag counterpart to [AnnotationSwatch] - same footprint, an up-arrow caret icon
 *  (mirroring frontend's RiArrowDropUpLine) tinted per tag category instead of a plain swatch. */
@Composable
internal fun AnnotationCaretIcon(color: Color) {
    Box(modifier = Modifier.size(AnnotationMenuIconSize), contentAlignment = Alignment.Center) {
        Icon(Icons.Filled.KeyboardArrowUp, contentDescription = null, tint = color)
    }
}

/** The pronunciation-mark counterpart to [AnnotationCaretIcon] - same footprint, a strikethrough
 *  glyph matching the inline mark's own strikeout shape (see drawPronunciationStrike). */
@Composable
internal fun PronunciationStrikeIcon(color: Color) {
    Box(modifier = Modifier.size(AnnotationMenuIconSize), contentAlignment = Alignment.Center) {
        Icon(Icons.Filled.FormatStrikethrough, contentDescription = null, tint = color)
    }
}

// Which narration role a paragraph segment plays in annotations mode -
// mirrors frontend's ReaderPage.tsx annotationKind exactly, computed client-side from
// isQuote rather than sent by the backend as its own field. isQuote (not
// speaker being set) is what marks a segment as dialogue - an unattributed quote still has
// speaker == "" but is dialogue all the same.
internal enum class AnnotationKind { NARRATOR, SPEAKER }

// A scare-quoted segment is structurally still IsQuote, but isn't actually spoken aloud (see
// backend store.Paragraph.ScareQuote) - it colors as plain narrator text, same as frontend's own
// annotationKind (utils/annotations.ts). The dashed scare-quote underline (see
// drawScareQuoteUnderline) is what still marks it as a flagged quote, layered on top of that
// narrator color rather than replacing it with the speaker one.
internal fun annotationKind(p: ParagraphDto): AnnotationKind = when {
    p.isQuote && !p.scareQuote -> AnnotationKind.SPEAKER
    else -> AnnotationKind.NARRATOR
}
