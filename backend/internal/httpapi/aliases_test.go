package httpapi

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

func TestCharacterAliasesEndpointAndMerge(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title: "One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Albert said.", Inline: true},
			{Kind: store.BlockText, Text: "“Bye,”", IsQuote: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	bert, _, _ := s.UpsertCharacter(scope, "Bert", false)
	sid, _, _ := s.UpsertCharacter(scope, "Sid", false)
	albert, _, _ := s.UpsertCharacter(scope, "Albert", false)
	base := ts.URL + "/api/books/" + bookID

	put := func(charID string, aliases ...string) int {
		resp := doJSON(t, http.MethodPut, base+"/characters/"+charID+"/aliases", map[string]any{"aliases": aliases}, nil)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := put(sid.ID, "Siddy", "Scarf Hangman"); code != http.StatusOK {
		t.Fatalf("set aliases: %d", code)
	}
	if code := put(sid.ID, "Bert and Sid"); code != http.StatusBadRequest {
		t.Fatalf("group alias: want 400, got %d", code)
	}
	if code := put(sid.ID, "bert"); code != http.StatusConflict {
		t.Fatalf("another character's name as alias: want 409, got %d", code)
	}

	// Merging Albert into Bert keeps "Albert" as one of Bert's names.
	resp := doJSON(t, http.MethodPost, base+"/characters/"+albert.ID+"/merge", map[string]string{"targetName": "Bert"}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("merge: %d", resp.StatusCode)
	}
	if got, _ := s.GetCharacter(bert.ID); got == nil || len(got.Aliases) != 1 || got.Aliases[0] != "Albert" {
		t.Fatalf("merge didn't record alias: %+v", got)
	}

	// A manual speaker edit typing an alias lands on the real character;
	// a group is refused.
	resp = doJSON(t, http.MethodPut, base+"/chapters/0/paragraphs/0/speaker", map[string]string{"speaker": "albert"}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set speaker by alias: %d", resp.StatusCode)
	}
	ch, _ := s.GetChapterByIdx(bookID, 0)
	ps, _ := s.ListParagraphsRaw(ch.ID)
	if ps[0].Speaker != "Bert" {
		t.Fatalf("alias speaker stored as %q, want Bert", ps[0].Speaker)
	}
	resp = doJSON(t, http.MethodPut, base+"/chapters/0/paragraphs/2/speaker", map[string]string{"speaker": "Bert and Sid"}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("group speaker: want 400, got %d", resp.StatusCode)
	}
	if c, _ := s.GetCharacterByName(scope, "Bert and Sid"); c != nil {
		t.Fatalf("group speaker was registered as a character")
	}
}

// Merging a descriptor-named "character" into its kind with asRole makes
// the target a role (even one that already existed as a plain character)
// and records no alias.
func TestMergeAsRole(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title:  "One",
		Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	older, _, _ := s.UpsertCharacter(scope, "the older man", false)
	man, _, _ := s.UpsertCharacter(scope, "Man", false)
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "the older man"}); err != nil {
		t.Fatal(err)
	}
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/characters/"+older.ID+"/merge", map[string]any{"targetName": "Man", "asRole": true}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("merge: %d", resp.StatusCode)
	}
	got, _ := s.GetCharacter(man.ID)
	if got == nil || !got.IsRole || len(got.Aliases) != 0 {
		t.Fatalf("target = %+v, want a role with no aliases", got)
	}
	ps, _ := s.ListParagraphsRaw(ch.ID)
	if ps[0].Speaker != "Man" {
		t.Fatalf("speaker = %q, want Man", ps[0].Speaker)
	}
	if c, _ := s.GetCharacter(older.ID); c != nil {
		t.Fatalf("merged-away character still exists")
	}
}

// A role stored with its number in its name ("Drake 8") merged into the
// plain role keeps the number on its lines, wrapped into the slots there
// are; a line of the plain role stays unnumbered.
func TestMergeNumberedRoleKeepsNumber(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	quote := store.BlockInput{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true}
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title:  "One",
		Blocks: []store.BlockInput{quote, quote, quote},
	}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	if _, _, err := s.UpsertCharacter(scope, "Drake", true); err != nil {
		t.Fatal(err)
	}
	eight, _, _ := s.UpsertCharacter(scope, "Drake 8", true)
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Drake", 1: "Drake 8", 2: "Drake 8"}); err != nil {
		t.Fatal(err)
	}
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/characters/"+eight.ID+"/merge", map[string]any{"targetName": "Drake", "noAlias": true}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("merge: %d", resp.StatusCode)
	}
	ps, _ := s.ListParagraphsRaw(ch.ID)
	for i, want := range []int{0, 2, 2} {
		if ps[i].Speaker != "Drake" || ps[i].SpeakerIndex != want {
			t.Fatalf("paragraph %d = %q #%d, want Drake #%d", i, ps[i].Speaker, ps[i].SpeakerIndex, want)
		}
	}
}

// splitAliasLLMBackend gives every line it is asked about back to Bert.
type splitAliasLLMBackend struct{}

func (splitAliasLLMBackend) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	return `[{"idx": 0, "speaker": "Bert"}, {"idx": 3, "speaker": "Bert"}]`, nil
}

// TestSplitCharacterAlias: splitting "Albert" off Bert takes the alias
// away, makes Albert a character, and re-judges Bert's lines only in the
// chapter that writes "Albert" - the tagged line moves, the one the model
// gives back to Bert stays his.
func TestSplitCharacterAlias(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, splitAliasLLMBackend{})
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{
		{Title: "One", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Morning,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Bert said.", Inline: true},
			{Kind: store.BlockText, Text: "The rain had stopped an hour ago and the yard was quiet."},
			{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Albert said.", Inline: true},
		}},
		{Title: "Two", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Nobody else is here.”", IsQuote: true},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	bert, _, _ := s.UpsertCharacter(scope, "Bert", false)
	if err := s.SetCharacterAliases(bert.ID, []string{"Albert", "Bertie"}); err != nil {
		t.Fatal(err)
	}
	one, _ := s.GetChapterByIdx(bookID, 0)
	two, _ := s.GetChapterByIdx(bookID, 1)
	if err := s.SetParagraphSpeakers(one.ID, map[int]string{0: "Bert", 3: "Bert"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphSpeakers(two.ID, map[int]string{0: "Bert"}); err != nil {
		t.Fatal(err)
	}
	url := ts.URL + "/api/books/" + bookID + "/characters/" + bert.ID + "/aliases/split"

	resp := doJSON(t, http.MethodPost, url, map[string]string{"alias": "Nobody"}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("splitting a name that isn't an alias: want 400, got %d", resp.StatusCode)
	}

	var body map[string]any
	resp = doJSON(t, http.MethodPost, url, map[string]string{"alias": "albert"}, &body)
	if resp.StatusCode != http.StatusAccepted || body["queued"] != float64(1) {
		t.Fatalf("split: %d %+v, want 202 with one chapter queued", resp.StatusCode, body)
	}
	if got, _ := s.GetCharacter(bert.ID); got == nil || !slices.Equal([]string(got.Aliases), []string{"Bertie"}) {
		t.Fatalf("Bert's aliases after the split: %+v", got)
	}
	if albert, _ := s.GetCharacterByName(scope, "Albert"); albert == nil {
		t.Fatal("no Albert character after the split")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		inFlight, queued := srv.Jobs.Snapshot()
		if len(inFlight) == 0 && len(queued) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("split didn't drain: inFlight=%+v queued=%+v", inFlight, queued)
		}
		time.Sleep(10 * time.Millisecond)
	}
	speakers := func(chapterID string) map[int]string {
		all, err := s.ListParagraphsRaw(chapterID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int]string{}
		for _, p := range all {
			out[p.Idx] = p.Speaker
		}
		return out
	}
	if got := speakers(one.ID); got[3] != "Albert" || got[0] != "Bert" {
		t.Fatalf("chapter one: %+v, want 0 Bert (kept) and 3 Albert (its speech tag)", got)
	}
	if got := speakers(two.ID); got[0] != "Bert" {
		t.Fatalf("chapter two never writes Albert, yet its line moved: %+v", got)
	}

	// The same through roster review: a split_alias proposal splits when
	// accepted, and with ?split=0 only takes the name off.
	review := ts.URL + "/api/books/" + bookID + "/roster-review/proposals"
	file := func(alias string) string {
		var filed struct {
			ID string `json:"id"`
		}
		resp := doJSON(t, http.MethodPost, review, map[string]any{"kind": "split_alias", "subject": "Bert", "detail": alias}, &filed)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("file split_alias %q: %d", alias, resp.StatusCode)
		}
		return filed.ID
	}
	if err := s.SetCharacterAliases(bert.ID, []string{"Bertie", "Sid"}); err != nil {
		t.Fatal(err)
	}
	resp = doJSON(t, http.MethodPost, review+"/"+file("Bertie")+"/accept", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("accept split_alias: %d", resp.StatusCode)
	}
	if c, _ := s.GetCharacterByName(scope, "Bertie"); c == nil {
		t.Error("no Bertie character after accepting the split")
	}
	resp = doJSON(t, http.MethodPost, review+"/"+file("Sid")+"/accept?split=0", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("accept split_alias with split=0: %d", resp.StatusCode)
	}
	if c, _ := s.GetCharacterByName(scope, "Sid"); c != nil {
		t.Error("split=0 still made Sid a character")
	}
	if got, _ := s.GetCharacter(bert.ID); got == nil || len(got.Aliases) != 0 {
		t.Fatalf("Bert's aliases after both proposals: %+v", got)
	}
}

// TestSplitCharacterAliasesMultiway: several names split off one character
// at once each become a character, in one pass over the chapters that
// write any of them; one bad name refuses the lot.
func TestSplitCharacterAliasesMultiway(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, splitAliasLLMBackend{})
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{
		{Title: "One", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Morning,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Bert said.", Inline: true},
			{Kind: store.BlockText, Text: "The rain had stopped an hour ago and the yard was quiet."},
			{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Albert said.", Inline: true},
		}},
		{Title: "Two", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Evening,”", IsQuote: true},
			{Kind: store.BlockText, Text: "Sid said.", Inline: true},
		}},
		{Title: "Three", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Nobody else is here.”", IsQuote: true},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	bert, _, _ := s.UpsertCharacter(scope, "Bert", false)
	if err := s.SetCharacterAliases(bert.ID, []string{"Albert", "Bertie", "Sid"}); err != nil {
		t.Fatal(err)
	}
	var chapters []*store.Chapter
	for i, lines := range []map[int]string{{0: "Bert", 3: "Bert"}, {0: "Bert"}, {0: "Bert"}} {
		ch, _ := s.GetChapterByIdx(bookID, i)
		if err := s.SetParagraphSpeakers(ch.ID, lines); err != nil {
			t.Fatal(err)
		}
		chapters = append(chapters, ch)
	}
	url := ts.URL + "/api/books/" + bookID + "/characters/" + bert.ID + "/aliases/split"

	resp := doJSON(t, http.MethodPost, url, map[string]any{"aliases": []string{"Albert", "Nobody"}}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("one name that isn't an alias: want 400, got %d", resp.StatusCode)
	}
	if got, _ := s.GetCharacter(bert.ID); got == nil || len(got.Aliases) != 3 {
		t.Fatalf("a refused split changed Bert's aliases: %+v", got)
	}

	var body map[string]any
	resp = doJSON(t, http.MethodPost, url, map[string]any{"aliases": []string{"Albert", "sid", "Sid"}}, &body)
	if resp.StatusCode != http.StatusAccepted || body["queued"] != float64(2) {
		t.Fatalf("split: %d %+v, want 202 with two chapters queued", resp.StatusCode, body)
	}
	if got, _ := s.GetCharacter(bert.ID); got == nil || !slices.Equal([]string(got.Aliases), []string{"Bertie"}) {
		t.Fatalf("Bert's aliases after the split: %+v", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		inFlight, queued := srv.Jobs.Snapshot()
		if len(inFlight) == 0 && len(queued) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("split didn't drain: inFlight=%+v queued=%+v", inFlight, queued)
		}
		time.Sleep(10 * time.Millisecond)
	}
	want := []map[int]string{{0: "Bert", 3: "Albert"}, {0: "Sid"}, {0: "Bert"}}
	for i, ch := range chapters {
		all, err := s.ListParagraphsRaw(ch.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range all {
			if w, ok := want[i][p.Idx]; ok && p.Speaker != w {
				t.Errorf("chapter %d line %d: speaker %q, want %q", i, p.Idx, p.Speaker, w)
			}
		}
	}
}

// TestMergeWithoutAlias: a merge asked for with noAlias moves the lines
// and leaves the target's other names as they were.
func TestMergeWithoutAlias(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title:  "One",
		Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	bert, _, _ := s.UpsertCharacter(scope, "Bert", false)
	stranger, _, _ := s.UpsertCharacter(scope, "Stranger", false)
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Stranger"}); err != nil {
		t.Fatal(err)
	}
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/characters/"+stranger.ID+"/merge", map[string]any{"targetName": "Bert", "noAlias": true}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("merge: %d", resp.StatusCode)
	}
	if got, _ := s.GetCharacter(bert.ID); got == nil || len(got.Aliases) != 0 {
		t.Fatalf("Bert's aliases after a merge without alias: %+v", got)
	}
	if all, _ := s.ListParagraphsRaw(ch.ID); len(all) != 1 || all[0].Speaker != "Bert" {
		t.Fatalf("the line didn't move to Bert: %+v", all)
	}

	// The same for a roster-review merge accepted with ?alias=0.
	if _, _, err := s.UpsertCharacter(scope, "Visitor", false); err != nil {
		t.Fatal(err)
	}
	review := ts.URL + "/api/books/" + bookID + "/roster-review/proposals"
	var filed struct {
		ID string `json:"id"`
	}
	resp = doJSON(t, http.MethodPost, review, map[string]any{"kind": "merge", "subject": "Visitor", "target": "Bert"}, &filed)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("file proposal: %d", resp.StatusCode)
	}
	resp = doJSON(t, http.MethodPost, review+"/"+filed.ID+"/accept?alias=0", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("accept: %d", resp.StatusCode)
	}
	if c, _ := s.GetCharacterByName(scope, "Visitor"); c != nil {
		t.Error("Visitor still a character after the merge")
	}
	if got, _ := s.GetCharacter(bert.ID); got == nil || len(got.Aliases) != 0 {
		t.Fatalf("Bert's aliases after a proposal accepted without alias: %+v", got)
	}
}
