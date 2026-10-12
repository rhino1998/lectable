package httpapi

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// The parts of fact finding that are not world facts: where time passes
// (time-jump proposals) and what the text states of single characters
// (their dossier - store.CharacterNote). Both read one sweep of the
// series' text (sweepSeries) for the phrases that make a paragraph a
// candidate; what the model then finds is stored pending, for the reader.
// See speakerattr/timegaps.go and speakerattr/dossier.go for each pass and
// what it was measured to do.

const (
	// timeGapSearchResults is how many of the closest paragraphs each of
	// speakerattr.TimeGapQueries adds to the ones found by phrase.
	timeGapSearchResults = 30
	// maxTimeCandidates and maxAgeCandidates bound the paragraphs one run
	// reads for each; a long serial has a few hundred of either.
	maxTimeCandidates = 600
	maxAgeCandidates  = 400
	timeGapBatch      = 25
	ageBatch          = 20
	kindClaimBatch    = 25
	// maxKindClaims bounds VerifyKinds' work in one run.
	maxKindClaims = 400
	// maxKindWords is how many of the words used of several characters
	// NameKinds is asked about.
	maxKindWords = 120
	// timeGapRunChapters is how far past a run's first statement a later
	// one still belongs to it.
	timeGapRunChapters = 30
	// timeGapOpening is how many of a series' first chapters are left out:
	// "it's been five years" in chapter 1 is about before the story.
	timeGapOpening = 3
	// timeJumpNear is how close, in chapters, a found jump may fall to one
	// the book already has or the reader turned down and count as the same.
	timeJumpNear = 5
)

// seriesChapter is one chapter of a series, in reading order; its place
// in the slice is the chapter number the finding prompts use.
type seriesChapter struct {
	bookID, id, title string
	idx               int
}

// sweptPassage is a candidate paragraph: where it is and how the prompts
// show it.
type sweptPassage struct {
	chapter         int
	text            string
	before, speaker string
}

// kindHit is one speakerattr.KindMention of a roster character.
type kindHit struct {
	chapter  int
	sentence string
}

// seriesSweep is what one read of a series' text turns up for the
// finders.
type seriesSweep struct {
	chapters []seriesChapter
	time     []sweptPassage
	ages     []sweptPassage
	// kinds is, per character id and word, where the text names them
	// beside it; most words are not kinds.
	kinds map[string]map[string][]kindHit
}

// roster resolves a name as the text writes it to one of a scope's
// characters.
type roster struct {
	byName map[string]*store.Character
}

func newRoster(characters []store.Character) roster {
	r := roster{map[string]*store.Character{}}
	for i := range characters {
		c := &characters[i]
		if c.Invalid || c.IsRole {
			continue
		}
		for _, n := range append([]string{c.Name}, c.Aliases...) {
			n = strings.ToLower(strings.TrimSpace(n))
			if len(n) < 3 {
				continue
			}
			if other, taken := r.byName[n]; taken && other != nil && other.ID != c.ID {
				r.byName[n] = nil // two characters answer to it: nobody's
				continue
			}
			r.byName[n] = c
		}
	}
	return r
}

// find returns the character name names, trying it whole and then
// without its leading words ("But Loth" -> "Loth"); nil for nobody.
func (r roster) find(name string) *store.Character {
	words := strings.Fields(name)
	for i := range words {
		if c := r.byName[strings.ToLower(strings.Join(words[i:], " "))]; c != nil {
			return c
		}
	}
	return nil
}

// sweepSeries reads every paragraph of books once and keeps the ones the
// finders want: those with a phrase of elapsed time, those with a phrase
// of age, and every place a character of r is named beside a word that
// may be their kind.
func (s *Server) sweepSeries(ctx context.Context, books []store.Book, r roster) (*seriesSweep, error) {
	sw := &seriesSweep{kinds: map[string]map[string][]kindHit{}}
	for _, b := range books {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := s.Store.CountChapters(b.ID)
		if err != nil {
			return nil, err
		}
		pos := map[string]int{}
		for i := range n {
			ch, err := s.Store.GetChapterByIdx(b.ID, i)
			if err != nil {
				return nil, err
			}
			if ch == nil {
				continue
			}
			pos[ch.ID] = len(sw.chapters)
			sw.chapters = append(sw.chapters, seriesChapter{bookID: b.ID, id: ch.ID, title: strings.TrimSpace(ch.Title), idx: ch.Idx})
		}
		paragraphs, err := s.Store.ListParagraphsRawForBook(b.ID)
		if err != nil {
			return nil, err
		}
		for i, p := range paragraphs {
			at, ok := pos[p.ChapterID]
			if !ok || len(p.Text) < 12 {
				continue
			}
			isTime, isAge := speakerattr.TimePhrase(p.Text), speakerattr.AgePhrase(p.Text)
			if isTime || isAge {
				c := sweptPassage{chapter: at, text: speakerattr.FactPassage(p.Text)}
				if i > 0 && paragraphs[i-1].ChapterID == p.ChapterID {
					c.before = speakerattr.FactPassage(paragraphs[i-1].Text)
				}
				if p.IsQuote && p.Speaker != "" && p.Speaker != "Narrator" {
					c.speaker = p.Speaker
				}
				if isTime && len(sw.time) < maxTimeCandidates {
					sw.time = append(sw.time, c)
				}
				if isAge && len(sw.ages) < maxAgeCandidates {
					sw.ages = append(sw.ages, c)
				}
			}
			for _, m := range speakerattr.KindMentions(p.Text) {
				c := r.find(m.Name)
				if c == nil {
					continue
				}
				if sw.kinds[c.ID] == nil {
					sw.kinds[c.ID] = map[string][]kindHit{}
				}
				if hits := sw.kinds[c.ID][m.Word]; len(hits) < 3 {
					sw.kinds[c.ID][m.Word] = append(hits, kindHit{at, m.Sentence})
				}
			}
		}
	}
	return sw, nil
}

// gapStatement is one speakerattr.TimeGap with where it is.
type gapStatement struct {
	chapter int
	years   float64
	skip    bool
	text    string
}

// timeGapRuns groups statements (any order) into runs of nearby chapters:
// a statement within timeGapRunChapters of a run's first joins it.
// Statements in the series' opening chapters are dropped, and so is a run
// of one statement that only looks back - one remark is as likely about a
// year the chapters told.
func timeGapRuns(statements []gapStatement) [][]gapStatement {
	sort.SliceStable(statements, func(i, j int) bool { return statements[i].chapter < statements[j].chapter })
	var runs [][]gapStatement
	for _, st := range statements {
		if st.chapter < timeGapOpening {
			continue
		}
		if n := len(runs); n > 0 && st.chapter-runs[n-1][0].chapter <= timeGapRunChapters {
			runs[n-1] = append(runs[n-1], st)
			continue
		}
		runs = append(runs, []gapStatement{st})
	}
	kept := runs[:0]
	for _, run := range runs {
		if len(run) >= 2 || run[0].skip {
			kept = append(kept, run)
		}
	}
	return kept
}

// placedJump is a jump the book has, was proposed, or was turned down:
// where it falls along the series and how long it is.
type placedJump struct {
	chapter, years int
}

// explained reports whether run is accounted for by one of jumps: one
// falls within timeJumpNear chapters of where the run starts, or the run
// only looks back, and no further than an earlier jump in the same book
// reaches ("the last three years", a hundred chapters after five passed).
func (sw *seriesSweep) explained(run []gapStatement, jumps []placedJump) bool {
	start, most, lookback := run[0].chapter, 0.0, true
	for _, st := range run {
		most = max(most, st.years)
		lookback = lookback && !st.skip
	}
	for _, j := range jumps {
		if d := start - j.chapter; d >= -timeJumpNear && d <= timeJumpNear {
			return true
		}
		if lookback && j.chapter <= start && sw.chapters[j.chapter].bookID == sw.chapters[start].bookID && most <= float64(j.years)+1 {
			return true
		}
	}
	return false
}

// findTimeJumps proposes the time jumps the series' text shows
// (speakerattr/timegaps.go) and returns how many now wait for the reader.
func (s *Server) findTimeJumps(ctx context.Context, books []store.Book, sw *seriesSweep, search meaningSearch) (int, error) {
	chapterAt := map[string]int{}
	for i, ch := range sw.chapters {
		chapterAt[ch.id] = i
	}
	candidates := append([]sweptPassage{}, sw.time...)
	seen := map[string]bool{}
	for _, c := range candidates {
		seen[c.text] = true
	}
	if search != nil {
		bookIDs := make([]string, len(books))
		for i, b := range books {
			bookIDs[i] = b.ID
		}
		for _, q := range speakerattr.TimeGapQueries {
			hits, err := search(ctx, bookIDs, q, timeGapSearchResults)
			if err != nil {
				return 0, err
			}
			for _, h := range hits {
				at, ok := chapterAt[h.ChapterID]
				if text := speakerattr.FactPassage(h.Text); ok && !seen[text] {
					seen[text] = true
					candidates = append(candidates, sweptPassage{chapter: at, text: text})
				}
			}
		}
	}

	var statements []gapStatement
	for start := 0; start < len(candidates); start += timeGapBatch {
		batch := candidates[start:min(start+timeGapBatch, len(candidates))]
		texts := make([]string, len(batch))
		for i, c := range batch {
			texts[i] = c.text
		}
		gaps, err := s.Speaker.FindTimeGaps(ctx, texts)
		if err != nil {
			return 0, err
		}
		for _, g := range gaps {
			if g.Years >= 1 {
				statements = append(statements, gapStatement{batch[g.Passage].chapter, g.Years, g.Skip, g.Evidence})
			}
		}
	}

	// What each book already has or the reader already turned down.
	var known []placedJump
	for _, b := range books {
		jumps, err := s.Store.ListTimeJumps(b.ID)
		if err != nil {
			return 0, err
		}
		for _, j := range jumps {
			if at, ok := chapterAt[j.ChapterID]; ok {
				known = append(known, placedJump{at, j.Years})
			}
		}
		proposals, err := s.Store.ListTimeJumpProposals(b.ID)
		if err != nil {
			return 0, err
		}
		for _, p := range proposals {
			if at, ok := chapterAt[p.ChapterID]; ok && p.Status == store.ProposalSkipped {
				known = append(known, placedJump{at, 0})
			}
		}
	}

	proposed := 0
	for _, run := range timeGapRuns(statements) {
		if sw.explained(run, known) {
			continue
		}
		shown := run[:min(len(run), 12)]
		gs := make([]speakerattr.GapStatement, len(shown))
		for i, st := range shown {
			gs[i] = speakerattr.GapStatement{Chapter: st.chapter, Text: st.text}
		}
		var notes []speakerattr.ChapterNote
		for at := max(0, run[0].chapter-3); at <= min(len(sw.chapters)-1, run[0].chapter+1); at++ {
			n := speakerattr.ChapterNote{Chapter: at, Title: sw.chapters[at].title}
			if cn, err := s.Store.GetChapterNotes(sw.chapters[at].id); err != nil {
				return proposed, err
			} else if cn != nil {
				n.Summary = cn.Summary
			}
			notes = append(notes, n)
		}
		placed, err := s.Speaker.PlaceTimeJump(ctx, gs, notes)
		if err != nil {
			return proposed, err
		}
		if !placed.Jump {
			continue
		}
		known = append(known, placedJump{placed.Before, placed.Years})
		var evidence []string
		for _, st := range shown[:min(len(shown), 6)] {
			evidence = append(evidence, fmt.Sprintf("%s: %s", sw.chapters[st.chapter].title, st.text))
		}
		ch := sw.chapters[placed.Before]
		waiting, err := s.Store.ProposeTimeJump(ch.bookID, ch.id, placed.Years, strings.Join(evidence, "\n"))
		if err != nil {
			return proposed, err
		}
		if waiting {
			proposed++
		}
	}
	return proposed, nil
}

// findCharacterNotes proposes what the series' text states of single
// characters - their kind, their age at some chapter
// (speakerattr/dossier.go) - and returns how many notes now wait for the
// reader.
func (s *Server) findCharacterNotes(ctx context.Context, book *store.Book, characters []store.Character, r roster, sw *seriesSweep) (int, error) {
	byID := map[string]*store.Character{}
	for i := range characters {
		byID[characters[i].ID] = &characters[i]
	}
	proposed := 0
	propose := func(n store.CharacterNote) error {
		waiting, err := s.Store.ProposeCharacterNote(n)
		if waiting {
			proposed++
		}
		return err
	}

	// Kinds. The words that may be kinds are the ones facts are filed
	// under, here or for every book, and the ones the text uses of several
	// characters; NameKinds picks the kinds out of both - a fact's word
	// can be a rule's ("royalty", "climber") rather than a kind's.
	candidates := map[string]bool{}
	facts, err := store.BookWorldFacts(s.Store, book)
	if err != nil {
		return 0, err
	}
	own, err := s.Store.ListWorldFacts(store.SeriesScope(book))
	if err != nil {
		return 0, err
	}
	for _, f := range append(facts, own...) {
		// Not a title, role or place that says nothing else: a rank is no
		// kind of being.
		if f.Status != store.WorldFactSkipped && (f.Fact != "" || f.Class == "" || f.Class == store.WordRace) {
			for _, w := range f.Words() {
				candidates[w] = true
			}
		}
	}
	used := map[string]int{}
	for _, words := range sw.kinds {
		for w := range words {
			used[w]++
		}
	}
	var ask []string
	for w, n := range used {
		if n >= 2 || candidates[w] {
			ask = append(ask, w)
		}
	}
	sort.Slice(ask, func(i, j int) bool {
		if candidates[ask[i]] != candidates[ask[j]] {
			return candidates[ask[i]]
		}
		if used[ask[i]] != used[ask[j]] {
			return used[ask[i]] > used[ask[j]]
		}
		return ask[i] < ask[j]
	})
	named, err := s.Speaker.NameKinds(ctx, ask[:min(len(ask), maxKindWords)])
	if err != nil {
		return 0, err
	}
	kinds := map[string]bool{}
	for _, w := range named {
		kinds[w] = true
	}

	type claim struct {
		char *store.Character
		word string
		hit  kindHit
	}
	var claims []claim
	ids := make([]string, 0, len(sw.kinds))
	for id := range sw.kinds {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		words := make([]string, 0, len(sw.kinds[id]))
		for w := range sw.kinds[id] {
			if kinds[w] {
				words = append(words, w)
			}
		}
		sort.Strings(words)
		for _, w := range words {
			hits := sw.kinds[id][w]
			for _, h := range hits[:min(len(hits), 2)] {
				claims = append(claims, claim{byID[id], w, h})
			}
		}
	}
	claims = claims[:min(len(claims), maxKindClaims)]
	held := map[string]bool{} // character id + word
	for start := 0; start < len(claims); start += kindClaimBatch {
		batch := claims[start:min(start+kindClaimBatch, len(claims))]
		kc := make([]speakerattr.KindClaim, len(batch))
		for i, c := range batch {
			kc[i] = speakerattr.KindClaim{Names: append([]string{c.char.Name}, c.char.Aliases...), Kind: c.word, Sentence: c.hit.sentence}
		}
		holds, err := s.Speaker.VerifyKinds(ctx, kc)
		if err != nil {
			return proposed, err
		}
		for i, c := range batch {
			if key := c.char.ID + "\x00" + c.word; holds[i] && !held[key] {
				held[key] = true
				if err := propose(store.CharacterNote{CharacterID: c.char.ID, Kind: store.NoteKind, Value: c.word, ChapterID: sw.chapters[c.hit.chapter].id, Evidence: c.hit.sentence}); err != nil {
					return proposed, err
				}
			}
		}
	}

	// Ages: read out of the candidates, then checked a character at a
	// time, where "since he was twelve" shows beside "I'm fifteen".
	type stated struct {
		passage sweptPassage
		age     int
		quote   string
	}
	byChar := map[string][]stated{}
	for start := 0; start < len(sw.ages); start += ageBatch {
		batch := sw.ages[start:min(start+ageBatch, len(sw.ages))]
		ps := make([]speakerattr.AgePassage, len(batch))
		for i, c := range batch {
			ps[i] = speakerattr.AgePassage{Text: c.text, Before: c.before, Speaker: c.speaker}
		}
		ages, err := s.Speaker.FindAges(ctx, ps)
		if err != nil {
			return proposed, err
		}
		for _, a := range ages {
			if c := r.find(a.Who); c != nil {
				byChar[c.ID] = append(byChar[c.ID], stated{batch[a.Passage], a.Age, a.Evidence})
			}
		}
	}
	ids = ids[:0]
	for id := range byChar {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		found := byChar[id]
		statements := make([]speakerattr.AgeStatement, len(found))
		for i, f := range found {
			statements[i] = speakerattr.AgeStatement{Chapter: f.passage.chapter, Age: f.age, Text: f.passage.text, Before: f.passage.before}
		}
		holds, err := s.Speaker.SettleAges(ctx, byID[id].Name, statements)
		if err != nil {
			return proposed, err
		}
		// Nobody grows younger: an age lower than one kept before it is
		// dropped - a flashback's "12 years old" a hundred chapters after
		// "I'm fifteen".
		var ages []int
		var at []int
		for i, f := range found {
			if holds[i] {
				ages, at = append(ages, f.age), append(at, i)
			}
		}
		for i := range holds {
			holds[i] = false
		}
		for _, k := range neverYounger(ages) {
			holds[at[k]] = true
		}
		seen := map[int]bool{}
		for i, f := range found {
			if !holds[i] || seen[f.age] {
				continue
			}
			seen[f.age] = true
			if err := propose(store.CharacterNote{CharacterID: id, Kind: store.NoteAge, Value: strconv.Itoa(f.age), ChapterID: sw.chapters[f.passage.chapter].id, Evidence: f.quote}); err != nil {
				return proposed, err
			}
		}
	}
	return proposed, nil
}

// neverYounger returns the indexes of ages, in series order, kept when
// each must be at least the one kept before it, starting from the first.
// The first is trusted because an age is mostly stated where someone is
// introduced; the longest run that never falls would instead have kept
// Will's two flashback ages (12, 14) over the "I'm fifteen" before them.
func neverYounger(ages []int) []int {
	var out []int
	for i, a := range ages {
		if len(out) == 0 || a >= ages[out[len(out)-1]] {
			out = append(out, i)
		}
	}
	return out
}

// findStory is fact finding's second half: time jumps and character
// notes, from one sweep of the series.
func (s *Server) findStory(ctx context.Context, book *store.Book, search meaningSearch) error {
	books, err := s.booksInScope(book)
	if err != nil {
		return err
	}
	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return err
	}
	r := newRoster(characters)
	sw, err := s.sweepSeries(ctx, books, r)
	if err != nil {
		return err
	}
	jumps, err := s.findTimeJumps(ctx, books, sw, search)
	if err != nil {
		return fmt.Errorf("finding time jumps: %w", err)
	}
	notes, err := s.findCharacterNotes(ctx, book, characters, r, sw)
	if err != nil {
		return fmt.Errorf("finding character notes: %w", err)
	}
	log.Printf("fact finding for %q: %d chapters swept, %d time and %d age passages, %d characters named beside a word; %d time jumps and %d character notes waiting for review",
		book.Title, len(sw.chapters), len(sw.time), len(sw.ages), len(sw.kinds), jumps, notes)
	return nil
}

// acceptedKinds returns, per character id, the kinds the reader has
// accepted of scope's characters - what makes someone a kobold beside
// their name and casting saying so.
func (s *Server) acceptedKinds(scope string) (map[string][]string, error) {
	notes, err := s.Store.ListCharacterNotes(scope)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, n := range notes {
		if (n.Kind == store.NoteKind || n.Kind == store.NoteNationality) && n.Status == store.ProposalAccepted {
			out[n.CharacterID] = append(out[n.CharacterID], n.Value)
		}
	}
	return out, nil
}
