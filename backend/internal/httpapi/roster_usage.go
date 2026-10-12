package httpapi

import (
	"log"
	"slices"

	"github.com/rhino1998/lectable/backend/internal/semantic"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

const (
	// rosterAlikeMin is how alike (semantic.Alike) a series must use two
	// names that share their words for a roster review to count that
	// toward their being one person. Measured 2026-10-11 on four rosters
	// before their merges: pairs the reader went on to merge read 0.46 to
	// 0.91 (the lowest a first name and its full name, which the rules
	// propose anyway), different people who share a rank or a surname
	// ("Captain Desai"/"Captain Giraud", "Mark Wyrd"/"Frederick Wyrd") up
	// to 0.68 with most under 0.5, people who share nothing under 0.35.
	rosterAlikeMin = 0.6
	// rosterAlikeParagraphs is how many embedded paragraphs must write
	// each name for the two to be compared at all.
	rosterAlikeParagraphs = 3
)

// rosterUsage measures, for a review's pairs of names that share their
// words (speakerattr.RosterPair.SharesName), whether the series uses the
// two the same way: the paragraphs writing only the one against the
// paragraphs writing only the other, by their embeddings. Chapters two
// names share don't tell one person from two (docs/speakerattr.md); what
// is said around each name does. It is what lets a review call "Dr.
// Zheng" and "Doctor Zheng" one person, which no passage naming both ever
// will. A nil one (no index) measures nothing.
type rosterUsage struct {
	sem    *semantic.Service
	among  semantic.Usage
	pairs  map[string]*pairUsage
	failed bool
}

type pairUsage struct {
	pair  speakerattr.RosterPair
	a, b  semantic.Usage
	asked bool
}

// newRosterUsage prepares to measure asked, the pairs this run reads
// about, and with them every other pair of all (the roster's pairs) that
// one of their names is in: "Zodiac" may be short for "Lord Zodiac" or for
// "Alicia Zodiac", and only the closer of the two counts.
func (s *Server) newRosterUsage(all, asked []speakerattr.RosterPair) *rosterUsage {
	if s.Semantic == nil {
		return nil
	}
	u := &rosterUsage{sem: s.Semantic, pairs: map[string]*pairUsage{}}
	in := map[string]bool{}
	for _, p := range asked {
		if p.SharesName() {
			u.pairs[rosterPairKey(p.A.ID, p.B.ID)] = &pairUsage{pair: p, asked: true}
			in[p.A.ID], in[p.B.ID] = true, true
		}
	}
	for _, p := range all {
		if k := rosterPairKey(p.A.ID, p.B.ID); u.pairs[k] == nil && p.SharesName() && (in[p.A.ID] || in[p.B.ID]) {
			u.pairs[k] = &pairUsage{pair: p}
		}
	}
	return u
}

func (u *rosterUsage) fail(err error) {
	log.Printf("httpapi: roster review: reading embeddings: %v", err)
	u.failed = true
}

// addBook takes in one book: paragraphs is all of it, in the order ix was
// made from.
func (u *rosterUsage) addBook(bookID string, paragraphs []store.Paragraph, ix *speakerattr.IdentityIndex) {
	if u == nil || u.failed || len(u.pairs) == 0 {
		return
	}
	if err := u.sem.AddBook(&u.among, bookID); err != nil {
		u.fail(err)
		return
	}
	for _, m := range u.pairs {
		if !m.asked {
			u.add(bookID, paragraphs, m.pair, ix.PairMentions(m.pair.Entries()))
		}
	}
}

// add takes in where one book names p's two entries.
func (u *rosterUsage) add(bookID string, paragraphs []store.Paragraph, p speakerattr.RosterPair, found speakerattr.PairMentions) {
	if u == nil || u.failed {
		return
	}
	m := u.pairs[rosterPairKey(p.A.ID, p.B.ID)]
	if m == nil {
		return
	}
	ids := func(idxs []int) []string {
		out := make([]string, len(idxs))
		for i, idx := range idxs {
			out[i] = paragraphs[idx].ID
		}
		return out
	}
	if err := u.sem.AddParagraphs(&m.a, bookID, ids(found.NameA)); err != nil {
		u.fail(err)
		return
	}
	if err := u.sem.AddParagraphs(&m.b, bookID, ids(found.NameB)); err != nil {
		u.fail(err)
	}
}

// alike reports whether the series uses p's two names the same way: each
// written in rosterAlikeParagraphs embedded paragraphs or more, and those
// reading at least rosterAlikeMin alike. A name that could be short for
// several entries counts only toward the one it reads closest to
// ("Sanguis" is the Baron, not his son the Prince) - unless those entries
// read alike themselves ("Chang" with "Lt. Colonel Chang" and "Lt Colonel
// Chang"), when they aren't rivals for it. outdone reports a pair that
// read alike enough and lost only to such a rival: once the roster has
// changed it may not. Each measurement is logged, for tuning
// rosterAlikeMin.
func (u *rosterUsage) alike(scope string, p speakerattr.RosterPair) (alike, outdone bool) {
	if u == nil || u.failed {
		return false, false
	}
	m := u.pairs[rosterPairKey(p.A.ID, p.B.ID)]
	sim, ok := u.measure(m)
	if !ok {
		return false, false
	}
	partner := func(q speakerattr.RosterPair, id string) string {
		if q.A.ID == id {
			return q.B.ID
		}
		return q.A.ID
	}
	closest := true
	for _, other := range u.pairs {
		if s, ok := u.measure(other); other == m || !ok || s <= sim {
			continue
		}
		for _, short := range p.Shorter() {
			if !slices.ContainsFunc(other.pair.Shorter(), func(e speakerattr.RosterEntry) bool { return e.ID == short.ID }) {
				continue
			}
			rivals, ok := u.measure(u.pairs[rosterPairKey(partner(p, short.ID), partner(other.pair, short.ID))])
			if !ok || rivals < rosterAlikeMin {
				closest = false
			}
		}
	}
	log.Printf("httpapi: roster review %s: %q and %q read %.2f alike (%d/%d paragraphs, closest %v)", scope, p.A.Name, p.B.Name, sim, m.a.N(), m.b.N(), closest)
	return closest && sim >= rosterAlikeMin, !closest && sim >= rosterAlikeMin
}

// measure is how alike the series uses m's two names; ok is false when
// too few paragraphs write one of them to say.
func (u *rosterUsage) measure(m *pairUsage) (sim float64, ok bool) {
	if m == nil || m.a.N() < rosterAlikeParagraphs || m.b.N() < rosterAlikeParagraphs {
		return 0, false
	}
	return semantic.Alike(&m.a, &m.b, &u.among), true
}
