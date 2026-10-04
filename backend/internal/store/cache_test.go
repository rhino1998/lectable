package store

import (
	"reflect"
	"testing"
)

func TestCachedServesRepeatReadsFromMemory(t *testing.T) {
	c := NewCached(openTestStore(t))
	_, chapterID := oneChapterBook(t, c.Store.(*SQLiteStore), "", 0, "One.", "Two.")

	for range 3 {
		ps, err := c.ListParagraphsRaw(chapterID)
		if err != nil || len(ps) != 2 {
			t.Fatalf("ListParagraphsRaw: %v, %d paragraphs", err, len(ps))
		}
	}
	if hits, misses := c.Stats(); hits != 2 || misses != 1 {
		t.Fatalf("hits/misses = %d/%d, want 2/1", hits, misses)
	}
}

// A write through the store - even one made through the inner store
// directly, not via Cached - drops the entries reading its table.
func TestCachedInvalidatesOnWrite(t *testing.T) {
	inner := openTestStore(t)
	c := NewCached(inner)
	bookID, chapterID := oneChapterBook(t, inner, "", 0, `"Hi,"`)

	if ps, _ := c.ListParagraphsRaw(chapterID); ps[0].Speaker != "" {
		t.Fatalf("speaker %q before attribution, want empty", ps[0].Speaker)
	}
	if err := inner.SetParagraphSpeakers(chapterID, map[int]string{0: "Alice"}); err != nil {
		t.Fatalf("SetParagraphSpeakers: %v", err)
	}
	if ps, _ := c.ListParagraphsRaw(chapterID); ps[0].Speaker != "Alice" {
		t.Fatalf("speaker %q after attribution, want Alice (stale cache)", ps[0].Speaker)
	}

	// Position saves report the books.position pseudo-table, which
	// GetBook also depends on.
	if _, err := c.GetBook(bookID); err != nil {
		t.Fatalf("GetBook: %v", err)
	}
	if err := c.UpdatePosition(bookID, 0, 0, 12.5); err != nil {
		t.Fatalf("UpdatePosition: %v", err)
	}
	if b, _ := c.GetBook(bookID); b.PosSeconds != 12.5 {
		t.Fatalf("PosSeconds %v after UpdatePosition, want 12.5 (stale cache)", b.PosSeconds)
	}
	// Saving a position stamps the book as read, in both the single and
	// the list read.
	if b, _ := c.GetBook(bookID); b.LastReadAt == 0 {
		t.Fatalf("LastReadAt unset after UpdatePosition")
	}
	if books, _ := c.ListBooks(); len(books) != 1 || books[0].LastReadAt == 0 {
		t.Fatalf("ListBooks after UpdatePosition = %+v, want LastReadAt set", books)
	}
}

// Unrelated writes leave other tables' entries cached.
func TestCachedKeepsUnrelatedEntries(t *testing.T) {
	inner := openTestStore(t)
	c := NewCached(inner)
	bookID, chapterID := oneChapterBook(t, inner, "", 0, "One.")

	if _, err := c.ListParagraphsRaw(chapterID); err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if err := inner.UpdatePosition(bookID, 0, 0, 1); err != nil {
		t.Fatalf("UpdatePosition: %v", err)
	}
	if _, err := c.ListParagraphsRaw(chapterID); err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if hits, _ := c.Stats(); hits != 1 {
		t.Fatalf("hits = %d, want 1 (a books write dropped a paragraphs entry)", hits)
	}
}

// Callers get copies: mutating a returned value doesn't change what the
// next caller sees.
func TestCachedReturnsCopies(t *testing.T) {
	inner := openTestStore(t)
	c := NewCached(inner)
	bookID, chapterID := oneChapterBook(t, inner, "", 0, "One.")

	ps, _ := c.ListParagraphsRaw(chapterID)
	ps[0].Text = "mutated"
	if again, _ := c.ListParagraphsRaw(chapterID); again[0].Text != "One." {
		t.Fatalf("cached paragraph changed by a caller's mutation: %+v", again[0])
	}

	b, _ := c.GetBook(bookID)
	b.Title = "mutated"
	if again, _ := c.GetBook(bookID); again.Title != "Test Book" {
		t.Fatalf("cached book changed by a caller's mutation: %q", again.Title)
	}
}

// A write landing while a read is loading must keep that read's
// (possibly stale) result out of the cache.
func TestCachedDropsResultOfReadRacingWrite(t *testing.T) {
	c := NewCached(openTestStore(t))

	v, err := cachedRead(c, "k", []string{"paragraphs"}, same[string], func() (string, error) {
		c.invalidate([]string{"paragraphs"}) // a write commits mid-load
		return "stale", nil
	})
	if err != nil || v != "stale" {
		t.Fatalf("cachedRead = %q, %v", v, err)
	}
	c.mu.Lock()
	_, cached := c.entries["k"]
	c.mu.Unlock()
	if cached {
		t.Fatalf("result of a read that raced a write was cached")
	}
}

func TestCachedAllTablesClearsEverything(t *testing.T) {
	inner := openTestStore(t)
	c := NewCached(inner)
	_, chapterID := oneChapterBook(t, inner, "", 0, "One.")

	if _, err := c.ListParagraphsRaw(chapterID); err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	c.invalidate([]string{AllTables})
	if _, err := c.ListParagraphsRaw(chapterID); err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if _, misses := c.Stats(); misses != 2 {
		t.Fatalf("misses = %d, want 2", misses)
	}
}

// ChapterNarration's cached form re-reads only what a write changed, and
// always agrees with the uncached query.
func TestCachedChapterNarrationFollowsWrites(t *testing.T) {
	inner := openTestStore(t)
	c := NewCached(inner)
	bookID, chapterID := oneChapterBook(t, inner, "", 0, "One.", `"Two," said Alice.`, "Three.")
	paras, err := inner.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatal(err)
	}
	overrides := []SpeakerVoice{{Speaker: "Alice", VoiceID: "alice-voice"}}

	check := func(step, voiceID string, overrides []SpeakerVoice) {
		t.Helper()
		got, err := c.ChapterNarration(bookID, voiceID, overrides, nil)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		want, err := inner.ChapterNarration(bookID, voiceID, overrides, nil)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: cached %+v, want %+v", step, got, want)
		}
	}

	check("empty", "book-voice", overrides)
	if err := inner.SetParagraphReady(paras[0].ID, "book-voice", 1.5); err != nil {
		t.Fatal(err)
	}
	check("one ready", "book-voice", overrides)
	hits, _ := c.Stats()
	check("unchanged", "book-voice", overrides)
	if after, _ := c.Stats(); after != hits+1 {
		t.Fatalf("an unchanged book wasn't served from the cache (hits %d -> %d)", hits, after)
	}

	if err := inner.SetParagraphReady(paras[2].ID, "book-voice", 1.5); err != nil {
		t.Fatal(err)
	}
	check("two ready", "book-voice", overrides)
	if err := inner.SetParagraphSpeakers(chapterID, map[int]string{1: "Alice"}); err != nil {
		t.Fatal(err)
	}
	check("attributed", "book-voice", overrides)
	if err := inner.SetParagraphReady(paras[1].ID, "alice-voice", 2); err != nil {
		t.Fatal(err)
	}
	check("character voice ready", "book-voice", overrides)
	check("other voices", "other-voice", nil)
	if err := inner.ResetParagraphAudio(paras[0].ID, "book-voice"); err != nil {
		t.Fatal(err)
	}
	check("reset", "book-voice", overrides)
}
