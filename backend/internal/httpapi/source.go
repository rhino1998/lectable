package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/epub"
	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/royalroad"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/wanderinginn"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// A book can be imported from a website instead of an uploaded epub: any
// site with a websource.Source (Server.sources - Royal Road, The Wandering
// Inn), picked by the link itself. The book is created from the work's
// first chapter, so it is in the library and readable at once, and the
// rest are fetched in the background and appended in order
// (syncBookSource). It remembers its source (store.BookSource), so the
// same pass run again later - "check for new chapters" - adds whatever has
// been published since; chapters the book already has are never touched.
//
// What is fetched is also kept as the book's source epub, one spine file
// per chapter, so re-importing a single chapter works as for any book.
//
// A site can also be read as the Internet Archive saved it: a Wayback
// Machine link imports a work as it stood then (sourceReader), and a
// chapter the live site lists but no longer serves is looked for among its
// archived copies before the check gives up on it (see syncBookSource,
// and source_fill.go for filling such chapters from an uploaded epub). An
// archive.org item link is something else - a file to download, imported
// like an upload (source_archiveorg.go).

// bookSourceKind names the website a book was imported from.
type bookSourceKind string

const (
	sourceRoyalRoad    bookSourceKind = royalroad.Kind
	sourceWanderingInn bookSourceKind = wanderinginn.Kind
	// sourceWayback is a work imported from a Wayback Machine link: a
	// supported site as archived at one time. Its stored id is
	// "<site kind>:<time>:<work id>".
	sourceWayback bookSourceKind = "wayback"
)

// bookSourceDTO is where a book imported from a website came from, and
// the state of fetching its chapters.
type bookSourceDTO struct {
	Kind bookSourceKind `json:"kind"`
	// Name is the site's name, for display ("Royal Road").
	Name string `json:"name"`
	// URL is the work's page at the source.
	URL string `json:"url"`
	// CheckedAt is when the source was last checked for chapters (unix
	// seconds), absent if never.
	CheckedAt int64 `json:"checkedAt,omitempty"`
	// RemoteChapters is the book's chapter count plus the chapters the
	// last check found at the source and hadn't fetched yet: more than
	// chapterCount while a check is running, or after one that stopped
	// early.
	RemoteChapters int `json:"remoteChapters"`
	// Error is why the last check stopped early, absent if it finished.
	Error string `json:"error,omitempty"`
	// Blocked is the title of the chapter the last check stopped at
	// because the source lists it but doesn't serve it yet (early access,
	// patrons only); absent if none. The book never skips past one.
	Blocked string `json:"blocked,omitempty"`
	// Syncing is true while chapters are being fetched.
	Syncing bool `json:"syncing"`
}

type importBookURLRequest struct {
	// URL is a link to a work at a supported site, or one of its chapters.
	URL string `json:"url"`
}

const (
	// sourceBatch is how many fetched chapters are appended at a time:
	// each append rebuilds the library's book summaries and rewrites the
	// kept epub, so not one per chapter.
	sourceBatch = 10
	// sourceSyncTimeout bounds one whole check; what it fetched before
	// then is kept, and the next check carries on from there.
	sourceSyncTimeout = 3 * time.Hour
)

// defaultSources is every site books can be imported from, used when
// Server.Sources is unset; one set for the process, so every import from
// a site shares its request pacing.
var defaultSources = []websource.Source{&royalroad.Source{}, &wanderinginn.Source{}}

func (s *Server) sources() []websource.Source {
	if s.Sources != nil {
		return s.Sources
	}
	return defaultSources
}

// sourceOfKind returns the source books of that kind came from, nil if
// this build has none (a book restored from another library's export).
func (s *Server) sourceOfKind(kind string) websource.Source {
	for _, src := range s.sources() {
		if src.Kind() == kind {
			return src
		}
	}
	return nil
}

// defaultWayback reads the Wayback Machine when Server.Wayback is unset.
var defaultWayback = &websource.Wayback{}

func (s *Server) wayback() *websource.Wayback {
	if s.Wayback != nil {
		return s.Wayback
	}
	return defaultWayback
}

// sourceReader is a site read one particular way: live, or as the Wayback
// Machine has it at one time.
type sourceReader struct {
	site  websource.Source
	fetch websource.Fetcher
	// timestamp is the archived time read at, "" for the live site.
	timestamp string
	wayback   *websource.Wayback
}

func (r sourceReader) archived() bool { return r.timestamp != "" }

// kind and storedID are what a book read this way records as its source.
func (r sourceReader) kind() string {
	if r.archived() {
		return string(sourceWayback)
	}
	return r.site.Kind()
}

func (r sourceReader) storedID(workID string) string {
	if r.archived() {
		return r.site.Kind() + ":" + r.timestamp + ":" + workID
	}
	return workID
}

func (r sourceReader) name() string {
	if r.archived() {
		return r.site.Name() + " (Wayback Machine)"
	}
	return r.site.Name()
}

// workURL is the page a book read this way links back to.
func (r sourceReader) workURL(work *websource.Work) string {
	if r.archived() {
		return r.wayback.LinkAt(r.timestamp, work.URL)
	}
	return work.URL
}

func (r sourceReader) meta(work *websource.Work) websource.Meta {
	return websource.Meta{Kind: r.kind(), WorkID: r.storedID(work.ID), URL: r.workURL(work), Title: work.Title, Author: work.Author}
}

// readerForLink parses a pasted link and finds the site it belongs to; a
// Wayback Machine link to a supported site reads that site as archived.
func (s *Server) readerForLink(raw string) (sourceReader, *url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return sourceReader{}, nil, errors.New("no link given")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return sourceReader{}, nil, errors.New("that isn't a link")
	}
	timestamp, original, archived := websource.ParseWaybackLink(u)
	if archived {
		u = original
	}
	names := make([]string, 0, len(s.sources()))
	for _, src := range s.sources() {
		if !src.Match(u) {
			names = append(names, src.Name())
			continue
		}
		if archived {
			return sourceReader{site: src, fetch: s.wayback().At(timestamp), timestamp: timestamp, wayback: s.wayback()}, u, nil
		}
		return sourceReader{site: src, fetch: src.Fetch()}, u, nil
	}
	return sourceReader{}, nil, fmt.Errorf("can't import from %s - supported sites: %s (also as Wayback Machine links), and archive.org items with an epub", u.Hostname(), strings.Join(names, ", "))
}

// readerForBook returns how a book's stored source is read, and the work's
// id at the site; ok is false when this build has no reader for it (a book
// restored from another library's export).
func (s *Server) readerForBook(src *store.BookSource) (r sourceReader, workID string, ok bool) {
	kind, workID := src.Kind, src.SourceID
	if src.Kind == string(sourceWayback) {
		parts := strings.SplitN(src.SourceID, ":", 3)
		if len(parts) != 3 {
			return sourceReader{}, "", false
		}
		kind, workID = parts[0], parts[2]
		r.timestamp = parts[1]
		r.wayback = s.wayback()
	}
	if r.site = s.sourceOfKind(kind); r.site == nil {
		return sourceReader{}, "", false
	}
	r.fetch = r.site.Fetch()
	if r.archived() {
		r.fetch = r.wayback.At(r.timestamp)
	}
	return r, workID, true
}

// bookSource builds bookID's source DTO, nil for a book from an epub.
func (s *Server) bookSource(bookID string) (*bookSourceDTO, error) {
	src, err := s.Store.GetBookSource(bookID)
	if err != nil || src == nil {
		return nil, err
	}
	syncing := s.sourceBusy(bookID)
	name := src.Kind
	if r, _, ok := s.readerForBook(src); ok {
		name = r.name()
	}
	return &bookSourceDTO{
		Kind:           bookSourceKind(src.Kind),
		Name:           name,
		URL:            src.URL,
		CheckedAt:      src.CheckedAt,
		RemoteChapters: src.RemoteChapters,
		Error:          src.Error,
		Blocked:        src.Blocked,
		Syncing:        syncing,
	}, nil
}

// handleImportBookURL imports a work from a website as a new book - a
// Royal Road fiction, a volume of The Wandering Inn, either of them as a
// Wayback Machine link - picking the site by the link: its first chapter
// now, the rest in the background (the book's source.syncing /
// remoteChapters show the progress). Nothing is generated for it until
// it is read or asked for. 409 book_exists if the library
// already has that work - check it for new chapters instead. A link to an
// archive.org item instead downloads its epub and imports that, as an
// upload would.
func (s *Server) handleImportBookURL(w http.ResponseWriter, r *http.Request) {
	var req importBookURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if item, ok := archiveOrgItem(req.URL); ok {
		summary, err := s.importArchiveOrgItem(r.Context(), item)
		if err != nil {
			writeBuildError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, summary)
		return
	}
	reader, link, err := s.readerForLink(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	site := reader.site

	// One import at a time, so the same link sent twice can't make two books.
	s.sourceImportMu.Lock()
	defer s.sourceImportMu.Unlock()

	ctx := r.Context()
	candidates, err := site.Resolve(ctx, reader.fetch, link)
	if err != nil {
		writeSourceError(w, site, err, http.StatusBadRequest)
		return
	}
	// A link to a whole site names several works (a serial's volumes):
	// take the first the library doesn't have.
	workID, existing := "", ""
	for _, id := range candidates {
		found, err := s.Store.FindBookBySource(reader.kind(), reader.storedID(id))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if found == "" {
			workID = id
			break
		}
		if existing == "" {
			existing = found
		}
	}
	if workID == "" {
		title := "This"
		if b, _ := s.Store.GetBook(existing); b != nil {
			title = "\"" + b.Title + "\""
		}
		writeErrorCode(w, http.StatusConflict, errBookExists, title+" is already in the library - check it for new chapters instead")
		return
	}

	work, err := site.Work(ctx, reader.fetch, workID)
	if err != nil {
		writeSourceError(w, site, err, http.StatusBadGateway)
		return
	}
	if len(work.Chapters) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "\""+work.Title+"\" has no chapters to read yet")
		return
	}
	first, err := s.readChapter(ctx, reader, work.Chapters[0])
	if errors.Is(err, websource.ErrLocked) {
		writeError(w, http.StatusUnprocessableEntity, "\""+work.Title+"\" can't be read yet: its first chapter is locked at "+site.Name())
		return
	}
	if err != nil {
		writeSourceError(w, site, err, http.StatusBadGateway)
		return
	}
	meta := reader.meta(work)
	if work.CoverURL != "" {
		// Best-effort, like an epub's cover.
		if data, ext, err := websource.Cover(ctx, reader.fetch, work.CoverURL); err == nil && ext != "" {
			meta.Cover, meta.CoverExt = data, ext
		}
	}

	input, imageBytes, err := sourceChapterInput(meta, first.ch)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	title := work.Title
	if reader.archived() {
		// Beside the live work's book, if both are in the library.
		title += " (archived " + archiveDate(reader.timestamp) + ")"
	}
	bookID, createdImages, err := s.Store.CreateBook(title, work.Author, "en", meta.CoverExt, work.SeriesName, work.SeriesIndex, []store.ChapterInput{input})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save book: "+err.Error())
		return
	}
	if err := s.Store.SetBookSource(bookID, store.BookSource{
		Kind: reader.kind(), SourceID: reader.storedID(work.ID), URL: reader.workURL(work), CheckedAt: time.Now().Unix(), RemoteChapters: len(work.Chapters),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save book: "+err.Error())
		return
	}
	if err := s.writeSourceEpub(bookID, meta, []*websource.Chapter{first.ch}, true); err != nil {
		log.Printf("source: keeping epub of %s: %v", bookID, err)
	}
	s.writeRawChapter(bookID, first)
	if meta.CoverExt != "" {
		if err := audiopath.EnsureCoverDir(s.DataDir); err == nil {
			_ = os.WriteFile(audiopath.CoverFile(s.DataDir, bookID, meta.CoverExt), meta.Cover, 0o644)
		}
	}
	s.writeChapterImages(bookID, createdImages, imageBytes)

	b, err := s.Store.GetBook(bookID)
	if err != nil || b == nil {
		writeError(w, http.StatusInternalServerError, "book saved but could not be reloaded")
		return
	}
	if len(work.Chapters) > 1 {
		s.startSourceSync(bookID, nil)
	}
	summary, _, err := s.buildBookSummary(*b)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Unlike an upload, nothing is queued for the first chapter - no sample
	// narration, no length estimate: links come in by the handful (a
	// serial's volumes one after another), and each would otherwise start
	// rendering at once. The book generates when it is read or asked to.
	writeJSON(w, http.StatusCreated, summary)
}

// archiveDate formats a Wayback Machine timestamp's date (2024-05-16),
// as much of it as the timestamp gives.
func archiveDate(timestamp string) string {
	switch {
	case len(timestamp) >= 8:
		return timestamp[:4] + "-" + timestamp[4:6] + "-" + timestamp[6:8]
	case len(timestamp) >= 6:
		return timestamp[:4] + "-" + timestamp[4:6]
	}
	return timestamp
}

// sourced is a chapter with what it was made from - the pages as fetched,
// kept beside the book (writeRawChapter) so the chapter can be cleaned up
// again later without fetching (see source_raw.go).
type sourced struct {
	ch  *websource.Chapter
	raw *websource.RawChapter
}

// fetchChapter reads one chapter through f, keeping what was fetched.
func fetchChapter(ctx context.Context, site websource.Source, f websource.Fetcher, ref websource.ChapterRef, via string) (sourced, error) {
	rec := &websource.Recorder{Inner: f}
	ch, err := site.Chapter(ctx, rec, ref)
	if err != nil {
		return sourced{}, err
	}
	return sourced{ch: ch, raw: &websource.RawChapter{Ref: ref, Via: via, FetchedAt: time.Now().Unix(), Fetches: rec.Fetches()}}, nil
}

// via names how a reader's chapters are read, for RawChapter.Via.
func (r sourceReader) via() string {
	if r.archived() {
		return "wayback"
	}
	return "live"
}

// readChapter fetches one chapter through r. One the live site lists but
// doesn't serve to the public is looked for in the Wayback Machine before
// answering ErrLocked (archivedChapter).
func (s *Server) readChapter(ctx context.Context, r sourceReader, ref websource.ChapterRef) (sourced, error) {
	got, err := fetchChapter(ctx, r.site, r.fetch, ref, r.via())
	if errors.Is(err, websource.ErrLocked) {
		return s.archivedChapter(ctx, r, ref)
	}
	return got, err
}

// archivedChapter reads a chapter the live site won't serve from the
// Wayback Machine: the last copy of its page saved while it was readable.
// ErrLocked when there is none - and without looking for a chapter the
// site's own list marks locked, which was never public (early access).
func (s *Server) archivedChapter(ctx context.Context, r sourceReader, ref websource.ChapterRef) (sourced, error) {
	if r.archived() || !ref.Unlocked {
		return sourced{}, websource.ErrLocked
	}
	var got sourced
	_, err := s.wayback().ArchivedChapter(ctx, ref.URL, func(f websource.Fetcher) (*websource.Chapter, error) {
		var err error
		got, err = fetchChapter(ctx, r.site, f, ref, "wayback")
		return got.ch, err
	})
	if err != nil {
		if !errors.Is(err, websource.ErrLocked) {
			log.Printf("source: looking for %q in the Wayback Machine: %v", ref.Title, err)
		}
		return sourced{}, websource.ErrLocked
	}
	log.Printf("source: %q is locked at %s; read from the Wayback Machine", ref.Title, r.site.Name())
	return got, nil
}

// handleSyncBookSource checks a book's source for chapters it doesn't have
// and appends them, as a job-queue task (joining a check already queued or
// running - see startSourceSync).
// 409 for a book that wasn't imported from a website.
func (s *Server) handleSyncBookSource(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	b, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	src, err := s.Store.GetBookSource(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if src == nil {
		writeError(w, http.StatusConflict, "this book wasn't imported from a website, so there is nowhere to check for new chapters")
		return
	}
	if _, _, ok := s.readerForBook(src); !ok {
		writeError(w, http.StatusConflict, "this server can't read the site this book came from ("+src.Kind+")")
		return
	}
	s.startSourceSync(bookID, nil)
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// writeSourceError answers for a failed read from site; status is what a
// failure other than "no such work" answers with.
func writeSourceError(w http.ResponseWriter, site websource.Source, err error, status int) {
	if errors.Is(err, websource.ErrNotFound) {
		writeError(w, http.StatusNotFound, site.Name()+" has no such work (it may have been removed)")
		return
	}
	if status == http.StatusBadGateway {
		writeError(w, status, "could not read from "+site.Name()+": "+err.Error())
		return
	}
	writeError(w, status, err.Error())
}

// sourceSyncJobKey is the jobs.BulkGroup.Key of a book's check for new
// chapters (startSourceSync).
const sourceSyncJobKey = "source_sync"

// sourceBusy reports whether bookID's check for new chapters is queued or
// running, or its stored pages are being re-read (which holds sourceSyncs
// too).
func (s *Server) sourceBusy(bookID string) bool {
	if _, held := s.sourceSyncs.Load(bookID); held {
		return true
	}
	queued, running := s.Jobs.BulkState(bookID, sourceSyncJobKey)
	return queued || running
}

// startSourceSync queues syncBookSource for bookID as a job-queue task (a
// Jobs-page row, Kind "pipeline_source_sync"), unless one is already
// queued or running (false). Cancelling the row stops the check, keeping
// the chapters it has added. fill, if not nil, is an uploaded epub to take
// chapters from where the site won't serve them.
func (s *Server) startSourceSync(bookID string, fill *epubFill) bool {
	if s.sourceBusy(bookID) {
		return false
	}
	label := "New chapters"
	if src, err := s.Store.GetBookSource(bookID); err == nil && src != nil {
		if r, _, ok := s.readerForBook(src); ok {
			label = "New chapters from " + r.name()
		}
	}
	if fill != nil {
		label += " (with an epub)"
	}
	s.Jobs.EnqueueBulk(bookID, jobs.BulkGroup{
		Kind:  "pipeline_source_sync",
		Key:   sourceSyncJobKey,
		Label: label,
		Tier:  jobs.TierNormal,
	}, func(ctx context.Context, _ func() int) error {
		// Held while it runs, against a re-read of the stored pages.
		if _, held := s.sourceSyncs.LoadOrStore(bookID, struct{}{}); held {
			return errors.New("this book's stored pages are being re-read")
		}
		ctx, cancel := context.WithTimeout(ctx, sourceSyncTimeout)
		defer cancel()
		added, err := s.syncBookSource(ctx, bookID, fill)
		// A cancelled check isn't a failed one.
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		if err != nil {
			log.Printf("source: checking %s stopped after %d new chapters: %v", bookID, added, err)
		} else if added > 0 {
			log.Printf("source: added %d chapters to %s", added, bookID)
		}
		s.sourceSyncs.Delete(bookID)
		if src, gerr := s.Store.GetBookSource(bookID); gerr == nil && src != nil {
			src.CheckedAt = time.Now().Unix()
			src.Error = ""
			if err != nil {
				src.Error = err.Error()
			}
			_ = s.Store.SetBookSource(bookID, *src)
		}
		return nil
	})
	// A write, so the book's live topics show it syncing (and drop the
	// last check's error) right away.
	if src, err := s.Store.GetBookSource(bookID); err == nil && src != nil {
		src.Error = ""
		_ = s.Store.SetBookSource(bookID, *src)
	}
	return true
}

// syncBookSource fetches every chapter bookID's source offers that the
// book doesn't have, in the source's order, and appends them. Chapters are
// matched by the source's own ids, so a retitled or reordered one isn't
// fetched again; one published in between chapters the book already has is
// still added at the end.
//
// A chapter the source lists but doesn't serve to the public is taken, in
// this order, from fill (an uploaded epub, if this check was given one)
// or from the Wayback Machine's last readable copy of it (readChapter).
// With neither, the check stops there (store.BookSource.Blocked), so the
// book never skips ahead of a chapter - it carries on from it once it
// opens. A chapter the source no longer has at all is passed over; any
// other failure stops the check, keeping what was added.
func (s *Server) syncBookSource(ctx context.Context, bookID string, fill *epubFill) (added int, err error) {
	src, err := s.Store.GetBookSource(bookID)
	if err != nil {
		return 0, err
	}
	if src == nil {
		return 0, errors.New("book has no source")
	}
	reader, workID, ok := s.readerForBook(src)
	if !ok {
		return 0, fmt.Errorf("no reader for source %q", src.Kind)
	}
	work, err := reader.site.Work(ctx, reader.fetch, workID)
	if err != nil {
		return 0, err
	}
	haveIDs, err := s.Store.ChapterSourceIDs(bookID)
	if err != nil {
		return 0, err
	}
	have := make(map[string]bool, len(haveIDs))
	for _, id := range haveIDs {
		have[id] = true
	}
	src.Blocked = ""
	var missing []websource.ChapterRef
	for _, ref := range work.Chapters {
		if have[ref.ID] {
			continue
		}
		// A chapter the list itself marks locked still gets its turn below:
		// the epub or the archive may have it.
		missing = append(missing, ref)
	}
	count, err := s.Store.CountChapters(bookID)
	if err != nil {
		return 0, err
	}
	src.RemoteChapters = count + len(missing)
	src.URL = reader.workURL(work)
	src.Error = ""
	if err := s.Store.SetBookSource(bookID, *src); err != nil {
		return 0, err
	}

	meta := reader.meta(work)
	for len(missing) > 0 {
		batch := missing[:min(sourceBatch, len(missing))]
		missing = missing[len(batch):]

		var fetched []sourced
		var fetchErr error
		dropped := 0 // chapters that won't be appended after all
		for i, ref := range batch {
			// covered: the epub chapter standing in for this one was
			// already taken for the chapter before it.
			chapters, covered, err := s.readOrFill(ctx, reader, ref, fill)
			if errors.Is(err, websource.ErrNotFound) {
				dropped++
				continue
			}
			if errors.Is(err, websource.ErrLocked) {
				// It and everything after it wait.
				src.Blocked = ref.Title
				dropped += len(batch) - i + len(missing)
				missing = nil
				break
			}
			if err != nil {
				fetchErr = err
				break
			}
			if covered {
				dropped++
				if n := len(fetched); n > 0 {
					fetched[n-1].ch.ID += " " + ref.ID
				} else if err := s.addLastChapterSourceID(bookID, ref.ID); err != nil {
					return added, err
				}
				continue
			}
			fetched = append(fetched, chapters...)
			// One web chapter that is several ebook chapters.
			src.RemoteChapters += len(chapters) - 1
		}
		if len(fetched) > 0 {
			if err := s.appendSourceChapters(bookID, meta, fetched); err != nil {
				return added, err
			}
			added += len(fetched)
		}
		src.RemoteChapters -= dropped
		if err := s.Store.SetBookSource(bookID, *src); err != nil {
			return added, err
		}
		if fetchErr != nil {
			return added, fetchErr
		}
	}
	s.fetchMissingRaw(ctx, bookID, reader, work)
	return added, nil
}

// readOrFill gets one chapter for a check: from the site, and when the
// site won't serve it, from the uploaded epub (which may give several
// chapters for it, or report it covered by ones already given) or the
// Wayback Machine.
func (s *Server) readOrFill(ctx context.Context, r sourceReader, ref websource.ChapterRef, fill *epubFill) (chapters []sourced, covered bool, err error) {
	got, err := fetchChapter(ctx, r.site, r.fetch, ref, r.via())
	if err == nil {
		return []sourced{got}, false, nil
	}
	if !errors.Is(err, websource.ErrLocked) {
		return nil, false, err
	}
	if fill != nil {
		if filled, ok := fill.take(ref); ok {
			for _, ch := range filled {
				// No page to keep: the chapter itself is what it was made from.
				chapters = append(chapters, sourced{ch: ch, raw: &websource.RawChapter{Ref: ref, Via: "epub", FetchedAt: time.Now().Unix(), Chapter: ch}})
			}
			return chapters, len(filled) == 0, nil
		}
	}
	got, err = s.archivedChapter(ctx, r, ref)
	if err != nil {
		return nil, false, err
	}
	return []sourced{got}, false, nil
}

// appendSourceChapters parses fetched chapters and adds them to the end of
// bookID: rows, images, the kept epub, and what each was made from. A
// chapter's ID is its source id - several, space-separated, when it stands
// for more than one of the source's chapters.
func (s *Server) appendSourceChapters(bookID string, meta websource.Meta, fetched []sourced) error {
	chapters := make([]*websource.Chapter, 0, len(fetched))
	inputs := make([]store.ChapterInput, 0, len(fetched))
	var imageBytes [][]byte
	for _, f := range fetched {
		input, chImages, err := sourceChapterInput(meta, f.ch)
		if err != nil {
			return err
		}
		chapters = append(chapters, f.ch)
		inputs = append(inputs, input)
		imageBytes = append(imageBytes, chImages...)
	}
	created, err := s.Store.AppendChapters(bookID, inputs)
	if err != nil {
		return err
	}
	s.writeChapterImages(bookID, created, imageBytes)
	// Best-effort, like the copy kept at upload: without it a chapter
	// re-import asks for a file.
	if err := s.writeSourceEpub(bookID, meta, chapters, false); err != nil && !errors.Is(err, websource.ErrForeignEpub) {
		log.Printf("source: adding to the epub of %s: %v", bookID, err)
	}
	for _, f := range fetched {
		s.writeRawChapter(bookID, f)
	}
	return nil
}

// sourceChapterInput reads one fetched chapter the way the book's epub
// will be read if the chapter is ever re-imported from it: laid out as an
// epub of that one chapter and put through the epub parser. Never as a
// play - a web serial's stat blocks and chat logs aren't speeches.
func sourceChapterInput(meta websource.Meta, ch *websource.Chapter) (store.ChapterInput, [][]byte, error) {
	var buf bytes.Buffer
	meta.Cover, meta.CoverExt = nil, ""
	if err := websource.WriteEPUB(&buf, meta, nil, []*websource.Chapter{ch}); err != nil {
		return store.ChapterInput{}, nil, fmt.Errorf("chapter %q: %w", ch.Title, err)
	}
	parsed, err := epub.ParseAs(bytes.NewReader(buf.Bytes()), int64(buf.Len()), epub.PlayOff)
	if err != nil {
		return store.ChapterInput{}, nil, fmt.Errorf("chapter %q: %w", ch.Title, err)
	}
	if len(parsed.Chapters) != 1 {
		return store.ChapterInput{}, nil, fmt.Errorf("chapter %q read as %d chapters", ch.Title, len(parsed.Chapters))
	}
	blocks, imageBytes := chapterBlockInputs(parsed.Chapters[0])
	return store.ChapterInput{Title: parsed.Chapters[0].Title, Blocks: blocks, SourceID: ch.ID}, imageBytes, nil
}

// writeChapterImages writes each created image's bytes under its id -
// best-effort, as at upload. images and data run parallel.
func (s *Server) writeChapterImages(bookID string, images []store.CreatedImage, data [][]byte) {
	for i, img := range images {
		if i >= len(data) {
			break
		}
		if err := audiopath.EnsureImageDir(s.DataDir, bookID, img.ChapterID); err == nil {
			_ = os.WriteFile(audiopath.ImageFile(s.DataDir, bookID, img.ChapterID, img.ImageID, img.Ext), data[i], 0o644)
		}
	}
}

// writeSourceEpub adds chapters to bookID's kept source epub, rewriting it
// via a temp file. fresh starts a new one (a new book); otherwise the
// existing file is extended, and one that is missing or was replaced by an
// uploaded epub (websource.ErrForeignEpub) is left as it is - it couldn't
// be completed anyway.
func (s *Server) writeSourceEpub(bookID string, meta websource.Meta, chapters []*websource.Chapter, fresh bool) error {
	if err := audiopath.EnsureSourceEpubDir(s.DataDir); err != nil {
		return err
	}
	dst := audiopath.SourceEpubFile(s.DataDir, bookID)
	var prev *zip.Reader
	if !fresh {
		old, err := zip.OpenReader(dst)
		if err != nil {
			return websource.ErrForeignEpub
		}
		defer old.Close()
		prev = &old.Reader
	}
	f, err := os.CreateTemp(filepath.Dir(dst), bookID+"-*.tmp")
	if err != nil {
		return err
	}
	err = websource.WriteEPUB(f, meta, prev, chapters)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), dst)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}
