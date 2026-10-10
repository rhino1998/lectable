package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// A book can track several sources (store.BookSources): the site it was
// imported from and any added since (handleAddBookSource) - a serial's
// advance chapters on Patreon beside its public ones on Royal Road, a
// site beside an archived copy of it. A check goes through them in the
// order they were added, each with its own plan and review.
//
// The same chapter is usually posted in more than one of them, under ids
// that have nothing to do with each other. So a source's entries the book
// doesn't have by id are first looked for among the chapters it took from
// elsewhere - by title before anything is fetched (matchOtherSources),
// and by how the chapter opens once it has been (sameOpening) - and one
// found is remembered as had (store.SourcePlan.Matched) instead of being
// added a second time.
//
// A source added to a book keeps its chapter ids apart from the others'
// by a prefix (store.BookSource.Prefix, "patreon:"): the reader that
// source is read through puts it on every id coming in and takes it off
// every id going out (prefixedSite), so nothing past the reader knows.

// trackedSourceDTO is one of the sources a book tracks.
type trackedSourceDTO struct {
	// ID names the source for DELETE /api/books/{id}/sources/{sourceId}.
	ID   string         `json:"id"`
	Kind bookSourceKind `json:"kind"`
	// Name is the site's name, for display ("Royal Road").
	Name string `json:"name"`
	// URL is the work's page there.
	URL string `json:"url"`
	// CheckedAt is when it was last checked (unix seconds), absent if never.
	CheckedAt int64 `json:"checkedAt,omitempty"`
	// Error is why its last check stopped early, absent if it finished.
	Error string `json:"error,omitempty"`
	// Blocked is the title of the chapter its last check stopped at
	// because the source lists it but doesn't serve it yet; absent if none.
	Blocked string `json:"blocked,omitempty"`
	// Waiting is how many of its entries wait for the reader's say, and
	// Locked how many more it lists that it won't serve this reader
	// (patrons only, early access) - there, but not importable yet.
	Waiting int `json:"waiting"`
	Locked  int `json:"locked"`
	// TitleRule is the source's title rule, absent when it has none (PUT
	// /api/books/{id}/sources/{sourceId}/title-rule).
	TitleRule *sourceTitleRuleDTO `json:"titleRule,omitempty"`
}

type addBookSourceRequest struct {
	// URL is a link to the work at a supported site, or one of its chapters.
	URL string `json:"url"`
	// TitleRule, if given, is the source's title rule from the start.
	TitleRule *sourceTitleRuleDTO `json:"titleRule,omitempty"`
}

// sourceTitleRuleDTO is one source's title rule (store.TitleRule): a
// regular expression over the titles the source lists its entries under,
// saying which are chapters and what each is called. Kept with the source
// and applied by every check of it.
type sourceTitleRuleDTO struct {
	// Pattern is the expression (Go/RE2 syntax, case-sensitive unless it
	// starts "(?i)"), matched anywhere in a title. "" is no rule.
	Pattern string `json:"pattern"`
	// Output, if not empty, is what an entry whose title matches is
	// called: a template over the match's capture groups - "Chapter $1",
	// "${name}"; "$$" is a dollar sign. Empty keeps the title.
	Output string `json:"output"`
	// Only leaves out every entry whose title doesn't match: it is
	// neither imported nor offered for review.
	Only bool `json:"only"`
}

// titleRule checks a rule from a request; nil for none.
func titleRule(dto *sourceTitleRuleDTO) (*store.TitleRule, error) {
	if dto == nil || strings.TrimSpace(dto.Pattern) == "" {
		if dto != nil && strings.TrimSpace(dto.Output) != "" {
			return nil, errors.New("a title output needs a pattern whose capture groups it is made from")
		}
		return nil, nil
	}
	if _, err := regexp.Compile(dto.Pattern); err != nil {
		return nil, fmt.Errorf("the title pattern isn't a regular expression: %s", strings.TrimPrefix(err.Error(), "error parsing regexp: "))
	}
	return &store.TitleRule{Pattern: dto.Pattern, Output: strings.TrimSpace(dto.Output), Only: dto.Only}, nil
}

func titleRuleDTO(rule *store.TitleRule) *sourceTitleRuleDTO {
	if rule == nil {
		return nil
	}
	return &sourceTitleRuleDTO{Pattern: rule.Pattern, Output: rule.Output, Only: rule.Only}
}

// cleanTitle is a title as a site lists it with its spacing put right:
// line breaks, tabs, no-break and other odd spaces become one ordinary
// space, and characters that aren't seen (zero-width ones, soft hyphens,
// controls) go. Sites' titles carry these more often than they look.
func cleanTitle(title string) string {
	var b strings.Builder
	space := false
	for _, r := range title {
		switch {
		case unicode.IsSpace(r):
			space = b.Len() > 0
		case r == '\u00ad' || unicode.Is(unicode.Cf, r) || unicode.IsControl(r):
		default:
			if space {
				b.WriteByte(' ')
				space = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// foldTitle is a title as a title rule's pattern sees it: cleaned, and
// with Unicode's compatibility forms folded to the plain ones (NFKC) -
// full-width and styled letters and digits, ligatures, "…" as "...", a
// letter and its combining accent as one character - so a pattern typed
// on a keyboard matches a title that only looks like what was typed.
func foldTitle(title string) string {
	return cleanTitle(norm.NFKC.String(title))
}

// applyTitleRule returns refs as rule has them, every title cleaned
// (cleanTitle) first: an entry whose title, folded (foldTitle), matches
// the pattern is called what the output template makes of the match (its
// own title when there is no template, or the template comes out empty),
// and one that doesn't match is left out when the rule is Only. No rule,
// or one that no longer compiles, only cleans.
func applyTitleRule(rule *store.TitleRule, refs []websource.ChapterRef) []websource.ChapterRef {
	out := make([]websource.ChapterRef, 0, len(refs))
	var re *regexp.Regexp
	if rule != nil && rule.Pattern != "" {
		re, _ = regexp.Compile(rule.Pattern)
	}
	for _, ref := range refs {
		folded := foldTitle(ref.Title)
		ref.Title = cleanTitle(ref.Title)
		if re == nil {
			out = append(out, ref)
			continue
		}
		m := re.FindStringSubmatchIndex(folded)
		if m == nil {
			if !rule.Only {
				out = append(out, ref)
			}
			continue
		}
		if rule.Output != "" {
			if title := cleanTitle(string(re.ExpandString(nil, rule.Output, folded, m))); title != "" {
				ref.Title = title
			}
		}
		out = append(out, ref)
	}
	return out
}

// handleSetSourceTitleRule sets (or, with an empty pattern, clears) the
// title rule of one of a book's sources, and checks the book again so
// what waits for review is listed by it. Chapters the book already has
// keep their names. 400 for a pattern that isn't a regular expression,
// 409 while the book is being checked.
func (s *Server) handleSetSourceTitleRule(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	var req sourceTitleRuleDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	rule, err := titleRule(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	src, err := s.Store.GetBookSource(r.PathValue("sourceId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if src == nil || src.BookID != bookID {
		writeError(w, http.StatusNotFound, "this book has no such source")
		return
	}
	if s.sourceBusy(bookID) {
		writeError(w, http.StatusConflict, "chapters are being fetched for this book right now - try again when that finishes")
		return
	}
	plan, err := s.Store.GetBookSourcePlan(src.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	plan.Title = rule
	if err := s.Store.SetBookSourcePlan(src.ID, plan); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.startSourceSync(bookID, nil)
	writeNoContent(w)
}

// idPrefixPattern is the prefix an added source's chapter ids carry.
var idPrefixPattern = regexp.MustCompile(`^[a-z]+:`)

// idPrefix is the prefix a stored chapter id carries, "" for an id of the
// source its book was imported from.
func idPrefix(id string) string {
	return idPrefixPattern.FindString(id)
}

// ownsID reports whether a stored chapter id is one of src's.
func ownsID(src *store.BookSource, id string) bool {
	return !strings.HasPrefix(id, epubOnlyPrefix) && idPrefix(id) == src.Prefix
}

// siteKind is the kind of the site a stored source reads: for a Wayback
// Machine one, the archived site's.
func siteKind(src *store.BookSource) string {
	if src.Kind == string(sourceWayback) {
		kind, _, _ := strings.Cut(src.SourceID, ":")
		return kind
	}
	return src.Kind
}

// prefixedSite is a site whose chapter ids all carry a prefix.
type prefixedSite struct {
	websource.Source
	prefix string
}

// withPrefix returns site with prefix on its chapter ids; site itself
// when there is none.
func withPrefix(site websource.Source, prefix string) websource.Source {
	if prefix == "" {
		return site
	}
	p := prefixedSite{Source: site, prefix: prefix}
	if d, ok := site.(websource.Delister); ok {
		return prefixedDelister{prefixedSite: p, delister: d}
	}
	return p
}

func (p prefixedSite) inner(ref websource.ChapterRef) websource.ChapterRef {
	ref.ID = strings.TrimPrefix(ref.ID, p.prefix)
	return ref
}

func (p prefixedSite) outer(refs []websource.ChapterRef) []websource.ChapterRef {
	out := make([]websource.ChapterRef, len(refs))
	for i, ref := range refs {
		ref.ID = p.prefix + ref.ID
		out[i] = ref
	}
	return out
}

func (p prefixedSite) Work(ctx context.Context, f websource.Fetcher, workID string) (*websource.Work, error) {
	work, err := p.Source.Work(ctx, f, workID)
	if err != nil {
		return nil, err
	}
	out := *work
	out.Chapters = p.outer(work.Chapters)
	return &out, nil
}

func (p prefixedSite) Chapter(ctx context.Context, f websource.Fetcher, ref websource.ChapterRef) (*websource.Chapter, error) {
	ch, err := p.Source.Chapter(ctx, f, p.inner(ref))
	if err != nil {
		return nil, err
	}
	ch.ID = ref.ID
	return ch, nil
}

type prefixedDelister struct {
	prefixedSite
	delister websource.Delister
}

func (p prefixedDelister) unprefixed(work *websource.Work) *websource.Work {
	out := *work
	out.Chapters = make([]websource.ChapterRef, len(work.Chapters))
	for i, ref := range work.Chapters {
		out.Chapters[i] = p.inner(ref)
	}
	return &out
}

func (p prefixedDelister) ChapterPrefix(work *websource.Work) string {
	return p.delister.ChapterPrefix(p.unprefixed(work))
}

func (p prefixedDelister) ArchivedChapter(work *websource.Work, pageURL *url.URL) (websource.ChapterRef, bool) {
	ref, ok := p.delister.ArchivedChapter(p.unprefixed(work), pageURL)
	ref.ID = p.prefix + ref.ID
	return ref, ok
}

func (p prefixedDelister) ListedChapters(ctx context.Context, f websource.Fetcher, work *websource.Work) ([]websource.ChapterRef, error) {
	refs, err := p.delister.ListedChapters(ctx, f, p.unprefixed(work))
	if err != nil {
		return nil, err
	}
	return p.outer(refs), nil
}

func (p prefixedDelister) Before(a, b websource.ChapterRef) bool {
	return p.delister.Before(p.inner(a), p.inner(b))
}

// readerForChapter returns how the chapter stored under sourceID (as
// chapters.source_id holds it) is read: through the first of sources that
// owns its id - any of them for a chapter put in from an epub, which is
// kept as it was read and needs no site's parser. ok is false when none
// does, or this build can't read it.
func (s *Server) readerForChapter(sources []store.BookSource, sourceID string) (sourceReader, bool) {
	ids := strings.Fields(sourceID)
	if len(ids) == 0 {
		return sourceReader{}, false
	}
	fromEpub := strings.HasPrefix(ids[0], epubOnlyPrefix)
	for i := range sources {
		if fromEpub {
			if r, _, ok := s.readerForBook(&sources[i]); ok {
				return r, true
			}
			continue
		}
		if ownsID(&sources[i], ids[0]) {
			r, _, ok := s.readerForBook(&sources[i])
			return r, ok
		}
	}
	return sourceReader{}, false
}

// newSourcePrefix picks the id prefix for a source of the site kind being
// added to a book that tracks existing: none when its ids can't be taken
// for another site's - the book has no chapters from a website yet, or
// its unprefixed ones are from this same site.
func (s *Server) newSourcePrefix(bookID string, existing []store.BookSource, kind string) (string, error) {
	for i := range existing {
		if existing[i].Prefix == "" && siteKind(&existing[i]) == kind {
			return "", nil
		}
	}
	if len(existing) == 0 {
		ids, err := s.Store.ChapterSourceIDs(bookID)
		if err != nil {
			return "", err
		}
		bare := false
		for _, id := range ids {
			bare = bare || (idPrefix(id) == "" && !strings.HasPrefix(id, epubOnlyPrefix))
		}
		if !bare {
			return "", nil
		}
	}
	return kind + ":", nil
}

// handleAddBookSource has a book track another source: a link to the same
// work somewhere else it is posted. Nothing is imported by this: the check
// it starts recognizes the entries the book already has (by title) and
// leaves the rest waiting for review, like anything a later check finds.
// 201 with the source; 409 while the book is being checked, or when the
// library already tracks that work.
func (s *Server) handleAddBookSource(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	var req addBookSourceRequest
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
	if _, ok := archiveOrgItem(req.URL); ok {
		writeError(w, http.StatusBadRequest, "an archive.org item is a file, not somewhere new chapters appear - import it as a book instead")
		return
	}
	reader, link, err := s.readerForLink(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rule, err := titleRule(req.TitleRule)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.sourceBusy(bookID) {
		writeError(w, http.StatusConflict, "chapters are being fetched for this book right now - try again when that finishes")
		return
	}
	s.sourceImportMu.Lock()
	defer s.sourceImportMu.Unlock()
	work, err := s.resolveWork(r, reader, link)
	if err != nil {
		writeBuildError(w, err)
		return
	}
	if existing, err := s.Store.FindBookBySource(reader.kind(), reader.storedID(work.ID)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if existing == bookID {
		writeError(w, http.StatusConflict, "this book already tracks \""+work.Title+"\" at "+reader.name())
		return
	} else if existing != "" {
		title := "another book"
		if other, _ := s.Store.GetBook(existing); other != nil {
			title = "\"" + other.Title + "\""
		}
		writeErrorCode(w, http.StatusConflict, errBookExists, "\""+work.Title+"\" at "+reader.name()+" is already tracked by "+title)
		return
	}
	sources, err := s.Store.BookSources(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	prefix, err := s.newSourcePrefix(bookID, sources, reader.site.Kind())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	src := store.BookSource{BookID: bookID, Kind: reader.kind(), SourceID: reader.storedID(work.ID), URL: reader.workURL(work), Prefix: prefix}
	if err := s.Store.SaveBookSource(&src); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Reviewed with nothing approved: whatever the check doesn't recognize
	// as a chapter the book has waits for the reader.
	if err := s.Store.SetBookSourcePlan(src.ID, store.SourcePlan{Reviewed: true, Title: rule}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.startSourceSync(bookID, nil)
	writeJSON(w, http.StatusCreated, trackedSourceDTO{ID: src.ID, Kind: bookSourceKind(src.Kind), Name: reader.name(), URL: src.URL, TitleRule: titleRuleDTO(rule)})
}

// handleRemoveBookSource has a book stop tracking one of its sources. The
// chapters it took from there stay. 409 while the book is being checked.
func (s *Server) handleRemoveBookSource(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	src, err := s.Store.GetBookSource(r.PathValue("sourceId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if src == nil || src.BookID != bookID {
		writeError(w, http.StatusNotFound, "this book has no such source")
		return
	}
	if s.sourceBusy(bookID) {
		writeError(w, http.StatusConflict, "chapters are being fetched for this book right now - try again when that finishes")
		return
	}
	if err := s.Store.DeleteBookSource(src.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// otherChapters are the chapters of a book that src's entries may be
// second copies of: the ones holding no id of src's own.
func (s *Server) otherChapters(src *store.BookSource) ([]store.SourcedChapter, error) {
	all, err := s.Store.ChapterSources(src.BookID)
	if err != nil {
		return nil, err
	}
	var out []store.SourcedChapter
next:
	for _, c := range all {
		for _, id := range strings.Fields(c.SourceID) {
			if ownsID(src, id) {
				continue next
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// bookChapter is the numbering a title opens with when it gives both its
// book and its chapter in it: "5-19. The Wind", "5.19 The Wind".
var bookChapter = regexp.MustCompile(`^\s*(\d+)\s*[-.]\s*(\d+)(?:\D|$)`)

// numberingKey is that numbering ("5-19"), "" for a title without one.
func numberingKey(title string) string {
	if m := bookChapter.FindStringSubmatch(foldTitle(title)); m != nil {
		return m[1] + "-" + m[2]
	}
	return ""
}

// matchOtherSources finds, among the entries of src the book doesn't have
// by id, the ones it has from somewhere else, by title: an entry and a
// chapter that share a title (titleKey) nothing else on either side
// shares, and don't number themselves differently - or, the names
// differing (an author renames a chapter between posting it in one place
// and another), that open with the same book-and-chapter numbering
// (numberingKey), again shared by nothing else. It returns their ids.
func matchOtherSources(others []store.SourcedChapter, planned []websource.ChapterRef, have map[string]bool) []string {
	if len(others) == 0 {
		return nil
	}
	byKey := map[string][]store.SourcedChapter{}
	for _, c := range others {
		if key := titleKey(c.Title); key != "" {
			byKey[key] = append(byKey[key], c)
		}
	}
	entries := map[string]int{}
	for _, ref := range planned {
		if !have[ref.ID] {
			entries[titleKey(ref.Title)]++
		}
	}
	var matched []string
	taken := map[int]bool{}   // chapters matched by title
	done := map[string]bool{} // entries matched by title
	for _, ref := range planned {
		key := titleKey(ref.Title)
		if have[ref.ID] || entries[key] != 1 || len(byKey[key]) != 1 {
			continue
		}
		a, aNumbered := chapterNumber(ref.Title)
		b, bNumbered := chapterNumber(byKey[key][0].Title)
		if aNumbered && bNumbered && a != b {
			continue
		}
		matched = append(matched, ref.ID)
		taken[byKey[key][0].Idx] = true
		done[ref.ID] = true
	}
	// What is left, by numbering.
	byNumber := map[string][]store.SourcedChapter{}
	for _, c := range others {
		if key := numberingKey(c.Title); key != "" && !taken[c.Idx] {
			byNumber[key] = append(byNumber[key], c)
		}
	}
	numbered := map[string]int{}
	for _, ref := range planned {
		if !have[ref.ID] && !done[ref.ID] {
			numbered[numberingKey(ref.Title)]++
		}
	}
	for _, ref := range planned {
		key := numberingKey(ref.Title)
		if key == "" || have[ref.ID] || done[ref.ID] || numbered[key] != 1 || len(byNumber[key]) != 1 {
			continue
		}
		matched = append(matched, ref.ID)
	}
	return matched
}

// lockedCopies finds, among the entries of a source the book doesn't have
// by id, the locked ones named like a chapter the book has from somewhere
// else: the same title (titleKey) as exactly one such chapter without
// numbering itself differently, or the same book-and-chapter numbering.
// Unlike matchOtherSources it doesn't ask that the entry be the only one
// of its name - a creator who posts each chapter twice, early for patrons
// and again for everyone, lists two - because nothing is lost by it: a
// locked entry can't be imported, and these are only left out of what
// waits for review while they stay locked, not remembered as had.
func lockedCopies(others []store.SourcedChapter, planned []websource.ChapterRef, have map[string]bool) []string {
	if len(others) == 0 {
		return nil
	}
	byKey := map[string][]store.SourcedChapter{}
	byNumber := map[string]int{}
	for _, c := range others {
		if key := titleKey(c.Title); key != "" {
			byKey[key] = append(byKey[key], c)
		}
		if key := numberingKey(c.Title); key != "" {
			byNumber[key]++
		}
	}
	var copies []string
	for _, ref := range planned {
		if have[ref.ID] || ref.Unlocked || ref.Delisted {
			continue
		}
		if same := byKey[titleKey(ref.Title)]; len(same) == 1 {
			a, aNumbered := chapterNumber(ref.Title)
			b, bNumbered := chapterNumber(same[0].Title)
			if !aNumbered || !bNumbered || a == b {
				copies = append(copies, ref.ID)
				continue
			}
		}
		if key := numberingKey(ref.Title); key != "" && byNumber[key] == 1 {
			copies = append(copies, ref.ID)
		}
	}
	return copies
}

// openingOf is how a parsed chapter starts: its first paragraphs after
// the title, as store.ChapterOpenings gives a stored one's.
func openingOf(input store.ChapterInput) string {
	var b strings.Builder
	for _, blk := range input.Blocks {
		if b.Len() >= openingChars {
			break
		}
		if blk.Kind == store.BlockText && blk.Text != input.Title {
			b.WriteString(blk.Text)
			b.WriteByte(' ')
		}
	}
	return b.String()
}

// sameOpening reports whether a fetched chapter opens the way one of the
// book's chapters from another source does - the same chapter under a
// title that didn't match.
func sameOpening(fetched *websource.Chapter, openings []map[string]bool) bool {
	if len(openings) == 0 {
		return false
	}
	input, _, err := sourceChapterInput(websource.Meta{Title: fetched.Title}, fetched)
	if err != nil {
		return false
	}
	want := shingles(openingOf(input))
	if len(want) < 8 {
		return false
	}
	for _, have := range openings {
		shared := 0
		for sh := range want {
			if have[sh] {
				shared++
			}
		}
		if float64(shared)/float64(min(len(want), len(have))) >= 0.7 {
			return true
		}
	}
	return false
}

// otherOpenings is how each of others opens, for sameOpening.
func (s *Server) otherOpenings(bookID string, others []store.SourcedChapter) []map[string]bool {
	if len(others) == 0 {
		return nil
	}
	want := make(map[int]bool, len(others))
	for _, c := range others {
		want[c.Idx] = true
	}
	all, err := s.Store.ChapterOpenings(bookID, openingChars)
	if err != nil {
		return nil
	}
	var out []map[string]bool
	for _, o := range all {
		if set := shingles(o.Text); want[o.Idx] && len(set) >= 8 {
			out = append(out, set)
		}
	}
	return out
}
