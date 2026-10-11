package jobs

import (
	"context"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// matterTask is book's matter pass as a task at tier, fn its work. The
// book is marked tried whatever the outcome (see EnsureBookMatter).
func (m *Manager) matterTask(book *store.Book, tier int, fn BookMatterFinder) *task {
	return &task{
		kind:   KindBookMatter,
		tier:   tier,
		bookID: book.ID,
		llmKey: book.ID,
		label:  "Front and back matter",
		runLLM: func(ctx context.Context) (int, func(), error) {
			err := fn(ctx, book)
			m.matterTried.Store(book.ID, true)
			return 0, nil, err
		},
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
}

// EnqueueBookMatter queues book's matter pass at tier and returns at once:
// TierUrgent for a book just imported or a reader's "check again" (one
// short call, and where the book opens waits on it), TierBackground for a
// library from before the pass existed. fn is the work; deduped per book.
func (m *Manager) EnqueueBookMatter(book *store.Book, tier int, fn BookMatterFinder) {
	m.pushTask(m.matterTask(book, tier, fn))
}

// EnsureBookMatter blocks until bookID's matter pass has run, when it has
// chapters nobody has judged (store.BookNeedsMatterPass): what whole-book
// work - "generate audio", preprocessing - calls before deciding which
// chapters to leave out, so it never narrates or attributes a title page
// just because it got there first. Joins the book's queued pass if there is
// one. A pass that failed isn't waited for again in this process: its
// chapters are simply all treated as story, as before the pass existed.
func (m *Manager) EnsureBookMatter(ctx context.Context, bookID string, tier int) {
	if m.findMatter == nil || m.store == nil {
		return
	}
	if _, tried := m.matterTried.Load(bookID); tried {
		return
	}
	if need, err := m.store.BookNeedsMatterPass(bookID); err != nil || !need {
		return
	}
	book := m.lookupBook(bookID)
	if book == nil {
		return
	}
	t := m.matterTask(book, tier, m.findMatter)
	done := make(chan llmOutcome, 1)
	t.llmWaiters = []chan llmOutcome{done}
	t.parent = parentOf(ctx)
	m.pushTask(t)
	select {
	case <-ctx.Done():
	case <-done:
	}
}
