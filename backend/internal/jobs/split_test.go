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
