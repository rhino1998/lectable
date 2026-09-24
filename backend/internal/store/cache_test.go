package store

import "testing"

func TestCachedServesRepeatReadsFromMemory(t *testing.T) {
	c := NewCached(openTestStore(t))
	_, chapterID := oneChapterBook(t, c.Store.(*DuckStore), "", 0, "One.", "Two.")

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
	ps[0].DescribesCharacters = append(ps[0].DescribesCharacters, "Bob")
	if again, _ := c.ListParagraphsRaw(chapterID); again[0].Text != "One." || len(again[0].DescribesCharacters) != 0 {
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
