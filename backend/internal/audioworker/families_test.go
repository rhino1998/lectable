package audioworker

import (
	"math"
	"strings"
	"testing"
)

func TestFireRedTTS3Language(t *testing.T) {
	for _, tc := range []struct{ lang, text, want string }{
		{"Auto", "Hello there.", "English"},
		{"Auto", "今天天气很好。", "Chinese"},
		{"", "Hello there.", "English"},
		{"english", "Hello there.", "English"},
		{"japanese", "こんにちは", "Japanese"},
	} {
		if got := fireRedTTS3Language(tc.lang, tc.text); got != tc.want {
			t.Errorf("fireRedTTS3Language(%q, %q) = %q, want %q", tc.lang, tc.text, got, tc.want)
		}
	}
}

func TestFireRedAudioLanguage(t *testing.T) {
	for _, tc := range []struct{ lang, text, want string }{
		{"Auto", "Hello there.", "en"},
		{"Auto", "今天天气很好。", "zh"},
		{"english", "Hello there.", "en"},
		{"french", "Bonjour.", "en"},
		{"chinese", "Hello", "zh"},
		{"japanese", "Hello", "zh"},
	} {
		if got := fireRedAudioLanguage(tc.lang, tc.text); got != tc.want {
			t.Errorf("fireRedAudioLanguage(%q, %q) = %q, want %q", tc.lang, tc.text, got, tc.want)
		}
	}
}

func TestEstimateCloneDuration(t *testing.T) {
	// A 10s mono reference speaking 100 characters: 0.1s per character.
	req := GenerateRequest{
		Text:          "  twenty characters\n\n here ",
		RefSamples:    make([]float32, 24000*10),
		RefSampleRate: 24000,
		RefChannels:   1,
		RefText:       strings.Repeat("a", 100),
	}
	// "twenty characters here" is 22 characters once whitespace collapses.
	if got := estimateCloneDuration(req); math.Abs(got-2.2) > 1e-9 {
		t.Errorf("estimateCloneDuration = %v, want 2.2", got)
	}

	// Stereo halves the frame count for the same number of samples.
	req.RefChannels = 2
	if got := estimateCloneDuration(req); math.Abs(got-1.1) > 1e-9 {
		t.Errorf("stereo estimateCloneDuration = %v, want 1.1", got)
	}

	// No transcript falls back to aukCharsPerSecond, floored at one second.
	req.RefText = ""
	req.Text = "Hi."
	if got := estimateCloneDuration(req); got != 1 {
		t.Errorf("no-transcript estimateCloneDuration = %v, want 1", got)
	}
}

func TestDesignEngineIDsMatchKeys(t *testing.T) {
	for key, e := range designEngines {
		if e.id != key {
			t.Errorf("designEngines[%q].id = %q", key, e.id)
		}
	}
	for id, key := range cloneModelFamilies {
		if _, ok := cloneFamilies[key]; !ok {
			t.Errorf("clone model %q maps to missing family %q", id, key)
		}
	}
}
