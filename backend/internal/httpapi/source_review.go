package httpapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"

	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// What a site lists as a work's chapters isn't all story: authors post
// announcements, release notices and summaries in the same list. So a
// book only takes entries its reader agreed to. The ones chosen at import
// are approved then; an entry a later check finds waits for review
// (store.SourcePlan.Pending), and the check stops before it - a chapter
// approved late would otherwise land after the chapters that follow it.
// The reader says import or skip for each (handleReviewBookSource), and
// the check carries on.
//
// Entries whose title reads like a notice are marked suspect: unticked by
// default when choosing chapters at import, held for review by an import
// that didn't choose, and pre-set to "skip" in a review.

// A title that numbers itself as a chapter is one, whatever else it says
// ("Chapter 12: Jail Break", "3.05 Notice Board").
var numberedTitle = regexp.MustCompile(`(?i)^\s*(?:(?:chapter|ch\.?|episode|part)\s*\d+|\d+(?:\.\d+)?\s*(?:[.:\-–—]|$)|\d+\.\d+)`)

// Words an author's notice is titled with.
var noticeTitle = regexp.MustCompile(`(?i)\b(?:announcements?|notice|hiatus|break|delay(?:ed|s)?|release[sd]?|releasing|kindle|audible|amazon|patreon|pre-?orders?|updates?|schedule|summary|recap|stub(?:bed|bing)?|coming down|taken down|giveaway|cover reveal|poll|q\s*&\s*a|ama|author'?s? note|not a chapter|non[- ]chapter|book \w+ (?:is )?(?:out|launch\w*|live))\b`)

// suspectTitle reports whether a listed entry's title reads like an
// author's notice rather than a chapter.
func suspectTitle(title string) bool {
	return !numberedTitle.MatchString(title) && noticeTitle.MatchString(title)
}

// sourceReviewEntryDTO is one entry waiting for the reader's say.
type sourceReviewEntryDTO struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Source is the name of the site that lists it ("Patreon").
	Source string `json:"source"`
	// Suspect is true when the title reads like an author's notice.
	Suspect bool `json:"suspect"`
	// Locked is true for an entry the site lists but won't serve this
	// reader (patrons only, early access): it shows what is there, and
	// can't be imported until it opens. "Skip" still drops it.
	Locked bool `json:"locked"`
	// Unavailable is true for a chapter that was agreed to but couldn't
	// be read - a delisted one the Internet Archive has no usable copy
	// of. "Import" tries it again; "skip" lets the book go on without it.
	Unavailable bool `json:"unavailable"`
}

type reviewBookSourceRequest struct {
	// Import and Skip are ids of entries waiting for review
	// (source.review): take them, or leave them out for good. Entries
	// named in neither keep waiting.
	Import []string `json:"import"`
	Skip   []string `json:"skip"`
}

// handleReviewBookSource records the reader's decision on entries a check
// found - import them or skip them - and runs a check to act on it. 202
// with how many are to be imported; 409 for a book with no source.
func (s *Server) handleReviewBookSource(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	var req reviewBookSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	b, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	sources, err := s.Store.BookSources(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(sources) == 0 {
		writeError(w, http.StatusConflict, "this book wasn't imported from a website, so it has nothing to review")
		return
	}
	// An entry is decided in the plan of each source it waits in (two
	// sources reading one site list the same entries).
	imported := map[string]bool{}
	for _, src := range sources {
		plan, err := s.Store.GetBookSourcePlan(src.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Only what is actually waiting can be decided.
		waiting := map[string]bool{}
		for _, p := range plan.Pending {
			waiting[p.ID] = true
		}
		decided := false
		for _, id := range req.Import {
			if waiting[id] {
				plan.Approved = append(plan.Approved, id)
				delete(waiting, id)
				imported[id] = true
				decided = true
			}
		}
		for _, id := range req.Skip {
			if waiting[id] {
				plan.Skip = append(plan.Skip, id)
				delete(waiting, id)
				decided = true
			}
		}
		if !decided {
			continue
		}
		plan.Pending = slices.DeleteFunc(plan.Pending, func(p store.PlannedChapter) bool { return !waiting[p.ID] })
		if err := s.Store.SetBookSourcePlan(src.ID, plan); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	taken := len(imported)
	s.startSourceSync(bookID, nil)
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: taken})
}

// reviewPlan splits the chapters a check would work through into the ones
// to fetch now and the ones waiting for review, by what plan has approved.
// have are the source ids the book already holds. Fetching stops at the
// first entry waiting: what follows it waits too, approved or not, so the
// book stays in order - except behind an entry that is locked. A plan from before reviewing existed approves
// everything listed now.
func reviewPlan(plan *store.SourcePlan, planned []websource.ChapterRef, have map[string]bool) (fetch []websource.ChapterRef, pending []store.PlannedChapter) {
	if !plan.Reviewed {
		plan.Reviewed = true
		for _, ref := range planned {
			plan.Approved = append(plan.Approved, ref.ID)
		}
	}
	approved := make(map[string]bool, len(plan.Approved))
	for _, id := range plan.Approved {
		approved[id] = true
	}
	// What an earlier check couldn't read keeps saying so.
	unreadable := map[string]bool{}
	for _, p := range plan.Pending {
		if p.Unreadable {
			unreadable[p.ID] = true
		}
	}
	// An entry the source won't serve this reader waits without holding
	// back what follows it: there is no saying when it will open, and
	// what follows is usually a public post.
	held := false
	for _, ref := range planned {
		locked := !ref.Unlocked && !ref.Delisted
		switch {
		case have[ref.ID]:
		case !approved[ref.ID]:
			pending = append(pending, store.PlannedChapter{ID: ref.ID, Title: ref.Title, URL: ref.URL, Suspect: suspectTitle(ref.Title), Unreadable: unreadable[ref.ID], Locked: locked})
			held = held || !locked
		case !held:
			fetch = append(fetch, ref)
		}
	}
	return fetch, pending
}

// holdUnreadable moves chapters that were agreed to but couldn't be read
// back to waiting for the reader, marked as such, ahead of whatever else
// waits.
func holdUnreadable(plan *store.SourcePlan, refs []websource.ChapterRef) {
	held := make(map[string]bool, len(refs))
	var entries []store.PlannedChapter
	for _, ref := range refs {
		held[ref.ID] = true
		entries = append(entries, store.PlannedChapter{ID: ref.ID, Title: ref.Title, URL: ref.URL, Unreadable: true})
	}
	plan.Approved = slices.DeleteFunc(plan.Approved, func(id string) bool { return held[id] })
	plan.Pending = append(entries, slices.DeleteFunc(plan.Pending, func(p store.PlannedChapter) bool { return held[p.ID] })...)
}
