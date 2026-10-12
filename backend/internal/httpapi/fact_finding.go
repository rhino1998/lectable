package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"slices"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/semantic"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

const (
	// factSearchResults is how many of the closest paragraphs each of a
	// sort's Queries contributes: every known lifespan statement sat
	// within the top 35 of one of them, in a series of 46,000 paragraphs.
	factSearchResults = 40
	// factBatchPassages is how many passages one FindFacts call
	// reads.
	factBatchPassages = 30
	// factKindResults is how many of the paragraphs closest to a kind's
	// own query join the ones that named it, for SettleFact.
	factKindResults = 8
	// maxFactKinds bounds one sort's SettleFact calls in a run. The most a
	// series proposed in the trials was 13.
	maxFactKinds = 24
	// maxTitleProposals bounds the titles one run proposes by rule, the
	// ones leading the most names first.
	maxTitleProposals = 20
	// maxWordCandidates bounds the words one run asks the model to class,
	// the ones most names use first (The Wandering Inn's 2,000 entries
	// gave 230), wordBatch at a time.
	maxWordCandidates = 400
	wordBatch         = 25
)

// meaningSearch is semantic.Service.Search, the part of the index
// FindFacts uses.
type meaningSearch func(ctx context.Context, bookIDs []string, query string, limit int) ([]semantic.Hit, error)

// factPassage is one paragraph as a fact-finding prompt shows it.
type factPassage struct {
	text      string
	chapterID string
}

// FindFacts is the KindFactFinding task: it searches book's series for
// what it says of each kind of being - how long it lives, how it speaks
// (speakerattr.FactSorts) - and stores what it finds, one pending world
// fact a kind, with the sentence it rests on, for the reader to
// accept or skip - nothing it finds is in force until then. See
// speakerattr/factfinding.go for the pass and what it was measured to do.
// The same run then looks for where time passes and for what the text
// states of single characters (findStory, dossier.go), proposed the same
// way - as are the world's own titles, roles, races, nationalities and
// places among the roster's names (findWorldWords).
func (s *Server) FindFacts(ctx context.Context, book *store.Book) error {
	titlesErr := s.findWorldWords(ctx, book)
	if s.Semantic == nil || s.Speaker == nil {
		log.Printf("fact finding for %q: no embedding or language model, so only the words of names were looked at", book.Title)
		return titlesErr
	}
	// The parts don't depend on each other: one failing still leaves
	// what the others found.
	return errors.Join(titlesErr, s.findFacts(ctx, book, s.Semantic.Search), s.findStory(ctx, book, s.Semantic.Search))
}

// findWorldWords proposes what sort of word each of the words book's
// series' character names share is, when it is one the roster must never
// read as a person's name: a title ("Prognugator Tersk", "Prognugator
// Dekass"), a role ("Baker"), a race ("Drake Tesy", "One of the Drakes"),
// a nationality ("Pallassian Gnoll") or a place ("King of Khelt"). The
// words are speakerattr.WordCandidates that the text writes as one of
// those at least speakerattr.MinTitleUses times (TitleUses); the model
// says which each is from sentences that use it (ClassifyWords), family
// names and ordinary words left out. A new word becomes a pending fact
// with its class, no text and the first such sentence; a word the series
// already has a fact about (a race with its lifespan) gets the class
// proposed for that fact (ClassProposed), its text untouched. A word
// already classed, here or for every book, or whose class the reader
// turned down, is left alone. Nothing changes in the roster until the
// reader accepts one and runs a roster review.
//
// Without a language model only titles are looked for, by rules
// (findTitlesByRule).
func (s *Server) findWorldWords(ctx context.Context, book *store.Book) error {
	if s.Speaker == nil {
		return s.findTitlesByRule(ctx, book)
	}
	scope := store.SeriesScope(book)
	books, err := s.booksInScope(book)
	if err != nil {
		return err
	}
	chars, err := s.Store.ListCharacters(scope)
	if err != nil {
		return err
	}
	lines, err := s.rosterLineCounts(books)
	if err != nil {
		return err
	}
	facts, err := s.Store.ListWorldFacts(scope)
	if err != nil {
		return err
	}
	own := map[string]store.WorldFact{}
	for _, f := range facts {
		for _, w := range f.Words() {
			own[w] = f
		}
	}
	var candidates []speakerattr.WordCandidate
	var words []string
	for _, c := range speakerattr.WordCandidates(rosterEntries(chars, lines), s.titles(book)) {
		// Asked about: a word with no fact, one whose fact is in force
		// with no class proposed or declined, and one skipped as some
		// class (it may be another).
		if f, ok := own[strings.ToLower(c.Word)]; ok {
			switch {
			case f.Status == store.WorldFactActive && f.Class == "" && f.ClassProposed == "":
			case f.Status == store.WorldFactSkipped && f.Class != "" && f.Fact == "":
			default:
				continue
			}
		}
		candidates = append(candidates, c)
		words = append(words, c.Word)
	}
	// And what the series' own facts are filed under, whether or not a
	// name uses it: a kind fact finding wrote up ("drowned folk", "half-elf")
	// is a race or a nationality all the same.
	for _, f := range facts {
		if f.Status == store.WorldFactActive && f.Class == "" && f.ClassProposed == "" &&
			!slices.ContainsFunc(words, func(w string) bool { return strings.EqualFold(w, f.Key) }) {
			candidates = append(candidates, speakerattr.WordCandidate{Word: f.Key})
			words = append(words, f.Key)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	uses := speakerattr.NewTitleUses(words)
	for _, b := range books {
		if err := ctx.Err(); err != nil {
			return err
		}
		all, err := s.Store.ListParagraphsRawForBook(b.ID)
		if err != nil {
			return err
		}
		for _, p := range all {
			uses.Add(p.ChapterID, p.Text)
		}
	}
	var samples []speakerattr.WordSample
	for _, c := range candidates {
		if use := uses.Of(c.Word); use != nil && use.Paragraphs >= speakerattr.MinTitleUses {
			if samples = append(samples, speakerattr.WordSample{Word: c.Word, Names: c.Names, Sentences: use.Samples}); len(samples) == maxWordCandidates {
				break
			}
		}
	}
	proposed := 0
	for start := 0; start < len(samples); start += wordBatch {
		classed, err := s.Speaker.ClassifyWords(ctx, samples[start:min(start+wordBatch, len(samples))])
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// One batch's failed call doesn't cost the rest; its words are
			// asked about again next run.
			log.Printf("httpapi: classifying %q's words: %v", book.Title, err)
			continue
		}
		for _, c := range classed {
			class, use := store.WordClass(c.Class), uses.Of(c.Word)
			var waiting bool
			if f, ok := own[strings.ToLower(c.Word)]; ok && f.Status == store.WorldFactActive {
				waiting, err = s.Store.ProposeWorldFactClass(scope, f.ID, class)
			} else {
				waiting, err = s.Store.ProposeWorldFact(scope, store.WorldFact{
					Key: c.Word, Class: class, Evidence: use.Evidence, EvidenceChapterID: use.EvidenceAt,
				})
			}
			if err != nil {
				return err
			}
			if waiting {
				proposed++
			}
		}
	}
	log.Printf("word finding for %q: %d words shared by names, %d read by the model, %d waiting for review", book.Title, len(candidates), len(samples), proposed)
	return nil
}

// findTitlesByRule is findWorldWords without a model: it proposes the
// titles alone - each word that leads
// the names of several of its series' characters ("Prognugator Tersk",
// "Prognugator Dekass" - speakerattr.TitleCandidates) and that the text
// writes as a title at least speakerattr.MinTitleUses times ("the
// Prognugator") and follows with a name rather than an ordinary word
// (NamedHolders: "Drake Trader" is a kind and a job), as a pending title
// fact with the first such sentence.
// Rules only, no model. A word the series or the library already has a
// fact for - a kind ("Drake Trader", "Drake Tesy"), a title turned down -
// is left alone. Nothing changes in the roster until the reader accepts
// one and runs a roster review.
func (s *Server) findTitlesByRule(ctx context.Context, book *store.Book) error {
	scope := store.SeriesScope(book)
	books, err := s.booksInScope(book)
	if err != nil {
		return err
	}
	chars, err := s.Store.ListCharacters(scope)
	if err != nil {
		return err
	}
	lines, err := s.rosterLineCounts(books)
	if err != nil {
		return err
	}
	decided := map[string]bool{}
	for _, sc := range []string{scope, store.GlobalFactScope} {
		facts, err := s.Store.ListWorldFacts(sc)
		if err != nil {
			return err
		}
		for _, f := range facts {
			if f.Status != store.WorldFactPending {
				for _, w := range f.Words() {
					decided[w] = true
				}
			}
		}
	}
	var candidates []speakerattr.TitleCandidate
	var words []string
	for _, c := range speakerattr.TitleCandidates(rosterEntries(chars, lines), s.titles(book)) {
		if !decided[strings.ToLower(c.Word)] {
			candidates = append(candidates, c)
			words = append(words, c.Word)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	uses := speakerattr.NewTitleUses(words)
	var after []string
	for _, c := range candidates {
		after = append(after, c.HolderWords()...)
	}
	named := speakerattr.NewWordCaseCounts(after)
	for _, b := range books {
		if err := ctx.Err(); err != nil {
			return err
		}
		all, err := s.Store.ListParagraphsRawForBook(b.ID)
		if err != nil {
			return err
		}
		for _, p := range all {
			uses.Add(p.ChapterID, p.Text)
			named.Add(p.Text)
		}
	}
	proposed := 0
	for _, c := range candidates {
		use := uses.Of(c.Word)
		if use == nil || use.Paragraphs < speakerattr.MinTitleUses || len(c.NamedHolders(named)) < speakerattr.MinTitleHolders {
			continue
		}
		waiting, err := s.Store.ProposeWorldFact(scope, store.WorldFact{
			Key: c.Word, Class: store.WordTitle, Source: store.WorldFactRule, Evidence: use.Evidence, EvidenceChapterID: use.EvidenceAt,
		})
		if err != nil {
			return err
		}
		if waiting {
			if proposed++; proposed == maxTitleProposals {
				break
			}
		}
	}
	log.Printf("title finding for %q: %d words lead several names, %d waiting for review", book.Title, len(candidates), proposed)
	return nil
}

func (s *Server) findFacts(ctx context.Context, book *store.Book, search meaningSearch) error {
	if s.Speaker == nil {
		return errors.New("fact finding needs a language model")
	}
	books, err := s.booksInScope(book)
	if err != nil {
		return err
	}
	bookIDs := make([]string, len(books))
	for i, b := range books {
		bookIDs[i] = b.ID
	}
	scope := store.SeriesScope(book)

	// A kind the reader has already ruled on - their own fact, one they
	// accepted, one they skipped - isn't asked about again.
	existing, err := s.Store.ListWorldFacts(scope)
	if err != nil {
		return err
	}
	decided := map[string]bool{}
	for _, f := range existing {
		if f.Status != store.WorldFactPending {
			for _, w := range f.Words() {
				decided[w] = true
			}
		}
	}

	// found is what each kind's proposal is built from: one settled fact
	// per sort that had one, in FactSorts' order.
	found := map[string][]settledFact{}
	// races are the kinds with a lifespan: what has one is a kind of
	// being, so its fact is proposed as a race's.
	races := map[string]bool{}
	var kinds []string
	searched := 0
	for _, sort := range speakerattr.FactSorts {
		settled, passages, err := s.findFactsOfSort(ctx, sort, bookIDs, decided, search)
		if err != nil {
			return err
		}
		searched += passages
		for _, f := range settled {
			if sort.Name == speakerattr.LifespanFacts.Name {
				races[f.kind] = true
			}
			if _, ok := found[f.kind]; !ok {
				kinds = append(kinds, f.kind)
			}
			found[f.kind] = append(found[f.kind], f)
		}
	}
	if searched == 0 {
		return errors.New("none of this series' chapters have been embedded yet (the preprocessing \"embedding\" phase)")
	}

	proposed := 0
	for _, kind := range kinds {
		// One fact per key: a kind with a lifespan and a way of speaking
		// gets both sentences, each quote on its own line.
		var facts, quotes []string
		for _, f := range found[kind] {
			facts = append(facts, f.fact)
			quotes = append(quotes, f.evidence)
		}
		var class store.WordClass
		if races[kind] {
			class = store.WordRace
		}
		waiting, err := s.Store.ProposeWorldFact(scope, store.WorldFact{
			Key: kind, Class: class, Fact: strings.Join(facts, " "), Evidence: strings.Join(quotes, "\n"), EvidenceChapterID: found[kind][0].chapterID,
		})
		if err != nil {
			return err
		}
		if waiting {
			proposed++
		}
	}
	log.Printf("fact finding for %q: %d passages read, %d kinds with a fact, %d waiting for review", book.Title, searched, len(kinds), proposed)
	return nil
}

// settledFact is one sort's fact about a kind, with where its quote is.
type settledFact struct {
	kind, fact, evidence, chapterID string
}

// findFactsOfSort runs one sort of the pass over bookIDs: the facts it
// settled on, each with a quote found in the passages shown, and how many
// passages its search turned up. Kinds in decided are not asked about.
func (s *Server) findFactsOfSort(ctx context.Context, sort speakerattr.FactSort, bookIDs []string, decided map[string]bool, search meaningSearch) ([]settledFact, int, error) {
	var candidates []factPassage
	seen := map[string]bool{}
	for _, q := range sort.Queries {
		hits, err := search(ctx, bookIDs, q, factSearchResults)
		if err != nil {
			return nil, 0, err
		}
		for _, h := range hits {
			if !seen[h.ParagraphID] {
				seen[h.ParagraphID] = true
				candidates = append(candidates, factPassage{speakerattr.FactPassage(h.Text), h.ChapterID})
			}
		}
	}

	// leads are the passages each kind was named from, in the order found.
	leads := map[string][]factPassage{}
	var kinds []string
	for start := 0; start < len(candidates); start += factBatchPassages {
		batch := candidates[start:min(start+factBatchPassages, len(candidates))]
		texts := passageTexts(batch)
		found, err := s.Speaker.FindFacts(ctx, sort, texts)
		if err != nil {
			return nil, 0, err
		}
		for _, f := range found {
			i := speakerattr.QuotedIn(f.Evidence, texts)
			if i < 0 || decided[f.Kind] {
				continue
			}
			if _, ok := leads[f.Kind]; !ok {
				kinds = append(kinds, f.Kind)
			}
			leads[f.Kind] = append(leads[f.Kind], batch[i])
		}
	}

	var out []settledFact
	for n, kind := range kinds {
		if n == maxFactKinds {
			break
		}
		passages := leads[kind]
		have := map[string]bool{}
		for _, p := range passages {
			have[p.text] = true
		}
		hits, err := search(ctx, bookIDs, sort.KindQuery(kind), factKindResults)
		if err != nil {
			return nil, 0, err
		}
		for _, h := range hits {
			if text := speakerattr.FactPassage(h.Text); !have[text] {
				have[text] = true
				passages = append(passages, factPassage{text, h.ChapterID})
			}
		}
		texts := passageTexts(passages)
		fact, err := s.Speaker.SettleFact(ctx, sort, kind, texts)
		if err != nil {
			return nil, 0, err
		}
		if i := speakerattr.QuotedIn(fact.Evidence, texts); fact.Fact != "" && i >= 0 {
			out = append(out, settledFact{kind, fact.Fact, fact.Evidence, passages[i].chapterID})
		}
	}
	return out, len(candidates), nil
}

func passageTexts(passages []factPassage) []string {
	out := make([]string, len(passages))
	for i, p := range passages {
		out[i] = p.text
	}
	return out
}

// handleFindBookFacts serves POST /api/books/{id}/facts/find: queues a
// search of the book's series for world facts. What it finds arrives on
// the bookFacts topic as proposed facts.
func (s *Server) handleFindBookFacts(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	s.Jobs.EnqueueFactFinding(book, store.SeriesScope(book), func(ctx context.Context) error {
		return s.FindFacts(ctx, book)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// resetFactsResponse is POST .../facts/reset's reply.
type resetFactsResponse struct {
	// Removed is how many of the series' facts were removed.
	Removed int `json:"removed"`
}

// handleResetBookFacts serves POST /api/books/{id}/facts/reset: undoes
// fact finding for the book's series (store.ResetFoundWorldFacts) - every
// fact a pass proposed is removed, accepted and skipped ones included, and
// classes proposed or declined are forgotten - so the next run proposes
// everything again. Facts the reader wrote stay. Nobody is recast, and
// nothing a roster review already changed is undone.
func (s *Server) handleResetBookFacts(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	removed, err := s.Store.ResetFoundWorldFacts(store.SeriesScope(book))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resetFactsResponse{Removed: removed})
}

// handleReviewBookFact serves POST /api/books/{id}/facts/{factId}/review:
// puts a fact the reader turned down back in front of them
// (store.ReviewWorldFact) - a skipped fact is proposed again, and a fact
// in force whose proposed class was declined has it proposed again. 409
// when the fact has nothing turned down.
func (s *Server) handleReviewBookFact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	back, err := s.Store.ReviewWorldFact(store.SeriesScope(book), r.PathValue("factId"))
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	case !back:
		writeError(w, http.StatusConflict, "nothing was turned down for that fact")
	default:
		writeNoContent(w)
	}
}

// handleSkipBookFact serves POST /api/books/{id}/facts/{factId}/skip:
// turns a proposed fact down for good - it is kept, out of sight, so the
// same kind isn't proposed again. (Accepting one is PUT .../facts with its
// id, edited or not.) 404 when the series has no such fact, 409 for one
// that isn't waiting. For a fact in force whose word has a class proposed
// (proposedClass), it is that class that is turned down for good; the
// fact stays.
func (s *Server) handleSkipBookFact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	scope, id := store.SeriesScope(book), r.PathValue("factId")
	facts, err := s.Store.ListWorldFacts(scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, f := range facts {
		if f.ID != id {
			continue
		}
		if f.Status != store.WorldFactPending {
			// A fact in force with a class proposed: the class is what is
			// turned down.
			declined, err := s.Store.DeclineWorldFactClass(scope, id)
			switch {
			case err != nil:
				writeError(w, http.StatusInternalServerError, err.Error())
			case !declined:
				writeError(w, http.StatusConflict, "that fact is not waiting for review")
			default:
				writeNoContent(w)
			}
			return
		}
		if _, err := s.Store.SetWorldFactStatus(scope, id, store.WorldFactSkipped); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeNoContent(w)
		return
	}
	writeError(w, http.StatusNotFound, "fact not found")
}

// factEvidence fills dto's evidence fields from f: the quoted sentence and
// where in the series it is.
func (s *Server) factEvidence(dto *worldFactDTO, f store.WorldFact, titles map[string]string) error {
	dto.Evidence = f.Evidence
	if f.EvidenceChapterID == "" {
		return nil
	}
	ch, err := s.Store.GetChapterByID(f.EvidenceChapterID)
	if err != nil || ch == nil {
		return err // a chapter since deleted leaves the quote alone
	}
	idx := ch.Idx
	dto.EvidenceBookID, dto.EvidenceChapterIdx, dto.EvidenceChapterTitle = ch.BookID, &idx, strings.TrimSpace(ch.Title)
	if _, ok := titles[ch.BookID]; !ok {
		titles[ch.BookID] = ""
		if b, err := s.Store.GetBook(ch.BookID); err != nil {
			return err
		} else if b != nil {
			titles[ch.BookID] = b.Title
		}
	}
	dto.EvidenceBookTitle = titles[ch.BookID]
	return nil
}
