package jobs

import (
	"context"
	"strconv"

	"github.com/rhino1998/lectable/backend/internal/sched"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// The reader rules (docs/scheduler-design.md): each reader is its own
// request source, withdrawn as the reader moves; the passes of the
// chapters just ahead are requested before the reader gets there; a pass a
// reader is blocked on runs over the paragraphs at the playhead first; and
// a book set to strict order narrates a chapter only once it's attributed.

// preworkChapters is how many chapters past the reader's own have their
// passes requested at TierLookahead, so one LLM stint covers several
// chapters and happens while the reader's audio window is full.
const preworkChapters = 3

// passWindowParagraphs is the size of a window pass: the emotion pass's
// own batch size, so a window is one model call.
const passWindowParagraphs = 30

// readerSource is the scheduler request source of one reader of a book.
// clientID is whatever the client sends with its lookahead calls (random,
// kept per installation); clients that send none share one source.
func readerSource(bookID, clientID string) string {
	return "reader:" + bookID + ":" + clientID
}

// newPassTask builds the lazily-run task of one chapter pass - what a
// dependency creates and what prework requests, so both are the same task.
func (m *Manager) newPassTask(kind Kind, book *store.Book, ch *store.Chapter, tier int) *task {
	chVal := *ch
	var run func(ctx context.Context) (int, func(), error)
	switch kind {
	case KindSpeechDirection:
		run = m.passRun(kind, m.direct, m.directWindow, book, &chVal)
	case KindPronunciation:
		run = m.passRun(kind, m.pronounce, m.pronounceWindow, book, &chVal)
	case KindSpeakerAttribution:
		run = func(ctx context.Context) (int, func(), error) { return m.attribute(ctx, book, &chVal) }
	case KindScareQuote:
		run = func(ctx context.Context) (int, func(), error) { return m.scareQuote(ctx, book, &chVal) }
	case KindSummary:
		run = func(ctx context.Context) (int, func(), error) { return m.summarize(ctx, book, &chVal) }
	}
	return &task{
		kind:        kind,
		tier:        tier,
		bookID:      book.ID,
		chapterID:   ch.ID,
		chapterIdx:  ch.Idx,
		llmKey:      ch.ID,
		runLLM:      run,
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
}

// passRun is a whole-chapter pass that leaves out what window runs have
// already covered, so a line a reader has heard isn't labeled a second
// time (a different answer would discard its audio).
func (m *Manager) passRun(kind Kind, full func(context.Context, *store.Book, *store.Chapter) (int, func(), error), window ChapterWindowPass, book *store.Book, ch *store.Chapter) func(context.Context) (int, func(), error) {
	return func(ctx context.Context) (int, func(), error) {
		if window != nil {
			if rest, ok := m.uncovered(kind, ch.ID); ok {
				return window(ctx, book, ch, rest, false)
			}
		}
		return full(ctx, book, ch)
	}
}

func windowKey(kind Kind, chapterID string) string { return string(kind) + ":" + chapterID }

func (m *Manager) covered(kind Kind, chapterID string, idx int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.passWindows[windowKey(kind, chapterID)][idx]
}

// uncovered lists the chapter's paragraph idxs no window run has covered;
// ok is false when none has run (the pass should just run whole).
func (m *Manager) uncovered(kind Kind, chapterID string) (rest map[int]bool, ok bool) {
	m.mu.Lock()
	done := len(m.passWindows[windowKey(kind, chapterID)])
	m.mu.Unlock()
	if done == 0 {
		return nil, false
	}
	paragraphs, err := m.store.ListParagraphsRaw(chapterID)
	if err != nil {
		return nil, false
	}
	rest = map[int]bool{}
	for _, p := range paragraphs {
		if !m.covered(kind, chapterID, p.Idx) {
			rest[p.Idx] = true
		}
	}
	return rest, true
}

// windowDependency is what an urgent paragraph waits for instead of its
// chapter's whole pass: a run over just the window of paragraphs around
// it (keyed "<pass>:<chapterID>@<start>"), a single model call rather than
// the chapter's worth a reader who jumped here would otherwise sit
// through. The whole pass still runs, at whatever priority the rest of the
// reader's window gives it, over what the windows left (passRun). ok is
// false when t should wait for the whole pass as usual.
func (m *Manager) windowDependency(kind Kind, window ChapterWindowPass, t *task, book *store.Book, ch *store.Chapter) (needs []sched.Need, ok bool) {
	if window == nil || t.tier != TierUrgent {
		return nil, false
	}
	idx := t.paragraph.Idx
	if m.covered(kind, ch.ID, idx) {
		return nil, true
	}
	start := idx / passWindowParagraphs * passWindowParagraphs
	only := make(map[int]bool, passWindowParagraphs)
	for i := start; i < start+passWindowParagraphs; i++ {
		only[i] = true
	}
	chVal := *ch
	wt := &task{
		kind:       kind,
		tier:       t.tier,
		bookID:     t.bookID,
		chapterID:  ch.ID,
		chapterIdx: ch.Idx,
		paragraph:  store.Paragraph{Idx: start},
		llmKey:     ch.ID + "@" + strconv.Itoa(start),
		runLLM: func(ctx context.Context) (int, func(), error) {
			n, _, err := window(ctx, book, &chVal, only, true)
			if err == nil {
				m.mu.Lock()
				key := windowKey(kind, chVal.ID)
				if m.passWindows[key] == nil {
					m.passWindows[key] = map[int]bool{}
				}
				for i := range only {
					m.passWindows[key][i] = true
				}
				m.mu.Unlock()
			}
			return n, nil, err
		},
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return []sched.Need{{Key: wt.Key(), New: m.depTask(wt)}}, true
}

// strictBlocked reports whether ch's narration has to wait for its
// attribution: a multi-voice book set to strict order, whose lines would
// otherwise be narrated in the book's own voice now and again in their
// speaker's once attribution reaches the chapter.
func (m *Manager) strictBlocked(book *store.Book, ch *store.Chapter) bool {
	return book.StrictOrder && book.MultiVoice() && m.attribute != nil && !ch.Passes.Attribution
}

// requestChapterPasses asks, for source at tier, for the passes ch's
// narration will wait on that haven't run - the reader's prework. Returns
// the keys it asked for.
func (m *Manager) requestChapterPasses(book *store.Book, ch *store.Chapter, tier int, source string) []string {
	var kinds []Kind
	if book.SpeechDirection {
		if m.direct != nil && !ch.Passes.Direction {
			kinds = append(kinds, KindSpeechDirection)
		}
		if m.pronounce != nil && !ch.Passes.Pronunciation {
			kinds = append(kinds, KindPronunciation)
		}
	}
	if m.strictBlocked(book, ch) {
		kinds = append(kinds, KindSpeakerAttribution)
	}
	keys := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		t := m.newPassTask(kind, book, ch, tier)
		t.source = source
		m.pushTask(t)
		keys = append(keys, t.Key())
	}
	return keys
}

// ensureAttributed blocks until ch's attribution has run (queueing it at
// tier, under ctx's group if it has one) when the book's strict order
// needs it before narration; a no-op otherwise.
func (m *Manager) ensureAttributed(ctx context.Context, book *store.Book, ch *store.Chapter, tier int) error {
	if !m.strictBlocked(book, ch) {
		return nil
	}
	t := m.newPassTask(KindSpeakerAttribution, book, ch, tier)
	done := make(chan llmOutcome, 1)
	t.llmWaiters = []chan llmOutcome{done}
	t.parent = parentOf(ctx)
	m.pushTask(t)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case out := <-done:
		return out.err
	}
}
