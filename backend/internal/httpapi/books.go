package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/epub"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
)

type bookSummaryDTO struct {
	ID                    string  `json:"id"`
	Title                 string  `json:"title"`
	Author                string  `json:"author"`
	SeriesName            string  `json:"seriesName,omitempty"`
	SeriesIndex           float64 `json:"seriesIndex,omitempty"`
	CoverURL              string  `json:"coverUrl,omitempty"`
	ChapterCount          int     `json:"chapterCount"`
	PosChapterIdx         int     `json:"posChapterIdx"`
	PosParagraphIdx       int     `json:"posParagraphIdx"`
	PosSeconds            float64 `json:"posSeconds"`
	ProgressPercent       float64 `json:"progressPercent"`
	Finished              bool    `json:"finished"`
	EstimatedTotalSeconds float64 `json:"estimatedTotalSeconds"`
	EstimateCalibrated    bool    `json:"estimateCalibrated"`
	GeneratedPercent      float64 `json:"generatedPercent"`
	// Preprocessing mirrors jobs.Manager.IsPipelineRunning - true while the
	// preprocessing pipeline (POST .../preprocess) is still working
	// through this book's attribution/characterization/voice-provisioning/
	// direction-tagging sequence. Set by buildBookSummary/handleListBooks,
	// not toBookSummary itself (a plain function with no Server access) -
	// see their own call sites.
	Preprocessing bool `json:"preprocessing"`
	// MusicEnabled mirrors store.Book.MusicEnabled - included here (not
	// just on voiceSettingsDTO, which is what actually reads/writes it)
	// purely as a read convenience so pages already holding a BookDetail/
	// BookSummary (the reader, the library) don't need a separate voice-
	// settings fetch just to know whether background music is on.
	MusicEnabled bool `json:"musicEnabled"`
}

// Generic fallback narration rate (~150 words/minute at ~6 characters per
// word including a trailing space), used only until a book has at least
// one really-generated paragraph to calibrate from.
const fallbackSecondsPerChar = 0.0667

// estimateTotalSeconds extrapolates the book's total narration length from
// the best calibration currently available, in order:
//
//  1. seconds-per-character for this book's own paragraphs that already
//     have real audio, applied to the book's total character count - the
//     most accurate tier, since it's the book's own actual voice/pace, but
//     only available once some of that voice's audio is ready.
//  2. fastSecPerChar (jobs.Manager.EnqueueLengthEstimate's own PocketTTS-
//     calibrated sample, store.Book.EstimateSecPerChar) - a real,
//     generated-audio measurement too, just not in this book's own voice,
//     so it's a strictly better guess than the generic WPM fallback below
//     without needing this book's own (possibly much slower) engine to
//     have rendered anything yet.
//  3. fallbackSecondsPerChar, a generic ~150wpm guess, reported
//     uncalibrated - only reached before either real sample above exists.
func estimateTotalSeconds(stats store.NarrationStats, fastSecPerChar float64) (seconds float64, calibrated bool) {
	if stats.TotalChars == 0 {
		return 0, false
	}
	if stats.ReadyChars > 0 {
		secPerChar := stats.ReadySeconds / float64(stats.ReadyChars)
		return secPerChar * float64(stats.TotalChars), true
	}
	if fastSecPerChar > 0 {
		return fastSecPerChar * float64(stats.TotalChars), true
	}
	return fallbackSecondsPerChar * float64(stats.TotalChars), false
}

// toBookSummary derives reading progress from the book's saved position
// plus each chapter's paragraph count, rather than storing a redundant
// progress value: percent-through-book is (paragraphs before the current
// chapter + paragraphs into the current chapter) / total paragraphs.
// "Finished" means the saved position is on (or past) the last paragraph
// of the last chapter.
func toBookSummary(b store.Book, chapters []store.ChapterSummary, stats store.NarrationStats) bookSummaryDTO {
	dto := bookSummaryDTO{
		ID:              b.ID,
		Title:           b.Title,
		Author:          b.Author,
		SeriesName:      b.SeriesName,
		SeriesIndex:     b.SeriesIndex,
		ChapterCount:    len(chapters),
		PosChapterIdx:   b.PosChapterIdx,
		PosParagraphIdx: b.PosParagraphIdx,
		PosSeconds:      b.PosSeconds,
		MusicEnabled:    b.MusicEnabled,
	}
	if b.CoverExt != "" {
		dto.CoverURL = "/api/books/" + b.ID + "/cover"
	}

	totalParagraphs := 0
	completedParagraphs := 0
	for i, c := range chapters {
		totalParagraphs += c.ParagraphCount
		switch {
		case c.Idx < b.PosChapterIdx:
			completedParagraphs += c.ParagraphCount
		case c.Idx == b.PosChapterIdx:
			n := b.PosParagraphIdx
			if n > c.ParagraphCount {
				n = c.ParagraphCount
			}
			completedParagraphs += n
			if i == len(chapters)-1 && b.PosParagraphIdx >= c.ParagraphCount-1 {
				dto.Finished = true
			}
		}
	}
	if totalParagraphs > 0 {
		dto.ProgressPercent = float64(completedParagraphs) / float64(totalParagraphs) * 100
		if dto.ProgressPercent > 100 {
			dto.ProgressPercent = 100
		}
	}

	dto.EstimatedTotalSeconds, dto.EstimateCalibrated = estimateTotalSeconds(stats, b.EstimateSecPerChar)

	if stats.TotalChars > 0 {
		dto.GeneratedPercent = float64(stats.ReadyChars) / float64(stats.TotalChars) * 100
		if dto.GeneratedPercent > 100 {
			dto.GeneratedPercent = 100
		}
	}
	return dto
}

// buildBookSummary computes the book's current voice (a hash of its
// preset/instruct/language) and fetches everything toBookSummary needs
// scoped to that voice, returning the chapter list too since most callers
// need it anyway (e.g. for a full chapter listing, or to find the first
// chapter's id). Chapter readiness and narration stats are inherently
// per-voice - a different voice for the same book has its own, separately
// cached generation progress.
func (s *Server) buildBookSummary(b store.Book) (bookSummaryDTO, []store.ChapterSummary, error) {
	bookVoice, err := s.Narration.BookVoice(&b)
	if err != nil {
		return bookSummaryDTO{}, nil, err
	}
	voiceID := bookVoice.VoiceID()

	overrides, err := s.characterVoiceOverrides(&b, bookVoice)
	if err != nil {
		return bookSummaryDTO{}, nil, err
	}

	chapters, err := s.Store.ListChapterSummaries(b.ID, voiceID, overrides)
	if err != nil {
		return bookSummaryDTO{}, nil, err
	}
	stats, err := s.Store.BookNarrationStats(b.ID, voiceID, overrides)
	if err != nil {
		return bookSummaryDTO{}, nil, err
	}
	dto := toBookSummary(b, chapters, stats)
	dto.Preprocessing = s.isPreprocessing(b.ID)
	return dto, chapters, nil
}

// characterVoiceOverrides resolves book's own character roster into
// ListChapterSummaries/BookNarrationStats' SpeakerVoice form - one entry
// per character, each mapping their name (exactly what paragraphs.speaker
// stores for their dialogue) to their own fully-resolved voice_id, the
// same resolution internal/narration.Resolver.ForParagraph/handleGetChapter
// already apply per paragraph. nil for a book with book.MultiVoice() off -
// every paragraph narrates in bookVoice regardless of speaker, so there's
// nothing to override and callers should skip the extra JOIN entirely.
func (s *Server) characterVoiceOverrides(book *store.Book, bookVoice narration.ResolvedVoice) ([]store.SpeakerVoice, error) {
	if !book.MultiVoice() {
		return nil, nil
	}
	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return nil, err
	}
	if len(characters) == 0 {
		return nil, nil
	}
	characterIDs := make([]string, len(characters))
	for i, c := range characters {
		characterIDs[i] = c.ID
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	presetIDByChar, err := s.Store.CharacterVoicesForModel(characterIDs, cloneModel)
	if err != nil {
		return nil, err
	}
	overrides := make([]store.SpeakerVoice, len(characters))
	for i, c := range characters {
		resolved, err := s.Narration.ResolveCharacterVoice(book, bookVoice, c, cloneModel, presetIDByChar[c.ID])
		if err != nil {
			return nil, err
		}
		overrides[i] = store.SpeakerVoice{Speaker: c.Name, VoiceID: resolved.VoiceID()}
	}
	return overrides, nil
}

func (s *Server) handleUploadBook(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(200 << 20); err != nil { // up to 200MB epub
		writeError(w, http.StatusBadRequest, "could not parse upload: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()

	tmp, err := os.CreateTemp("", "upload-*.epub")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	size, err := io.Copy(tmp, file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to buffer upload")
		return
	}

	book, err := epub.Parse(tmp, size)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not parse epub: "+err.Error())
		return
	}
	if book.Title == "Untitled" {
		if base := strings.TrimSuffix(header.Filename, ".epub"); base != "" {
			book.Title = base
		}
	}

	// imageBytes runs parallel to the image blocks across all chapters, in
	// the same order CreateBook will insert them in, so its returned
	// CreatedImage IDs can be zipped back up with the actual bytes below.
	var imageBytes [][]byte
	chapterInputs := make([]store.ChapterInput, len(book.Chapters))
	for i, ch := range book.Chapters {
		blocks, chImages := chapterBlockInputs(ch)
		imageBytes = append(imageBytes, chImages...)
		chapterInputs[i] = store.ChapterInput{Title: ch.Title, Blocks: blocks}
	}

	coverExt := ""
	if len(book.CoverData) > 0 {
		coverExt = extensionForMediaType(book.CoverMediaType)
	}

	bookID, createdImages, err := s.Store.CreateBook(book.Title, book.Author, book.Language, coverExt, book.SeriesName, book.SeriesIndex, chapterInputs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to save book: "+err.Error())
		return
	}

	// Keep the original epub so single chapters can be re-imported from
	// it later (handleReimportChapter) - best-effort, like the cover and
	// images below: a book without it can still be re-imported by
	// uploading the epub again.
	if _, err := tmp.Seek(0, io.SeekStart); err == nil {
		_ = saveSourceEpub(s.DataDir, bookID, tmp)
	}

	if coverExt != "" {
		if err := audiopath.EnsureCoverDir(s.DataDir); err == nil {
			_ = os.WriteFile(audiopath.CoverFile(s.DataDir, bookID, coverExt), book.CoverData, 0o644)
		}
	}
	for i, img := range createdImages {
		if i >= len(imageBytes) {
			break // defensive; should always match len(imageBytes)
		}
		if err := audiopath.EnsureImageDir(s.DataDir, bookID, img.ChapterID); err == nil {
			_ = os.WriteFile(audiopath.ImageFile(s.DataDir, bookID, img.ChapterID, img.ImageID, img.Ext), imageBytes[i], 0o644)
		}
	}

	b, err := s.Store.GetBook(bookID)
	if err != nil || b == nil {
		writeError(w, http.StatusInternalServerError, "book saved but could not be reloaded")
		return
	}

	summary, chapters, err := s.buildBookSummary(*b)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if len(chapters) > 0 {
		// Real narration in the book's own voice, so the reader has
		// something to listen to immediately - this alone used to also be
		// what calibrated the length estimate below, which meant that
		// estimate's own accuracy was hostage to however slow the book's
		// selected engine happens to be.
		s.Jobs.EnqueueSample(bookID, chapters[0].ID, chapters[0].Idx, 1000)
		// A separate, much cheaper PocketTTS-cloned sample (see
		// jobs.Manager.EnqueueLengthEstimate) so the library page can show
		// a real, generated-audio-calibrated length estimate right away
		// regardless of the line above - it's superseded the moment the
		// book's own voice has ready audio of its own (see
		// estimateTotalSeconds's own tiering).
		s.Jobs.EnqueueLengthEstimate(bookID, chapters[0].ID, 300)
	}

	writeJSON(w, http.StatusCreated, summary)
}

// chapterBlockInputs converts one parsed epub chapter into store block
// inputs, plus each image block's bytes in the same order, for the caller
// to write to disk under whatever IDs the store hands back.
func chapterBlockInputs(ch epub.Chapter) (blocks []store.BlockInput, imageBytes [][]byte) {
	blocks = make([]store.BlockInput, len(ch.Blocks))
	for j, b := range ch.Blocks {
		switch b.Kind {
		case epub.BlockImage:
			blocks[j] = store.BlockInput{Kind: store.BlockImage, Ext: b.ImageExt}
			imageBytes = append(imageBytes, b.ImageData)
		case epub.BlockBreak:
			blocks[j] = store.BlockInput{Kind: store.BlockBreak}
		default:
			blocks[j] = store.BlockInput{Kind: store.BlockText, Text: b.Text, Inline: b.Inline, IsQuote: b.IsQuote, Emphasis: b.Emphasis}
		}
	}
	return blocks, imageBytes
}

// saveSourceEpub writes r to bookID's kept source epub
// (audiopath.SourceEpubFile), via a temp file + rename so a failed write
// never leaves a truncated copy behind.
func saveSourceEpub(dataDir, bookID string, r io.Reader) error {
	if err := audiopath.EnsureSourceEpubDir(dataDir); err != nil {
		return err
	}
	dst := audiopath.SourceEpubFile(dataDir, bookID)
	f, err := os.CreateTemp(filepath.Dir(dst), bookID+"-*.tmp")
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(f.Name())
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), dst)
}

// reimportCancelWait bounds how long handleReimportChapter waits for the
// chapter's in-flight jobs to notice their cancellation before giving up.
const reimportCancelWait = 30 * time.Second

// handleReimportChapter re-parses one chapter from the book's source epub
// and replaces its content (store.ReplaceChapterContent) - for applying a
// parser fix to the chapters that need it without deleting the whole book
// and losing every other chapter's attribution, manual fixes, and audio.
// The source is the epub kept at upload (audiopath.SourceEpubFile), or a
// multipart "file" field, which also becomes the kept copy (for books
// imported before uploads were kept, or to swap in a corrected epub).
//
// The re-parsed book must have the same number of chapters, so idx still
// names the same chapter; its title must match too unless ?force=true
// (a parser change can legitimately retitle a chapter). Those two
// recoverable 409s carry a code (writeErrorCode): "no_source_epub" and
// "title_mismatch". Every queued and
// in-flight job for the chapter is cancelled first (jobs.Manager.
// CancelChapter) - 409 if something is still running after
// reimportCancelWait. Everything derived from the old text goes: audio,
// speakers, emotions, pronunciation, SFX, music regions, pass flags. The
// chapter then needs preprocessing again like a fresh import.
func (s *Server) handleReimportChapter(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	var src *os.File
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(200 << 20); err != nil {
			writeError(w, http.StatusBadRequest, "could not parse upload: "+err.Error())
			return
		}
		file, _, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, "missing 'file' field")
			return
		}
		defer file.Close()
		if err := saveSourceEpub(s.DataDir, bookID, file); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to store epub: "+err.Error())
			return
		}
	}
	src, err = os.Open(audiopath.SourceEpubFile(s.DataDir, bookID))
	if errors.Is(err, os.ErrNotExist) {
		writeErrorCode(w, http.StatusConflict, "no_source_epub", "no source epub is stored for this book - upload it with this request")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	parsed, err := epub.Parse(src, info.Size())
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not parse epub: "+err.Error())
		return
	}

	count, err := s.Store.CountChapters(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(parsed.Chapters) != count {
		writeError(w, http.StatusConflict, fmt.Sprintf("epub has %d chapters but the book has %d - is this the right epub?", len(parsed.Chapters), count))
		return
	}
	fresh := parsed.Chapters[idx]
	if fresh.Title != ch.Title && r.URL.Query().Get("force") != "true" {
		writeErrorCode(w, http.StatusConflict, "title_mismatch", fmt.Sprintf("chapter %d is titled %q in the epub but %q in the library", idx, fresh.Title, ch.Title))
		return
	}

	cancelCtx, cancel := context.WithTimeout(r.Context(), reimportCancelWait)
	defer cancel()
	if !s.Jobs.CancelChapter(cancelCtx, ch.ID) {
		writeError(w, http.StatusConflict, "this chapter still has a job running - try again in a moment")
		return
	}

	blocks, imageBytes := chapterBlockInputs(fresh)
	oldImages, newImages, err := s.Store.ReplaceChapterContent(ch.ID, fresh.Title, blocks)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The whole chapter audio dir (narration under every voice, SFX,
	// music) belonged to the old paragraphs.
	_ = os.RemoveAll(fmt.Sprintf("%s/audio/%s/%s", s.DataDir, bookID, ch.ID))
	for _, img := range oldImages {
		_ = os.Remove(audiopath.ImageFile(s.DataDir, bookID, ch.ID, img.ImageID, img.Ext))
	}
	for i, img := range newImages {
		if i >= len(imageBytes) {
			break
		}
		if err := audiopath.EnsureImageDir(s.DataDir, bookID, ch.ID); err == nil {
			_ = os.WriteFile(audiopath.ImageFile(s.DataDir, bookID, ch.ID, img.ImageID, img.Ext), imageBytes[i], 0o644)
		}
	}

	paragraphs := 0
	for _, b := range blocks {
		if b.Kind == store.BlockText {
			paragraphs++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "title": fresh.Title, "paragraphs": paragraphs})
}

func extensionForMediaType(mt string) string {
	switch mt {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}

// handleListBooks batch-fetches every book's chapter summaries and
// narration stats in two queries total (one per store.ListChapterSummariesForBooks/
// BookNarrationStatsForBooks call), not two queries per book -
// buildBookSummary's own per-book pair of queries is fine for the
// single-book callers (upload, get-book), but looping it once per book
// here meant 2*N serialized whole-book scans on the single shared DuckDB
// connection (internal/store's own doc comment) every time the library
// page loaded, stalling every other concurrent request behind whichever
// scan was running.
func (s *Server) handleListBooks(w http.ResponseWriter, r *http.Request) {
	writeBuilt(w)(s.buildBooks())
}

func (s *Server) buildBooks() (any, error) {
	books, err := s.Store.ListBooks()
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}

	bookVoices := make([]store.BookVoice, len(books))
	var overrides []store.BookSpeakerVoice
	for i, b := range books {
		resolved, err := s.Narration.BookVoice(&b)
		if err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
		bookVoices[i] = store.BookVoice{BookID: b.ID, VoiceID: resolved.VoiceID()}

		// Only a MultiVoice book with an actual character roster pays for
		// this - characterVoiceOverrides short-circuits to nil otherwise,
		// so most libraries never issue these extra (small, character-table-
		// scoped, not paragraph-scoped) queries at all.
		bookOverrides, err := s.characterVoiceOverrides(&b, resolved)
		if err != nil {
			return nil, httpError(http.StatusInternalServerError, err.Error())
		}
		for _, ov := range bookOverrides {
			overrides = append(overrides, store.BookSpeakerVoice{BookID: b.ID, Speaker: ov.Speaker, VoiceID: ov.VoiceID})
		}
	}
	chaptersByBook, err := s.Store.ListChapterSummariesForBooks(bookVoices, overrides)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	statsByBook, err := s.Store.BookNarrationStatsForBooks(bookVoices, overrides)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}

	out := make([]bookSummaryDTO, 0, len(books))
	for _, b := range books {
		dto := toBookSummary(b, chaptersByBook[b.ID], statsByBook[b.ID])
		dto.Preprocessing = s.isPreprocessing(b.ID)
		out = append(out, dto)
	}
	return out, nil
}

type chapterSummaryDTO struct {
	Idx            int    `json:"idx"`
	Title          string `json:"title"`
	ParagraphCount int    `json:"paragraphCount"`
	ReadyCount     int    `json:"readyCount"`
	// Passes is the chapter's own store.Passes, passed straight through -
	// see that type's own doc comment. Replaces the old separate
	// attributed bool/directed map[string]bool DTO fields (a holdover from
	// when direction-tagging was tracked per-clone-model); a client reads
	// passes.attribution/passes.direction directly instead.
	Passes store.Passes `json:"passes"`
	// Music* are this chapter's background-music regions: how many were
	// scored, and how many of those have a ready clip / failed. All 0 for
	// a chapter that isn't scored.
	MusicRegionCount int `json:"musicRegionCount"`
	MusicReadyCount  int `json:"musicReadyCount"`
	MusicErrorCount  int `json:"musicErrorCount"`
}

type bookDetailDTO struct {
	bookSummaryDTO
	Chapters []chapterSummaryDTO `json:"chapters"`
}

func (s *Server) handleGetBook(w http.ResponseWriter, r *http.Request) {
	writeBuilt(w)(s.buildBook(r.PathValue("id")))
}

func (s *Server) buildBook(id string) (any, error) {
	b, err := s.Store.GetBook(id)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if b == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	summary, chapters, err := s.buildBookSummary(*b)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}

	musicCounts, err := s.Store.MusicRegionCounts(b.ID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}

	dto := bookDetailDTO{bookSummaryDTO: summary}
	for _, c := range chapters {
		mc := musicCounts[c.ID]
		dto.Chapters = append(dto.Chapters, chapterSummaryDTO{
			Idx: c.Idx, Title: c.Title, ParagraphCount: c.ParagraphCount, ReadyCount: c.ReadyCount,
			Passes:           c.Passes,
			MusicRegionCount: mc.Total, MusicReadyCount: mc.Ready, MusicErrorCount: mc.Error,
		})
	}
	return dto, nil
}

func (s *Server) handleDeleteBook(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.Store.GetBook(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	// Stop a running preprocess pipeline (POST .../preprocess) before it
	// keeps working through a book that's about to stop existing - see
	// Server.cancelPipeline/jobs.Manager.CancelPipeline. Best-effort: it
	// only removes not-yet-dispatched phases and cancels whichever phase
	// is currently in flight, not whatever real per-item work that phase
	// already dispatched into a poolLLM/poolDesign slot.
	s.cancelPipeline(id)
	if err := s.Store.DeleteBook(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.RemoveAll(fmt.Sprintf("%s/audio/%s", s.DataDir, id))
	if b.CoverExt != "" {
		_ = os.Remove(audiopath.CoverFile(s.DataDir, id, b.CoverExt))
	}
	_ = os.Remove(audiopath.SourceEpubFile(s.DataDir, id))
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteBookAudio deletes every generated audio file for book - the
// reader's own "Delete generated audio" button, for reclaiming disk space
// or forcing a full regenerate, without touching the book's text,
// chapters, speaker attribution, or voice settings (unlike handleDeleteBook,
// which removes the whole book, or handleDeleteBookSpeakerData, which
// clears attribution/characters instead of audio). Every paragraph reports
// pending again on the next fetch - store.DeleteBookAudio drops every
// paragraph_audio row for this book across every voice_id it's ever been
// generated under, not just the book's current one, and the actual .wav
// files live under one shared per-book directory regardless of voice_id
// (see audiopath.ParagraphFile/VoiceDir), so removing that directory
// wholesale - same call handleDeleteBook itself uses - covers all of them
// in one step rather than needing to enumerate voice_ids.
func (s *Server) handleDeleteBookAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.Store.GetBook(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	if err := s.Store.DeleteBookAudio(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// This book's own music regions' generated clips just went away too
	// (RemoveAll below wipes each chapter's audio dir wholesale, which
	// nests audiopath.MusicDir the same way it nests SFXDir/VoiceDir) - a
	// region's target duration is derived from narration that's about to
	// be regenerated, so it's stale regardless (see store.MusicRegion's
	// own doc comment). Scored mood/prompt/transition stay untouched.
	if err := s.Store.ResetAllChapterMusicAudioForBook(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.RemoveAll(fmt.Sprintf("%s/audio/%s", s.DataDir, id))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteChapterAudio is handleDeleteBookAudio's single-chapter
// counterpart - the reader-facing chapter-header "Clear generation" button,
// for resetting just one chapter's narration (e.g. after reassigning
// speakers, or to force a clean re-render) without wiping the whole book's
// progress.
func (s *Server) handleDeleteChapterAudio(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	if err := s.Store.DeleteChapterAudio(ch.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// See handleDeleteBookAudio's own identical comment - this chapter's
	// own music regions' generated clips are about to be wiped along with
	// the rest of its audio dir, and their target durations are stale
	// regardless once narration regenerates.
	if _, err := s.Store.ResetChapterMusicAudio(ch.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.RemoveAll(fmt.Sprintf("%s/audio/%s/%s", s.DataDir, bookID, ch.ID))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleGetCover(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.Store.GetBook(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil || b.CoverExt == "" {
		writeError(w, http.StatusNotFound, "no cover")
		return
	}
	http.ServeFile(w, r, audiopath.CoverFile(s.DataDir, id, b.CoverExt))
}
