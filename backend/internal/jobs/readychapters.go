package jobs

import (
	"slices"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// readyChapters remembers the chapters paragraphsNeedingGeneration found
// nothing to generate in, until a write that could change that. Why:
// enqueueLookahead walks on from the reader's position until it has found
// its window of unready paragraphs, which in a book already narrated ahead
// is every remaining chapter - 45ms a call 30 chapters from the end of one,
// several calls a second while skipping through a chapter.
type readyChapters struct {
	subscribe sync.Once
	mu        sync.Mutex
	gen       uint64
	chapters  map[string]readyChapter
}

type readyChapter struct {
	bookID     string
	paragraphs int
}

// readyChapterTables are the tables that decide what a chapter still needs
// generated: its paragraphs and their audio, and whatever resolves a
// speaker to a voice. A saved position (store.PositionTable) isn't one.
var readyChapterTables = []string{
	"chapters", "paragraphs", "paragraph_audio",
	"books", "characters", "character_voices", "character_stages", "voice_presets", "default_voice",
}

func (r *readyChapters) changed(tables []string) {
	scope, hit := "", false
	for _, t := range tables {
		if id, ok := store.ScopedBook(t); ok {
			scope = id
		} else if t == store.AllTables || slices.Contains(readyChapterTables, t) {
			hit = true
		}
	}
	if !hit {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	if scope == "" {
		r.chapters = nil
		return
	}
	for id, ch := range r.chapters {
		if ch.bookID == scope {
			delete(r.chapters, id)
		}
	}
}

// get reports whether chapterID is known to need nothing generated, with
// its paragraph count if so. Otherwise gen is what to hand put with the
// answer once it has been worked out.
func (r *readyChapters) get(st store.Store, chapterID string) (paragraphs int, ok bool, gen uint64) {
	r.subscribe.Do(func() { st.OnChange(r.changed) })
	r.mu.Lock()
	defer r.mu.Unlock()
	ch, ok := r.chapters[chapterID]
	return ch.paragraphs, ok, r.gen
}

// put records that chapterID needed nothing generated, unless something
// was written since gen was read.
func (r *readyChapters) put(gen uint64, bookID, chapterID string, paragraphs int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.gen != gen {
		return
	}
	if r.chapters == nil {
		r.chapters = map[string]readyChapter{}
	}
	r.chapters[chapterID] = readyChapter{bookID: bookID, paragraphs: paragraphs}
}
