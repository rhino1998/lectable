package jobs

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/ttsworker/ttsworkertest"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// syntheticRefWav builds a minimal silent WAV of exactly dur seconds.
func syntheticRefWav(t *testing.T, seconds float64) []byte {
	t.Helper()
	const sampleRate = 16000
	n := int(seconds * sampleRate)
	data, err := wav.Encode(make([]float32, n), sampleRate, 1)
	if err != nil {
		t.Fatalf("wav.Encode: %v", err)
	}
	return data
}

func wordsText(n int) string {
	words := make([]string, n)
	for i := range words {
		words[i] = "word"
	}
	return strings.Join(words, " ")
}

// sentencesText builds nSentences sentences of wordsPerSentence words each
// (uniform length, so bisectSentencesFrac's character-count-based split
// lands at a predictable sentence boundary) - generateCloneSplit's own
// bisection needs at least two real sentences to have anything to split.
func sentencesText(nSentences, wordsPerSentence int) string {
	sentences := make([]string, nSentences)
	for i := range sentences {
		sentences[i] = wordsText(wordsPerSentence) + "."
	}
	return strings.Join(sentences, " ")
}

const maxTokensOverflowMsg = "runtime error: Higgs TTS generation reached max_tokens (8192) before EOC for this text chunk; raise it with --max-tokens on the CLI"

// TestGenerateCloneSplitsOnTokenOverflow is the regression test for the
// real incident this mechanism exists to fix: a paragraph whose full text
// runs out of the worker's own flat max_tokens budget must be bisected at
// a sentence boundary and each half generated (and concatenated)
// independently, rather than surfacing the error to the reader.
func TestGenerateCloneSplitsOnTokenOverflow(t *testing.T) {
	fake := ttsworkertest.New(t)
	var seenTexts []string
	fake.OnGenerate = func(req ttsproto.GenerateRequest) ([]byte, error) {
		seenTexts = append(seenTexts, req.Text)
		if len(strings.Fields(req.Text)) > 40 {
			return nil, errors.New(maxTokensOverflowMsg)
		}
		return syntheticRefWav(t, 2), nil
	}
	mgr := newTestManager()
	mgr.tts = fake.Manager()

	// 6 sentences of 10 words each (60 words total) - fails whole, splits
	// into two 3-sentence/30-word halves, each under the fake's own
	// 40-word threshold.
	text := sentencesText(6, 10)
	refAudio := syntheticRefWav(t, 8)
	refText := wordsText(20)

	audio, err := mgr.generateClone(t.Context(), "audiocpp-higgs-4b", refAudio, refText, "en", text, "")
	if err != nil {
		t.Fatalf("generateClone: %v", err)
	}
	if len(seenTexts) != 3 {
		t.Fatalf("expected exactly 3 generate calls (the failed whole text, then each half), got %d: %v", len(seenTexts), seenTexts)
	}
	if got := len(strings.Fields(seenTexts[0])); got != 60 {
		t.Fatalf("expected the first attempt to use the whole 60-word text, got %d words", got)
	}
	for _, half := range seenTexts[1:] {
		if got := len(strings.Fields(half)); got != 30 {
			t.Fatalf("expected each half to be 30 words, got %d in %q", got, half)
		}
	}

	// The two successful halves (2s of audio each) must be concatenated,
	// not just one of them returned.
	dur, err := wav.Duration(audio)
	if err != nil {
		t.Fatalf("wav.Duration: %v", err)
	}
	if dur < 3900*time.Millisecond || dur > 4100*time.Millisecond {
		t.Fatalf("expected ~4s of concatenated audio (two 2s halves), got %v", dur)
	}
}

// TestGenerateCloneSplitsRecursivelyWhenOneHalfStillOverflows confirms the
// bisection recurses independently per half (not just a single one-shot
// split): only the second half here is still too long, and it must itself
// split again rather than surfacing the error for the whole paragraph.
func TestGenerateCloneSplitsRecursivelyWhenOneHalfStillOverflows(t *testing.T) {
	fake := ttsworkertest.New(t)
	fake.OnGenerate = func(req ttsproto.GenerateRequest) ([]byte, error) {
		if len(strings.Fields(req.Text)) > 25 {
			return nil, errors.New(maxTokensOverflowMsg)
		}
		return syntheticRefWav(t, 1), nil
	}
	mgr := newTestManager()
	mgr.tts = fake.Manager()

	text := sentencesText(6, 10) // 60 words; halves are 30 words, still over 25
	audio, err := mgr.generateClone(t.Context(), "audiocpp-higgs-4b", syntheticRefWav(t, 8), wordsText(20), "en", text, "")
	if err != nil {
		t.Fatalf("generateClone: %v", err)
	}
	if len(audio) == 0 {
		t.Fatalf("expected non-empty concatenated audio")
	}
}

// TestGenerateCloneGivesUpWhenSplitBudgetExhausted confirms
// maxGenerationSplitDepth actually bounds recursion: a text that keeps
// overflowing no matter how far it's split must eventually surface the
// original error rather than splitting forever.
func TestGenerateCloneGivesUpWhenSplitBudgetExhausted(t *testing.T) {
	fake := ttsworkertest.New(t)
	calls := 0
	fake.OnGenerate = func(ttsproto.GenerateRequest) ([]byte, error) {
		calls++
		return nil, errors.New(maxTokensOverflowMsg)
	}
	mgr := newTestManager()
	mgr.tts = fake.Manager()

	// Plenty of sentences to split maxGenerationSplitDepth times over.
	text := sentencesText(32, 10)
	_, err := mgr.generateClone(t.Context(), "audiocpp-higgs-4b", syntheticRefWav(t, 8), wordsText(20), "en", text, "")
	if err == nil {
		t.Fatalf("expected an error once the split budget is exhausted")
	}
	if !isMaxTokensExceeded(err) {
		t.Fatalf("expected the surfaced error to still be the max_tokens-exceeded one, got %v", err)
	}
	if calls < 2 {
		t.Fatalf("expected at least one split attempt before giving up, got %d calls", calls)
	}
}

// TestGenerateCloneNeverSplitsAnUnsplittableSingleSentence confirms a
// single-sentence overflow (bisectForGeneration has nothing to split)
// surfaces the error immediately rather than looping.
func TestGenerateCloneNeverSplitsAnUnsplittableSingleSentence(t *testing.T) {
	fake := ttsworkertest.New(t)
	calls := 0
	fake.OnGenerate = func(ttsproto.GenerateRequest) ([]byte, error) {
		calls++
		return nil, errors.New(maxTokensOverflowMsg)
	}
	mgr := newTestManager()
	mgr.tts = fake.Manager()

	text := wordsText(60) // no sentence-ending punctuation at all - one sentence
	_, err := mgr.generateClone(t.Context(), "audiocpp-higgs-4b", syntheticRefWav(t, 8), wordsText(20), "en", text, "")
	if err == nil {
		t.Fatalf("expected an error for an unsplittable single-sentence overflow")
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 call (no split possible), got %d", calls)
	}
}

// fixedPaceAlign makes the fake aligner place every word at a fixed
// 0.2s/word pace regardless of the clip - the real aligner's own behavior
// when words are missing from the audio: they land past the clip's end.
func fixedPaceAlign(fake *ttsworkertest.Server) {
	fake.OnAlign = func(req ttsproto.AlignRequest) ([]ttsproto.Word, error) {
		fields := strings.Fields(req.Text)
		words := make([]ttsproto.Word, len(fields))
		for i, f := range fields {
			words[i] = ttsproto.Word{Text: f, Start: float64(i) * 0.2, End: float64(i+1) * 0.2}
		}
		return words, nil
	}
}

// TestGenerateCloneCheckedRetriesTruncatedAudioWithSmallerChunks is the
// regression test for Higgs's early-EOC failure mode: a clip whose
// alignment puts words past its end must be regenerated with a
// text_chunk_size override, keeping the complete retry and its own words.
func TestGenerateCloneCheckedRetriesTruncatedAudioWithSmallerChunks(t *testing.T) {
	fake := ttsworkertest.New(t)
	fixedPaceAlign(fake)
	var chunkSizes []int
	fake.OnGenerate = func(req ttsproto.GenerateRequest) ([]byte, error) {
		chunkSizes = append(chunkSizes, req.TextChunkSize)
		if req.TextChunkSize == 0 {
			return syntheticRefWav(t, 2), nil // 10 of 20 words' worth
		}
		return syntheticRefWav(t, 4), nil
	}
	mgr := newTestManager()
	mgr.tts = fake.Manager()

	text := sentencesText(4, 5) // 20 words
	audio, words, err := mgr.generateCloneChecked(t.Context(), "audiocpp-higgs-4b", syntheticRefWav(t, 8), wordsText(20), "en", text, "", text)
	if err != nil {
		t.Fatalf("generateCloneChecked: %v", err)
	}
	if len(chunkSizes) != 2 || chunkSizes[0] != 0 || chunkSizes[1] != retryChunkSizes(text)[0] {
		t.Fatalf("expected a default attempt then one retry at %d, got %v", retryChunkSizes(text)[0], chunkSizes)
	}
	if dur, _ := wav.Duration(audio); dur < 3900*time.Millisecond {
		t.Fatalf("expected the complete 4s retry to be kept, got %v", dur)
	}
	if len(words) != 20 || missingWordCount(words, 4) != 0 {
		t.Fatalf("expected the retry's own 20 in-bounds words, got %d", len(words))
	}
}

// TestGenerateCloneCheckedSkipsRetryForCompleteAudio confirms the common
// case costs exactly one generation.
func TestGenerateCloneCheckedSkipsRetryForCompleteAudio(t *testing.T) {
	fake := ttsworkertest.New(t)
	fixedPaceAlign(fake)
	fake.OnGenerate = func(ttsproto.GenerateRequest) ([]byte, error) {
		return syntheticRefWav(t, 4), nil
	}
	mgr := newTestManager()
	mgr.tts = fake.Manager()

	text := sentencesText(4, 5)
	_, words, err := mgr.generateCloneChecked(t.Context(), "audiocpp-higgs-4b", syntheticRefWav(t, 8), wordsText(20), "en", text, "", text)
	if err != nil {
		t.Fatalf("generateCloneChecked: %v", err)
	}
	if n := len(fake.GenerateCalls()); n != 1 {
		t.Fatalf("expected exactly 1 generate call, got %d", n)
	}
	if len(words) != 20 {
		t.Fatalf("expected the check's own 20 words returned, got %d", len(words))
	}
}

// TestGenerateCloneCheckedRetriesExtraWords covers the transcription half
// of the check: a clip that transcribes to the text plus a run of extra
// words (a repeat, a leaked reference line) must be regenerated even
// though alignment finds nothing missing, keeping the clean retry.
func TestGenerateCloneCheckedRetriesExtraWords(t *testing.T) {
	fake := ttsworkertest.New(t)
	fixedPaceAlign(fake)
	text := sentencesText(4, 5) // 20 words
	fake.OnGenerate = func(req ttsproto.GenerateRequest) ([]byte, error) {
		if req.TextChunkSize == 0 {
			return syntheticRefWav(t, 6), nil
		}
		return syntheticRefWav(t, 4), nil
	}
	fake.OnTranscribe = func(ttsproto.TranscribeRequest) (string, error) {
		if len(fake.TranscribeCalls()) == 1 {
			return text + " and then some words nobody wrote", nil
		}
		return text, nil
	}
	mgr := newTestManager()
	mgr.tts = fake.Manager()

	audio, _, err := mgr.generateCloneChecked(t.Context(), "audiocpp-higgs-4b", syntheticRefWav(t, 8), wordsText(20), "en", text, "", text)
	if err != nil {
		t.Fatalf("generateCloneChecked: %v", err)
	}
	if n := len(fake.GenerateCalls()); n != 2 {
		t.Fatalf("expected the default attempt plus one retry, got %d calls", n)
	}
	if dur, _ := wav.Duration(audio); dur > 4100*time.Millisecond {
		t.Fatalf("expected the clean 4s retry kept, got %v", dur)
	}
}

// TestGenerateCloneCheckedIgnoresTranscriptionFailure confirms a failed
// transcription only skips the extra-words check - no retry, no error.
func TestGenerateCloneCheckedIgnoresTranscriptionFailure(t *testing.T) {
	fake := ttsworkertest.New(t)
	fixedPaceAlign(fake)
	fake.OnGenerate = func(ttsproto.GenerateRequest) ([]byte, error) {
		return syntheticRefWav(t, 4), nil
	}
	fake.OnTranscribe = func(ttsproto.TranscribeRequest) (string, error) {
		return "", errors.New("transcriber unavailable")
	}
	mgr := newTestManager()
	mgr.tts = fake.Manager()

	text := sentencesText(4, 5)
	_, words, err := mgr.generateCloneChecked(t.Context(), "audiocpp-higgs-4b", syntheticRefWav(t, 8), wordsText(20), "en", text, "", text)
	if err != nil {
		t.Fatalf("generateCloneChecked: %v", err)
	}
	if n := len(fake.GenerateCalls()); n != 1 {
		t.Fatalf("expected exactly 1 generate call, got %d", n)
	}
	if len(words) != 20 {
		t.Fatalf("expected the check's own 20 words returned, got %d", len(words))
	}
}

// TestGenerateCloneCheckedKeepsMostCompleteAttempt confirms that when no
// retry is fully complete, the attempt missing the fewest words wins -
// never an error.
func TestGenerateCloneCheckedKeepsMostCompleteAttempt(t *testing.T) {
	fake := ttsworkertest.New(t)
	fixedPaceAlign(fake)
	seconds := map[int]float64{} // chunk size -> clip length
	sizes := retryChunkSizes(sentencesText(20, 5))
	seconds[0] = 5
	seconds[sizes[0]] = 15 // best: 25 of 100 words missing
	seconds[sizes[1]] = 10
	fake.OnGenerate = func(req ttsproto.GenerateRequest) ([]byte, error) {
		return syntheticRefWav(t, seconds[req.TextChunkSize]), nil
	}
	mgr := newTestManager()
	mgr.tts = fake.Manager()

	text := sentencesText(20, 5) // 100 words, 20s at the fake aligner's pace
	audio, _, err := mgr.generateCloneChecked(t.Context(), "audiocpp-higgs-4b", syntheticRefWav(t, 8), wordsText(20), "en", text, "", text)
	if err != nil {
		t.Fatalf("generateCloneChecked: %v", err)
	}
	if n := len(fake.GenerateCalls()); n != 3 {
		t.Fatalf("expected the default attempt plus both retries, got %d calls", n)
	}
	if dur, _ := wav.Duration(audio); dur < 14900*time.Millisecond || dur > 15100*time.Millisecond {
		t.Fatalf("expected the 15s attempt kept, got %v", dur)
	}
}

func TestMissingWordCount(t *testing.T) {
	words := []ttsproto.Word{{End: 1}, {End: 2}, {End: 2.05}, {End: 2.5}, {End: 3}}
	if got := missingWordCount(words, 2); got != 2 {
		t.Fatalf("expected 2 words past the end (2.05 is within tolerance), got %d", got)
	}
}

func TestRetryChunkSizes(t *testing.T) {
	if got := retryChunkSizes(strings.Repeat("a", 600)); len(got) != 2 || got[0] != 300 || got[1] != 150 {
		t.Fatalf("600 chars: expected [300 150], got %v", got)
	}
	if got := retryChunkSizes(strings.Repeat("a", 100)); len(got) != 1 || got[0] != minRetryChunkChars {
		t.Fatalf("100 chars: expected a single retry at the floor, got %v", got)
	}
}
