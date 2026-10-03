package narration

import (
	"path/filepath"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

func openTestStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "library.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func createBook(t *testing.T, s *store.SQLiteStore) *store.Book {
	t.Helper()
	id, _, err := s.CreateBook("Book", "Author", "en", "", "", 0, []store.ChapterInput{
		{Title: "Ch1", Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "hello"}}},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	book, err := s.GetBook(id)
	if err != nil || book == nil {
		t.Fatalf("GetBook: %v", err)
	}
	return book
}

func TestBookVoiceBuiltInPreset(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	// A freshly created book resolves to the seeded default voice's preset
	// id (voices.DefaultPresetID, per Store.Open's own seeding) - confirm
	// BookVoice fills in RefText/SpeedMultiplier/CloneModel from the
	// built-in voices.PresetsByID table for it, not a store-backed one.
	r := NewResolver(s)
	v, err := r.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	builtin, ok := voices.PresetsByID[book.VoicePresetID]
	if !ok {
		t.Fatalf("expected book's default preset id %q to be a built-in preset", book.VoicePresetID)
	}
	if v.RefText != builtin.RefText || v.SpeedMultiplier != builtin.SpeedMultiplier {
		t.Fatalf("expected built-in preset fields to be resolved, got %+v", v)
	}
	if v.CloneModel != book.CloneModel || v.CloneModel != voices.DefaultCloneModel {
		t.Fatalf("expected the book's own (default) clone model, got %q", v.CloneModel)
	}
}

func TestBookVoiceCustomPreset(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	preset, err := s.CreateVoicePreset("Custom", "speak softly", "ref text", 3, 0.9, "qwen3_tts")
	if err != nil {
		t.Fatalf("CreateVoicePreset: %v", err)
	}
	if err := s.UpdateVoice(book.ID, preset.ID, "speak softly", "English", 3, "audiocpp-qwen3-0.6b", store.CharacterVoiceModeNarrator, false, false); err != nil {
		t.Fatalf("UpdateVoice: %v", err)
	}
	book, err = s.GetBook(book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}

	r := NewResolver(s)
	v, err := r.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	if v.RefText != "ref text" || v.SpeedMultiplier != 0.9 || v.CloneModel != "audiocpp-qwen3-0.6b" || v.DesignModel != "qwen3_tts" {
		t.Fatalf("expected custom preset fields resolved, got %+v", v)
	}
}

func TestBookVoiceFullyCustomInstructHasNoPreset(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	if err := s.UpdateVoice(book.ID, "", "speak like a robot", "English", 0, book.CloneModel, store.CharacterVoiceModeNarrator, false, false); err != nil {
		t.Fatalf("UpdateVoice: %v", err)
	}
	book, err := s.GetBook(book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}

	r := NewResolver(s)
	v, err := r.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	if v.PresetID != "" {
		t.Fatalf("expected no preset id for a fully custom instruct, got %q", v.PresetID)
	}
	if v.Instruct != "speak like a robot" {
		t.Fatalf("expected instruct to carry through, got %q", v.Instruct)
	}
	if EffectiveCloneModel(v) != voices.DefaultCloneModel {
		t.Fatalf("expected EffectiveCloneModel to fall back to the default, got %q", EffectiveCloneModel(v))
	}
}

func TestForParagraphMultiVoiceOffAlwaysUsesBookVoice(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	scope := store.SeriesScope(book)
	char, _, err := s.UpsertCharacter(scope, "Alice", false)
	if err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}
	preset, err := s.CreateVoicePreset("Alice Voice", "speak brightly", "ref", 1, 1.0, voices.DefaultDesignModel)
	if err != nil {
		t.Fatalf("CreateVoicePreset: %v", err)
	}
	if err := s.SetCharacterVoice(char.ID, voices.DefaultCloneModel, preset.ID); err != nil {
		t.Fatalf("SetCharacterVoice: %v", err)
	}

	r := NewResolver(s)
	bookVoice, err := r.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}

	// book.MultiVoice defaults to false - a character's assigned voice must
	// not override the book's own voice until it's explicitly turned on.
	got, err := r.ForParagraph(book, "Alice", 0)
	if err != nil {
		t.Fatalf("ForParagraph: %v", err)
	}
	if got.VoiceID() != bookVoice.VoiceID() {
		t.Fatalf("expected book voice with MultiVoice off, got a different voice: %+v vs %+v", got, bookVoice)
	}
}

func TestForParagraphMultiVoiceOnUsesCharacterVoice(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, store.CharacterVoiceModeAssigned, false, false); err != nil {
		t.Fatalf("UpdateVoice (enable MultiVoice): %v", err)
	}
	book, err := s.GetBook(book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}

	scope := store.SeriesScope(book)
	char, _, err := s.UpsertCharacter(scope, "Alice", false)
	if err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}
	preset, err := s.CreateVoicePreset("Alice Voice", "speak brightly", "ref", 1, 1.0, voices.DefaultDesignModel)
	if err != nil {
		t.Fatalf("CreateVoicePreset: %v", err)
	}
	if err := s.SetCharacterVoice(char.ID, voices.DefaultCloneModel, preset.ID); err != nil {
		t.Fatalf("SetCharacterVoice: %v", err)
	}

	r := NewResolver(s)
	got, err := r.ForParagraph(book, "Alice", 0)
	if err != nil {
		t.Fatalf("ForParagraph: %v", err)
	}
	if got.PresetID != preset.ID {
		t.Fatalf("expected Alice's assigned preset, got %+v", got)
	}

	// Narrator and unattributed ("") both still resolve to the book's own
	// voice even with MultiVoice on.
	bookVoice, err := r.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	for _, speaker := range []string{"", "Narrator"} {
		got, err := r.ForParagraph(book, speaker, 0)
		if err != nil {
			t.Fatalf("ForParagraph(%q): %v", speaker, err)
		}
		if got.VoiceID() != bookVoice.VoiceID() {
			t.Fatalf("expected book voice for speaker %q, got %+v", speaker, got)
		}
	}
}

// TestForParagraphAgreesWithBookVoicesOnCase: the audio endpoint resolves
// through ForParagraph and the chapter topic through BookVoices - a speaker
// matching a character only case-insensitively ("Unknown" vs an "unknown"
// row) must resolve to the same voice through both, or the chapter reports a
// clip ready that the audio endpoint 404s.
func TestForParagraphAgreesWithBookVoicesOnCase(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, store.CharacterVoiceModeAssigned, false, false); err != nil {
		t.Fatalf("UpdateVoice (enable MultiVoice): %v", err)
	}
	book, err := s.GetBook(book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	char, _, err := s.UpsertCharacter(store.SeriesScope(book), "unknown", false)
	if err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}
	preset, err := s.CreateVoicePreset("Unknown Voice", "speak flatly", "ref", 1, 1.0, voices.DefaultDesignModel)
	if err != nil {
		t.Fatalf("CreateVoicePreset: %v", err)
	}
	if err := s.SetCharacterVoice(char.ID, voices.DefaultCloneModel, preset.ID); err != nil {
		t.Fatalf("SetCharacterVoice: %v", err)
	}

	r := NewResolver(s)
	bv, err := r.BookVoices(book)
	if err != nil {
		t.Fatalf("BookVoices: %v", err)
	}
	for _, speaker := range []string{"Unknown", "unknown"} {
		one, err := r.ForParagraph(book, speaker, 0)
		if err != nil {
			t.Fatalf("ForParagraph(%q): %v", speaker, err)
		}
		bulk, err := bv.ForSpeaker(speaker, 0)
		if err != nil {
			t.Fatalf("ForSpeaker(%q): %v", speaker, err)
		}
		if one.VoiceID() != bulk.VoiceID() {
			t.Fatalf("speaker %q: ForParagraph voice %s, BookVoices voice %s", speaker, one.VoiceID(), bulk.VoiceID())
		}
	}
}

func TestForParagraphFallsBackWhenCharacterHasNoVoiceYet(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, store.CharacterVoiceModeAssigned, false, false); err != nil {
		t.Fatalf("UpdateVoice (enable MultiVoice): %v", err)
	}
	book, err := s.GetBook(book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	scope := store.SeriesScope(book)
	if _, _, err := s.UpsertCharacter(scope, "Bob", false); err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}

	r := NewResolver(s)
	bookVoice, err := r.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	got, err := r.ForParagraph(book, "Bob", 0)
	if err != nil {
		t.Fatalf("ForParagraph: %v", err)
	}
	if got.VoiceID() != bookVoice.VoiceID() {
		t.Fatalf("expected fallback to book voice for a character with no assigned voice yet, got %+v", got)
	}
}

// bookWithInstructedClone sets book up with MultiVoice + InstructCharacterVoices
// on and its own voice resolving through voices.InstructedCloneModel (the
// only clone model that honors CloneInstruct) - the shared setup for both
// tests below.
func bookWithInstructedClone(t *testing.T, s *store.SQLiteStore, book *store.Book) *store.Book {
	t.Helper()
	preset, err := s.CreateVoicePreset("Narrator", "speak warmly", "ref", 1, 1.0, voices.DefaultDesignModel)
	if err != nil {
		t.Fatalf("CreateVoicePreset: %v", err)
	}
	if err := s.UpdateVoice(book.ID, preset.ID, "speak warmly", book.VoiceLanguage, 1, voices.InstructedCloneModel, store.CharacterVoiceModeInstructUnassigned, false, false); err != nil {
		t.Fatalf("UpdateVoice: %v", err)
	}
	book, err = s.GetBook(book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	return book
}

// TestForParagraphInstructCharacterVoicesUsesNarratorRefWithCloneInstruct is
// the core regression test for store.Book.InstructCharacterVoices: a
// characterized-but-unassigned character resolves to the book's own
// narrator voice (same PresetID/RefText/CloneModel/SpeedMultiplier/
// DesignModel - no separate preset), styled by their own Summary sent as
// CloneInstruct - and two differently-characterized such fallbacks must
// still land on distinct cache keys (VoiceID), even though they share the
// exact same underlying PresetID/Instruct.
func TestForParagraphInstructCharacterVoicesUsesNarratorRefWithCloneInstruct(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	book = bookWithInstructedClone(t, s, book)

	scope := store.SeriesScope(book)
	alice, _, err := s.UpsertCharacter(scope, "Alice", false)
	if err != nil {
		t.Fatalf("UpsertCharacter(Alice): %v", err)
	}
	if err := s.SetCharacterSummary(alice.ID, "Speak as a bright, energetic teenager.", "ref line"); err != nil {
		t.Fatalf("SetCharacterSummary(Alice): %v", err)
	}
	bob, _, err := s.UpsertCharacter(scope, "Bob", false)
	if err != nil {
		t.Fatalf("UpsertCharacter(Bob): %v", err)
	}
	if err := s.SetCharacterSummary(bob.ID, "Speak as a gruff, world-weary old sailor.", "ref line"); err != nil {
		t.Fatalf("SetCharacterSummary(Bob): %v", err)
	}

	r := NewResolver(s)
	bookVoice, err := r.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}

	aliceVoice, err := r.ForParagraph(book, "Alice", 0)
	if err != nil {
		t.Fatalf("ForParagraph(Alice): %v", err)
	}
	if aliceVoice.PresetID != bookVoice.PresetID || aliceVoice.RefText != bookVoice.RefText {
		t.Fatalf("expected Alice to clone through the narrator's own preset/ref clip, got %+v vs narrator %+v", aliceVoice, bookVoice)
	}
	if aliceVoice.CloneInstruct != "Speak as a bright, energetic teenager." {
		t.Fatalf("expected Alice's own characterization as CloneInstruct, got %q", aliceVoice.CloneInstruct)
	}
	if aliceVoice.VoiceID() == bookVoice.VoiceID() {
		t.Fatalf("expected Alice's voice_id to differ from the narrator's own despite sharing a preset")
	}

	bobVoice, err := r.ForParagraph(book, "Bob", 0)
	if err != nil {
		t.Fatalf("ForParagraph(Bob): %v", err)
	}
	if bobVoice.VoiceID() == aliceVoice.VoiceID() {
		t.Fatalf("expected Alice and Bob to land on distinct voice_ids despite sharing PresetID/Instruct")
	}
}

// TestForParagraphInstructCharacterVoicesNoOpForOtherCloneModel confirms the
// setting is a no-op (falls back to today's plain book-voice behavior, same
// as TestForParagraphFallsBackWhenCharacterHasNoVoiceYet) whenever the
// book's resolved clone model isn't voices.InstructedCloneModel, even with
// both flags on and the character already characterized.
func TestForParagraphInstructCharacterVoicesNoOpForOtherCloneModel(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, store.CharacterVoiceModeInstructUnassigned, false, false); err != nil {
		t.Fatalf("UpdateVoice: %v", err)
	}
	book, err := s.GetBook(book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	// book's own clone model is voices.DefaultCloneModel, not
	// voices.InstructedCloneModel.

	scope := store.SeriesScope(book)
	char, _, err := s.UpsertCharacter(scope, "Alice", false)
	if err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}
	if err := s.SetCharacterSummary(char.ID, "Speak as a bright, energetic teenager.", "ref line"); err != nil {
		t.Fatalf("SetCharacterSummary: %v", err)
	}

	r := NewResolver(s)
	bookVoice, err := r.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	got, err := r.ForParagraph(book, "Alice", 0)
	if err != nil {
		t.Fatalf("ForParagraph: %v", err)
	}
	if got.VoiceID() != bookVoice.VoiceID() || got.CloneInstruct != "" {
		t.Fatalf("expected plain book voice with no CloneInstruct for a non-instructed clone model, got %+v", got)
	}
}

// TestForParagraphInstructAllOverridesExplicitAssignment is the core
// regression test for CharacterVoiceModeInstructAll: unlike
// InstructUnassigned (only a fallback for a character with nothing else
// assigned - see TestForParagraphInstructCharacterVoicesUsesNarratorRefWithCloneInstruct),
// InstructAll must resolve a character straight to the narrator-instructed
// voice even when they already have their own distinct, explicitly
// assigned preset - that assignment is ignored for resolution while this
// mode is active, not consulted at all.
func TestForParagraphInstructAllOverridesExplicitAssignment(t *testing.T) {
	s := openTestStore(t)
	book := createBook(t, s)
	book = bookWithInstructedClone(t, s, book)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, store.CharacterVoiceModeInstructAll, false, false); err != nil {
		t.Fatalf("UpdateVoice: %v", err)
	}
	book, err := s.GetBook(book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}

	scope := store.SeriesScope(book)
	alice, _, err := s.UpsertCharacter(scope, "Alice", false)
	if err != nil {
		t.Fatalf("UpsertCharacter(Alice): %v", err)
	}
	if err := s.SetCharacterSummary(alice.ID, "Speak as a bright, energetic teenager.", "ref line"); err != nil {
		t.Fatalf("SetCharacterSummary(Alice): %v", err)
	}
	// Alice has her own, wholly distinct assigned preset - under
	// InstructUnassigned this would win outright; under InstructAll it
	// must be ignored entirely.
	assigned, err := s.CreateVoicePreset("Alice's Own Voice", "speak brightly", "ref", 1, 1.0, voices.DefaultDesignModel)
	if err != nil {
		t.Fatalf("CreateVoicePreset: %v", err)
	}
	if err := s.SetCharacterVoice(alice.ID, voices.InstructedCloneModel, assigned.ID); err != nil {
		t.Fatalf("SetCharacterVoice: %v", err)
	}

	r := NewResolver(s)
	bookVoice, err := r.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	got, err := r.ForParagraph(book, "Alice", 0)
	if err != nil {
		t.Fatalf("ForParagraph: %v", err)
	}
	if got.PresetID != bookVoice.PresetID {
		t.Fatalf("expected Alice to be resolved through the narrator's own preset (ignoring her own assignment), got %+v", got)
	}
	if got.PresetID == assigned.ID {
		t.Fatalf("expected Alice's own explicitly assigned preset to be ignored under InstructAll, got it anyway")
	}
	if got.CloneInstruct != "Speak as a bright, energetic teenager." {
		t.Fatalf("expected Alice's own characterization as CloneInstruct, got %q", got.CloneInstruct)
	}
}

// A role's numbered lines past the first get a sibling of the role's voice
// - same casting, another seed, its own preset id - and nothing else does.
func TestMemberVoice(t *testing.T) {
	base := ResolvedVoice{PresetID: "p1", Instruct: "a stern guard", Seed: 42, RefText: "Halt.", SpeedMultiplier: 1, Language: "English"}
	guard := store.Character{Name: "Guard", IsRole: true}

	two, three := MemberVoice(base, guard, 2), MemberVoice(base, guard, 3)
	if two.PresetID == base.PresetID || two.Seed == base.Seed || two.Instruct != base.Instruct || two.RefText != base.RefText {
		t.Fatalf("number 2 = %+v, want the same casting under its own preset id and seed", two)
	}
	if two.PresetID == three.PresetID || two.Seed == three.Seed || two.VoiceID() == three.VoiceID() {
		t.Fatalf("numbers 2 and 3 share a voice: %+v / %+v", two, three)
	}
	if again := MemberVoice(base, guard, 2); again != two {
		t.Fatalf("number 2 isn't stable: %+v vs %+v", again, two)
	}
	recast := base
	recast.Instruct = "a weary old guard"
	if MemberVoice(recast, guard, 2).PresetID == two.PresetID {
		t.Fatal("a recast role kept its numbered voice's preset id (and so its old clip)")
	}

	for name, v := range map[string]ResolvedVoice{
		"number 1":            MemberVoice(base, guard, 1),
		"no number":           MemberVoice(base, guard, 0),
		"not a role":          MemberVoice(base, store.Character{Name: "Bert"}, 2),
		"no preset":           MemberVoice(ResolvedVoice{Instruct: "x"}, guard, 2),
		"instructed narrator": MemberVoice(instructedNarratorVoice(base, "gruff"), guard, 2),
	} {
		if v.PresetID != base.PresetID && v.PresetID != "" {
			t.Errorf("%s: got preset %q, want the voice unchanged", name, v.PresetID)
		}
	}
}
