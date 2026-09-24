package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// The offline-sync hash tree: a client holding an offline copy of a book
// (the Android app's downloads) reconciles it against the backend, which
// always wins, by walking this tree top-down and only fetching what
// differs:
//
//	book root  = H(book metadata, [chapter idx + chapter hash]...)
//	chapter    = H(title, content order, [paragraph content + audio hash]...)
//	paragraph  = contentHash (text/annotations) + audioHash (the clip)
//
// Paragraph and chapter hashes ride along on every chapter value
// (buildChapter - REST and the live "chapter" topic alike), so a client
// records them at download time; the root comes from GET
// /api/books/{id}/manifest. Everything is hashed from the DTOs actually
// served, so a hash changes exactly when what a client would store does.
// Reading position is deliberately left out of the root (it changes every
// few seconds of playback) and returned alongside it instead.
//
// The stored tree: building a chapter's hash means a full buildChapter
// (seconds for a long book), so every one handleBookManifest builds is
// stored (store.PutSyncTreeNodes, table sync_tree_nodes) tagged with a
// fingerprint of the rows it was built from (store.SyncFingerprints) and
// the build of the code that hashed it (syncTreeVersion); a later request
// rebuilds only chapters whose fingerprint moved. Validated by value on
// every read rather than invalidated by writes: store.Store.OnChange only
// reports which table a write touched, never which book or chapter, and
// scoping it would mean instrumenting every write path by hand - one
// missed path would leave a client stale forever, where a fingerprint
// that over-covers its inputs can at worst cause a needless rebuild.
//
// Soundness rests on ordering: fingerprints are taken before a build and
// again after it, and a hash is only stored when the two agree, so a
// stored hash always describes the rows its fingerprint does. (The one gap
// - rows changing and changing back to byte-identical values within a
// single build - isn't worth closing.)

// hashJSON is the one hash function every level uses: sha256 over v's JSON
// encoding (struct fields encode in declaration order, so this is
// deterministic), truncated to 128 bits - collision resistance far beyond
// what change detection needs, at half the bytes on the wire.
func hashJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Every caller hashes plain structs/slices of plain values, which
		// can't fail to encode.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// paragraphContentHash covers exactly the paragraph fields an offline copy
// stores for reading (see android's OfflineParagraph) - not Words/SFX/
// audio status, which either aren't stored offline or belong to
// paragraphAudioHash.
func paragraphContentHash(p paragraphDTO) string {
	return hashJSON(struct {
		Text                string
		Speaker             string
		Inline              bool
		IsQuote             bool
		ScareQuote          bool
		DescribesCharacters []string
		Emotion             string
		DirectionMarks      []directionMarkDTO
		PronunciationMarks  []pronunciationMarkDTO
	}{p.Text, p.Speaker, p.Inline, p.IsQuote, p.ScareQuote, p.DescribesCharacters, p.Emotion, p.DirectionMarks, p.PronunciationMarks})
}

// paragraphAudioHash identifies the audio a client would download for p:
// its resolved voice (so a book voice change, a multi-voice toggle or a
// speaker re-attribution all change it), its status (a not-yet-ready
// paragraph never matches a downloaded one), which clip backs it
// (AudioURL - differs for a scare-quote merge group member) and that
// clip's duration/pointer, plus its emotion (which picks the reference
// clip it was cloned from, without changing the voice id). A regeneration
// under the same voice and emotion that happened to produce a clip of
// exactly the same duration isn't detected - the same heuristic the
// Android client already used before this existed.
func paragraphAudioHash(p paragraphDTO, voiceID string) string {
	return hashJSON(struct {
		VoiceID         string
		Emotion         string
		Status          string
		AudioURL        string
		DurationSeconds float64
		PointerSeconds  float64
	}{voiceID, p.Emotion, p.AudioStatus, p.AudioURL, p.DurationSeconds, p.AudioPointerSeconds})
}

// chapterHash is a chapter's node: its own title and content order plus
// every paragraph's two leaves. Generating is excluded - it's transient
// job state, not content.
func chapterHash(c chapterDetailDTO) string {
	type leaf struct {
		Idx     int
		Content string
		Audio   string
	}
	leaves := make([]leaf, len(c.Paragraphs))
	for i, p := range c.Paragraphs {
		leaves[i] = leaf{p.Idx, p.ContentHash, p.AudioHash}
	}
	return hashJSON(struct {
		Title      string
		Content    []contentItemDTO
		Paragraphs []leaf
	}{c.Title, c.Content, leaves})
}

type chapterHashDTO struct {
	Idx  int    `json:"idx"`
	Hash string `json:"hash"`
}

type bookManifestDTO struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Author       string `json:"author"`
	CoverURL     string `json:"coverUrl,omitempty"`
	ChapterCount int    `json:"chapterCount"`
	// Hash is the root over the book's metadata and the chapters listed
	// in Chapters - see handleBookManifest.
	Hash            string           `json:"hash"`
	Chapters        []chapterHashDTO `json:"chapters"`
	PosChapterIdx   int              `json:"posChapterIdx"`
	PosParagraphIdx int              `json:"posParagraphIdx"`
	PosSeconds      float64          `json:"posSeconds"`
}

// handleBookManifest returns a book's offline-sync root hash plus its
// chapter hashes. `?chapters=0,3,7` limits it to those chapters (unknown
// indexes are just absent from the result) - a client only ever holds a
// few chapters of a book offline, so it asks for just what it has; the
// root then covers only that subset, which is still exactly what it needs
// to compare against. Without the parameter, every chapter is included.
//
// Chapter hashes come from the stored tree (store.SyncTreeNodes) wherever
// their inputs haven't moved, so an unchanged book costs a fingerprint
// query plus two small reads, not a buildChapter per chapter - see "The
// stored tree" above.
func (s *Server) handleBookManifest(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")

	var wanted map[int]bool
	if raw := r.URL.Query().Get("chapters"); raw != "" {
		wanted = map[int]bool{}
		for _, part := range strings.Split(raw, ",") {
			idx, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid chapters list")
				return
			}
			wanted[idx] = true
		}
	}

	version := syncTreeVersion()
	s.syncTreePrune.Do(func() {
		if err := s.Store.PruneSyncTree(version); err != nil {
			log.Printf("prune sync tree: %v", err)
		}
	})

	// Fingerprint before building anything - see "The stored tree" above
	// for why the order matters.
	bookFP, chapterFPs, err := s.Store.SyncFingerprints(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	indexes := make([]int, 0, len(chapterFPs))
	for idx := range chapterFPs {
		if wanted == nil || wanted[idx] {
			indexes = append(indexes, idx)
		}
	}
	slices.Sort(indexes)

	stored, err := s.Store.SyncTreeNodes(bookID, version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	hashes := make(map[int]string, len(indexes))
	var built []int
	for _, idx := range indexes {
		if n, ok := stored[idx]; ok && n.BookFP == bookFP && n.ChapterFP == chapterFPs[idx] {
			hashes[idx] = n.Hash
			continue
		}
		ch, err := s.buildChapter(bookID, idx)
		if err != nil {
			writeBuilt(w)(nil, err)
			return
		}
		hashes[idx] = ch.(chapterDetailDTO).Hash
		built = append(built, idx)
	}
	if len(built) > 0 {
		// Only keep a freshly built hash if nothing it depends on moved
		// while it was being built - otherwise it can't be tied to either
		// fingerprint, and the next request just builds it again.
		afterBookFP, afterFPs, err := s.Store.SyncFingerprints(bookID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		var nodes []store.SyncTreeNode
		for _, idx := range built {
			if afterBookFP == bookFP && afterFPs[idx] == chapterFPs[idx] {
				nodes = append(nodes, store.SyncTreeNode{ChapterIdx: idx, BookFP: bookFP, ChapterFP: chapterFPs[idx], Hash: hashes[idx]})
			}
		}
		// Only a cache - failing to store it costs the next request a
		// rebuild, not this one its answer.
		if err := s.Store.PutSyncTreeNodes(bookID, version, nodes); err != nil {
			log.Printf("store sync tree for book %s: %v", bookID, err)
		}
	}

	dto := bookManifestDTO{
		ID:              book.ID,
		Title:           book.Title,
		Author:          book.Author,
		ChapterCount:    len(chapterFPs),
		Chapters:        make([]chapterHashDTO, len(indexes)),
		PosChapterIdx:   book.PosChapterIdx,
		PosParagraphIdx: book.PosParagraphIdx,
		PosSeconds:      book.PosSeconds,
	}
	if book.CoverExt != "" {
		dto.CoverURL = "/api/books/" + book.ID + "/cover"
	}
	for i, idx := range indexes {
		dto.Chapters[i] = chapterHashDTO{Idx: idx, Hash: hashes[idx]}
	}
	dto.Hash = hashJSON(struct {
		Title        string
		Author       string
		Cover        string
		ChapterCount int
		Chapters     []chapterHashDTO
	}{book.Title, book.Author, s.coverIdentity(book), dto.ChapterCount, dto.Chapters})
	writeJSON(w, http.StatusOK, dto)
}

// syncTreeVersion identifies the code that computes hashes, so a stored
// node is only ever reused by the same build that hashed it: a sha256 of
// the running executable. The row fingerprints can't see code - a changed
// hash field, DTO shape, or compiled-in voice preset (internal/voices)
// changes chapter hashes without touching a single row - and a hand-bumped
// version constant would be one forgotten bump from serving stale hashes.
// Go builds are reproducible, so this changes exactly when the compiled
// code does (uncommitted edits included), and a new build just rebuilds
// each book's tree once. If the executable can't be read, a random
// per-process value degrades this to an in-memory cache rather than risk
// reuse.
var syncTreeVersion = sync.OnceValue(func() string {
	if path, err := os.Executable(); err == nil {
		if f, err := os.Open(path); err == nil {
			defer f.Close()
			h := sha256.New()
			if _, err := io.Copy(h, f); err == nil {
				return hex.EncodeToString(h.Sum(nil)[:16])
			}
		}
	}
	log.Printf("sync tree: can't hash own executable; stored nodes won't outlive this process")
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "process-" + hex.EncodeToString(b)
})

// coverIdentity stands in for the cover image's bytes in the root hash -
// its URL never changes, so a replaced cover is only visible in the
// file's own size/mtime. "" when there's no cover.
func (s *Server) coverIdentity(b *store.Book) string {
	if b.CoverExt == "" {
		return ""
	}
	fi, err := os.Stat(audiopath.CoverFile(s.DataDir, b.ID, b.CoverExt))
	if err != nil {
		return ""
	}
	return strconv.FormatInt(fi.Size(), 10) + "@" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
}
