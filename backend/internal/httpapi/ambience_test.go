package httpapi

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// lightingOnlyLLM answers only lighting calls, failing anything else - so a
// test can prove the music sub-pass never ran.
type lightingOnlyLLM struct {
	mu    sync.Mutex
	users []string
	other int
}

func (l *lightingOnlyLLM) LLMGenerate(_ context.Context, sys, user string, _ float32, _ int) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !strings.Contains(sys, "lighting designer") {
		l.other++
		return "", nil
	}
	l.users = append(l.users, user)
	return `{"light": "firelight", "color": "#ff8038", "accent": "", "brightness": 35, "effect": "flicker"}`, nil
}

// TestAmbienceLightsScoredChapterWithoutRescoring is the backfill path: a
// chapter scored before lighting existed (passes.ambience only) gets just
// the lighting sub-pass - its regions keep their ids, a length-only split
// shares its region's call, and a cut gets its own call shown the previous
// cue.
func TestAmbienceLightsScoredChapterWithoutRescoring(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	llm := &lightingOnlyLLM{}
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, llm)

	line := "The lanterns swung in the wind as the party walked down the long stone corridor, arguing quietly about the map."
	bookID := createTestBook(t, s, "", 0, line, line, line, line, line, line)
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil || ch == nil {
		t.Fatalf("GetChapterByIdx: %v, %v", ch, err)
	}
	regions, err := s.AppendMusicRegions(ch.ID, []store.MusicRegionInput{
		{StartIdx: 0, Mood: "calm", Prompt: "p1", Transition: store.MusicTransitionCut},
		{StartIdx: 2, Mood: "calm", Prompt: "p1", Transition: store.MusicTransitionContinuation},
		{StartIdx: 4, Mood: "tense", Prompt: "p2", Transition: store.MusicTransitionCut},
	}, 5)
	if err != nil {
		t.Fatalf("AppendMusicRegions: %v", err)
	}
	if err := s.SetChapterAmbienceScored(ch.ID); err != nil {
		t.Fatalf("SetChapterAmbienceScored: %v", err)
	}
	ch, _ = s.GetChapterByID(ch.ID)
	book, _ := s.GetBook(bookID)

	n, _, err := srv.ambienceChapter(context.Background(), book, ch)
	if err != nil {
		t.Fatalf("ambienceChapter: %v", err)
	}
	if n != 3 || len(llm.users) != 2 || llm.other != 0 {
		t.Fatalf("lit %d regions in %d lighting calls (%d other calls), want 3 in 2 (0)", n, len(llm.users), llm.other)
	}
	if strings.Contains(llm.users[0], "Previous stretch") || !strings.Contains(llm.users[1], "Previous stretch") || !strings.Contains(llm.users[1], "new scene") {
		t.Fatalf("second call should see the first's cue as a new scene:\n%s", llm.users[1])
	}
	after, err := s.ListMusicRegions(ch.ID)
	if err != nil {
		t.Fatalf("ListMusicRegions: %v", err)
	}
	for i, r := range after {
		if r.ID != regions[i].ID || r.Lighting.Color != "#ff8038" || r.Lighting.Effect != store.LightEffectFlicker {
			t.Fatalf("region %d = %+v", i, r)
		}
	}
	ch, _ = s.GetChapterByID(ch.ID)
	if !ch.Passes.AmbienceDone() {
		t.Fatalf("passes = %+v, want ambience and lighting", ch.Passes)
	}
}

// TestAmbienceGivesTinyRegionsNeutralLight covers a title page: too little
// text to read a scene from, so it's lit neutral with no LLM call.
func TestAmbienceGivesTinyRegionsNeutralLight(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	llm := &lightingOnlyLLM{}
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, llm)

	bookID := createTestBook(t, s, "", 0, "Arise: Alpha", "By Someone")
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if _, err := s.AppendMusicRegions(ch.ID, []store.MusicRegionInput{{StartIdx: 0, Mood: "quiet", Prompt: "p", Transition: store.MusicTransitionCut}}, 1); err != nil {
		t.Fatalf("AppendMusicRegions: %v", err)
	}
	if err := s.SetChapterAmbienceScored(ch.ID); err != nil {
		t.Fatalf("SetChapterAmbienceScored: %v", err)
	}
	ch, _ = s.GetChapterByID(ch.ID)
	book, _ := s.GetBook(bookID)
	if _, _, err := srv.ambienceChapter(context.Background(), book, ch); err != nil {
		t.Fatalf("ambienceChapter: %v", err)
	}
	regions, _ := s.ListMusicRegions(ch.ID)
	if len(llm.users) != 0 || regions[0].Lighting.Color != speakerattr.NeutralLighting.Color {
		t.Fatalf("calls=%d lighting=%+v, want 0 calls and neutral light", len(llm.users), regions[0].Lighting)
	}
}
