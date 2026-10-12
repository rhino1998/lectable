package httpapi

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/semantic"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// factLLM answers the fact-finding calls: the first round names kobolds
// (with a quote from the text), giants (one individual) and elves (a
// quote that isn't in the text); the second settles kobolds and turns
// giants down. Anything else is a casting.
type factLLM struct{ fakeLLMBackend }

func (l factLLM) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	switch {
	case strings.HasPrefix(userPrompt, "Passages:"):
		return `{"facts": [
			{"kind": "Kobolds", "fact": "Kobolds do not live long.", "evidence": "“Kobolds only live to fifteen or so.”"},
			{"kind": "giant", "fact": "Giants are old.", "evidence": "The giant was very old."},
			{"kind": "elves", "fact": "Elves live forever.", "evidence": "Elves never die."}]}`, nil
	case strings.HasPrefix(userPrompt, "Kind: kobold"):
		return `{"kind": "kobold", "fact": "Kobolds live to about fifteen.", "evidence": "Kobolds only live to fifteen or so."}`, nil
	case strings.HasPrefix(userPrompt, "Kind: "):
		return `{"kind": "giant", "fact": "", "evidence": ""}`, nil
	}
	return l.fakeLLMBackend.LLMGenerate(ctx, systemPrompt, userPrompt, temp, maxTokens)
}

// TestFindFacts runs the fact-finding pass over a book with a stand-in
// search and model: what it finds waits as a proposed fact with its quote
// and chapter, is in force only once accepted - which recasts the
// characters it is about - and a skipped one is not proposed again.
func TestFindFacts(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, factLLM{})
	bookID := createTestBook(t, s, "Tower", 1, "“Kobolds only live to fifteen or so.”", "The giant was very old.")
	book, _ := s.GetBook(bookID)
	ch, _ := s.GetChapterByIdx(bookID, 0)
	paragraphs, _ := s.ListParagraphsRaw(ch.ID)
	search := func(ctx context.Context, bookIDs []string, query string, limit int) ([]semantic.Hit, error) {
		var hits []semantic.Hit
		for _, p := range paragraphs {
			hits = append(hits, semantic.Hit{BookID: bookID, ChapterID: ch.ID, ParagraphID: p.ID, Text: p.Text})
		}
		return hits, nil
	}
	scope := store.SeriesScope(book)
	grak, _, _ := s.UpsertCharacter(scope, "Grak the Kobold", false)
	will, _, _ := s.UpsertCharacter(scope, "Will", false)
	for _, c := range []store.Character{grak, will} {
		if err := s.SetCharacterSummary(c.ID, "Speak as a young man. He is brisk.", "A line to read."); err != nil {
			t.Fatal(err)
		}
	}

	if err := srv.findFacts(context.Background(), book, search); err != nil {
		t.Fatal(err)
	}
	factsURL := ts.URL + "/api/books/" + bookID + "/facts"
	proposed := func() []worldFactDTO {
		t.Helper()
		var all, out []worldFactDTO
		doJSON(t, http.MethodGet, factsURL, nil, &all)
		for _, f := range all {
			if f.Proposed {
				out = append(out, f)
			}
		}
		return out
	}
	got := proposed()
	if len(got) != 1 || got[0].Key != "kobold" || got[0].Fact != "Kobolds live to about fifteen." ||
		got[0].Evidence != "Kobolds only live to fifteen or so." || got[0].EvidenceBookID != bookID ||
		got[0].EvidenceChapterIdx == nil || *got[0].EvidenceChapterIdx != 0 || got[0].Replaces == "" {
		t.Fatalf("proposed = %+v, want the kobold fact with its quote and chapter, replacing the baseline one", got)
	}
	if facts, _ := store.BookWorldFacts(s, book); len(facts) == 0 || facts[0].Scope == scope {
		t.Fatalf("a proposed fact is in force before it is accepted: %+v", facts[0])
	}

	// Accepting it, reworded, puts it in force and recasts the kobold only.
	var saved upsertFactResponse
	resp := doJSON(t, http.MethodPut, factsURL, upsertWorldFactRequest{ID: got[0].ID, Key: "kobold", Fact: "Short-lived: old at fifteen."}, &saved)
	if resp.StatusCode != http.StatusOK || saved.Recast != 1 {
		t.Fatalf("accept: status %d, recast %d, want 1", resp.StatusCode, saved.Recast)
	}
	facts, _ := store.BookWorldFacts(s, book)
	if len(facts) == 0 || facts[0].Key != "kobold" || facts[0].Fact != "Short-lived: old at fifteen." || facts[0].Source != store.WorldFactLLM {
		t.Fatalf("after accepting: %+v", facts)
	}
	if len(proposed()) != 0 {
		t.Fatalf("still proposed after accepting: %+v", proposed())
	}
	// Editing it again accepts nothing, so recasts nobody.
	doJSON(t, http.MethodPut, factsURL, upsertWorldFactRequest{ID: got[0].ID, Key: "kobold", Fact: "Short-lived."}, &saved)
	if saved.Recast != 0 {
		t.Fatalf("an edit recast %d characters", saved.Recast)
	}

	// A skipped proposal is neither listed nor proposed again.
	if _, err := s.ProposeWorldFact(scope, store.WorldFact{Key: "imp", Fact: "Imps live a year."}); err != nil {
		t.Fatal(err)
	}
	imp := proposed()
	if len(imp) != 1 {
		t.Fatalf("proposed = %+v, want the imp fact", imp)
	}
	if resp := doJSON(t, http.MethodPost, factsURL+"/"+imp[0].ID+"/skip", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("skip: status %d", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPost, factsURL+"/"+imp[0].ID+"/skip", nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("skip twice: status %d", resp.StatusCode)
	}
	if waiting, err := s.ProposeWorldFact(scope, store.WorldFact{Key: "Imp", Fact: "Imps live two years."}); err != nil || waiting {
		t.Fatalf("a skipped fact was proposed again: %v, %v", waiting, err)
	}
	if got := proposed(); len(got) != 0 {
		t.Fatalf("proposed after skipping = %+v", got)
	}
}

// TestKindAccents checks the per-kind accent list - a kind a fact names
// and one only the castings name - and that choosing an accent writes it
// into the series' fact for the kind, keeping the rest of the general
// fact, and recasts only the characters cast otherwise.
func TestKindAccents(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, fakeLLMBackend{})
	bookID := createTestBook(t, s, "Tower", 1, "“Hi.”")
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	cast := func(name, summary string) store.Character {
		t.Helper()
		c, _, err := s.UpsertCharacter(scope, name, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetCharacterSummary(c.ID, summary, "A line to read."); err != nil {
			t.Fatal(err)
		}
		return c
	}
	cast("Grak", "Speak as a young kobold man. Adopt a General American accent.")
	cast("Loth", "Speak as a middle-aged kobold woman with a raspy voice. Use a General American accent.")
	odd := cast("Snik the Kobold", "Speak as a small creature. Adopt a guttural Cockney accent.")
	cast("Will", "Speak as a young man. Adopt a General American accent.")
	for _, n := range []string{"Ssar", "Zel", "Olesm"} {
		cast(n, "Speak as a brisk drake officer. Use a standard British accent.")
	}

	// Words every roster's castings open with name no kind.
	for _, series := range []string{"Other", "Third", "Fourth"} {
		other, _ := s.GetBook(createTestBook(t, s, series, 1, "“Hi.”"))
		c, _, _ := s.UpsertCharacter(store.SeriesScope(other), "Someone", false)
		if err := s.SetCharacterSummary(c.ID, "Speak as a brisk officer. Adopt a General American accent.", "A line."); err != nil {
			t.Fatal(err)
		}
	}

	accentsURL := ts.URL + "/api/books/" + bookID + "/facts/accents"
	var kinds []kindAccentsDTO
	doJSON(t, http.MethodGet, accentsURL, nil, &kinds)
	byKey := map[string]kindAccentsDTO{}
	for _, k := range kinds {
		byKey[k.Key] = k
	}
	kobold, drake := byKey["kobold"], byKey["drake"]
	if kobold.Members != 3 || !kobold.Global || len(kobold.Accents) != 2 || kobold.Accents[0].Accent != "General American" ||
		len(kobold.Accents[1].Characters) != 1 || kobold.Accents[1].Characters[0].ID != odd.ID {
		t.Fatalf("kobold = %+v, want 3 members under the general fact, Snik the one Cockney", kobold)
	}
	if drake.Members != 3 || drake.Fact != "" || len(drake.Accents) != 1 || drake.Accents[0].Accent != "British" {
		t.Fatalf("drake = %+v, want a kind found from the castings alone, all British", drake)
	}
	if len(kinds) == 0 || kinds[0].Key != "kobold" {
		t.Fatalf("kinds = %+v, want the one with an outlier first", kinds)
	}
	for _, generic := range []string{"speak", "brisk", "officer"} {
		if _, ok := byKey[generic]; ok {
			t.Errorf("%q is listed as a kind", generic)
		}
	}
	var saved upsertFactResponse
	resp := doJSON(t, http.MethodPost, accentsURL, setKindAccentRequest{Key: "kobold", Accent: "General American"}, &saved)
	if resp.StatusCode != http.StatusOK || saved.Recast != 1 {
		t.Fatalf("set accent: status %d, recast %d, want 1", resp.StatusCode, saved.Recast)
	}
	facts, _ := store.BookWorldFacts(s, book)
	if len(facts) == 0 || facts[0].Scope != scope || facts[0].Key != "kobold" ||
		!strings.HasSuffix(facts[0].Fact, accentSentence("General American")) || !strings.Contains(facts[0].Fact, "reptilian") {
		t.Fatalf("kobold fact after choosing = %+v, want the general fact's text and then the accent", facts[0])
	}
	// A class the fact has is kept when its accent is chosen again.
	if _, err := s.UpsertWorldFact(scope, store.WorldFact{ID: facts[0].ID, Key: facts[0].Key, Fact: facts[0].Fact, Class: store.WordRace}); err != nil {
		t.Fatal(err)
	}
	doJSON(t, http.MethodPost, accentsURL, setKindAccentRequest{Key: "kobold", Accent: "General American"}, &saved)
	if facts, _ = store.BookWorldFacts(s, book); facts[0].Class != store.WordRace {
		t.Fatalf("kobold fact lost its class: %+v", facts[0])
	}
	// Choosing again replaces the accent rather than adding a second.
	doJSON(t, http.MethodPost, accentsURL, setKindAccentRequest{Key: "kobold", Accent: "Welsh"}, &saved)
	facts, _ = store.BookWorldFacts(s, book)
	if !strings.HasSuffix(facts[0].Fact, accentSentence("Welsh")) || strings.Contains(facts[0].Fact, "General American") || saved.Recast != 3 {
		t.Fatalf("after choosing again: %q, recast %d, want one accent sentence and all three recast", facts[0].Fact, saved.Recast)
	}
	if resp := doJSON(t, http.MethodPost, accentsURL, setKindAccentRequest{Key: "unicorn", Accent: "Welsh"}, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown kind: status %d", resp.StatusCode)
	}
}

// TestFindTitles: a word leading several characters' names that the text
// writes as a title is proposed as a title fact, with its sentence and
// who holds it; once accepted (no fact text needed, nobody recast) the
// roster rules stop reading it as a name, and a skipped one stays skipped.
func TestFindTitles(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "Hive", 1,
		"Zhao Wei bowed, and Zhao Na did not.",
		"“The Prognugator will see you now,” the Worker said.",
		"Two of the Prognugators stood at the gate.",
		"Every Hive had its Prognugator.",
		"A Prognugator does not sleep, and no Prognugator had.",
		"“Ask another Prognugator,” Klbkch said.",
		"Tersk and Dekass waited. So did Tesy, beside a trader. The trader left; a relc is a kind of spear, and one relc broke.",
		"The Drake laughed. A Drake always did. The Drakes left.")
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	for _, name := range []string{"Prognugator", "Prognugator Tersk", "Prognugator Dekass", "Zhao Wei", "Zhao Na", "Drake Tesy", "Drake Relc", "Hive Trader", "Hive Relc"} {
		if _, _, err := s.UpsertCharacter(scope, name, false); err != nil {
			t.Fatal(err)
		}
	}
	// A kind the library has a fact for is no title.
	if _, err := s.UpsertWorldFact(store.GlobalFactScope, store.WorldFact{Key: "drake", Fact: "Drakes hiss."}); err != nil {
		t.Fatal(err)
	}
	if err := srv.findTitlesByRule(context.Background(), book); err != nil {
		t.Fatal(err)
	}
	factsURL := ts.URL + "/api/books/" + bookID + "/facts"
	proposed := func() []worldFactDTO {
		t.Helper()
		var all, out []worldFactDTO
		doJSON(t, http.MethodGet, factsURL, nil, &all)
		for _, f := range all {
			if f.Proposed {
				out = append(out, f)
			}
		}
		return out
	}
	got := proposed()
	if len(got) != 1 || got[0].Key != "Prognugator" || got[0].Class != store.WordTitle || got[0].Fact != "" ||
		got[0].Evidence != "“The Prognugator will see you now,” the Worker said." || got[0].EvidenceChapterIdx == nil ||
		strings.Join(got[0].UsedBy, ", ") != "Prognugator Tersk, Prognugator Dekass" {
		t.Fatalf("proposed = %+v", got)
	}
	if srv.titles(book) != nil {
		t.Fatal("a proposed title is in force before it is accepted")
	}
	chars, _ := s.ListCharacters(scope)
	bare := chars[slices.IndexFunc(chars, func(c store.Character) bool { return c.Name == "Prognugator" })]
	if shortNameTarget(bare, chars[:2], nil) == nil {
		t.Fatal("without the title, Prognugator reads as short for Prognugator Tersk")
	}

	var saved upsertFactResponse
	if resp := doJSON(t, http.MethodPut, factsURL, upsertWorldFactRequest{ID: got[0].ID, Key: "Prognugator", Class: store.WordTitle}, &saved); resp.StatusCode != http.StatusOK || saved.Recast != 0 {
		t.Fatalf("accept: %d, %+v", resp.StatusCode, saved)
	}
	titles := srv.titles(book)
	if !titles.Has("prognugator") || shortNameTarget(bare, chars[:2], titles) != nil {
		t.Fatalf("accepted title not in force: %v", titles)
	}
	if resp := doJSON(t, http.MethodPut, factsURL, upsertWorldFactRequest{Key: "Revalantor"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a fact with no text that is no title: %d", resp.StatusCode)
	}
	// Found again: nothing new to propose.
	if err := srv.findTitlesByRule(context.Background(), book); err != nil {
		t.Fatal(err)
	}
	if again := proposed(); len(again) != 0 {
		t.Errorf("proposed again: %+v", again)
	}
}

// wordLLM classes the words findWorldWords asks about.
type wordLLM struct{ fakeLLMBackend }

func (l wordLLM) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	if strings.HasPrefix(userPrompt, "Word: ") {
		return `{"words": [{"word": "Drake", "is": "race"}, {"word": "Pallassian", "is": "nationality"}, {"word": "Pallass", "is": "place"},
			{"word": "Trader", "is": "role"}, {"word": "Shivertail", "is": "name"}, {"word": "Elf", "is": "race"}]}`, nil
	}
	return l.fakeLLMBackend.LLMGenerate(ctx, systemPrompt, userPrompt, temp, maxTokens)
}

// TestFindWorldWords: the words names share are classed by the model. A
// new word waits as a pending fact with its class; a word the series has
// a fact about gets the class proposed for that fact, which keeps its
// text when the class is accepted and when it is turned down; a family
// name and a word the model wasn't asked about are left out. With the
// classes in force the roster rules mark "Drake Trader" a role.
func TestFindWorldWords(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, wordLLM{})
	var text []string
	for range 5 {
		text = append(text, "The Drake left the Pallassian gates. A trader in Pallass saw a Drake. Of Shivertail, the Trader said nothing; the Pallassians of Pallass did.")
	}
	bookID := createTestBook(t, s, "Walls", 1, text...)
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	for _, name := range []string{"Drake Trader", "Drake Tesy", "Pallassian Drake", "Pallassian Gnoll", "Lord of Pallass", "Queen of Pallass", "Tekshia Shivertail", "Zevara Shivertail", "Human Trader"} {
		if _, _, err := s.UpsertCharacter(scope, name, false); err != nil {
			t.Fatal(err)
		}
	}
	lifespan, err := s.UpsertWorldFact(scope, store.WorldFact{Key: "drake", Fact: "Drakes live to seventy or eighty."})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.findWorldWords(context.Background(), book); err != nil {
		t.Fatal(err)
	}
	factsURL := ts.URL + "/api/books/" + bookID + "/facts"
	list := func() map[string]worldFactDTO {
		t.Helper()
		var all []worldFactDTO
		doJSON(t, http.MethodGet, factsURL, nil, &all)
		out := map[string]worldFactDTO{}
		for _, f := range all {
			if !f.Global {
				out[f.Key] = f
			}
		}
		return out
	}
	got := list()
	if len(got) != 4 {
		t.Fatalf("facts = %+v", got)
	}
	for word, class := range map[string]store.WordClass{"Pallassian": store.WordNationality, "Pallass": store.WordPlace, "Trader": store.WordRole} {
		if f := got[word]; !f.Proposed || f.Class != class || f.Fact != "" || f.Evidence == "" || len(f.UsedBy) < 2 {
			t.Errorf("%s = %+v, want a proposed %s", word, f, class)
		}
	}
	drake := got["drake"]
	if drake.Proposed || drake.Class != "" || drake.ProposedClass != store.WordRace || drake.Fact == "" || len(drake.UsedBy) != 3 {
		t.Fatalf("drake = %+v, want its fact in force with race proposed", drake)
	}
	if srv.titles(book) != nil {
		t.Fatal("a proposed class is in force before it is accepted")
	}

	// Accepting the class keeps the text; turning one down keeps the fact.
	var saved upsertFactResponse
	doJSON(t, http.MethodPut, factsURL, upsertWorldFactRequest{ID: drake.ID, Key: drake.Key, Fact: drake.Fact, Class: store.WordRace}, &saved)
	doJSON(t, http.MethodPut, factsURL, upsertWorldFactRequest{ID: got["Trader"].ID, Key: "Trader", Class: store.WordRole}, &saved)
	if resp := doJSON(t, http.MethodPost, factsURL+"/"+got["Pallass"].ID+"/skip", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("skip: %d", resp.StatusCode)
	}
	got = list()
	if f := got["drake"]; f.Class != store.WordRace || f.ProposedClass != "" || f.Fact != lifespan.Fact || f.ID != lifespan.ID {
		t.Fatalf("accepted drake = %+v", f)
	}
	words := srv.titles(book)
	if !words.Has("drake") || !words.Has("trader") || words.Has("pallass") || words.Has("pallassian") {
		t.Fatalf("words in force = %v", words)
	}
	// Reset: what the pass proposed goes, accepted or not; the reader's
	// own fact stays, and its class with it.
	var reset resetFactsResponse
	doJSON(t, http.MethodPost, factsURL+"/reset", nil, &reset)
	if after := list(); reset.Removed != 3 || len(after) != 1 || after["drake"].Class != store.WordRace || after["drake"].Fact != lifespan.Fact {
		t.Fatalf("reset removed %d, left %+v", reset.Removed, after)
	}
	if !words.IsPeopleDescription("Drake Trader") || words.IsPeopleDescription("Drake Tesy") {
		t.Error("Drake Trader is a kind of person, Drake Tesy a person")
	}
	if resp := doJSON(t, http.MethodPut, factsURL, upsertWorldFactRequest{Key: "Gnoll", Class: "species"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown class: %d", resp.StatusCode)
	}
}

// A class proposed for a fact in force can be turned down, once.
func TestDeclineProposedClass(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "Walls", 1, "A line.")
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	f, _ := s.UpsertWorldFact(scope, store.WorldFact{Key: "horse", Fact: "Horses live thirty years."})
	if ok, err := s.ProposeWorldFactClass(scope, f.ID, store.WordRace); err != nil || !ok {
		t.Fatal(ok, err)
	}
	skipURL := ts.URL + "/api/books/" + bookID + "/facts/" + f.ID + "/skip"
	if resp := doJSON(t, http.MethodPost, skipURL, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("decline: %d", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPost, skipURL, nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("decline again: %d", resp.StatusCode)
	}
	if ok, _ := s.ProposeWorldFactClass(scope, f.ID, store.WordRace); ok {
		t.Error("a declined class was proposed again")
	}
	// Listed with what was declined, and put back for review on request.
	factsURL := ts.URL + "/api/books/" + bookID + "/facts"
	horse := func() worldFactDTO {
		t.Helper()
		var all []worldFactDTO
		doJSON(t, http.MethodGet, factsURL, nil, &all)
		return all[slices.IndexFunc(all, func(d worldFactDTO) bool { return d.ID == f.ID })]
	}
	if d := horse(); d.DeclinedClass != store.WordRace || d.ProposedClass != "" || d.Skipped {
		t.Fatalf("declined = %+v", d)
	}
	reviewURL := factsURL + "/" + f.ID + "/review"
	if resp := doJSON(t, http.MethodPost, reviewURL, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("review: %d", resp.StatusCode)
	}
	if d := horse(); d.ProposedClass != store.WordRace || d.DeclinedClass != "" {
		t.Fatalf("back for review = %+v", d)
	}
	if resp := doJSON(t, http.MethodPost, reviewURL, nil, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("review with nothing turned down: %d", resp.StatusCode)
	}
	// A skipped proposal is listed as skipped and comes back as a proposal.
	if _, err := s.ProposeWorldFact(scope, store.WorldFact{Key: "imp", Fact: "Imps live a year."}); err != nil {
		t.Fatal(err)
	}
	var all []worldFactDTO
	all = nil
	doJSON(t, http.MethodGet, factsURL, nil, &all)
	imp := all[slices.IndexFunc(all, func(d worldFactDTO) bool { return d.Key == "imp" })]
	doJSON(t, http.MethodPost, factsURL+"/"+imp.ID+"/skip", nil, nil)
	all = nil
	doJSON(t, http.MethodGet, factsURL, nil, &all)
	if d := all[slices.IndexFunc(all, func(d worldFactDTO) bool { return d.Key == "imp" })]; !d.Skipped || d.Proposed {
		t.Fatalf("skipped imp = %+v", d)
	}
	doJSON(t, http.MethodPost, factsURL+"/"+imp.ID+"/review", nil, nil)
	all = nil
	doJSON(t, http.MethodGet, factsURL, nil, &all)
	if d := all[slices.IndexFunc(all, func(d worldFactDTO) bool { return d.Key == "imp" })]; d.Skipped || !d.Proposed {
		t.Fatalf("imp back for review = %+v", d)
	}
	facts, _ := store.BookWorldFacts(s, book)
	if len(facts) == 0 || facts[0].Fact != f.Fact || facts[0].Class != "" {
		t.Errorf("facts = %+v", facts)
	}
}

// A speaker's race and nationality set by hand are their one accepted
// note of each kind, listed with the speaker, and bring the facts filed
// under those words to their casting.
func TestSetCharacterPeople(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "Walls", 1, "“Hi,” Relc said.")
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	relc, _, _ := s.UpsertCharacter(scope, "Relc", false)
	if _, err := s.UpsertWorldFact(scope, store.WorldFact{Key: "drake", Fact: "Drakes hiss their s's.", Class: store.WordRace}); err != nil {
		t.Fatal(err)
	}
	// What fact finding proposed, and the reader overrides.
	if _, err := s.ProposeCharacterNote(store.CharacterNote{CharacterID: relc.ID, Kind: store.NoteKind, Value: "gnoll", Evidence: "Relc was no Gnoll."}); err != nil {
		t.Fatal(err)
	}
	peopleURL := ts.URL + "/api/books/" + bookID + "/characters/" + relc.ID + "/people"
	if resp := doJSON(t, http.MethodPut, peopleURL, setCharacterPeopleRequest{Race: "Drake", Nationality: "Liscorian", Age: 34}, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set: %d", resp.StatusCode)
	}
	notes := strings.Join(srv.castingWorldNotes(book, relc, ""), "\n")
	for _, want := range []string{"drake: Drakes hiss their s's.", "Relc's kind: Drake", "Relc's nationality: Liscorian", "Relc's age: 34 years old"} {
		if !strings.Contains(notes, want) {
			t.Errorf("casting notes lack %q:\n%s", want, notes)
		}
	}
	if strings.Contains(notes, "gnoll") {
		t.Errorf("a proposed note reached casting:\n%s", notes)
	}
	speakers, err := srv.buildSpeakers(bookID)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(speakers, func(d speakerDTO) bool { return d.ID == relc.ID }); i < 0 || speakers[i].Race != "Drake" || speakers[i].Nationality != "Liscorian" || speakers[i].AgeYears != 34 || speakers[i].Age != "34" {
		t.Fatalf("speakers = %+v", speakers)
	}
	// Accepting a proposed race settles the others proposed for them.
	for _, v := range []string{"lizardfolk", "wyvern"} {
		if _, err := s.ProposeCharacterNote(store.CharacterNote{CharacterID: relc.ID, Kind: store.NoteKind, Value: v, Evidence: "Relc, the " + v + "."}); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := s.ListCharacterNotes(scope)
	wyvern := all[slices.IndexFunc(all, func(n store.CharacterNote) bool { return n.Value == "wyvern" })]
	var ruled ruleCharacterNotesResponse
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/character-notes/rule", ruleCharacterNotesRequest{IDs: []string{wyvern.ID}, Accept: true}, &ruled)
	all, _ = s.ListCharacterNotes(scope)
	for _, n := range all {
		want := store.ProposalSkipped
		if n.Value == "wyvern" || n.Kind != store.NoteKind {
			want = store.ProposalAccepted
		}
		if n.Status != want {
			t.Errorf("after accepting wyvern, %s %q is %s", n.Kind, n.Value, n.Status)
		}
	}
	// Changed, the old one is turned down; cleared, there is none.
	doJSON(t, http.MethodPut, peopleURL, setCharacterPeopleRequest{Race: "gnoll", Age: -1}, nil) // refused
	doJSON(t, http.MethodPut, peopleURL, setCharacterPeopleRequest{Race: "gnoll"}, nil)
	own, _ := s.CharacterNotes(relc.ID)
	if len(own) != 1 || own[0].Value != "gnoll" || own[0].Evidence == "" { // nationality and age cleared too
		t.Fatalf("accepted notes = %+v", own)
	}
}
