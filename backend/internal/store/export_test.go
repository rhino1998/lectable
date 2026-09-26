package store

import (
	"encoding/json"
	"slices"
	"testing"
)

// A book's exported rows import back into an empty library unchanged -
// JSON columns, booleans and numbers included - and a second import of
// the same series' roster keeps the rows already there.
func TestExportImportRoundTrip(t *testing.T) {
	src := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, src, "Saga", 1, "Narration.", "\"Hello,\" she said.")
	book, err := src.GetBook(bookID)
	if err != nil {
		t.Fatal(err)
	}
	scope := SeriesScope(book)
	pid, err := src.GetParagraphIDByIdx(chapterID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.SetParagraphReady(pid, "v1", 1.25); err != nil {
		t.Fatal(err)
	}
	if err := src.SetParagraphWordTimings(pid, "v1", `[{"text":"Hello,","start":0,"end":0.5}]`); err != nil {
		t.Fatal(err)
	}
	char, _, err := src.UpsertCharacter(scope, "Ann", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.SetCharacterAliases(char.ID, []string{"Annie"}); err != nil {
		t.Fatal(err)
	}
	preset, err := src.CreateVoicePreset("Ann's voice", "Speak softly", "ref", 7, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := src.SetCharacterVoice(char.ID, "model-a", preset.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := src.UpsertBookmark(pid, "note"); err != nil {
		t.Fatal(err)
	}

	// Everything an export carries moves the fingerprint - a bookmark
	// note too, which SyncFingerprints doesn't see.
	_, before, err := src.ExportFingerprints(bookID, scope)
	if err != nil {
		t.Fatal(err)
	}
	marks, err := src.ListBookmarks(bookID)
	if err != nil || len(marks) != 1 {
		t.Fatalf("ListBookmarks: %v %v", marks, err)
	}
	if err := src.UpdateBookmarkNote(marks[0].ID, "changed"); err != nil {
		t.Fatal(err)
	}
	if _, after, err := src.ExportFingerprints(bookID, scope); err != nil {
		t.Fatal(err)
	} else if before[0] == after[0] {
		t.Error("chapter fingerprint didn't move with a bookmark note")
	}

	rows, err := src.ExportBookRows(bookID, scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"books", "chapters", "paragraphs", "paragraph_audio", "characters", "character_voices", "voice_presets", "bookmarks"} {
		if len(rows[table]) == 0 {
			t.Errorf("no %s rows exported", table)
		}
	}

	dst := openTestStore(t)
	if err := dst.ImportBookRows(rows); err != nil {
		t.Fatalf("ImportBookRows: %v", err)
	}
	again, err := dst.ExportBookRows(bookID, scope)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range ExportTables {
		if got, want := canonical(t, again[table]), canonical(t, rows[table]); !slices.Equal(got, want) {
			t.Errorf("%s after import:\n got %v\nwant %v", table, got, want)
		}
	}

	// The book itself can't be imported twice...
	if err := dst.ImportBookRows(rows); err == nil {
		t.Error("second import of the same book succeeded")
	}
	// ...but shared rows alone are kept, not duplicated or failed on.
	shared := map[string][]json.RawMessage{}
	for _, table := range sharedExportTables {
		shared[table] = rows[table]
	}
	if err := dst.ImportBookRows(shared); err != nil {
		t.Errorf("re-importing shared rows: %v", err)
	}
}

// canonical re-encodes each row so key order and spacing don't matter,
// sorted.
func canonical(t *testing.T, list []json.RawMessage) []string {
	t.Helper()
	out := make([]string, len(list))
	for i, raw := range list {
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		out[i] = string(b)
	}
	slices.Sort(out)
	return out
}
