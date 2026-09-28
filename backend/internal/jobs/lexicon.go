package jobs

import (
	"log"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// lexiconFor is bookID's scope's pronunciation rules (store.ListLexicon,
// cached), for ResolveGenerationText. Best-effort: a lookup failure is
// logged and generation goes ahead without the rules, the same way a
// paragraph renders without its LLM pronunciation fixes before that pass
// has run.
func (m *Manager) lexiconFor(bookID string) []pronounce.LexiconEntry {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		if err != nil {
			log.Printf("jobs: lexicon: load book %s: %v", bookID, err)
		}
		return nil
	}
	entries, err := m.store.ListLexicon(store.SeriesScope(book))
	if err != nil {
		log.Printf("jobs: lexicon for book %s: %v", bookID, err)
		return nil
	}
	return store.PronounceLexicon(entries)
}

// snapshotLexicon looks up t's lexicon for generate and records it on t
// (see task.genLexicon).
func (m *Manager) snapshotLexicon(t *task) []pronounce.LexiconEntry {
	lexicon := m.lexiconFor(t.bookID)
	t.genLexicon, t.genLexiconSet = lexicon, true
	return lexicon
}
