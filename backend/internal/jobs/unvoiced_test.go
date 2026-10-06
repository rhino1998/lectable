package jobs

import (
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

func TestUnvoicedTaskRendersSilenceWithoutTTS(t *testing.T) {
	// A zero Manager has no worker, store or data dir: generate must not
	// reach for any of them.
	m := &Manager{}
	ut := &task{kind: KindVoiceClone, presetID: "preset", paragraph: store.Paragraph{Text: "“…”", IsQuote: true}}
	audio, words, err := m.generate(t.Context(), ut)
	if err != nil {
		t.Fatal(err)
	}
	if words == nil || len(words) != 0 {
		t.Fatalf("words = %v, want empty and non-nil (nothing to align)", words)
	}
	dur, err := wav.Duration(audio.WAV)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Duration(unvoicedSeconds * float64(time.Second)); dur != want {
		t.Fatalf("duration = %v, want %v", dur, want)
	}
	clip, err := wav.Decode(audio.WAV)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range clip.Samples {
		if s != 0 {
			t.Fatal("clip isn't silent")
		}
	}
	if needs := m.needs(ut); needs != nil {
		t.Fatalf("needs = %v, want none", needs)
	}

	if unvoicedTask(&task{kind: KindVoiceClone, paragraph: store.Paragraph{Text: "“Well…”"}}) {
		t.Fatal("a spoken line was taken for unvoiced")
	}
	group := []store.Paragraph{{Text: "…"}, {Text: "so-called"}}
	if unvoicedTask(&task{kind: KindVoiceClone, paragraph: group[0], mergeParagraphs: group}) {
		t.Fatal("a merge group was taken for unvoiced")
	}
}
