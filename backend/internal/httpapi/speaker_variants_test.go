package httpapi

import (
	"os"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
)

func TestNumberedVoices(t *testing.T) {
	s := &Server{DataDir: t.TempDir()}
	voice := narration.ResolvedVoice{PresetID: "guard", Instruct: "a gruff guard", Seed: 7}
	role := store.Character{Name: "Guard", IsRole: true}
	counts := map[int]int{1: 4, 2: 3, 3: 1}

	two := narration.MemberVoice(voice, role, 2)
	if err := audiopath.EnsureVoiceRefDir(s.DataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audiopath.VoicePresetRefFile(s.DataDir, two.PresetID), []byte("wav"), 0o644); err != nil {
		t.Fatal(err)
	}

	emotionCounts := map[int]map[string]int{1: {"angry": 2}, 2: {"angry": 1}}
	got := s.numberedVoices(voice, role, counts, emotionCounts)
	if len(got) != 3 {
		t.Fatalf("got %d variants, want 3: %+v", len(got), got)
	}
	for i, want := range []struct {
		count, angry int
		audio        bool
	}{{4, 2, false}, {3, 1, true}, {1, 0, false}} {
		v := got[i]
		angry := 0
		if len(v.Emotions) == 1 && v.Emotions[0].Emotion == "angry" {
			angry = v.Emotions[0].Count
		}
		if v.Number != i+1 || v.ParagraphCount != want.count || v.NeutralCount != want.count-want.angry || angry != want.angry || (v.RefAudioURL != "") != want.audio {
			t.Errorf("number %d: got %+v, want %+v", i+1, v, want)
		}
	}

	// Not a role: its lines are all the character's own.
	if got := s.numberedVoices(voice, store.Character{Name: "Guard"}, counts, emotionCounts); got != nil {
		t.Errorf("non-role has variants: %+v", got)
	}
}
