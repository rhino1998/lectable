package bookexport

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// Exports are kept on disk so downloading one again is just serving a
// file:
//
//	DATA_DIR/exports/<book id>/<artifact id><ext>    the export
//	DATA_DIR/exports/<book id>/<artifact id>.json    its Artifact
//
// An artifact is identified by its Options (Options.ID), so building the
// same selection again replaces it in place. Whether it's still current
// is decided when listing, by comparing the fingerprint it was built from
// with the library's now (store.ExportFingerprints) - nothing invalidates
// it on write.

// formatVersion is folded into every fingerprint: bump it when a change
// here changes what an export of unchanged data contains, so existing
// artifacts show as stale.
const formatVersion = 1

// progressInterval rate-limits progress notifications during a build.
const progressInterval = 500 * time.Millisecond

// Artifact describes one built export - its .json sidecar.
type Artifact struct {
	ID              string    `json:"id"`
	BookID          string    `json:"bookId"`
	Options         Options   `json:"options"`
	CreatedAt       time.Time `json:"createdAt"`
	SizeBytes       int64     `json:"sizeBytes"`
	DurationSeconds float64   `json:"durationSeconds"`
	MissingAudio    int       `json:"missingAudio"`
	// Fingerprint is the library state it was built from - taken before
	// the build started, so a write during the build makes it stale.
	Fingerprint string `json:"fingerprint"`
	// FileName is what a download is saved as.
	FileName string `json:"fileName"`
}

// Status is one artifact slot of a book: a built export, a requested one
// still waiting or rendering or building, a failed build - or several of
// those at once (rebuilding a stale export keeps serving the old one
// until it's done).
type Status struct {
	ID      string
	Options Options
	// Artifact is the built export, nil if there isn't one yet.
	Artifact *Artifact
	// Stale means Artifact no longer matches the library.
	Stale bool
	// Pending means a build was requested (Request) and hasn't finished -
	// whether its task is still queued is the job queue's to say.
	Pending bool
	// Rendering means the build is waiting for its chapters' audio.
	Rendering bool
	Building  bool
	// Progress is a running build's fraction done, 0-1.
	Progress float64
	// Error is the last build's failure, cleared by the next request.
	Error string
}

// ErrNotFound is returned for an artifact that doesn't exist.
var ErrNotFound = errors.New("export not found")

// Manager keeps exports on disk and tracks the ones being made. It
// doesn't schedule anything itself: a caller queues the work (httpapi, as
// a job-queue task) and drives it through Request, Rendering and Build,
// ending with Build or Abort.
type Manager struct {
	src    Source
	dir    string
	notify func()

	mu     sync.Mutex
	runs   map[string]*run    // by bookID/artifactID
	failed map[string]failure // last build error, same key
	now    func() time.Time
}

type failure struct {
	opts Options
	msg  string
}

// run is one requested export on its way to a file.
type run struct {
	opts        Options
	rendering   bool
	building    bool
	done, total int64
	lastNotify  time.Time
}

// NewManager keeps exports under src.DataDir/exports. notify is called
// (never with the Manager's lock held) whenever something List reports
// changes. Leftover temp files from a build interrupted by a restart are
// removed.
func NewManager(src Source, notify func()) *Manager {
	m := &Manager{
		src: src, dir: filepath.Join(src.DataDir, "exports"), notify: notify,
		runs: map[string]*run{}, failed: map[string]failure{}, now: time.Now,
	}
	books, _ := os.ReadDir(m.dir)
	for _, b := range books {
		entries, _ := os.ReadDir(filepath.Join(m.dir, b.Name()))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				_ = os.Remove(filepath.Join(m.dir, b.Name(), e.Name()))
			}
		}
	}
	return m
}

func runKey(bookID, id string) string { return bookID + "/" + id }

func (m *Manager) bookDir(bookID string) string { return filepath.Join(m.dir, bookID) }

func (m *Manager) filePath(bookID string, opts Options) string {
	return filepath.Join(m.bookDir(bookID), opts.ID()+opts.Format.Ext())
}

func (m *Manager) metaPath(bookID, id string) string {
	return filepath.Join(m.bookDir(bookID), id+".json")
}

func (m *Manager) changed() {
	if m.notify != nil {
		m.notify()
	}
}

// Normalize validates opts against bookID's chapter count - see
// Options.Normalize.
func (m *Manager) Normalize(bookID string, opts Options) (Options, error) {
	n, err := m.src.Store.CountChapters(bookID)
	if err != nil {
		return opts, err
	}
	return opts.Normalize(n)
}

// Request records that an export of opts (Normalized) was asked for, so
// List shows it before there's a file, and clears its last failure.
// Returns its artifact id. Requesting one already pending is harmless.
func (m *Manager) Request(bookID string, opts Options) string {
	id := opts.ID()
	key := runKey(bookID, id)
	m.mu.Lock()
	if _, ok := m.runs[key]; !ok {
		m.runs[key] = &run{opts: opts}
	}
	delete(m.failed, key)
	m.mu.Unlock()
	m.changed()
	return id
}

// Rendering marks a requested export as waiting for its chapters' audio.
func (m *Manager) Rendering(bookID string, opts Options) {
	m.mu.Lock()
	r := m.runLocked(bookID, opts)
	r.rendering = true
	m.mu.Unlock()
	m.changed()
}

func (m *Manager) runLocked(bookID string, opts Options) *run {
	key := runKey(bookID, opts.ID())
	r, ok := m.runs[key]
	if !ok {
		r = &run{opts: opts}
		m.runs[key] = r
	}
	return r
}

// Abort ends a requested export without building it: err is recorded as
// its failure unless it's nil or a cancellation (deleted or canceled on
// purpose).
func (m *Manager) Abort(bookID string, opts Options, err error) {
	key := runKey(bookID, opts.ID())
	m.mu.Lock()
	delete(m.runs, key)
	if err != nil && !errors.Is(err, context.Canceled) {
		m.failed[key] = failure{opts, err.Error()}
		log.Printf("bookexport: export %s failed: %v", key, err)
	}
	m.mu.Unlock()
	m.changed()
}

// ErrIncomplete is Build's error when requireComplete is set and some
// exported paragraph has no audio.
var ErrIncomplete = errors.New("chapters not fully rendered")

// Build writes the export of opts for bookID and ends its request (see
// Abort for how an error is kept). With requireComplete, a paragraph
// without audio fails the build instead of going in text-only.
func (m *Manager) Build(ctx context.Context, bookID string, opts Options, requireComplete bool) error {
	m.mu.Lock()
	r := m.runLocked(bookID, opts)
	r.rendering, r.building = false, true
	m.mu.Unlock()
	m.changed()

	start := m.now()
	err := m.build(ctx, bookID, r, requireComplete)
	if err == nil {
		log.Printf("bookexport: built %s/%s in %s", bookID, opts.ID(), m.now().Sub(start).Round(time.Millisecond))
	}
	m.Abort(bookID, opts, err)
	return err
}

func (m *Manager) build(ctx context.Context, bookID string, r *run, requireComplete bool) error {
	opts := r.opts
	fp, err := m.fingerprint(bookID, opts)
	if err != nil {
		return err
	}
	book, err := m.src.Collect(bookID, opts)
	if err != nil {
		return err
	}
	if requireComplete && book.MissingAudio > 0 {
		return fmt.Errorf("%w: %d paragraphs still have no audio (their generation failed) - regenerate them, or export as-is", ErrIncomplete, book.MissingAudio)
	}
	if err := os.MkdirAll(m.bookDir(bookID), 0o755); err != nil {
		return err
	}
	final := m.filePath(bookID, opts)
	f, err := os.CreateTemp(m.bookDir(bookID), "."+filepath.Base(final)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed

	now := m.now()
	bw := bufio.NewWriterSize(f, 1<<20)
	err = write(ctx, bw, book, opts, now, func(done, total int64) { m.progress(r, done, total) })
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	a := Artifact{
		ID: opts.ID(), BookID: bookID, Options: opts, CreatedAt: now, SizeBytes: fi.Size(),
		DurationSeconds: book.Duration(), MissingAudio: book.MissingAudio, Fingerprint: fp,
		FileName: fileName(book.Title, opts),
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return audiopath.WriteFileAtomic(m.metaPath(bookID, a.ID), data)
}

// write dispatches to opts.Format's writer - the one place a new format
// plugs in.
func write(ctx context.Context, w io.Writer, b *Book, opts Options, now time.Time, progress Progress) error {
	switch opts.Format {
	case FormatEPUB:
		return WriteEPUB(ctx, w, b, opts, now, progress)
	}
	return fmt.Errorf("unknown export format %q", opts.Format)
}

func (m *Manager) progress(r *run, done, total int64) {
	m.mu.Lock()
	r.done, r.total = done, total
	notify := m.now().Sub(r.lastNotify) >= progressInterval
	if notify {
		r.lastNotify = m.now()
	}
	m.mu.Unlock()
	if notify {
		m.changed()
	}
}

// fingerprint is the library state an export of opts reflects.
func (m *Manager) fingerprint(bookID string, opts Options) (string, error) {
	book, err := m.src.Store.GetBook(bookID)
	if err != nil {
		return "", err
	}
	if book == nil {
		return "", ErrBookNotFound
	}
	bookFP, chapters, err := m.src.Store.ExportFingerprints(bookID, store.SeriesScope(book))
	if err != nil {
		return "", err
	}
	return fingerprintOf(opts, bookFP, chapters), nil
}

func fingerprintOf(opts Options, bookFP string, chapters map[int]string) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d|%s|%s", formatVersion, opts.ID(), bookFP)
	idxs := opts.Chapters
	if opts.Full() {
		for idx := range chapters {
			idxs = append(idxs, idx)
		}
		slices.Sort(idxs)
	}
	for _, idx := range idxs {
		fmt.Fprintf(h, "|%d:%s", idx, chapters[idx])
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// List reports every artifact slot bookID has - built, requested or
// failed - newest first.
func (m *Manager) List(bookID string) ([]Status, error) {
	byID := map[string]*Status{}
	entries, err := os.ReadDir(m.bookDir(bookID))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		a, err := m.readArtifact(bookID, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue // its file is gone or the sidecar is unreadable
		}
		byID[a.ID] = &Status{ID: a.ID, Options: a.Options, Artifact: a}
	}

	m.mu.Lock()
	prefix := bookID + "/"
	for key, r := range m.runs {
		if id, ok := strings.CutPrefix(key, prefix); ok {
			s := byID[id]
			if s == nil {
				s = &Status{ID: id, Options: r.opts}
				byID[id] = s
			}
			s.Pending, s.Rendering, s.Building = true, r.rendering, r.building
			if r.total > 0 {
				s.Progress = float64(r.done) / float64(r.total)
			}
		}
	}
	for key, f := range m.failed {
		if id, ok := strings.CutPrefix(key, prefix); ok {
			s := byID[id]
			if s == nil {
				s = &Status{ID: id, Options: f.opts}
				byID[id] = s
			}
			s.Error = f.msg
		}
	}
	m.mu.Unlock()

	// Staleness needs the current fingerprints - one query for the book,
	// shared by every artifact.
	var bookFP string
	var chapters map[int]string
	for _, s := range byID {
		if s.Artifact == nil {
			continue
		}
		if chapters == nil {
			book, err := m.src.Store.GetBook(bookID)
			if err != nil {
				return nil, err
			}
			if book == nil {
				break
			}
			if bookFP, chapters, err = m.src.Store.ExportFingerprints(bookID, store.SeriesScope(book)); err != nil {
				return nil, err
			}
		}
		s.Stale = fingerprintOf(s.Artifact.Options, bookFP, chapters) != s.Artifact.Fingerprint
	}

	out := make([]Status, 0, len(byID))
	for _, s := range byID {
		out = append(out, *s)
	}
	slices.SortFunc(out, func(a, b Status) int {
		// In-progress first, then newest built.
		if (a.Artifact == nil) != (b.Artifact == nil) {
			if a.Artifact == nil {
				return -1
			}
			return 1
		}
		if a.Artifact != nil && !a.Artifact.CreatedAt.Equal(b.Artifact.CreatedAt) {
			return b.Artifact.CreatedAt.Compare(a.Artifact.CreatedAt)
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

func (m *Manager) readArtifact(bookID, id string) (*Artifact, error) {
	if !validID(id) {
		return nil, ErrNotFound
	}
	data, err := os.ReadFile(m.metaPath(bookID, id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, err
	}
	if _, err := os.Stat(m.filePath(bookID, a.Options)); err != nil {
		return nil, ErrNotFound
	}
	return &a, nil
}

// File returns a built artifact and the path to serve.
func (m *Manager) File(bookID, id string) (string, *Artifact, error) {
	a, err := m.readArtifact(bookID, id)
	if err != nil {
		return "", nil, err
	}
	return m.filePath(bookID, a.Options), a, nil
}

// Delete forgets id's request, if any, and removes its files and error.
// Canceling the work itself is the caller's (it queued it).
func (m *Manager) Delete(bookID, id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	key := runKey(bookID, id)
	m.mu.Lock()
	_, pending := m.runs[key]
	delete(m.runs, key)
	_, failed := m.failed[key]
	delete(m.failed, key)
	m.mu.Unlock()

	a, err := m.readArtifact(bookID, id)
	switch {
	case err == nil:
		if err := os.Remove(m.filePath(bookID, a.Options)); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Remove(m.metaPath(bookID, id)); err != nil && !os.IsNotExist(err) {
			return err
		}
	case errors.Is(err, ErrNotFound):
		_ = os.Remove(m.metaPath(bookID, id)) // a sidecar whose file is gone
		if !pending && !failed {
			return ErrNotFound
		}
	default:
		return err
	}
	m.changed()
	return nil
}

// DeleteBook forgets bookID's requests and removes all its exports - for
// a deleted book.
func (m *Manager) DeleteBook(bookID string) error {
	prefix := bookID + "/"
	m.mu.Lock()
	for key := range m.runs {
		if strings.HasPrefix(key, prefix) {
			delete(m.runs, key)
		}
	}
	for key := range m.failed {
		if strings.HasPrefix(key, prefix) {
			delete(m.failed, key)
		}
	}
	m.mu.Unlock()
	err := os.RemoveAll(m.bookDir(bookID))
	m.changed()
	return err
}

// validID reports whether id could be an Options.ID - it's used in file
// names, so anything else (a path) is rejected outright.
func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// fileName is a download name: the title with characters file systems
// dislike removed, plus the chapter selection and word-level marker.
func fileName(title string, opts Options) string {
	name := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 0x20 {
			return -1
		}
		return r
	}, title)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "book"
	}
	if label := ChapterLabel(opts.Chapters); label != "" {
		name += " (" + label + ")"
	}
	if opts.ExcludeMusic {
		name += " [no music]"
	}
	switch {
	case opts.WordLevel && opts.PhraseSeconds > 0:
		name += " [phrases]"
	case opts.WordLevel:
		name += " [words]"
	}
	return name + opts.Format.Ext()
}
