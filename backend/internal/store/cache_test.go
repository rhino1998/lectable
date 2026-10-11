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

// A write to one book's text leaves every other book's cached narration
// in place, and the written book still follows it: its chapter_text totals
// match its paragraphs through a reimport, an appended chapter and a
// deleted one.
func TestCachedChapterNarrationIsPerBook(t *testing.T) {
	inner := openTestStore(t)
	c := NewCached(inner)
	quietBook, _ := oneChapterBook(t, inner, "", 0, "Quiet one.", "Quiet two.")
	bookID, chapterID := oneChapterBook(t, inner, "", 0, "One.", `"Two," said Alice.`, "Three.")

	check := func(step string) {
		t.Helper()
		got, err := c.ChapterNarration(bookID, "book-voice", nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		want, err := inner.ChapterNarration(bookID, "book-voice", nil, nil)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: cached %+v, want %+v", step, got, want)
		}
		for _, ch := range got {
			var n int
			var chars, words int64
			if err := inner.db.QueryRow(`
				SELECT COUNT(*), COALESCE(SUM(LENGTH(content)), 0),
					COALESCE(SUM(CASE WHEN content = '' THEN 0 ELSE LENGTH(content) - LENGTH(REPLACE(content, ' ', '')) + 1 END), 0)
				FROM paragraphs WHERE chapter_id = ?`, ch.ID).Scan(&n, &chars, &words); err != nil {
				t.Fatalf("%s: %v", step, err)
			}
			if ch.ParagraphCount != n || ch.TotalChars != chars || ch.TotalWords != words {
				t.Fatalf("%s: chapter %d reports %d paragraphs, %d chars, %d words; its paragraphs are %d, %d, %d",
					step, ch.Idx, ch.ParagraphCount, ch.TotalChars, ch.TotalWords, n, chars, words)
			}
		}
	}
	quiet := func(step string) {
		t.Helper()
		hits, _ := c.Stats()
		if _, err := c.ChapterNarration(quietBook, "book-voice", nil, nil); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if after, _ := c.Stats(); after != hits+1 {
			t.Fatalf("%s: a write to another book dropped this one's narration", step)
		}
	}

	if _, err := c.ChapterNarration(quietBook, "book-voice", nil, nil); err != nil {
		t.Fatal(err)
	}
	check("created")

	if _, _, err := inner.ReplaceChapterContent(chapterID, "Chapter One", []BlockInput{
		{Kind: BlockText, Text: "A longer first paragraph now."},
		{Kind: BlockText, Text: ""},
	}); err != nil {
		t.Fatal(err)
	}
	check("reimported")
	quiet("reimported")

	if _, err := inner.AppendChapters(bookID, []ChapterInput{
		{Title: "Chapter Two", Blocks: []BlockInput{{Kind: BlockText, Text: "Four five six."}}},
	}); err != nil {
		t.Fatal(err)
	}
	check("appended")
	quiet("appended")

	if _, _, err := inner.DeleteChapter(bookID, 0); err != nil {
		t.Fatal(err)
	}
	check("deleted")
	quiet("deleted")
}

// fingerprintCounter counts ChapterFingerprints calls reaching the store.
type fingerprintCounter struct {
	Store
	calls int
}

func (f *fingerprintCounter) ChapterFingerprints(bookID string) (map[string]string, error) {
	f.calls++
	return f.Store.ChapterFingerprints(bookID)
}

// The library listing is rebuilt on every saved position; that mustn't
// read every book's fingerprints again when nothing they cover was written.
func TestCachedChapterNarrationSkipsFingerprintsWhenNothingWritten(t *testing.T) {
	inner := openTestStore(t)
	counter := &fingerprintCounter{Store: inner}
	c := NewCached(counter)
	bookID, chapterID := oneChapterBook(t, inner, "", 0, "One.", "Two.")
	read := func() []ChapterNarration {
		t.Helper()
		got, err := c.ChapterNarration(bookID, "book-voice", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	read()
	first := counter.calls
	if err := inner.UpdatePosition(bookID, 0, 1, 2.5); err != nil {
		t.Fatal(err)
	}
	read()
	if counter.calls != first {
		t.Errorf("a saved position read the fingerprints again (%d calls, was %d)", counter.calls, first)
	}

	paragraphs, err := inner.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatal(err)
	}
	if err := inner.SetParagraphReadyClip(paragraphs[0].ID, "book-voice", "0123456789abcdef0123456789abcdef", 1, "", nil); err != nil {
		t.Fatal(err)
	}
	got := read()
	if counter.calls != first+1 {
		t.Errorf("a clip becoming ready read the fingerprints %d times, want once", counter.calls-first)
	}
	want, err := inner.ChapterNarration(bookID, "book-voice", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after a clip became ready: cached %+v, want %+v", got, want)
	}
	read()
	if counter.calls != first+1 {
		t.Errorf("an unchanged book read the fingerprints again")
	}
}

// A clip becoming ready is the most frequent write while a book narrates;
// it must cost a fingerprint read for its own book only.
func TestCachedChapterNarrationReadsOnlyTheWrittenBooksFingerprints(t *testing.T) {
	inner := openTestStore(t)
	counter := &fingerprintCounter{Store: inner}
	c := NewCached(counter)
	bookA, chapterA := oneChapterBook(t, inner, "", 0, "One.", "Two.")
	bookB, _ := oneChapterBook(t, inner, "", 0, "Three.")
	read := func(bookID string) []ChapterNarration {
		t.Helper()
		got, err := c.ChapterNarration(bookID, "book-voice", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	read(bookA)
	read(bookB)
	base := counter.calls

	paras, err := inner.ListParagraphsRaw(chapterA)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetParagraphGenerating(paras[0].ID, "book-voice"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetParagraphReadyClip(paras[0].ID, "book-voice", "key", 2, "[]", nil); err != nil {
		t.Fatal(err)
	}
	if err := c.SetParagraphWordTimings(paras[0].ID, "book-voice", "[]"); err != nil {
		t.Fatal(err)
	}
	read(bookB)
	if counter.calls != base {
		t.Errorf("a clip in another book read this book's fingerprints")
	}
	got := read(bookA)
	if counter.calls != base+1 {
		t.Errorf("fingerprint reads after a clip became ready = %d, want 1", counter.calls-base)
	}
	want, err := inner.ChapterNarration(bookA, "book-voice", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after a clip became ready: cached %+v, want %+v", got, want)
	}

	// A pass finishing a chapter is one book's write too.
	if err := c.SetParagraphSpeakers(chapterA, map[int]string{0: "Ann"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetChapterAttributed(chapterA); err != nil {
		t.Fatal(err)
	}
	read(bookB)
	if counter.calls != base+1 {
		t.Errorf("a pass over another book's chapter read this book's fingerprints")
	}
	got = read(bookA)
	if counter.calls != base+2 {
		t.Errorf("fingerprint reads after a pass = %d, want 1", counter.calls-base-1)
	}
	if want, err = inner.ChapterNarration(bookA, "book-voice", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after a pass: cached %+v, want %+v", got, want)
	}

	// A write naming no book may have reached any of them.
	if err := c.DeleteBookAudio(bookA); err != nil {
		t.Fatal(err)
	}
	read(bookB)
	if counter.calls != base+3 {
		t.Errorf("an unscoped write didn't re-read another book's fingerprints")
	}
}

func TestReadyParagraphAudio(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := oneChapterBook(t, s, "", 0, "One.", "Two.", "Three.")
	paras, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphReady(paras[0].ID, "v", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphGenerating(paras[1].ID, "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphReady(paras[2].ID, "other", 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadyParagraphAudio([]string{paras[0].ID, paras[1].ID, paras[2].ID}, "v")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{paras[0].ID: true}; !reflect.DeepEqual(got, want) {
		t.Errorf("ready = %v, want %v", got, want)
	}
}
