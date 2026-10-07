package httpapi

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/epub"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// Importing by link can be looked over first. A preview resolves the link
// and answers with the work's chapter list for the reader to choose from;
// the import that follows names the preview and the chapters wanted. In
// between, the preview can be widened:
//
//   - with the Internet Archive, for a site that takes chapters off a
//     work's list (websource.Delister - Royal Road's "stubbed" fictions):
//     every chapter the archive has a copy of joins the list, in order;
//   - with an epub of the published part, whose chapters stand in for the
//     ones the site won't serve, matched by title (source_fill.go).
//
// A preview lives in memory (Server.importPreviews) for previewTTL; the
// import turns it into the book's store.SourcePlan - what was left out,
// and which delisted chapters are still to fetch - so later checks for
// new chapters respect the choice.

const previewTTL = 2 * time.Hour

// importPreview is one link looked over, waiting to be imported.
type importPreview struct {
	mu       sync.Mutex
	created  time.Time
	reader   sourceReader
	work     *websource.Work
	chapters []websource.ChapterRef // the list offered, in reading order
	// archive is true once the archive has been searched for delisted
	// chapters.
	archive bool
	fill    *epubFill
}

// importChapterSource says where a previewed chapter's text would come from.
type importChapterSource string

const (
	// chapterFromSite: the site serves it.
	chapterFromSite importChapterSource = "site"
	// chapterFromArchive: the site no longer lists it; the Internet
	// Archive has a copy of its page.
	chapterFromArchive importChapterSource = "archive"
	// chapterFromEpub: the site won't serve it, and the attached epub has it.
	chapterFromEpub importChapterSource = "epub"
	// chapterLocked: the site lists it but marks it unreadable (early
	// access), and nothing else has it: an import stops at it.
	chapterLocked importChapterSource = "locked"
)

type importPreviewChapter struct {
	ID     string              `json:"id"`
	Title  string              `json:"title"`
	Source importChapterSource `json:"source"`
	// ReplacedBy is set on a chapter from the archive whose place the site
	// has since given to something else - a notice or summary where a
	// chapter was: the title of what is listed there now. Both are in the
	// list, this one first.
	ReplacedBy string `json:"replacedBy,omitempty"`
}

type importPreviewResponse struct {
	// ID names this preview in the import request, and in a further
	// preview request that widens it.
	ID     string `json:"id"`
	Title  string `json:"title"`
	Author string `json:"author"`
	// Site is the site's name ("Royal Road").
	Site string `json:"site"`
	// Chapters is every chapter that can be chosen, in reading order.
	Chapters []importPreviewChapter `json:"chapters"`
	// CanSearchArchive is true when the site takes chapters off its lists
	// and the Internet Archive hasn't been searched for them yet.
	CanSearchArchive bool `json:"canSearchArchive"`
	// ArchiveSearched is true once it has.
	ArchiveSearched bool `json:"archiveSearched"`
	// EpubChapters is how many chapters the attached epub has, 0 with none
	// attached.
	EpubChapters int `json:"epubChapters"`
	// ExistingBookID is the library's book of this work, if it has one
	// already - an import would be refused.
	ExistingBookID string `json:"existingBookId,omitempty"`
}

// handlePreviewImportURL looks a link over before importing it:
// ?url=<link> resolves it and lists the work's chapters; ?preview=<id>
// returns to an earlier preview instead, to widen it. ?archive=true also
// searches the Internet Archive for chapters the site has delisted (slow:
// up to a minute), and a multipart "file" attaches an epub whose chapters
// fill in for ones the site won't serve. Nothing is imported.
func (s *Server) handlePreviewImportURL(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	s.pruneImportPreviews()

	var id string
	var p *importPreview
	if id = q.Get("preview"); id != "" {
		v, ok := s.importPreviews.Load(id)
		if !ok {
			writeError(w, http.StatusGone, "that preview has expired - look the link up again")
			return
		}
		p = v.(*importPreview)
	} else {
		link := q.Get("url")
		if _, ok := archiveOrgItem(link); ok {
			writeError(w, http.StatusBadRequest, "an archive.org item is imported whole - there are no chapters to choose")
			return
		}
		reader, u, err := s.readerForLink(link)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		work, err := s.resolveWork(r, reader, u)
		if err != nil {
			writeBuildError(w, err)
			return
		}
		id = store.NewID()
		p = &importPreview{created: time.Now(), reader: reader, work: work, chapters: work.Chapters}
		s.importPreviews.Store(id, p)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	delister, canArchive := p.reader.site.(websource.Delister)
	canArchive = canArchive && !p.reader.archived()
	if q.Get("archive") == "true" && canArchive && !p.archive {
		all, err := s.wayback().Delisted(r.Context(), delister, p.work)
		if err != nil {
			writeError(w, http.StatusBadGateway, "could not search the Internet Archive: "+err.Error())
			return
		}
		p.chapters, p.archive = all, true
	}
	// Optional, as on chapter re-import: a request with no file isn't
	// multipart at all.
	if file, _, err := r.FormFile("file"); err == nil {
		defer file.Close()
		fill, err := readEpubFill(file)
		if err != nil {
			writeError(w, http.StatusBadRequest, "could not parse epub: "+err.Error())
			return
		}
		p.fill = fill
	}

	resp := importPreviewResponse{
		ID: id, Title: p.work.Title, Author: p.work.Author, Site: p.reader.name(),
		Chapters:         make([]importPreviewChapter, 0, len(p.chapters)),
		CanSearchArchive: canArchive && !p.archive,
		ArchiveSearched:  p.archive,
	}
	if p.fill != nil {
		resp.EpubChapters = len(p.fill.chapters)
	}
	for _, ref := range p.chapters {
		src := chapterFromSite
		switch {
		case (ref.Delisted || !ref.Unlocked) && p.fill != nil && p.fill.has(ref):
			src = chapterFromEpub
		case ref.Delisted:
			src = chapterFromArchive
		case !ref.Unlocked:
			src = chapterLocked
		}
		resp.Chapters = append(resp.Chapters, importPreviewChapter{ID: ref.ID, Title: ref.Title, Source: src, ReplacedBy: ref.ReplacedBy})
	}
	if existing, err := s.Store.FindBookBySource(p.reader.kind(), p.reader.storedID(p.work.ID)); err == nil {
		resp.ExistingBookID = existing
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveWork finds the work a link means - for a link naming several
// (a serial's volumes), the first the library doesn't have - and fetches
// it. Errors carry their HTTP status (httpError).
func (s *Server) resolveWork(r *http.Request, reader sourceReader, link *url.URL) (*websource.Work, error) {
	ctx := r.Context()
	candidates, err := reader.site.Resolve(ctx, reader.fetch, link)
	if err != nil {
		return nil, sourceError(reader.site, err, http.StatusBadRequest)
	}
	workID := ""
	for _, id := range candidates {
		found, err := s.Store.FindBookBySource(reader.kind(), reader.storedID(id))
		if err != nil {
			return nil, err
		}
		if found == "" {
			workID = id
			break
		}
	}
	if workID == "" && len(candidates) > 0 {
		// All here already: show the first, which the preview then says
		// the library has.
		workID = candidates[0]
	}
	work, err := reader.site.Work(ctx, reader.fetch, workID)
	if err != nil {
		return nil, sourceError(reader.site, err, http.StatusBadGateway)
	}
	return work, nil
}

// sourceError is writeSourceError's answer as an error with its status.
func sourceError(site websource.Source, err error, status int) error {
	if errors.Is(err, websource.ErrNotFound) {
		return httpError(http.StatusNotFound, site.Name()+" has no such work (it may have been removed)")
	}
	if status == http.StatusBadGateway {
		return httpError(status, "could not read from "+site.Name()+": "+err.Error())
	}
	return httpError(status, err.Error())
}

// readEpubFill parses an uploaded epub for filling chapters from.
func readEpubFill(file io.Reader) (*epubFill, error) {
	tmp, err := os.CreateTemp("", "preview-*.epub")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, file)
	if err != nil {
		return nil, err
	}
	parsed, err := epub.ParseAs(tmp, size, epub.PlayOff)
	if err != nil {
		return nil, err
	}
	return newEpubFill(parsed), nil
}

// pruneImportPreviews forgets previews nobody came back for.
func (s *Server) pruneImportPreviews() {
	s.importPreviews.Range(func(key, value any) bool {
		if time.Since(value.(*importPreview).created) > previewTTL {
			s.importPreviews.Delete(key)
		}
		return true
	})
}

// chosenChapters returns the chapters of a preview an import asked for, in
// the preview's order, and the plan that remembers the choice: the ids
// left out, and the chosen delisted chapters. No ids means all of them.
func (p *importPreview) chosenChapters(ids []string) ([]websource.ChapterRef, store.SourcePlan) {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	var chosen []websource.ChapterRef
	var plan store.SourcePlan
	for _, ref := range p.chapters {
		if len(ids) > 0 && !want[ref.ID] {
			plan.Skip = append(plan.Skip, ref.ID)
			continue
		}
		chosen = append(chosen, ref)
		if ref.Delisted {
			plan.Extra = append(plan.Extra, store.PlannedChapter{ID: ref.ID, Title: ref.Title, URL: ref.URL})
		}
	}
	return chosen, plan
}

// plannedChapters is the chapter list a check of a book's source works
// through: what the site lists now, less what the book's plan leaves out,
// with the plan's delisted chapters put back in their places.
func plannedChapters(site websource.Source, listed []websource.ChapterRef, plan store.SourcePlan) []websource.ChapterRef {
	skip := make(map[string]bool, len(plan.Skip))
	for _, id := range plan.Skip {
		skip[id] = true
	}
	have := map[string]bool{}
	var kept []websource.ChapterRef
	for _, ref := range listed {
		if !skip[ref.ID] {
			kept = append(kept, ref)
			have[ref.ID] = true
		}
	}
	var extra []websource.ChapterRef
	for _, c := range plan.Extra {
		// Relisted since: the site's own entry stands.
		if !have[c.ID] {
			extra = append(extra, websource.ChapterRef{ID: c.ID, Title: c.Title, URL: c.URL, Unlocked: true, Delisted: true})
		}
	}
	if len(extra) == 0 {
		return kept
	}
	delister, ok := site.(websource.Delister)
	if !ok {
		return append(extra, kept...)
	}
	out := make([]websource.ChapterRef, 0, len(kept)+len(extra))
	for _, ref := range kept {
		for len(extra) > 0 && delister.Before(extra[0], ref) {
			out = append(out, extra[0])
			extra = extra[1:]
		}
		out = append(out, ref)
	}
	return append(out, extra...)
}
