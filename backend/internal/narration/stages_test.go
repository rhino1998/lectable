package narration

import (
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// seriesBook creates a three-chapter book in series at seriesIndex, with
// character voices on.
func seriesBook(t *testing.T, s *store.SQLiteStore, series string, seriesIndex float64) (*store.Book, []string) {
	t.Helper()
	var chapters []store.ChapterInput
	for _, title := range []string{"One", "Two", "Three"} {
		chapters = append(chapters, store.ChapterInput{Title: title, Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "hello"}}})
	}
	id, _, err := s.CreateBook("Book", "Author", "en", "", series, seriesIndex, chapters)
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	book, err := s.GetBook(id)
	if err != nil || book == nil {
		t.Fatalf("GetBook: %v", err)
	}
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, store.CharacterVoiceModeAssigned, false, false); err != nil {
		t.Fatalf("UpdateVoice: %v", err)
	}
	if book, err = s.GetBook(id); err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	ids := make([]string, 3)
	for i := range ids {
		ch, err := s.GetChapterByIdx(id, i)
		if err != nil || ch == nil {
			t.Fatalf("GetChapterByIdx(%d): %v", i, err)
		}
		ids[i] = ch.ID
	}
	return book, ids
}

func voicedCharacter(t *testing.T, s *store.SQLiteStore, book *store.Book, name string, isRole bool) store.Character {
	t.Helper()
	char, _, err := s.UpsertCharacter(store.SeriesScope(book), name, isRole)
	if err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}
	preset, err := s.CreateVoicePreset(name, "Speak as a teenage boy.", "ref", 7, 1.0, voices.DefaultDesignModel)
	if err != nil {
		t.Fatalf("CreateVoicePreset: %v", err)
	}
	if err := s.SetCharacterVoice(char.ID, voices.DefaultCloneModel, preset.ID); err != nil {
		t.Fatalf("SetCharacterVoice: %v", err)
	}
	return char
}

func TestStageVoiceFromItsChapterOn(t *testing.T) {
	s := openTestStore(t)
	book, chapters := seriesBook(t, s, "", 0)
	will := voicedCharacter(t, s, book, "Will", false)
	if _, err := s.SetCharacterStage(will.ID, book.ID, chapters[1], 20, "Speak as a young man.", ""); err != nil {
		t.Fatalf("SetCharacterStage: %v", err)
	}

	r := NewResolver(s)
	voiceAt := func(idx int) ResolvedVoice {
		t.Helper()
		v, err := r.ForParagraph(book, "Will", 0, idx)
		if err != nil {
			t.Fatalf("ForParagraph(%d): %v", idx, err)
		}
		return v
	}
	own, before, at, after := voiceAt(NoChapter), voiceAt(0), voiceAt(1), voiceAt(2)
	if before.VoiceID() != own.VoiceID() {
		t.Errorf("before the stage: got %+v, want his own voice %+v", before, own)
	}
	if at.VoiceID() == own.VoiceID() || at.VoiceID() != after.VoiceID() {
		t.Errorf("from the stage on: got %q then %q, want one voice other than his own %q", at.VoiceID(), after.VoiceID(), own.VoiceID())
	}
	if at.Instruct != "Speak as a young man." || at.Seed != own.Seed || at.RefText != own.RefText {
		t.Errorf("stage voice = %+v, want the stage's instruct under his own seed and reference line", at)
	}
	if !strings.HasPrefix(at.PresetID, own.PresetID+"~a20-") {
		t.Errorf("stage preset id = %q, want one derived from %q", at.PresetID, own.PresetID)
	}

	// The bulk resolver agrees.
	bv, err := r.BookVoices(book)
	if err != nil {
		t.Fatalf("BookVoices: %v", err)
	}
	for idx, want := range []ResolvedVoice{before, at, after} {
		got, err := bv.ForSpeaker("Will", 0, idx)
		if err != nil {
			t.Fatalf("ForSpeaker(%d): %v", idx, err)
		}
		if got.VoiceID() != want.VoiceID() {
			t.Errorf("ForSpeaker chapter %d = %q, want %q", idx, got.VoiceID(), want.VoiceID())
		}
	}

	// Editing the stage's casting gives it another voice.
	if _, err := s.SetCharacterStage(will.ID, book.ID, chapters[1], 20, "Speak as a man of twenty.", ""); err != nil {
		t.Fatalf("SetCharacterStage: %v", err)
	}
	if edited := voiceAt(1); edited.PresetID == at.PresetID {
		t.Errorf("edited stage kept preset id %q", edited.PresetID)
	}
}

func TestStageCarriesIntoLaterBooksOfTheSeries(t *testing.T) {
	s := openTestStore(t)
	first, chapters := seriesBook(t, s, "Saga", 1)
	second, _ := seriesBook(t, s, "Saga", 2)
	will := voicedCharacter(t, s, first, "Will", false)
	if _, err := s.SetCharacterStage(will.ID, first.ID, chapters[2], 20, "Speak as a young man.", ""); err != nil {
		t.Fatalf("SetCharacterStage: %v", err)
	}
	r := NewResolver(s)
	inFirst, err := r.ForParagraph(first, "Will", 0, 2)
	if err != nil {
		t.Fatalf("ForParagraph: %v", err)
	}
	inSecond, err := r.ForParagraph(second, "Will", 0, 0)
	if err != nil {
		t.Fatalf("ForParagraph: %v", err)
	}
	if inSecond.VoiceID() != inFirst.VoiceID() {
		t.Errorf("book two's first chapter = %q, want the stage voice %q", inSecond.VoiceID(), inFirst.VoiceID())
	}

	// A stage set in book two leaves book one alone.
	secondCh, err := s.GetChapterByIdx(second.ID, 1)
	if err != nil || secondCh == nil {
		t.Fatalf("GetChapterByIdx: %v", err)
	}
	if _, err := s.SetCharacterStage(will.ID, second.ID, secondCh.ID, 40, "Speak as a man of forty.", ""); err != nil {
		t.Fatalf("SetCharacterStage: %v", err)
	}
	stages, err := r.BookStages(first)
	if err != nil {
		t.Fatalf("BookStages: %v", err)
	}
	if got := stages.At(will.ID, 2); got == nil || got.Age != 20 {
		t.Errorf("book one, last chapter: stage %+v, want the one at 20", got)
	}
	stages, err = r.BookStages(second)
	if err != nil {
		t.Fatalf("BookStages: %v", err)
	}
	if got := stages.At(will.ID, 0); got == nil || got.Age != 20 {
		t.Errorf("book two, first chapter: stage %+v, want the one at 20", got)
	}
	if got := stages.At(will.ID, 1); got == nil || got.Age != 40 {
		t.Errorf("book two, second chapter: stage %+v, want the one at 40", got)
	}
}

func TestStageVoiceLeavesRolesAndUnvoicedCharactersAlone(t *testing.T) {
	s := openTestStore(t)
	book, chapters := seriesBook(t, s, "", 0)
	guard := voicedCharacter(t, s, book, "Guard", true)
	unvoiced, _, err := s.UpsertCharacter(store.SeriesScope(book), "Mara", false)
	if err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}
	for _, id := range []string{guard.ID, unvoiced.ID} {
		if _, err := s.SetCharacterStage(id, book.ID, chapters[0], 60, "Speak as an elderly man.", ""); err != nil {
			t.Fatalf("SetCharacterStage: %v", err)
		}
	}
	r := NewResolver(s)
	for _, name := range []string{"Guard", "Mara"} {
		own, err := r.ForParagraph(book, name, 0, NoChapter)
		if err != nil {
			t.Fatalf("ForParagraph: %v", err)
		}
		staged, err := r.ForParagraph(book, name, 0, 1)
		if err != nil {
			t.Fatalf("ForParagraph: %v", err)
		}
		if staged.VoiceID() != own.VoiceID() {
			t.Errorf("%s: a stage changed the voice to %+v", name, staged)
		}
	}
}
