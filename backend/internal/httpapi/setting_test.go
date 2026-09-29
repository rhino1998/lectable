package httpapi

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// settingLLM answers the setting and lighting calls, recording each.
type settingLLM struct {
	mu       sync.Mutex
	settings int
	lighting []string
}

func (l *settingLLM) LLMGenerate(_ context.Context, sys, user string, _ float32, _ int) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case strings.Contains(sys, "notes on a novel's setting"):
		l.settings++
		return `{"era": "present day", "technology": "modern: electric light, cars", "place": "Glasgow", "accent": "Scottish", "genre": "crime"}`, nil
	case strings.Contains(sys, "lighting designer"):
		l.lighting = append(l.lighting, user)
		return `{"light": "neon", "color": "#ff3080", "accent": "", "brightness": 30, "effect": "steady"}`, nil
	}
	return "", nil
}

// TestAmbienceDerivesBookSettingOnce: the setting task derives and stores
// it with one call; lighting sees the world but not the
// accent; a later caller reuses the stored one.
func TestAmbienceDerivesBookSettingOnce(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	llm := &settingLLM{}
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, llm)

	line := "The rain hammered the windows of the bar as the two detectives argued over the case file, neither willing to back down an inch tonight."
	lines := make([]string, 12)
	for i := range lines {
		lines[i] = line
	}
	bookID := createTestBook(t, s, "", 0, lines...)
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if _, err := s.AppendMusicRegions(ch.ID, []store.MusicRegionInput{{StartIdx: 0, Mood: "tense", Prompt: "p", Transition: store.MusicTransitionCut}}, 11); err != nil {
		t.Fatal(err)
	}
	if err := s.SetChapterAmbienceScored(ch.ID); err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	ch, _ = s.GetChapterByID(ch.ID)
	// Lighting alone never derives a setting - that's the setting task's
	// job (jobs.Manager.bookSettingDependency runs it first).
	if srv.bookSetting(context.Background(), book) != nil || llm.settings != 0 {
		t.Fatal("bookSetting derived inline")
	}
	for range 2 { // the second, lazy (non-forced) run finds one and skips
		if err := srv.deriveBookSettingForJob(context.Background(), book); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := srv.lightChapter(context.Background(), book, ch); err != nil {
		t.Fatal(err)
	}
	if got := srv.bookSetting(context.Background(), book); got == nil || got.Place != "Glasgow" {
		t.Fatalf("bookSetting = %+v", got)
	}
	if llm.settings != 1 || len(llm.lighting) != 1 {
		t.Fatalf("%d setting calls, %d lighting calls; want 1, 1", llm.settings, len(llm.lighting))
	}
	if u := llm.lighting[0]; !strings.Contains(u, "- era: present day") || strings.Contains(u, "Scottish") {
		t.Fatalf("lighting prompt:\n%s", u)
	}
	st, err := s.GetBookSetting(bookID)
	if err != nil || st == nil || st.Source != store.BookSettingAuto || st.Setting.Accent != "Scottish" {
		t.Fatalf("stored setting = %+v, %v", st, err)
	}
}
