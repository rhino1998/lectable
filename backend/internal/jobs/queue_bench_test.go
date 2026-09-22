package jobs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// benchQueueManager builds the steady state a long background generation
// run actually sits in: a Higgs book with SpeechDirection on (so every
// clone task runs characterization, direction, pronunciation and
// reference-clip dependency checks), every chapter already tagged, every
// speaker already characterized and every preset's reference clip already
// rendered - so none of those checks ever finds a real blocker, and each
// Pop's cost is purely the scheduling overhead of re-checking n tasks.
func benchQueueManager(b *testing.B, n int) *Manager {
	b.Helper()
	s, err := store.Open(filepath.Join(b.TempDir(), "library.duckdb"))
	if err != nil {
		b.Fatalf("store.Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	const chapters = 10
	var chInputs []store.ChapterInput
	for c := 0; c < chapters; c++ {
		chInputs = append(chInputs, store.ChapterInput{Title: fmt.Sprintf("Ch%d", c), Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "Text."}}})
	}
	bookID, _, err := s.CreateBook("Book", "Author", "en", "", "", 0, chInputs)
	if err != nil {
		b.Fatalf("CreateBook: %v", err)
	}
	book, _ := s.GetBook(bookID)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, "audiocpp-higgs-4b", book.CharacterVoiceMode, true, false); err != nil {
		b.Fatalf("UpdateVoice: %v", err)
	}
	var chapterIDs []string
	for c := 0; c < chapters; c++ {
		ch, err := s.GetChapterByIdx(bookID, c)
		if err != nil || ch == nil {
			b.Fatalf("GetChapterByIdx: %v", err)
		}
		_ = s.SetChapterDirected(ch.ID)
		_ = s.SetChapterPronounced(ch.ID)
		chapterIDs = append(chapterIDs, ch.ID)
	}
	speakers := []string{"Alice", "Bob", "Carol", "Dave"}
	for _, name := range speakers {
		char, _, err := s.UpsertCharacter(store.SeriesScope(book), name, false)
		if err != nil {
			b.Fatalf("UpsertCharacter: %v", err)
		}
		_ = s.SetCharacterSummary(char.ID, "a voice", "A line.")
	}

	m := newTestManagerWithStore(s)
	m.dataDir = b.TempDir()
	noop := func(context.Context, *store.Book, *store.Chapter) (int, func(), error) { return 0, nil, nil }
	m.direct = noop
	m.pronounce = noop
	m.characterize = func(context.Context, string, store.Character) error { return nil }

	presets := []string{"preset-a", "preset-b"}
	for _, p := range presets {
		path := audiopath.VoicePresetRefFile(m.dataDir, p)
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, []byte("x"), 0o644)
	}

	for i := 0; i < n; i++ {
		c := i % chapters
		m.queue.Push(&task{
			kind: KindVoiceClone, tier: TierBackground, bookID: bookID,
			chapterID: chapterIDs[c], chapterIdx: c, presetID: presets[i%len(presets)],
			paragraph: store.Paragraph{ID: fmt.Sprintf("p-%d", i), Idx: i, Speaker: speakers[i%len(speakers)]},
		})
	}
	return m
}

// BenchmarkQueuePop measures one Pop+Finish round trip against a queue
// holding n already-unblocked clone tasks - what the worker loop pays for
// every single dispatch.
func BenchmarkQueuePop(b *testing.B) {
	for _, n := range []int{100, 500, 1000, 2000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			m := benchQueueManager(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tk, ok := m.queue.Pop()
				if !ok {
					b.Fatal("expected a dispatchable task")
				}
				m.queue.Finish(tk.Key())
				// Put it straight back so the queue stays at n.
				m.queue.Push(tk.(*task).requeueCopy())
			}
		})
	}
}

// BenchmarkQueueSnapshot measures the Jobs dashboard's own read, which
// runs the same full dependency pass on every jobs change notification.
func BenchmarkQueueSnapshot(b *testing.B) {
	for _, n := range []int{500, 2000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			m := benchQueueManager(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.queue.Snapshot()
			}
		})
	}
}
