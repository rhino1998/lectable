package com.lectable.app.ui.reader

import com.lectable.app.data.remote.dto.WordTimingDto

data class WordToken(val word: String, val start: Int, val end: Int)

fun tokenizeWords(text: String): List<WordToken> {
    val tokens = mutableListOf<WordToken>()
    val regex = Regex("\\S+")
    for (match in regex.findAll(text)) {
        tokens.add(WordToken(match.value, match.range.first, match.range.last + 1))
    }
    return tokens
}

/**
 * Mirrors frontend/src/components/ParagraphText.tsx's wordStartTimes: real per-word start
 * times (seconds) from tts-service's forced alignment (see tts-service/alignment.py,
 * ttsclient.Align) when available and matching 1:1 with our own tokenization, falling back to
 * the old proportional character-position estimate otherwise. The two never mix mid-paragraph -
 * it's real timings for every word or the estimate for every word - since a partial match would
 * imply the word lists disagree on segmentation (contractions, hyphenation, etc.) and mixing
 * would misalign the rest of the paragraph.
 *
 * Every returned value is an absolute position within this paragraph's own audioUrl, matching
 * the raw (unrebased) playback seconds it's compared/seeked against elsewhere (see
 * ParagraphPlayer.playParagraph's own seek-target doc comment) - for an ordinary paragraph
 * (pointerOffsetSeconds 0) that's already 0-based, needing no adjustment; for a scare-quote merge
 * group's own non-anchor member, real alignment timings already come back absolute this way from
 * the backend (see jobs.Manager.handleMergedResult, which aligns the whole shared clip at once),
 * so only the character-position estimate fallback needs pointerOffsetSeconds added by hand here
 * to match.
 */
fun wordStartTimes(
    tokens: List<WordToken>,
    words: List<WordTimingDto>,
    textLength: Int,
    durationSeconds: Double,
    pointerOffsetSeconds: Double = 0.0,
): List<Double> {
    if (words.isNotEmpty() && words.size == tokens.size) {
        return words.map { it.start }
    }
    if (textLength == 0 || durationSeconds <= 0) return tokens.map { pointerOffsetSeconds }
    return tokens.map { pointerOffsetSeconds + (it.start.toDouble() / textLength) * durationSeconds }
}

// Highlighting a word exactly at its aligned/estimated start time reads as slightly late to the
// ear - the eye needs some lead-in to catch up with speech. Nudging the comparison point ahead
// by this much makes the next word light up ~100ms before its timing would strictly call for.
private const val HighlightLeadSeconds = 0.1

fun activeWordIndex(startTimes: List<Double>, currentSeconds: Double): Int {
    val target = currentSeconds + HighlightLeadSeconds
    var idx = -1
    for (i in startTimes.indices) {
        if (startTimes[i] <= target) idx = i else break
    }
    return idx
}
