package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// openTestStore opens a fresh DuckDB-backed Store in a per-test temp
// directory - each test gets its own file (Store.Open's own single
// sql.DB/sql.SetMaxOpenConns(1) makes a shared store across tests risky to
// reason about), and t.Cleanup closes it so it never leaks a connection
// past its own test.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "library.duckdb"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func oneChapterBook(t *testing.T, s *Store, seriesName string, seriesIndex float64, paragraphs ...string) (bookID string, chapterID string) {
	t.Helper()
	blocks := make([]BlockInput, len(paragraphs))
	for i, p := range paragraphs {
		blocks[i] = BlockInput{Kind: BlockText, Text: p, IsQuote: false}
	}
	bookID, _, err := s.CreateBook("Test Book", "Test Author", "en", ".jpg", seriesName, seriesIndex, []ChapterInput{
		{Title: "Chapter One", Blocks: blocks},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	chapters, err := s.ListChapterSummaries(bookID, "voice-x")
	if err != nil {
		t.Fatalf("ListChapterSummaries: %v", err)
	}
	if len(chapters) != 1 {
		t.Fatalf("expected 1 chapter, got %d", len(chapters))
	}
	return bookID, chapters[0].ID
}

func TestOpenSeedsDefaultVoice(t *testing.T) {
	s := openTestStore(t)
	v, err := s.GetDefaultVoice()
	if err != nil {
		t.Fatalf("GetDefaultVoice: %v", err)
	}
	if v.PresetID == "" {
		t.Fatalf("expected a non-empty seeded default preset id")
	}

	if err := s.SetDefaultVoice("custom-preset", "speak warmly", "English", 42); err != nil {
		t.Fatalf("SetDefaultVoice: %v", err)
	}
	v2, err := s.GetDefaultVoice()
	if err != nil {
		t.Fatalf("GetDefaultVoice after set: %v", err)
	}
	if v2 != (DefaultVoice{PresetID: "custom-preset", Instruct: "speak warmly", Language: "English", Seed: 42}) {
		t.Fatalf("unexpected default voice after set: %+v", v2)
	}
}

// TestMigrateCharactersRefLineBackfillsExisting simulates a pre-ref_line
// database (the characters table shape before this column existed) and
// confirms Open's migrateCharactersRefLine backfills an already-
// characterized character's ref_line to voices.DefaultRefText - not '' -
// matching what that character's already-cached reference clip was
// actually rendered from (see migrateCharactersRefLine's own doc comment).
// A regression test specifically because this is a real, one-off exception
// to this package's usual "no migrations" policy, and a future DuckDB
// upgrade changing ALTER TABLE ADD COLUMN behavior would otherwise fail
// silently for anyone with pre-existing data rather than as a build-time
// or even an easily-noticed runtime error.
func TestMigrateCharactersRefLineBackfillsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.duckdb")

	raw, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE characters (
		id TEXT PRIMARY KEY,
		scope TEXT NOT NULL,
		name TEXT NOT NULL,
		summary TEXT NOT NULL DEFAULT '',
		created_at BIGINT NOT NULL,
		UNIQUE(scope, name)
	)`); err != nil {
		t.Fatalf("create pre-migration characters table: %v", err)
	}
	if _, err := raw.Exec(
		`INSERT INTO characters (id, scope, name, summary, created_at) VALUES (?, ?, ?, ?, ?)`,
		"char-1", "scope-1", "Gandalf", "An old wizard, warm but commanding.", int64(1000),
	); err != nil {
		t.Fatalf("seed pre-migration character: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open (should migrate ref_line in): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	c, err := s.GetCharacter("char-1")
	if err != nil {
		t.Fatalf("GetCharacter: %v", err)
	}
	if c == nil {
		t.Fatalf("expected pre-migration character to survive Open")
	}
	if c.Summary != "An old wizard, warm but commanding." {
		t.Fatalf("summary lost during migration: %+v", c)
	}
	if c.RefLine != voices.DefaultRefText {
		t.Fatalf("ref_line not backfilled to voices.DefaultRefText: got %q", c.RefLine)
	}
}

func TestVoiceIDStableAndDistinct(t *testing.T) {
	a := VoiceID("preset-1", "instruct", "English")
	b := VoiceID("preset-1", "instruct", "English")
	if a != b {
		t.Fatalf("VoiceID should be deterministic: %q != %q", a, b)
	}
	c := VoiceID("preset-2", "instruct", "English")
	if a == c {
		t.Fatalf("VoiceID should differ for a different preset id")
	}
}

func TestCreateBookAndParagraphs(t *testing.T) {
	s := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, s, "The Saga", 2, "First paragraph.", `"Hello," she said.`)

	book, err := s.GetBook(bookID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if book == nil {
		t.Fatalf("GetBook returned nil for a just-created book")
	}
	if book.SeriesName != "The Saga" || book.SeriesIndex != 2 {
		t.Fatalf("unexpected series fields: %+v", book)
	}
	if book.Title != "Test Book" || book.Author != "Test Author" {
		t.Fatalf("unexpected book fields: %+v", book)
	}

	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if len(paragraphs) != 2 {
		t.Fatalf("expected 2 paragraphs, got %d", len(paragraphs))
	}
	if paragraphs[0].Idx != 0 || paragraphs[1].Idx != 1 {
		t.Fatalf("expected sequential Idx, got %d, %d", paragraphs[0].Idx, paragraphs[1].Idx)
	}
	if paragraphs[0].Text != "First paragraph." {
		t.Fatalf("unexpected paragraph text: %q", paragraphs[0].Text)
	}

	books, err := s.ListBooks()
	if err != nil {
		t.Fatalf("ListBooks: %v", err)
	}
	if len(books) != 1 || books[0].ID != bookID {
		t.Fatalf("ListBooks didn't return the created book: %+v", books)
	}
}

func TestSeriesScopeAndListSeriesBooks(t *testing.T) {
	s := openTestStore(t)
	book1, _ := oneChapterBook(t, s, "The Saga", 1, "p1")
	book2, _ := oneChapterBook(t, s, "The Saga", 2, "p1")
	standalone, _ := oneChapterBook(t, s, "", 0, "p1")

	b1, _ := s.GetBook(book1)
	b2, _ := s.GetBook(book2)
	bStandalone, _ := s.GetBook(standalone)

	if SeriesScope(b1) != SeriesScope(b2) {
		t.Fatalf("books in the same series should share one scope: %q vs %q", SeriesScope(b1), SeriesScope(b2))
	}
	if SeriesScope(bStandalone) == SeriesScope(b1) {
		t.Fatalf("a standalone book should not share a scope with a series")
	}
	if SeriesScope(bStandalone) != "book:"+standalone {
		t.Fatalf("standalone book scope should be keyed by its own id, got %q", SeriesScope(bStandalone))
	}

	seriesBooks, err := s.ListSeriesBooks("The Saga")
	if err != nil {
		t.Fatalf("ListSeriesBooks: %v", err)
	}
	if len(seriesBooks) != 2 {
		t.Fatalf("expected 2 books in the series, got %d", len(seriesBooks))
	}
	if seriesBooks[0].ID != book1 || seriesBooks[1].ID != book2 {
		t.Fatalf("expected series books in SeriesIndex order, got %+v", seriesBooks)
	}
}

func TestChapterPasses(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := oneChapterBook(t, s, "", 0, "p1")

	ch, err := s.GetChapterByID(chapterID)
	if err != nil {
		t.Fatalf("GetChapterByID: %v", err)
	}
	if ch.Passes != (Passes{}) {
		t.Fatalf("a fresh chapter should start with no passes set, got %+v", ch.Passes)
	}

	if err := s.SetChapterAttributed(chapterID); err != nil {
		t.Fatalf("SetChapterAttributed: %v", err)
	}
	ch, err = s.GetChapterByID(chapterID)
	if err != nil {
		t.Fatalf("GetChapterByID after attributed: %v", err)
	}
	if want := (Passes{Attribution: true, Description: true, ScareQuote: true}); ch.Passes != want {
		t.Fatalf("expected %+v, got %+v", want, ch.Passes)
	}

	if err := s.SetChapterDirected(chapterID); err != nil {
		t.Fatalf("SetChapterDirected: %v", err)
	}
	ch, err = s.GetChapterByID(chapterID)
	if err != nil {
		t.Fatalf("GetChapterByID after directed: %v", err)
	}
	if want := (Passes{Attribution: true, Description: true, ScareQuote: true, Direction: true}); ch.Passes != want {
		t.Fatalf("expected %+v, got %+v", want, ch.Passes)
	}

	// Independent facts, not a ranked/advance-only pipeline any more: a
	// chapter already directed going through attribution again must
	// leave Direction alone (SetChapterAttributed never touches it).
	if err := s.SetChapterAttributed(chapterID); err != nil {
		t.Fatalf("SetChapterAttributed after directed: %v", err)
	}
	ch, err = s.GetChapterByID(chapterID)
	if err != nil {
		t.Fatalf("GetChapterByID after re-attributing: %v", err)
	}
	if want := (Passes{Attribution: true, Description: true, ScareQuote: true, Direction: true}); ch.Passes != want {
		t.Fatalf("expected Direction to survive re-attribution, got %+v", ch.Passes)
	}
}

func TestClearBookSpeakersLeavesDirectionPassAlone(t *testing.T) {
	s := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, s, "", 0, "p1")

	if err := s.SetChapterDirected(chapterID); err != nil {
		t.Fatalf("SetChapterDirected: %v", err)
	}
	if err := s.ClearBookSpeakers(bookID); err != nil {
		t.Fatalf("ClearBookSpeakers: %v", err)
	}
	ch, err := s.GetChapterByID(chapterID)
	if err != nil {
		t.Fatalf("GetChapterByID: %v", err)
	}
	// ClearBookSpeakers only clears attribution data (paragraphs.speaker);
	// a chapter's own direction-tagging data (paragraphs.tts_tags) is
	// untouched, so Passes.Direction should survive rather than being
	// cleared too - see ClearBookSpeakers' own doc comment.
	if want := (Passes{Direction: true}); ch.Passes != want {
		t.Fatalf("expected Direction to survive ClearBookSpeakers, got %+v", ch.Passes)
	}

	// A chapter that only ever reached Attribution/Description (never
	// directed) should have both cleared back to false - attribution data
	// itself (the thing this actually cleared) is what those stood for.
	bookID2, chapterID2 := oneChapterBook(t, s, "", 0, "p1")
	if err := s.SetChapterAttributed(chapterID2); err != nil {
		t.Fatalf("SetChapterAttributed: %v", err)
	}
	if err := s.ClearBookSpeakers(bookID2); err != nil {
		t.Fatalf("ClearBookSpeakers: %v", err)
	}
	ch2, err := s.GetChapterByID(chapterID2)
	if err != nil {
		t.Fatalf("GetChapterByID: %v", err)
	}
	if ch2.Passes != (Passes{}) {
		t.Fatalf("expected Attribution/Description to be cleared by ClearBookSpeakers, got %+v", ch2.Passes)
	}
}

func TestParagraphSpeakerTagsAndDescriptions(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := oneChapterBook(t, s, "", 0, `"Hi," Alice said.`, "Bob looked tall and stern.")

	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}

	if err := s.SetParagraphSpeakers(chapterID, map[int]string{0: "Alice"}); err != nil {
		t.Fatalf("SetParagraphSpeakers: %v", err)
	}
	if err := s.SetParagraphDescriptions(chapterID, map[int][]string{1: {"Bob"}}); err != nil {
		t.Fatalf("SetParagraphDescriptions: %v", err)
	}
	if err := s.SetParagraphSentenceTags(chapterID, "audiocpp-higgs-4b", map[int]string{0: `<|emotion:elation|>"Hi," Alice said.`}); err != nil {
		t.Fatalf("SetParagraphSentenceTags: %v", err)
	}

	p0, err := s.GetParagraph(paragraphs[0].ID)
	if err != nil {
		t.Fatalf("GetParagraph(0): %v", err)
	}
	if p0.Speaker != "Alice" {
		t.Fatalf("expected speaker Alice, got %q", p0.Speaker)
	}
	if p0.Tags["audiocpp-higgs-4b"].SentenceText != `<|emotion:elation|>"Hi," Alice said.` {
		t.Fatalf("expected sentence tag to be set, got %v", p0.Tags)
	}
	if p0.Tags["audiocpp-higgs-4b"].InlineText != "" {
		t.Fatalf("expected no inline tag yet, got %v", p0.Tags)
	}
	if err := s.SetParagraphInlineTags(chapterID, "audiocpp-higgs-4b", map[int]string{0: `"Hi," Alice said.`}); err != nil {
		t.Fatalf("SetParagraphInlineTags: %v", err)
	}
	p0, err = s.GetParagraph(paragraphs[0].ID)
	if err != nil {
		t.Fatalf("GetParagraph(0) after inline set: %v", err)
	}
	if p0.Tags["audiocpp-higgs-4b"].SentenceText != `<|emotion:elation|>"Hi," Alice said.` {
		t.Fatalf("expected sentence tag to survive an independent inline update, got %v", p0.Tags)
	}

	p1, err := s.GetParagraph(paragraphs[1].ID)
	if err != nil {
		t.Fatalf("GetParagraph(1): %v", err)
	}
	if len(p1.DescribesCharacters) != 1 || p1.DescribesCharacters[0] != "Bob" {
		t.Fatalf("expected DescribesCharacters [Bob], got %v", p1.DescribesCharacters)
	}

	// SetParagraphSpeaker (singular) overrides one paragraph directly, the
	// manual-correction path (httpapi.handleSetParagraphSpeaker) rather
	// than a whole-chapter attribution batch.
	if err := s.SetParagraphSpeaker(paragraphs[1].ID, "Narrator"); err != nil {
		t.Fatalf("SetParagraphSpeaker: %v", err)
	}
	p1, err = s.GetParagraph(paragraphs[1].ID)
	if err != nil {
		t.Fatalf("GetParagraph(1) after manual override: %v", err)
	}
	if p1.Speaker != "Narrator" {
		t.Fatalf("expected manually-set speaker Narrator, got %q", p1.Speaker)
	}
}

// TestQuotesForBooksContextOnlyFromInlineNarration covers QuoteContext's
// own Inline requirement on the "next" self-join: a quote's Context should
// only ever be the attribution tag glued to it as a later segment of the
// *same* source paragraph (Inline=true), never a narration paragraph that
// merely happens to be the next stored row but actually starts a brand
// new, unrelated source paragraph (Inline=false) - which is narration
// (empty/Narrator speaker) too, so without this check it would have been
// wrongly picked up as if it were this quote's own attribution tag.
func TestQuotesForBooksContextOnlyFromInlineNarration(t *testing.T) {
	s := openTestStore(t)
	bookID, _, err := s.CreateBook("Test Book", "Test Author", "en", ".jpg", "", 0, []ChapterInput{
		{Title: "Chapter One", Blocks: []BlockInput{
			{Kind: BlockText, Text: `"Hi,"`, IsQuote: true, Inline: false},
			{Kind: BlockText, Text: "Alice said.", IsQuote: false, Inline: true},
			{Kind: BlockText, Text: `"Bye,"`, IsQuote: true, Inline: false},
			{Kind: BlockText, Text: "Bob walked in.", IsQuote: false, Inline: false},
		}},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	chapters, err := s.ListChapterSummaries(bookID, "voice-x")
	if err != nil {
		t.Fatalf("ListChapterSummaries: %v", err)
	}
	if err := s.SetParagraphSpeakers(chapters[0].ID, map[int]string{0: "Alice", 2: "Alice"}); err != nil {
		t.Fatalf("SetParagraphSpeakers: %v", err)
	}

	quotes, err := s.QuotesForBooks([]string{bookID}, "Alice", 10)
	if err != nil {
		t.Fatalf("QuotesForBooks: %v", err)
	}
	if len(quotes) != 2 {
		t.Fatalf("expected 2 Alice quotes, got %d: %+v", len(quotes), quotes)
	}
	if quotes[0].Text != `"Hi,"` || quotes[0].Context != "Alice said." {
		t.Fatalf(`expected "Hi," with Context "Alice said." (Inline attribution tag), got %+v`, quotes[0])
	}
	if quotes[1].Text != `"Bye,"` || quotes[1].Context != "" {
		t.Fatalf(`expected "Bye," with empty Context ("Bob walked in." is a new, non-Inline paragraph), got %+v`, quotes[1])
	}
}

// TestQuotesAndDescriptionsForBooksSingleQuery covers QuotesForBooks/
// DescriptionsForBooks' own rewrite from a loop issuing one query per book
// to a single IN (...) query across every book, with an explicit ORDER BY
// CASE ranking each result by the *given* bookIDs order - the part most at
// risk of a silent bug in that rewrite, since it no longer falls out for
// free the way a per-book loop's own natural iteration order did. Passes
// bookIDs in the *reverse* of their actual book_id/creation order
// specifically so a test that accidentally fell back to DB/creation order
// instead of the caller-given order would fail.
func TestQuotesAndDescriptionsForBooksSingleQuery(t *testing.T) {
	s := openTestStore(t)
	book1, _ := oneChapterBook(t, s, "The Saga", 1,
		`"First book," Alice said.`, "Bob looked tall in book one.", `"Ignore me," Carol said.`)
	book2, _ := oneChapterBook(t, s, "The Saga", 2,
		`"Second book," Alice said.`, "Bob looked stern in book two.")

	for bookID, speakers := range map[string]map[int]string{
		book1: {0: "Alice", 2: "Carol"},
		book2: {0: "Alice"},
	} {
		chapters, err := s.ListChapterSummaries(bookID, "voice-x")
		if err != nil {
			t.Fatalf("ListChapterSummaries(%s): %v", bookID, err)
		}
		if err := s.SetParagraphSpeakers(chapters[0].ID, speakers); err != nil {
			t.Fatalf("SetParagraphSpeakers(%s): %v", bookID, err)
		}
	}
	for bookID, describes := range map[string]map[int][]string{
		book1: {1: {"Bob"}},
		book2: {1: {"Bob"}},
	} {
		chapters, err := s.ListChapterSummaries(bookID, "voice-x")
		if err != nil {
			t.Fatalf("ListChapterSummaries(%s): %v", bookID, err)
		}
		if err := s.SetParagraphDescriptions(chapters[0].ID, describes); err != nil {
			t.Fatalf("SetParagraphDescriptions(%s): %v", bookID, err)
		}
	}

	// Reverse of creation order - QuotesForBooks/DescriptionsForBooks must
	// respect *this* order, not book1-before-book2 falling out of DB/
	// creation order by coincidence.
	reversed := []string{book2, book1}

	quotes, err := s.QuotesForBooks(reversed, "Alice", 10)
	if err != nil {
		t.Fatalf("QuotesForBooks: %v", err)
	}
	if len(quotes) != 2 {
		t.Fatalf("expected 2 Alice quotes across both books, got %d: %+v", len(quotes), quotes)
	}
	if quotes[0].Text != `"Second book," Alice said.` || quotes[1].Text != `"First book," Alice said.` {
		t.Fatalf("expected book2's quote before book1's (bookIDs order, not creation order), got %+v", quotes)
	}

	// limit=1 with book2 first should keep book2's quote, not book1's -
	// proving the ORDER BY (not just the row count) drives truncation.
	limited, err := s.QuotesForBooks(reversed, "Alice", 1)
	if err != nil {
		t.Fatalf("QuotesForBooks(limit=1): %v", err)
	}
	if len(limited) != 1 || limited[0].Text != `"Second book," Alice said.` {
		t.Fatalf("expected only book2's quote to survive limit=1, got %+v", limited)
	}

	// Carol only appears in book1 - confirms character-name filtering still
	// excludes Alice's/other speakers' own paragraphs under the new single
	// IN (...) query.
	carolQuotes, err := s.QuotesForBooks(reversed, "Carol", 10)
	if err != nil {
		t.Fatalf("QuotesForBooks(Carol): %v", err)
	}
	if len(carolQuotes) != 1 || carolQuotes[0].Text != `"Ignore me," Carol said.` {
		t.Fatalf("expected Carol's own single quote, got %+v", carolQuotes)
	}

	descriptions, err := s.DescriptionsForBooks(reversed, "Bob", 10)
	if err != nil {
		t.Fatalf("DescriptionsForBooks: %v", err)
	}
	if len(descriptions) != 2 {
		t.Fatalf("expected 2 Bob descriptions across both books, got %d: %+v", len(descriptions), descriptions)
	}
	if descriptions[0].Text != "Bob looked stern in book two." || descriptions[1].Text != "Bob looked tall in book one." {
		t.Fatalf("expected book2's description before book1's, got %+v", descriptions)
	}

	if empty, err := s.QuotesForBooks(nil, "Alice", 10); err != nil || empty != nil {
		t.Fatalf("QuotesForBooks(nil bookIDs) = %v, %v; want nil, nil", empty, err)
	}
	if empty, err := s.DescriptionsForBooks(nil, "Bob", 10); err != nil || empty != nil {
		t.Fatalf("DescriptionsForBooks(nil bookIDs) = %v, %v; want nil, nil", empty, err)
	}
}

func TestVoicePresetCRUD(t *testing.T) {
	s := openTestStore(t)
	preset, err := s.CreateVoicePreset("My Narrator", "speak calmly", "reference text", 7, 1.1, "audiocpp-higgs-4b", "breeze_tts")
	if err != nil {
		t.Fatalf("CreateVoicePreset: %v", err)
	}
	if preset.ID == "" {
		t.Fatalf("expected a generated preset id")
	}

	got, err := s.GetVoicePreset(preset.ID)
	if err != nil {
		t.Fatalf("GetVoicePreset: %v", err)
	}
	if got == nil || got.Name != "My Narrator" || got.CloneModel != "audiocpp-higgs-4b" || got.DesignModel != "breeze_tts" {
		t.Fatalf("unexpected preset: %+v", got)
	}

	if err := s.UpdateVoicePreset(preset.ID, "Renamed", "speak boldly", "reference text", 7, 1.2, "audiocpp-higgs-4b", "qwen3_tts"); err != nil {
		t.Fatalf("UpdateVoicePreset: %v", err)
	}
	got, err = s.GetVoicePreset(preset.ID)
	if err != nil {
		t.Fatalf("GetVoicePreset after update: %v", err)
	}
	if got.Name != "Renamed" || got.SpeedMultiplier != 1.2 || got.DesignModel != "qwen3_tts" {
		t.Fatalf("update did not apply: %+v", got)
	}

	all, err := s.ListVoicePresets()
	if err != nil {
		t.Fatalf("ListVoicePresets: %v", err)
	}
	found := false
	for _, p := range all {
		if p.ID == preset.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListVoicePresets did not include the created preset")
	}

	if err := s.DeleteVoicePreset(preset.ID); err != nil {
		t.Fatalf("DeleteVoicePreset: %v", err)
	}
	got, err = s.GetVoicePreset(preset.ID)
	if err != nil {
		t.Fatalf("GetVoicePreset after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}
}

func TestCharacterVoiceAssignmentPerCloneModel(t *testing.T) {
	s := openTestStore(t)
	book, _ := oneChapterBook(t, s, "", 0, "p1")
	scope := "book:" + book

	char, created, err := s.UpsertCharacter(scope, "Gandalf", false)
	if err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}
	if !created {
		t.Fatalf("expected UpsertCharacter to report creation for a brand-new name")
	}
	_, created, err = s.UpsertCharacter(scope, "Gandalf", false)
	if err != nil {
		t.Fatalf("UpsertCharacter (repeat): %v", err)
	}
	if created {
		t.Fatalf("expected UpsertCharacter to report no creation for an existing name")
	}

	presetA, err := s.CreateVoicePreset("Voice A", "speak wisely", "ref", 1, 1.0, "audiocpp-higgs-4b", "breeze_tts")
	if err != nil {
		t.Fatalf("CreateVoicePreset A: %v", err)
	}
	presetB, err := s.CreateVoicePreset("Voice B", "speak wisely", "ref", 1, 1.0, "audiocpp-qwen3-0.6b", "breeze_tts")
	if err != nil {
		t.Fatalf("CreateVoicePreset B: %v", err)
	}

	if err := s.SetCharacterVoice(char.ID, "audiocpp-higgs-4b", presetA.ID); err != nil {
		t.Fatalf("SetCharacterVoice (model A): %v", err)
	}
	if err := s.SetCharacterVoice(char.ID, "audiocpp-qwen3-0.6b", presetB.ID); err != nil {
		t.Fatalf("SetCharacterVoice (model B): %v", err)
	}

	gotA, err := s.CharacterVoiceForModel(char.ID, "audiocpp-higgs-4b")
	if err != nil {
		t.Fatalf("CharacterVoiceForModel A: %v", err)
	}
	if gotA != presetA.ID {
		t.Fatalf("expected preset A for model A, got %q", gotA)
	}
	gotB, err := s.CharacterVoiceForModel(char.ID, "audiocpp-qwen3-0.6b")
	if err != nil {
		t.Fatalf("CharacterVoiceForModel B: %v", err)
	}
	if gotB != presetB.ID {
		t.Fatalf("expected preset B for model B, got %q", gotB)
	}
	gotNone, err := s.CharacterVoiceForModel(char.ID, "some-unassigned-model")
	if err != nil {
		t.Fatalf("CharacterVoiceForModel (unassigned): %v", err)
	}
	if gotNone != "" {
		t.Fatalf("expected no voice for an unassigned model, got %q", gotNone)
	}

	batch, err := s.CharacterVoicesForModel([]string{char.ID}, "audiocpp-higgs-4b")
	if err != nil {
		t.Fatalf("CharacterVoicesForModel: %v", err)
	}
	if batch[char.ID] != presetA.ID {
		t.Fatalf("CharacterVoicesForModel mismatch: %v", batch)
	}

	byName, err := s.GetCharacterByName(scope, "Gandalf")
	if err != nil {
		t.Fatalf("GetCharacterByName: %v", err)
	}
	if byName == nil || byName.ID != char.ID {
		t.Fatalf("GetCharacterByName mismatch: %+v", byName)
	}

	if err := s.SetCharacterSummary(char.ID, "An old wizard, warm but commanding.", "The old stone tower had stood for centuries, its windows dark against the gathering storm."); err != nil {
		t.Fatalf("SetCharacterSummary: %v", err)
	}
	reloaded, err := s.GetCharacter(char.ID)
	if err != nil {
		t.Fatalf("GetCharacter: %v", err)
	}
	if reloaded.Summary != "An old wizard, warm but commanding." {
		t.Fatalf("summary not persisted: %+v", reloaded)
	}
	if reloaded.RefLine != "The old stone tower had stood for centuries, its windows dark against the gathering storm." {
		t.Fatalf("ref line not persisted: %+v", reloaded)
	}

	if err := s.DeleteCharacter(char.ID); err != nil {
		t.Fatalf("DeleteCharacter: %v", err)
	}
	afterDelete, err := s.GetCharacterByName(scope, "Gandalf")
	if err != nil {
		t.Fatalf("GetCharacterByName after delete: %v", err)
	}
	if afterDelete != nil {
		t.Fatalf("expected character to be gone after delete, got %+v", afterDelete)
	}
}

func TestParagraphAudioLifecycle(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := oneChapterBook(t, s, "", 0, "p1")
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	id := paragraphs[0].ID
	const voiceID = "voice-x"

	status, err := s.GetParagraphAudioStatus(id, voiceID)
	if err != nil {
		t.Fatalf("GetParagraphAudioStatus (untouched): %v", err)
	}
	if status != AudioPending {
		t.Fatalf("expected untouched paragraph to report pending, got %q", status)
	}

	if err := s.SetParagraphGenerating(id, voiceID); err != nil {
		t.Fatalf("SetParagraphGenerating: %v", err)
	}
	status, err = s.GetParagraphAudioStatus(id, voiceID)
	if err != nil {
		t.Fatalf("GetParagraphAudioStatus (generating): %v", err)
	}
	if status != AudioGenerating {
		t.Fatalf("expected generating, got %q", status)
	}

	if err := s.SetParagraphReady(id, voiceID, 1.5); err != nil {
		t.Fatalf("SetParagraphReady: %v", err)
	}
	states, err := s.ParagraphAudioStatuses([]string{id}, voiceID)
	if err != nil {
		t.Fatalf("ParagraphAudioStatuses: %v", err)
	}
	if states[id].Status != AudioReady || states[id].DurationSeconds != 1.5 {
		t.Fatalf("unexpected state after ready: %+v", states[id])
	}

	if err := s.SetParagraphError(id, voiceID, "boom"); err != nil {
		t.Fatalf("SetParagraphError: %v", err)
	}
	status, err = s.GetParagraphAudioStatus(id, voiceID)
	if err != nil {
		t.Fatalf("GetParagraphAudioStatus (error): %v", err)
	}
	if status != AudioError {
		t.Fatalf("expected error status, got %q", status)
	}

	if err := s.ResetParagraphAudio(id, voiceID); err != nil {
		t.Fatalf("ResetParagraphAudio: %v", err)
	}
	status, err = s.GetParagraphAudioStatus(id, voiceID)
	if err != nil {
		t.Fatalf("GetParagraphAudioStatus (after reset): %v", err)
	}
	if status != AudioPending {
		t.Fatalf("expected pending after reset, got %q", status)
	}

	// A different voiceID has its own independent state - central to how
	// per-character narration voices coexist with a book's own voice for
	// the same paragraph (see internal/narration).
	otherStatus, err := s.GetParagraphAudioStatus(id, "voice-y")
	if err != nil {
		t.Fatalf("GetParagraphAudioStatus (other voice): %v", err)
	}
	if otherStatus != AudioPending {
		t.Fatalf("expected a never-touched voice id to report pending, got %q", otherStatus)
	}
}

func TestCountReadyAudioForSpeaker(t *testing.T) {
	s := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, s, "", 0, `"Line one," Alice said.`, `"Line two," Alice said.`)
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if err := s.SetParagraphSpeakers(chapterID, map[int]string{0: "Alice", 1: "Alice"}); err != nil {
		t.Fatalf("SetParagraphSpeakers: %v", err)
	}
	const voiceID = "alice-voice"
	if err := s.SetParagraphReady(paragraphs[0].ID, voiceID, 1.0); err != nil {
		t.Fatalf("SetParagraphReady: %v", err)
	}

	n, err := s.CountReadyAudioForSpeaker(bookID, "Alice", voiceID)
	if err != nil {
		t.Fatalf("CountReadyAudioForSpeaker: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 ready paragraph for Alice, got %d", n)
	}

	if err := s.SetParagraphReady(paragraphs[1].ID, voiceID, 1.0); err != nil {
		t.Fatalf("SetParagraphReady (second): %v", err)
	}
	n, err = s.CountReadyAudioForSpeaker(bookID, "Alice", voiceID)
	if err != nil {
		t.Fatalf("CountReadyAudioForSpeaker (after second): %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 ready paragraphs for Alice, got %d", n)
	}
}

func TestDeleteBookCascadesChaptersAndParagraphs(t *testing.T) {
	s := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, s, "", 0, "p1", "p2")

	if err := s.DeleteBook(bookID); err != nil {
		t.Fatalf("DeleteBook: %v", err)
	}

	book, err := s.GetBook(bookID)
	if err != nil {
		t.Fatalf("GetBook after delete: %v", err)
	}
	if book != nil {
		t.Fatalf("expected book to be gone after delete, got %+v", book)
	}
	ch, err := s.GetChapterByID(chapterID)
	if err != nil {
		t.Fatalf("GetChapterByID after delete: %v", err)
	}
	if ch != nil {
		t.Fatalf("expected chapter to be gone after book delete, got %+v", ch)
	}
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw after delete: %v", err)
	}
	if len(paragraphs) != 0 {
		t.Fatalf("expected no paragraphs after book delete, got %d", len(paragraphs))
	}
}

func TestBookmarks(t *testing.T) {
	s := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, s, "", 0, "p1")
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}

	id, err := s.UpsertBookmark(paragraphs[0].ID, "remember this")
	if err != nil {
		t.Fatalf("UpsertBookmark: %v", err)
	}

	// Upserting again for the same paragraph updates the note in place
	// rather than creating a second bookmark row.
	id2, err := s.UpsertBookmark(paragraphs[0].ID, "updated note")
	if err != nil {
		t.Fatalf("UpsertBookmark (repeat): %v", err)
	}
	if id2 != id {
		t.Fatalf("expected the same bookmark id on repeat upsert, got %q vs %q", id2, id)
	}

	bookmarks, err := s.ListBookmarks(bookID)
	if err != nil {
		t.Fatalf("ListBookmarks: %v", err)
	}
	if len(bookmarks) != 1 || bookmarks[0].Note != "updated note" {
		t.Fatalf("unexpected bookmarks: %+v", bookmarks)
	}

	if err := s.DeleteBookmark(id); err != nil {
		t.Fatalf("DeleteBookmark: %v", err)
	}
	bookmarks, err = s.ListBookmarks(bookID)
	if err != nil {
		t.Fatalf("ListBookmarks after delete: %v", err)
	}
	if len(bookmarks) != 0 {
		t.Fatalf("expected no bookmarks after delete, got %+v", bookmarks)
	}
}

func TestSearchParagraphs(t *testing.T) {
	s := openTestStore(t)
	bookID, _ := oneChapterBook(t, s, "", 0, "The dragon slept.", "A quiet village morning.")

	results, err := s.SearchParagraphs(bookID, "dragon", 10)
	if err != nil {
		t.Fatalf("SearchParagraphs: %v", err)
	}
	if len(results) != 1 || results[0].Text != "The dragon slept." {
		t.Fatalf("unexpected search results: %+v", results)
	}

	results, err = s.SearchParagraphs(bookID, "DRAGON", 10)
	if err != nil {
		t.Fatalf("SearchParagraphs (case-insensitive): %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected case-insensitive match, got %+v", results)
	}

	results, err = s.SearchParagraphs(bookID, "nonexistent", 10)
	if err != nil {
		t.Fatalf("SearchParagraphs (no match): %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no matches, got %+v", results)
	}
}

// TestParagraphEmphasisRoundTripsAndAppliesAtGenerationTime confirms
// Emphasis (internal/epub's own structural-markup substitutions) survives
// CreateBook -> ListParagraphsRaw unchanged in the paragraph's real,
// on-screen Text, and is only ever applied at generation time via
// ResolveGenerationText - composed together with an independent
// Pronunciation substitution in one pass, exactly like the delivery-tag/
// pronunciation combination ResolveGenerationText already supports.
func TestParagraphEmphasisRoundTripsAndAppliesAtGenerationTime(t *testing.T) {
	s := openTestStore(t)
	const text = "I never said please to Dr. anyone."
	//            0123456789012345678901234567890123456
	//            0         1         2         3
	blocks := []BlockInput{
		{
			Kind: BlockText,
			Text: text,
			Emphasis: []pronounce.Substitution{
				{Offset: 2, Length: 5, Replacement: "NEVER"},
				{Offset: 13, Length: 6, Replacement: `"please"`},
			},
		},
	}
	bookID, _, err := s.CreateBook("Test Book", "Test Author", "en", ".jpg", "", 0, []ChapterInput{
		{Title: "Chapter One", Blocks: blocks},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	chapters, err := s.ListChapterSummaries(bookID, "voice-x")
	if err != nil {
		t.Fatalf("ListChapterSummaries: %v", err)
	}
	paragraphs, err := s.ListParagraphsRaw(chapters[0].ID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if len(paragraphs) != 1 {
		t.Fatalf("expected 1 paragraph, got %d", len(paragraphs))
	}
	p := paragraphs[0]

	if p.Text != text {
		t.Fatalf("on-screen Text should be untouched by round-tripping: got %q, want %q", p.Text, text)
	}
	if len(p.Emphasis) != 2 {
		t.Fatalf("expected 2 emphasis substitutions to round-trip, got %+v", p.Emphasis)
	}

	// Pronunciation, resolved independently (e.g. by a later
	// ResolvePronunciation run), must compose with Emphasis rather than
	// one clobbering the other.
	p.Pronunciation = []pronounce.Substitution{{Offset: 23, Length: 3, Replacement: "Doctor"}}

	got := p.ResolveGenerationText("audiocpp-higgs-4b")
	want := `I NEVER said "please" to Doctor anyone.`
	if got != want {
		t.Fatalf("ResolveGenerationText = %q, want %q", got, want)
	}
}

// TestResolveGenerationTextFiltersStaleDeliveryTags confirms
// ResolveGenerationText drops a persisted tag insertion that's no longer in
// speakerattr's own current valid set (see its own doc comment) - a real
// scenario for any paragraph tagged before a tag was dropped from
// validSentenceTags/validInlineTags (e.g. <|emotion:sadness|>, dropped
// after live use found it unreliable), whose SentenceText/InlineText was
// never re-generated afterward. A still-valid tag on the same paragraph
// must survive untouched.
func TestResolveGenerationTextFiltersStaleDeliveryTags(t *testing.T) {
	const text = "She walked in and sat down."
	p := Paragraph{
		Text: text,
		Tags: ParagraphTagMap{
			"audiocpp-higgs-4b": ParagraphDirection{
				// <|emotion:sadness|> was dropped from validSentenceTags -
				// simulates a paragraph tagged before that; <|style:shouting|>
				// is still valid and must survive.
				SentenceText: "<|emotion:sadness|>She walked in and sat down.",
			},
		},
	}
	got := p.ResolveGenerationText("audiocpp-higgs-4b")
	if got != text {
		t.Fatalf("ResolveGenerationText = %q, want the stale tag stripped entirely: %q", got, text)
	}

	p2 := Paragraph{
		Text: text,
		Tags: ParagraphTagMap{
			"audiocpp-higgs-4b": ParagraphDirection{
				SentenceText: "<|emotion:sadness|><|style:shouting|>She walked in and sat down.",
			},
		},
	}
	got2 := p2.ResolveGenerationText("audiocpp-higgs-4b")
	want2 := "<|style:shouting|>She walked in and sat down."
	if got2 != want2 {
		t.Fatalf("ResolveGenerationText = %q, want the stale tag dropped but the valid one kept: %q", got2, want2)
	}
}
