package jobs

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/textsplit"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

const (
	// assumedWordsPerMinute is a rough average narration pace, used only
	// to sanity-check generated audio length - not tuned per voice/preset,
	// since it only needs to be in the right ballpark to catch a
	// pathological outlier (see isImplausiblyLong), not to predict actual
	// duration.
	assumedWordsPerMinute = 150
	// minExpectedDuration floors how short a text's own WPM-based estimate
	// can be before the implausible-length check is skipped entirely - a
	// handful of words' estimate is too noisy (a single short exclamation
	// can legitimately take a couple of seconds) to safely gate a retry on.
	minExpectedDuration = 3 * time.Second
	// maxDurationRatio is how many multiples of the expected duration a
	// generation has to exceed before it's treated as implausible.
	maxDurationRatio = 2.5
	// maxDurationRetries caps how many extra times a single generation is
	// retried after an implausibly-long result before giving up and
	// accepting whatever the last attempt produced - guarantees
	// termination even if the worker keeps producing bad audio for this
	// exact input.
	maxDurationRetries = 2

	// maxGenerationSplitDepth bounds how many times generateClone will
	// bisect an overflowing paragraph's text (see isMaxTokensExceeded)
	// before giving up on splitting further and surfacing the underlying
	// error - each split roughly halves the text (and so, roughly, the
	// tokens a single generation call needs), so even this small a bound
	// covers a wide size range: 3 levels quarters the original length
	// down to an eighth.
	maxGenerationSplitDepth = 3

	// alignPastEndTolerance is how far past a clip's own end an aligned
	// word's End may land before it counts as missing from the audio (see
	// missingWordCount) - absorbs the aligner's own frame-rounding slop.
	alignPastEndTolerance = 0.1 // seconds
	// minMissingWords is how many words must align past the clip's end
	// before a generation counts as incomplete. Measured against a real
	// library (3838 ready paragraphs of "Spire's Spite", Higgs): 16
	// truncated paragraphs each had 3-17 words past the end (a whole
	// dropped final sentence or more); the only single-word cases were
	// one-word paragraphs, where the aligner's own minimum word span
	// alone can overrun a sub-second clip.
	minMissingWords = 2
	// minRetryChunkChars floors retryChunkSizes - chunks much shorter than
	// a typical sentence make audio.cpp's chunker cut mid-sentence, at a
	// real prosody cost.
	minRetryChunkChars = 80
)

// expectedDuration estimates how long text should take a narrator to
// speak, assuming assumedWordsPerMinute - a rough heuristic used only to
// catch a generation that's wildly, implausibly longer than it should be
// (see isImplausiblyLong), never as a target actual generations are
// expected to hit closely.
func expectedDuration(text string) time.Duration {
	words := len(strings.Fields(text))
	if words == 0 {
		return 0
	}
	minutes := float64(words) / assumedWordsPerMinute
	return time.Duration(minutes * float64(time.Minute))
}

// isImplausiblyLong reports whether actual is long enough beyond
// expected to suggest a bad generation - a real, observed audio.cpp
// failure mode where generation loops/repeats itself well past where it
// should have stopped, producing much more audio than the input text
// warrants - rather than ordinary pacing variance. Gated by both a ratio
// (maxDurationRatio) and an absolute floor (minExpectedDuration) so a very
// short paragraph, where a WPM-based estimate is least reliable, doesn't
// trigger a spurious retry.
func isImplausiblyLong(actual, expected time.Duration) bool {
	if expected < minExpectedDuration {
		return false
	}
	return actual > time.Duration(float64(expected)*maxDurationRatio)
}

// bisectSentencesFrac splits sentences into two roughly-equal-length (by
// character count) halves at a sentence boundary, rejoined into plain
// text, along with the actual achieved character-count fraction of the
// split (leftLen/totalLen - not necessarily exactly 0.5, since the split
// only happens at a sentence boundary). Used to proportionally slice a
// matching audio clip at roughly the same point in time as the text split
// - see (*Manager).alignWithSplit.
func bisectSentencesFrac(sentences []string) (left, right string, leftFrac float64) {
	lens := make([]int, len(sentences))
	total := 0
	for i, s := range sentences {
		lens[i] = len(s) + 1
		total += lens[i]
	}
	target := total / 2
	cum := 0
	splitAt := 1
	for i, l := range lens {
		cum += l
		if cum >= target {
			splitAt = i + 1
			break
		}
	}
	if splitAt < 1 {
		splitAt = 1
	}
	if splitAt > len(sentences)-1 {
		splitAt = len(sentences) - 1
	}
	leftLen := 0
	for _, l := range lens[:splitAt] {
		leftLen += l
	}
	return strings.Join(sentences[:splitAt], " "), strings.Join(sentences[splitAt:], " "), float64(leftLen) / float64(total)
}

// generateClone calls the worker's /generate once, or up to
// maxDurationRetries+1 times total, discarding and retrying any result
// whose duration is implausibly long relative to text's own word count -
// see isImplausiblyLong. Falls through and returns the last (still
// implausibly long) attempt's audio once the retry budget is exhausted,
// rather than failing the paragraph outright: a too-long clip that still
// contains the right words is a strictly better outcome for a reader than
// an error.
//
// On isMaxTokensExceeded (the worker's own flat max_tokens default -
// deliberately never overridden per call, see internal/audioworker's own
// higgsMaxTokens doc comment for why that was tried and reverted - wasn't
// enough to finish this text), bisects text at a sentence boundary
// (textsplit.SplitSentences/bisectSentencesFrac) and recurses on each
// half independently, concatenating the two resulting clips (wav.Concat)
// - the same reactive text-splitting alignWithSplit already does for the
// forced aligner's own fixed length ceiling, applied here to generation's
// instead. Both halves stay within maxGenerationSplitDepth so this always
// terminates; a paragraph short enough to need no splitting at all (the
// overwhelming common case) never pays for this at all. There's no
// app-side paragraph-length cap any more (see internal/epub - a stored
// paragraph can be arbitrarily long, relying entirely on audio.cpp's own
// internal chunker, plus this reactive fallback, for TTS-side safety) -
// this is the sole recovery mechanism for a chunk that overflows, whether
// from a long paragraph or (confirmed live) a short one at a slow,
// deliberate voice's own pace.
func (m *Manager) generateClone(ctx context.Context, cloneModel string, refAudio []byte, refText, language, text, cloneInstruct string) (ttsworker.Audio, error) {
	return m.generateCloneSplit(ctx, cloneModel, refAudio, refText, language, text, cloneInstruct, 0, maxGenerationSplitDepth)
}

// generateCloneChecked is generateClone plus a completeness check: it
// force-aligns alignText (the plain spoken words - never the tagged
// generation text, see generate's own comment) against the result and
// transcribes it (free ASR, see extraWordsIn), then regenerates with a
// smaller audio.cpp text_chunk_size (retryChunkSizes) if the clip is
// missing words (missingWordCount - a real, observed failure mode where
// Higgs reaches EOC early and silently drops a paragraph's final sentence
// or more) or speaks extra ones (a repeated phrase, a leaked reference
// line, babble past the text's end). Each retry is a fresh sample either
// way, and shorter chunks leave the model less room to stop early or run
// on. Keeps whichever attempt has the fewest missing plus extra words - a
// later attempt can be worse, and a clip with most of its words still
// beats an error - and returns that attempt's own word timings so the
// caller needn't align it again.
//
// words is nil when alignText is "" or alignment itself failed - the
// caller falls back to its ordinary detached alignment then, and the
// audio is returned unchecked rather than failed. A failed transcription
// only skips the extra-words half of the check.
func (m *Manager) generateCloneChecked(ctx context.Context, cloneModel string, refAudio []byte, refText, language, text, cloneInstruct, alignText string) (ttsworker.Audio, []ttsproto.Word, error) {
	audio, err := m.generateClone(ctx, cloneModel, refAudio, refText, language, text, cloneInstruct)
	if err != nil || alignText == "" {
		return audio, nil, err
	}
	words, missing, err := m.alignForCompleteness(ctx, alignText, audio.WAV, language)
	if err != nil {
		log.Printf("jobs: completeness check alignment failed, keeping audio unchecked: %v", err)
		completenessResults.WithLabelValues("unchecked").Inc()
		return audio, nil, nil
	}
	extra := m.extraWordsIn(ctx, audio.WAV, alignText, text)
	retried := false
	for _, chunkSize := range retryChunkSizes(text) {
		if missing < minMissingWords && extra < minExtraWords {
			break
		}
		retried = true
		completenessRetries.WithLabelValues(incompleteReason(missing, extra)).Inc()
		log.Printf(
			"jobs: generated audio is missing %d of %d words (aligned past the clip's end) and speaks %d extra; regenerating with text_chunk_size=%d",
			missing, len(words), extra, chunkSize,
		)
		retryAudio, err := m.generateCloneSplit(ctx, cloneModel, refAudio, refText, language, text, cloneInstruct, chunkSize, maxGenerationSplitDepth)
		if err != nil {
			log.Printf("jobs: text_chunk_size=%d retry failed: %v", chunkSize, err)
			continue
		}
		retryWords, retryMissing, err := m.alignForCompleteness(ctx, alignText, retryAudio.WAV, language)
		if err != nil {
			log.Printf("jobs: text_chunk_size=%d retry alignment failed: %v", chunkSize, err)
			continue
		}
		retryExtra := m.extraWordsIn(ctx, retryAudio.WAV, alignText, text)
		if retryMissing+retryExtra < missing+extra {
			audio, words, missing, extra = retryAudio, retryWords, retryMissing, retryExtra
		}
	}
	switch {
	case missing >= minMissingWords || extra >= minExtraWords:
		log.Printf("jobs: generated audio still missing %d of %d words and speaking %d extra after every chunking retry; keeping the best attempt", missing, len(words), extra)
		completenessResults.WithLabelValues("unfixed").Inc()
	case retried:
		completenessResults.WithLabelValues("fixed").Inc()
	default:
		completenessResults.WithLabelValues("clean").Inc()
	}
	return audio, words, nil
}

// incompleteReason labels why a generation failed its completeness check.
func incompleteReason(missing, extra int) string {
	switch {
	case missing >= minMissingWords && extra >= minExtraWords:
		return "both"
	case missing >= minMissingWords:
		return "missing"
	}
	return "extra"
}

// alignForCompleteness aligns text against audioWav and reports how many
// of its words fell past the clip's own end (missingWordCount).
func (m *Manager) alignForCompleteness(ctx context.Context, text string, audioWav []byte, language string) ([]ttsproto.Word, int, error) {
	dur, err := wav.Duration(audioWav)
	if err != nil {
		return nil, 0, fmt.Errorf("parse wav duration: %w", err)
	}
	words, err := m.alignWithSplit(ctx, text, audioWav, language)
	if err != nil {
		return nil, 0, err
	}
	return words, missingWordCount(words, dur.Seconds()), nil
}

// extraWordsIn transcribes audioWav and returns the longest run of words it
// speaks beyond the input text (extraWordRun), measured against both
// alignText (the plain words) and generationText (with any pronunciation
// respellings applied) and taking the closer match, since the audio may
// follow either. The forced aligner can't answer this itself: it places
// only the words it's given, and confirmed live, it stretches a word over
// any extra speech before or between them rather than leaving a gap.
// Returns 0 when transcription fails - the missing-words half of the
// check still stands on its own.
func (m *Manager) extraWordsIn(ctx context.Context, audioWav []byte, alignText, generationText string) int {
	transcript, err := m.tts.Transcribe(ctx, audioWav)
	if err != nil {
		log.Printf("jobs: completeness check transcription failed, skipping extra-words check: %v", err)
		return 0
	}
	extra := extraWordRun(alignText, transcript)
	if generationText != alignText {
		extra = min(extra, extraWordRun(generationText, transcript))
	}
	return extra
}

// missingWordCount counts aligned words that end past clipSeconds (plus
// alignPastEndTolerance). audio.cpp's forced aligner places every
// transcript word somewhere regardless of whether it was actually spoken,
// and confirmed live, it places words absent from the audio *beyond the
// clip's own end* (one aligner frame apiece) rather than squeezing them
// into what's there - so words past the end are words the generation
// dropped.
func missingWordCount(words []ttsproto.Word, clipSeconds float64) int {
	n := 0
	for _, w := range words {
		if w.End > clipSeconds+alignPastEndTolerance {
			n++
		}
	}
	return n
}

// retryChunkSizes is generateCloneChecked's escalation ladder of
// text_chunk_size overrides (codepoints): half of text's own length, then
// a quarter - each forcing audio.cpp's chunker (which prefers sentence,
// then clause, boundaries within the budget) to render text as more,
// shorter chunks, the shorter the less room each leaves Higgs to stop
// early. Floored at minRetryChunkChars and deduplicated, so a short text
// gets a single retry at the floor (still a fresh sample, even when that's
// no smaller than the family's own default chunk).
func retryChunkSizes(text string) []int {
	n := utf8.RuneCountInString(text)
	var sizes []int
	for _, div := range []int{2, 4} {
		size := max((n+div-1)/div, minRetryChunkChars)
		if len(sizes) == 0 || sizes[len(sizes)-1] != size {
			sizes = append(sizes, size)
		}
	}
	return sizes
}

// generateCloneSplit is generateClone's implementation. textChunkSize > 0
// overrides the clone family's own internal text chunk budget (see
// ttsworker.Manager.GenerateChunked) for this call and every split half
// it recurses into.
func (m *Manager) generateCloneSplit(ctx context.Context, cloneModel string, refAudio []byte, refText, language, text, cloneInstruct string, textChunkSize, splitBudget int) (ttsworker.Audio, error) {
	expected := expectedDuration(text)
	durationAttempt := 0
	for {
		audio, err := m.tts.GenerateChunkedAudio(ctx, text, cloneModel, refAudio, refText, language, cloneInstruct, textChunkSize)
		if err != nil {
			if isMaxTokensExceeded(err) && splitBudget > 0 {
				if leftText, rightText, ok := bisectForGeneration(text); ok {
					log.Printf(
						"jobs: generation ran out of tokens for a %d-word text; splitting at a sentence boundary into %d + %d words",
						len(strings.Fields(text)), len(strings.Fields(leftText)), len(strings.Fields(rightText)),
					)
					leftAudio, err := m.generateCloneSplit(ctx, cloneModel, refAudio, refText, language, leftText, cloneInstruct, textChunkSize, splitBudget-1)
					if err != nil {
						return ttsworker.Audio{}, err
					}
					rightAudio, err := m.generateCloneSplit(ctx, cloneModel, refAudio, refText, language, rightText, cloneInstruct, textChunkSize, splitBudget-1)
					if err != nil {
						return ttsworker.Audio{}, err
					}
					return concatCloneAudio(leftAudio, rightAudio)
				}
			}
			return ttsworker.Audio{}, err
		}
		actual, durErr := wav.Duration(audio.WAV)
		if durErr != nil || !isImplausiblyLong(actual, expected) || durationAttempt >= maxDurationRetries {
			return audio, nil
		}
		durationAttempt++
		log.Printf(
			"jobs: generated audio (%.1fs) implausibly long vs expected ~%.1fs for %d-word text; regenerating (attempt %d/%d)",
			actual.Seconds(), expected.Seconds(), len(strings.Fields(text)), durationAttempt, maxDurationRetries+1,
		)
	}
}

// isMaxTokensExceeded reports whether err is Higgs generation running out
// of its own max_tokens budget before reaching EOC ("...reached max_tokens
// (N) before EOC for this text chunk...", from audio.cpp via
// internal/ttsworker.Manager.Generate) - generateCloneSplit's own signal
// to bisect the text and retry each half rather than surfacing an error
// the reader would otherwise have to notice and manually "regenerate"
// past.
func isMaxTokensExceeded(err error) bool {
	return err != nil && strings.Contains(err.Error(), "before EOC for this text chunk")
}

// bisectForGeneration splits text into two sentence-boundary halves for
// generateCloneSplit's own retry - textsplit.SplitSentences/
// bisectSentencesFrac's shared implementation with alignWithSplit below,
// minus the audio-slicing half that only applies there. ok is false (text
// returned unsplit) for a single-sentence text, which can't be split any
// further - generateCloneSplit's own signal to give up rather than loop
// forever splitting nothing.
func bisectForGeneration(text string) (left, right string, ok bool) {
	sentences := textsplit.SplitSentences(text)
	if len(sentences) < 2 {
		return "", "", false
	}
	left, right, _ = bisectSentencesFrac(sentences)
	return left, right, true
}

// concatCloneAudio joins two independently-generated clone clips into one
// continuous clip, in order - generateCloneSplit's own counterpart to
// alignMergedAudio's approach to a scare-quote merge group's shared clip,
// but here the two halves are genuinely separate recordings (not slices of
// one shared take), stitched back together after being split apart for
// generation rather than alignment.
func concatCloneAudio(left, right ttsworker.Audio) (ttsworker.Audio, error) {
	leftClip, err := wav.Decode(left.WAV)
	if err != nil {
		return ttsworker.Audio{}, fmt.Errorf("decode left half: %w", err)
	}
	rightClip, err := wav.Decode(right.WAV)
	if err != nil {
		return ttsworker.Audio{}, fmt.Errorf("decode right half: %w", err)
	}
	joined, err := wav.Concat(leftClip, rightClip)
	if err != nil {
		return ttsworker.Audio{}, err
	}
	data, err := wav.Encode(joined.Samples, joined.SampleRate, joined.Channels)
	if err != nil {
		return ttsworker.Audio{}, err
	}
	// The halves decode independently and their audio is plainly
	// concatenated, so their latents concatenate as separate chunks. Drop
	// them rather than fail if they can't (say, one half has none).
	out := ttsworker.Audio{WAV: data}
	if left.Latents != nil && right.Latents != nil {
		if joinedLatents, err := latents.Concat(left.Latents, right.Latents); err == nil {
			out.Latents = joinedLatents
		} else {
			log.Printf("jobs: concatenating split halves' latents: %v", err)
		}
	}
	return out, nil
}

// isMaxSourcePositionsExceeded reports whether err is audio.cpp's forced
// aligner hitting its own fixed audio-length ceiling
// ("...exceeds max_source_positions"). This is a positional-embedding
// table size baked into the trained checkpoint - not adjustable via any
// load/session/request option (confirmed by reading audio.cpp's own
// source: qwen3_forced_aligner/session.cpp explicitly refuses to chunk
// internally - "does not support standalone audio chunking; chunk audio
// before ASR so each aligner request receives matching audio and
// transcript" - so the caller has to do exactly that).
func isMaxSourcePositionsExceeded(err error) bool {
	return err != nil && strings.Contains(err.Error(), "exceeds max_source_positions")
}

// alignWithSplit aligns text against audioWav, automatically retrying with
// smaller, sentence-boundary-split (text, audio) pairs merged back into one
// word list (offsetting each half's timestamps by the other's duration) if
// the whole clip exceeds the aligner's fixed max audio length - see
// isMaxSourcePositionsExceeded. The audio is sliced at roughly the same
// point in time as the text split, using the split's actual achieved
// character-count fraction (bisectSentencesFrac) as an estimate of where
// that point falls in the audio - an approximation (speech rate isn't
// perfectly proportional to character count), but only ever invoked as a
// last resort when the whole clip would otherwise get no word timings at
// all, so an occasional slightly-off split-point word is a strictly better
// outcome.
func (m *Manager) alignWithSplit(ctx context.Context, text string, audioWav []byte, language string) ([]ttsproto.Word, error) {
	words, err := m.tts.Align(ctx, text, audioWav, language)
	if err == nil {
		return words, nil
	}
	if !isMaxSourcePositionsExceeded(err) {
		return nil, err
	}
	sentences := textsplit.SplitSentences(text)
	if len(sentences) < 2 {
		return nil, err
	}
	leftText, rightText, leftFrac := bisectSentencesFrac(sentences)

	clip, decodeErr := wav.Decode(audioWav)
	if decodeErr != nil {
		return nil, err
	}
	splitIdx := int(float64(len(clip.Samples)) * leftFrac)
	splitIdx -= splitIdx % clip.Channels // keep interleaved frames intact
	if splitIdx <= 0 || splitIdx >= len(clip.Samples) {
		return nil, err
	}
	leftAudio, encErr := wav.Encode(clip.Samples[:splitIdx], clip.SampleRate, clip.Channels)
	if encErr != nil {
		return nil, err
	}
	rightAudio, encErr := wav.Encode(clip.Samples[splitIdx:], clip.SampleRate, clip.Channels)
	if encErr != nil {
		return nil, err
	}
	leftDuration := float64(splitIdx/clip.Channels) / float64(clip.SampleRate)

	log.Printf(
		"jobs: alignment exceeded max_source_positions for a %.1fs clip; splitting into %d + %d chars at a sentence boundary",
		float64(len(clip.Samples)/clip.Channels)/float64(clip.SampleRate), len(leftText), len(rightText),
	)

	leftWords, err := m.alignWithSplit(ctx, leftText, leftAudio, language)
	if err != nil {
		return nil, err
	}
	rightWords, err := m.alignWithSplit(ctx, rightText, rightAudio, language)
	if err != nil {
		return nil, err
	}
	for i := range rightWords {
		rightWords[i].Start += leftDuration
		rightWords[i].End += leftDuration
	}
	return append(leftWords, rightWords...), nil
}
