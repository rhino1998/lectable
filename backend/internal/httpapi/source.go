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
	"os"
	"path/filepath"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/epub"
	"github.com/rhino1998/lectable/backend/internal/royalroad"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// A book can be imported from a website instead of an uploaded epub - for
// now Royal Road (internal/royalroad). The book is created from the
// fiction's first chapter, so it is in the library and readable at once,
// and the rest are fetched in the background and appended in order
// (syncBookSource). It remembers its source (store.BookSource), so the
// same pass run again later - "check for new chapters" - adds whatever has
// been published since; chapters the book already has are never touched.
//
// What is fetched is also kept as the book's source epub, one spine file
// per chapter, so re-importing a single chapter works as for any book.

// bookSourceKind names the website a book was imported from.
type bookSourceKind string

const sourceRoyalRoad bookSourceKind = royalroad.Kind

// bookSourceDTO is where a book imported from a website came from, and
// the state of fetching its chapters.
type bookSourceDTO struct {
	Kind bookSourceKind `json:"kind"`
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
	// Syncing is true while chapters are being fetched.
	Syncing bool `json:"syncing"`
}

type importBookURLRequest struct {
	// URL is a link to a Royal Road fiction or any of its chapters.
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

// defaultRoyalRoad is the client used when Server.RoyalRoad is unset; one
// for the process, so every import shares its request pacing.
var defaultRoyalRoad = &royalroad.Client{}

func (s *Server) royalRoad() *royalroad.Client {
	if s.RoyalRoad != nil {
		return s.RoyalRoad
	}
	return defaultRoyalRoad
}

// bookSource builds bookID's source DTO, nil for a book from an epub.
func (s *Server) bookSource(bookID string) (*bookSourceDTO, error) {
	src, err := s.Store.GetBookSource(bookID)
	if err != nil || src == nil {
		return nil, err
	}
	_, syncing := s.sourceSyncs.Load(bookID)
	return &bookSourceDTO{
		Kind:           bookSourceKind(src.Kind),
		URL:            src.URL,
		CheckedAt:      src.CheckedAt,
		RemoteChapters: src.RemoteChapters,
		Error:          src.Error,
		Syncing:        syncing,
	}, nil
}

// handleImportBookURL imports a Royal Road fiction as a new book: its
// first chapter now, the rest in the background (the book's source.syncing
// / remoteChapters show the progress). 409 book_exists if the library
// already has that fiction - check it for new chapters instead.
func (s *Server) handleImportBookURL(w http.ResponseWriter, r *http.Request) {
	var req importBookURLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	fictionID, err := royalroad.ParseFictionURL(req.URL)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// One import at a time, so the same link sent twice can't make two books.
	s.sourceImportMu.Lock()
	defer s.sourceImportMu.Unlock()

	if existing, err := s.Store.FindBookBySource(royalroad.Kind, fictionID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if existing != "" {
		title := "This fiction"
		if b, _ := s.Store.GetBook(existing); b != nil {
			title = "\"" + b.Title + "\""
		}
		writeErrorCode(w, http.StatusConflict, errBookExists, title+" is already in the library - check it for new chapters instead")
		return
	}

	ctx := r.Context()
	rr := s.royalRoad()
	fic, err := rr.Fiction(ctx, fictionID)
	if err != nil {
		writeSourceError(w, err)
		return
	}
	refs := unlockedChapters(fic.Chapters)
	if len(refs) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "\""+fic.Title+"\" has no chapters to read yet")
		return
	}
	first, err := rr.Chapter(ctx, refs[0])
	if err != nil {
		writeSourceError(w, err)
		return
	}
	meta := royalroad.Meta{FictionID: fic.ID, URL: fic.URL, Title: fic.Title, Author: fic.Author}
	if fic.CoverURL != "" {
		// Best-effort, like an epub's cover.
		if data, ext, err := rr.Cover(ctx, fic.CoverURL); err == nil && ext != "" {
			meta.Cover, meta.CoverExt = data, ext
		}
	}

	input, imageBytes, err := sourceChapterInput(meta, first)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	bookID, createdImages, err := s.Store.CreateBook(fic.Title, fic.Author, "en", meta.CoverExt, "", 0, []store.ChapterInput{input})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save book: "+err.Error())
		return
	}
	if err := s.Store.SetBookSource(bookID, store.BookSource{
		Kind: royalroad.Kind, SourceID: fic.ID, URL: fic.URL, CheckedAt: time.Now().Unix(), RemoteChapters: len(refs),
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save book: "+err.Error())
		return
	}
	if err := s.writeSourceEpub(bookID, meta, []*royalroad.Chapter{first}, true); err != nil {
		log.Printf("source: keeping epub of %s: %v", bookID, err)
	}
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
	if len(refs) > 1 {
		s.startSourceSync(bookID)
	}
	summary, chapters, err := s.buildBookSummary(*b)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(chapters) > 0 {
		// As for an uploaded book - see handleUploadBook.
		s.Jobs.EnqueueSample(bookID, chapters[0].ID, chapters[0].Idx, 1000)
		s.Jobs.EnqueueLengthEstimate(bookID, chapters[0].ID, 300)
	}
	writeJSON(w, http.StatusCreated, summary)
}

// handleSyncBookSource checks a book's source for chapters it doesn't have
// and appends them, in the background (joining a check already running).
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
	if src == nil || src.Kind != royalroad.Kind {
		writeError(w, http.StatusConflict, "this book wasn't imported from a website, so there is nowhere to check for new chapters")
		return
	}
	s.startSourceSync(bookID)
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// writeSourceError answers for a failed fetch from the source.
func writeSourceError(w http.ResponseWriter, err error) {
	if errors.Is(err, royalroad.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Royal Road has no such fiction (it may have been removed)")
		return
	}
	writeError(w, http.StatusBadGateway, "could not read from Royal Road: "+err.Error())
}

func unlockedChapters(refs []royalroad.ChapterRef) []royalroad.ChapterRef {
	out := make([]royalroad.ChapterRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Unlocked {
			out = append(out, ref)
		}
	}
	return out
}

// startSourceSync runs syncBookSource for bookID in the background, unless
// one is already running.
func (s *Server) startSourceSync(bookID string) {
	if _, running := s.sourceSyncs.LoadOrStore(bookID, struct{}{}); running {
		return
	}
	// A write, so the book's live topics show it syncing (and drop the
	// last check's error) right away.
	if src, err := s.Store.GetBookSource(bookID); err == nil && src != nil {
		src.Error = ""
		_ = s.Store.SetBookSource(bookID, *src)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), sourceSyncTimeout)
		defer cancel()
		added, err := s.syncBookSource(ctx, bookID)
		if err != nil {
			log.Printf("source: checking %s stopped after %d new chapters: %v", bookID, added, err)
		} else if added > 0 {
			log.Printf("source: added %d chapters to %s", added, bookID)
		}
		// Cleared before the final write, so the rebuild that write
		// triggers already shows the check as over.
		s.sourceSyncs.Delete(bookID)
		if src, gerr := s.Store.GetBookSource(bookID); gerr == nil && src != nil {
			src.CheckedAt = time.Now().Unix()
			src.Error = ""
			if err != nil {
				src.Error = err.Error()
			}
			_ = s.Store.SetBookSource(bookID, *src)
		}
	}()
}

// syncBookSource fetches every chapter bookID's source offers that the
// book doesn't have, in the source's order, and appends them. Chapters are
// matched by the source's own ids, so a retitled or reordered one isn't
// fetched again; one published in between chapters the book already has is
// still added at the end. A chapter the source lists but no longer serves
// is skipped; any other failure stops the check, keeping what was added.
func (s *Server) syncBookSource(ctx context.Context, bookID string) (added int, err error) {
	src, err := s.Store.GetBookSource(bookID)
	if err != nil {
		return 0, err
	}
	if src == nil {
		return 0, errors.New("book has no source")
	}
	rr := s.royalRoad()
	fic, err := rr.Fiction(ctx, src.SourceID)
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
	var missing []royalroad.ChapterRef
	for _, ref := range unlockedChapters(fic.Chapters) {
		if !have[ref.ID] {
			missing = append(missing, ref)
		}
	}
	count, err := s.Store.CountChapters(bookID)
	if err != nil {
		return 0, err
	}
	src.RemoteChapters = count + len(missing)
	src.URL = fic.URL
	src.Error = ""
	if err := s.Store.SetBookSource(bookID, *src); err != nil {
		return 0, err
	}

	meta := royalroad.Meta{FictionID: fic.ID, URL: fic.URL, Title: fic.Title, Author: fic.Author}
	for len(missing) > 0 {
		batch := missing[:min(sourceBatch, len(missing))]
		missing = missing[len(batch):]

		var fetched []*royalroad.Chapter
		var fetchErr error
		gone := 0
		for _, ref := range batch {
			ch, err := rr.Chapter(ctx, ref)
			if errors.Is(err, royalroad.ErrNotFound) {
				gone++
				continue
			}
			if err != nil {
				fetchErr = err
				break
			}
			fetched = append(fetched, ch)
		}
		if len(fetched) > 0 {
			if err := s.appendSourceChapters(bookID, meta, fetched); err != nil {
				return added, err
			}
			added += len(fetched)
		}
		if gone > 0 {
			src.RemoteChapters -= gone
			if err := s.Store.SetBookSource(bookID, *src); err != nil {
				return added, err
			}
		}
		if fetchErr != nil {
			return added, fetchErr
		}
	}
	return added, nil
}

// appendSourceChapters parses fetched chapters and adds them to the end of
// bookID: rows, images, and the kept epub.
func (s *Server) appendSourceChapters(bookID string, meta royalroad.Meta, chapters []*royalroad.Chapter) error {
	inputs := make([]store.ChapterInput, 0, len(chapters))
	var imageBytes [][]byte
	for _, ch := range chapters {
		input, chImages, err := sourceChapterInput(meta, ch)
		if err != nil {
			return err
		}
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
	if err := s.writeSourceEpub(bookID, meta, chapters, false); err != nil && !errors.Is(err, royalroad.ErrForeignEpub) {
		log.Printf("source: adding to the epub of %s: %v", bookID, err)
	}
	return nil
}

// sourceChapterInput reads one fetched chapter the way the book's epub
// will be read if the chapter is ever re-imported from it: laid out as an
// epub of that one chapter and put through the epub parser. Never as a
// play - a web serial's stat blocks and chat logs aren't speeches.
func sourceChapterInput(meta royalroad.Meta, ch *royalroad.Chapter) (store.ChapterInput, [][]byte, error) {
	var buf bytes.Buffer
	meta.Cover, meta.CoverExt = nil, ""
	if err := royalroad.WriteEPUB(&buf, meta, nil, []*royalroad.Chapter{ch}); err != nil {
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
// uploaded epub (royalroad.ErrForeignEpub) is left as it is - it couldn't
// be completed anyway.
func (s *Server) writeSourceEpub(bookID string, meta royalroad.Meta, chapters []*royalroad.Chapter, fresh bool) error {
	if err := audiopath.EnsureSourceEpubDir(s.DataDir); err != nil {
		return err
	}
	dst := audiopath.SourceEpubFile(s.DataDir, bookID)
	var prev *zip.Reader
	if !fresh {
		old, err := zip.OpenReader(dst)
		if err != nil {
			return royalroad.ErrForeignEpub
		}
		defer old.Close()
		prev = &old.Reader
	}
	f, err := os.CreateTemp(filepath.Dir(dst), bookID+"-*.tmp")
	if err != nil {
		return err
	}
	err = royalroad.WriteEPUB(f, meta, prev, chapters)
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
