package narration

import (
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicefx"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// A character whose race is a kind the facts give a voice effect speaks
// with it, in whatever voice reads them; everyone else, and the narrator,
// has none - and the effect is no part of the voice's id.
func TestCharacterEffectFromTheirKindsFact(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, store.CharacterVoiceModeAssigned, false, false); err != nil {
		t.Fatal(err)
	}
	book, _ = s.GetBook(book.ID)
	scope := store.SeriesScope(book)

	pawn, _, _ := s.UpsertCharacter(scope, "Pawn", false)
	erin, _, _ := s.UpsertCharacter(scope, "Erin", false)
	bird, _, _ := s.UpsertCharacter(scope, "Bird", false) // no voice of his own
	for _, c := range []store.Character{pawn, erin} {
		preset, err := s.CreateVoicePreset(c.Name, "speak", "ref", 1, 1.0, voices.DefaultDesignModel)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetCharacterVoice(c.ID, voices.DefaultCloneModel, preset.ID); err != nil {
			t.Fatal(err)
		}
	}
	for id, race := range map[string]string{pawn.ID: "Antinium", bird.ID: "antinium", erin.ID: "Human"} {
		if err := s.SetCharacterNoteValue(id, store.NoteKind, race); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UpsertWorldFact(scope, store.WorldFact{Key: "antinium", Fact: "Insect people.", Class: store.WordRace, Effect: voicefx.Insectoid}); err != nil {
		t.Fatal(err)
	}

	r := NewResolver(s)
	narrator, err := r.ForParagraph(book, "", 0, NoChapter)
	if err != nil {
		t.Fatal(err)
	}
	for speaker, want := range map[string]voicefx.VoiceEffect{"Pawn": voicefx.Insectoid, "Bird": voicefx.Insectoid, "Erin": "", "": ""} {
		v, err := r.ForParagraph(book, speaker, 0, NoChapter)
		if err != nil {
			t.Fatal(err)
		}
		if v.Effect != want {
			t.Errorf("%q: effect %q, want %q", speaker, v.Effect, want)
		}
	}
	// A voice's reference clip is played with its character's effect; a
	// derived preset (a stage's, a numbered sibling's) is its base's.
	pawnPreset, _ := s.CharacterVoiceForModel(pawn.ID, voices.DefaultCloneModel)
	erinPreset, _ := s.CharacterVoiceForModel(erin.ID, voices.DefaultCloneModel)
	for preset, want := range map[string]voicefx.VoiceEffect{pawnPreset: voicefx.Insectoid, pawnPreset + "~2-abcd": voicefx.Insectoid, erinPreset: "", "nobody": ""} {
		if got, err := r.PresetEffect(preset); err != nil || got != want {
			t.Errorf("PresetEffect(%q) = %q, %v, want %q", preset, got, err, want)
		}
	}
	bv, err := r.BookVoices(book)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := bv.ForSpeaker("Bird", 0, 0); v.Effect != voicefx.Insectoid || v.VoiceID() != narrator.VoiceID() {
		t.Errorf("Bird reads in %q with effect %q, want the narrator's voice (%q) with the effect", v.VoiceID(), v.Effect, narrator.VoiceID())
	}
}
