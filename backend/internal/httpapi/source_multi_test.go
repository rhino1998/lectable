package httpapi

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/patreon/patreontest"
	"github.com/rhino1998/lectable/backend/internal/royalroad/royalroadtest"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// lockedLine is the titles waiting for review, the locked ones marked.
func lockedLine(b sourceTestBook) string {
	var parts []string
	for _, e := range b.Source.Review {
		title := e.Source + ": " + e.Title
		if e.Locked {
			title += " (locked)"
		}
		parts = append(parts, title)
	}
	return strings.Join(parts, "|")
}

// A serial on Royal Road whose advance chapters are on Patreon: the book
// tracks both, takes each chapter once, and shows what Patreon has even
// before the library is signed in to it.
func TestBookWithTwoSources(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	road := royalroadtest.New(t)
	pat := patreontest.New(t)
	srv.Sources = []websource.Source{road.Source(), pat.Source()}
	road.AddChapter("Chapter 1: Setting Out", `<p>The road was long.</p>`)
	road.AddChapter("Chapter 2: The Inn", `<p>The inn was warm.</p>`)
	road.AddChapter("Chapter 3: Onward", `<p>They left at dawn.</p>`)

	summit := []string{
		"The summit was further than it had looked from the valley floor that morning.",
		"Sam counted the switchbacks twice and got a different number each time, which seemed about right.",
	}
	pat.AddPublic("Welcome", `<p>Thanks for joining.</p>`)
	pat.AddPoll("Which side story?")
	pat.AddPatronsOnly("Chapter 1: Setting Out", "The road was long.")
	pat.AddPatronsOnly("Chapter 2 - The Inn", "The inn was warm.")
	pat.AddPatronsOnly("Chapter 3: Onward", "They left at dawn.")
	pat.AddPatronsOnly("Chapter 4: The Pass", "Snow closed the pass behind them.")
	pat.AddPatronsOnly("Chapter 5: Summit", summit...)

	var created sourceTestBook
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink}, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	if book := waitSourceSynced(t, ts.URL, created.ID); book.ChapterCount != 3 || len(book.Source.Tracked) != 1 {
		t.Fatalf("imported %d chapters, %d sources", book.ChapterCount, len(book.Source.Tracked))
	}

	// Patreon is added, signed out: the chapters the book has are
	// recognized by their titles, and the two it doesn't are there to
	// see, locked.
	var added trackedSourceDTO
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+created.ID+"/sources", addBookSourceRequest{URL: pat.Link()}, &added); resp.StatusCode != http.StatusCreated {
		t.Fatalf("add source: %d", resp.StatusCode)
	}
	if added.Name != "Patreon" || added.Kind != sourcePatreon {
		t.Errorf("added = %+v", added)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 3 || book.Source.Name != "Royal Road + Patreon" || len(book.Source.Tracked) != 2 {
		t.Fatalf("after adding Patreon: %d chapters, %q, %d sources", book.ChapterCount, book.Source.Name, len(book.Source.Tracked))
	}
	if got := lockedLine(book); got != "Patreon: Welcome|Patreon: Chapter 4: The Pass (locked)|Patreon: Chapter 5: Summit (locked)" {
		t.Fatalf("review = %q", got)
	}
	if p := book.Source.Tracked[1]; p.ID != added.ID || p.Waiting != 1 || p.Locked != 2 || p.CheckedAt == 0 || p.Error != "" {
		t.Errorf("Patreon as tracked = %+v", p)
	}
	// The same work can't be tracked twice, here or by another book.
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+created.ID+"/sources", addBookSourceRequest{URL: pat.Link()}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("adding it again: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: pat.Link()}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("importing it as a book of its own: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A locked entry agreed to stays where it is: the check stops at it.
	stuck := importReviewed(t, ts.URL, book, "Welcome")
	if stuck.ChapterCount != 3 || stuck.Source.Blocked != "Chapter 4: The Pass" {
		t.Fatalf("locked chapters agreed to: %d chapters, blocked at %q", stuck.ChapterCount, stuck.Source.Blocked)
	}

	// Signing in takes a session Patreon knows, and checks the book again.
	resp = doJSON(t, http.MethodPut, ts.URL+"/api/settings/patreon", patreonSessionRequest{SessionID: "made-up"}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a session Patreon doesn't know: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doJSON(t, http.MethodPut, ts.URL+"/api/settings/patreon", patreonSessionRequest{SessionID: "session_id=" + patreontest.MemberSession}, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("sign in: %d", resp.StatusCode)
	}
	resp.Body.Close()
	var settings sourceSettingsDTO
	getJSONInto(t, ts.URL+"/api/settings/sources", &settings)
	if settings.PatreonAccount != "Sam Reader" {
		t.Errorf("signed in as %q", settings.PatreonAccount)
	}
	book = waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 5 || book.Source.Blocked != "" || len(book.Source.Review) != 0 {
		t.Fatalf("signed in: %d chapters, blocked %q, review %q", book.ChapterCount, book.Source.Blocked, lockedLine(book))
	}
	if title, texts := chapterTexts(t, s, created.ID, 3); title != "Chapter 4: The Pass" || texts[len(texts)-1] != "Snow closed the pass behind them." {
		t.Errorf("chapter 3 = %q %q", title, texts)
	}
	// A chapter from an added source keeps its id apart from the first's.
	if ids, _ := s.ChapterSourceIDs(created.ID); len(ids) != 5 || !strings.HasPrefix(ids[4], "patreon:") || strings.Contains(ids[0], ":") {
		t.Errorf("chapter source ids = %v", ids)
	}

	// Royal Road catches up: chapter 4 under the same title, chapter 5
	// under another. Neither is taken twice - the first is known by its
	// title before it is fetched, the second by its text once it is.
	road.AddChapter("Chapter 4: The Pass", `<p>Snow closed the pass behind them.</p>`)
	road.AddChapter("5. At the Top of the World", `<p>`+summit[0]+`</p><p>`+summit[1]+`</p>`)
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/"+created.ID+"/source/sync", nil, nil)
	resp.Body.Close()
	book = waitSourceSynced(t, ts.URL, created.ID)
	if got := lockedLine(book); book.ChapterCount != 5 || got != "Royal Road: 5. At the Top of the World" {
		t.Fatalf("after Royal Road caught up: %d chapters, review %q", book.ChapterCount, got)
	}
	book = importReviewed(t, ts.URL, book)
	if book.ChapterCount != 5 || len(book.Source.Review) != 0 || book.Source.Error != "" {
		t.Fatalf("after importing the retitled chapter: %d chapters, review %q, error %q", book.ChapterCount, lockedLine(book), book.Source.Error)
	}
	// And stays known on the next check.
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/"+created.ID+"/source/sync", nil, nil)
	resp.Body.Close()
	if book = waitSourceSynced(t, ts.URL, created.ID); book.ChapterCount != 5 || len(book.Source.Review) != 0 {
		t.Fatalf("a later check: %d chapters, review %q", book.ChapterCount, lockedLine(book))
	}

	// A source can be dropped; what it gave the book stays.
	resp = doJSON(t, http.MethodDelete, ts.URL+"/api/books/"+created.ID+"/sources/"+added.ID, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove source: %d", resp.StatusCode)
	}
	resp.Body.Close()
	var after sourceTestBook
	getJSONInto(t, ts.URL+"/api/books/"+created.ID, &after)
	if after.ChapterCount != 5 || after.Source.Name != "Royal Road" || len(after.Source.Tracked) != 1 {
		t.Errorf("after removing Patreon: %d chapters, %q", after.ChapterCount, after.Source.Name)
	}
}

func TestMatchOtherSources(t *testing.T) {
	others := []store.SourcedChapter{
		{Idx: 0, Title: "Chapter 1: Setting Out"},
		{Idx: 1, Title: "Chapter 2: Interlude"},
		{Idx: 2, Title: "Chapter 3"},
		{Idx: 3, Title: "Epilogue"},
		{Idx: 4, Title: "Epilogue"},
		{Idx: 5, Title: "5-19. Creature of the Wind"},
		{Idx: 6, Title: "5-20. Dangling"},
		{Idx: 7, Title: "6-1. Twice"},
		{Idx: 8, Title: "6-1. Twice Again"},
	}
	planned := []websource.ChapterRef{
		{ID: "a", Title: "1 - Setting Out"},
		{ID: "b", Title: "Chapter 9: Interlude"}, // another interlude
		{ID: "c", Title: "Chapter 3"},
		{ID: "d", Title: "Epilogue"},        // which of two?
		{ID: "e", Title: "Chapter 4: New"},  // not in the book
		{ID: "f", Title: "Chapter 5: Twin"}, // listed twice
		{ID: "g", Title: "Chapter 5: Twin"},
		{ID: "h", Title: "Chapter 1: Setting Out"}, // the book has it by id
		{ID: "i", Title: "5-19. The Wind"},         // renamed: known by its numbering
		{ID: "j", Title: "5-20. Dangling"},
		{ID: "k", Title: "5-21. Good Things"}, // not in the book
		{ID: "l", Title: "6-1. Once"},         // which of two?
	}
	got := strings.Join(matchOtherSources(others, planned, map[string]bool{"h": true}), ",")
	if got != "a,c,j,i" {
		t.Errorf("matched %q, want a,c,j,i", got)
	}
}

// A creator who posts each chapter twice - early for patrons, then again
// for everyone - lists two entries of one name: the locked one is left
// out, and the open one is then known as the chapter the book has.
func TestLockedCopies(t *testing.T) {
	others := []store.SourcedChapter{
		{Idx: 0, Title: "Chapter 24"},
		{Idx: 1, Title: "Chapter 25"},
		{Idx: 2, Title: "5-19. Creature of the Wind"},
		{Idx: 3, Title: "Interlude"},
		{Idx: 4, Title: "Interlude"},
	}
	planned := []websource.ChapterRef{
		{ID: "24-early", Title: "Chapter 24"},
		{ID: "25-early", Title: "Chapter  25 "},
		{ID: "26-early", Title: "Chapter 26"}, // not in the book
		{ID: "wind-early", Title: "5-19. The Wind"},
		{ID: "interlude-early", Title: "Interlude"}, // which of two?
		{ID: "24", Title: "Chapter 24", Unlocked: true},
		{ID: "25", Title: "Chapter 25", Unlocked: true},
		{ID: "26", Title: "Chapter 26", Unlocked: true},
	}
	have := map[string]bool{}
	locked := lockedCopies(others, planned, have)
	if got := strings.Join(locked, ","); got != "24-early,25-early,wind-early" {
		t.Errorf("locked copies = %q", got)
	}
	// Before, the two entries of each name kept either from matching.
	// (the renamed one is alone in its numbering either way).
	if got := strings.Join(matchOtherSources(others, planned, have), ","); got != "wind-early" {
		t.Errorf("with the locked copies in the way, matched %q", got)
	}
	for _, id := range locked {
		have[id] = true
	}
	if got := strings.Join(matchOtherSources(others, planned, have), ","); got != "24,25" {
		t.Errorf("with them left out, matched %q, want 24,25", got)
	}
}

// Titles are compared as folded: spacing and look-alike characters don't
// make two copies of a chapter different ones.
func TestTitleComparisonsFold(t *testing.T) {
	for _, pair := range [][2]string{
		{"Chapter 12: The Caf\u00e9", "Chapter\u00a012:  The Cafe\u0301 "},
		{"Chapter 12: Onward", "Chapter\t12:\nOnward\u200b"},
		{"5-19. The Wind", "\uff15-\uff11\uff19.\u00a0The  Wind"},
		{"Chapter 7", " Chapter\u00a07 "},
	} {
		if a, b := titleKey(pair[0]), titleKey(pair[1]); a != b {
			t.Errorf("titleKey(%q) = %q, titleKey(%q) = %q", pair[0], a, pair[1], b)
		}
		an, aok := chapterNumber(pair[0])
		bn, bok := chapterNumber(pair[1])
		if an != bn || aok != bok {
			t.Errorf("chapterNumber(%q) = %d,%v, chapterNumber(%q) = %d,%v", pair[0], an, aok, pair[1], bn, bok)
		}
		if a, b := numberingKey(pair[0]), numberingKey(pair[1]); a != b {
			t.Errorf("numberingKey(%q) = %q, numberingKey(%q) = %q", pair[0], a, pair[1], b)
		}
	}
	// And so does the matching built on them.
	others := []store.SourcedChapter{{Idx: 0, Title: "Chapter\u00a012:  The Cafe\u0301 "}}
	planned := []websource.ChapterRef{{ID: "a", Title: "Chapter 12: The Caf\u00e9"}}
	if got := matchOtherSources(others, planned, map[string]bool{}); len(got) != 1 {
		t.Errorf("matched %v, want the one entry", got)
	}
}

func TestApplyTitleRule(t *testing.T) {
	refs := []websource.ChapterRef{
		{ID: "1", Title: "TWI 10.71 (Pt. 1) - The Garden"},
		{ID: "2", Title: "Side Stories Poll"},
		{ID: "3", Title: "TWI 10.72"},
	}
	line := func(rule *store.TitleRule) string {
		var out []string
		for _, ref := range applyTitleRule(rule, refs) {
			out = append(out, ref.ID+"="+ref.Title)
		}
		return strings.Join(out, "|")
	}
	for _, c := range []struct {
		rule *store.TitleRule
		want string
	}{
		{nil, "1=TWI 10.71 (Pt. 1) - The Garden|2=Side Stories Poll|3=TWI 10.72"},
		// A filter alone keeps the titles.
		{&store.TitleRule{Pattern: `^TWI`, Only: true}, "1=TWI 10.71 (Pt. 1) - The Garden|3=TWI 10.72"},
		// The output is made from the capture groups; an empty group leaves a tidy title.
		{&store.TitleRule{Pattern: `^TWI (\d+\.\d+)(?: \(Pt\. (?P<part>\d)\))?`, Output: "Chapter $1 ${part}", Only: true}, "1=Chapter 10.71 1|3=Chapter 10.72"},
		// Without Only, what doesn't match stays as it is.
		{&store.TitleRule{Pattern: `^TWI (\S+).*`, Output: "$1"}, "1=10.71|2=Side Stories Poll|3=10.72"},
		// A rule that no longer compiles changes nothing.
		{&store.TitleRule{Pattern: `(`, Only: true}, "1=TWI 10.71 (Pt. 1) - The Garden|2=Side Stories Poll|3=TWI 10.72"},
	} {
		if got := line(c.rule); got != c.want {
			t.Errorf("rule %+v:\n got %s\nwant %s", c.rule, got, c.want)
		}
	}
}

// Titles are cleaned before anything reads them, and a rule's pattern sees
// them with Unicode's look-alike forms folded - as Patreon lists them,
// stray double spaces and all.
func TestTitleRuleCleansAndFolds(t *testing.T) {
	refs := []websource.ChapterRef{
		{ID: "1", Title: "Path of Dragons  18 - Chapter 6 -  The Secret Life\nof Trees "},
		{ID: "2", Title: "Path\u00a0of Dragons 18 \u2013 Chapter\u200b 7 - Bogot\u00e1"},
		{ID: "3", Title: "\uff30ath of Dragons 18 - Chapter \uff18 - Caf\u0065\u0301\u2026"},
		{ID: "4", Title: "Weekly\tCheck-In"},
	}
	titles := func(rule *store.TitleRule) string {
		var out []string
		for _, ref := range applyTitleRule(rule, refs) {
			out = append(out, ref.Title)
		}
		return strings.Join(out, "|")
	}
	// No rule: spacing put right, the characters themselves left alone.
	if got, want := titles(nil), "Path of Dragons 18 - Chapter 6 - The Secret Life of Trees|Path of Dragons 18 \u2013 Chapter 7 - Bogot\u00e1|\uff30ath of Dragons 18 - Chapter \uff18 - Caf\u0065\u0301\u2026|Weekly Check-In"; got != want {
		t.Errorf("cleaned:\n got %q\nwant %q", got, want)
	}
	// A pattern typed with single spaces and plain letters matches them all.
	rule := &store.TitleRule{Pattern: `^Path of Dragons (\d+) [-\x{2013}] Chapter (\d+) - (.+)$`, Output: "$1-$2. $3", Only: true}
	if got, want := titles(rule), "18-6. The Secret Life of Trees|18-7. Bogot\u00e1|18-8. Caf\u00e9..."; got != want {
		t.Errorf("ruled:\n got %q\nwant %q", got, want)
	}
}

// A source's title rule is kept with it: it names and filters what an
// import takes, and what every later check lists.
func TestSourceTitleRule(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	road := royalroadtest.New(t)
	srv.Sources = []websource.Source{road.Source()}
	road.AddChapter("Book 1 - Ch 1: Setting Out", `<p>The road was long.</p>`)
	road.AddChapter("Cover poll", `<p>Vote.</p>`)
	road.AddChapter("Book 1 - Ch 2: The Inn", `<p>The inn was warm.</p>`)

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink, TitleRule: &sourceTitleRuleDTO{Pattern: "("}}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a pattern that doesn't parse: %d", resp.StatusCode)
	}
	resp.Body.Close()
	rule := sourceTitleRuleDTO{Pattern: `Ch (\d+): (.*)`, Output: "Chapter $1 - $2", Only: true}
	var created sourceTestBook
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink, TitleRule: &rule}, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 2 || len(book.Source.Review) != 0 {
		t.Fatalf("imported %d chapters, review %q", book.ChapterCount, reviewLine(book))
	}
	if title, _ := chapterTexts(t, s, created.ID, 1); title != "Chapter 2 - The Inn" {
		t.Errorf("chapter 1 = %q", title)
	}
	if got := book.Source.Tracked[0].TitleRule; got == nil || *got != rule {
		t.Errorf("the source's rule = %+v", got)
	}
	// A later check goes by it too.
	road.AddChapter("Hiatus notice", `<p>Back soon.</p>`)
	road.AddChapter("Book 1 - Ch 3: Onward", `<p>They left at dawn.</p>`)
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/"+created.ID+"/source/sync", nil, nil)
	resp.Body.Close()
	if book = waitSourceSynced(t, ts.URL, created.ID); reviewLine(book) != "Chapter 3 - Onward" {
		t.Fatalf("review under the rule = %q", reviewLine(book))
	}
	// Cleared, everything the site lists is back, under its own names.
	resp = doJSON(t, http.MethodPut, ts.URL+"/api/books/"+created.ID+"/sources/"+book.Source.Tracked[0].ID+"/title-rule", sourceTitleRuleDTO{}, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear the rule: %d", resp.StatusCode)
	}
	resp.Body.Close()
	book = waitSourceSynced(t, ts.URL, created.ID)
	if got := reviewLine(book); got != "Cover poll*|Hiatus notice*|Book 1 - Ch 3: Onward" || book.Source.Tracked[0].TitleRule != nil {
		t.Fatalf("review with no rule = %q", got)
	}
}
