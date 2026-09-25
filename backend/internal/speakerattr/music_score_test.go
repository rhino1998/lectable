package speakerattr

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// scriptedMusicLLM answers pass 1 with one region per batch and pass 2
// with a fixed description, marking "new" scene only for regions whose
// text contains "Later".
type scriptedMusicLLM struct {
	mu        sync.Mutex
	describes int
}

func (s *scriptedMusicLLM) LLMGenerate(_ context.Context, sys, user string, _ float32, _ int) (string, error) {
	if sys == musicSystemPrompt {
		first := user[strings.Index(user, "Paragraphs:\n")+len("Paragraphs:\n"):]
		first = first[:strings.IndexByte(first, ':')]
		return `[{"startIdx": ` + first + `, "transition": "cut"}]`, nil
	}
	s.mu.Lock()
	s.describes++
	s.mu.Unlock()
	scene := "same"
	if strings.Contains(user, "Later") {
		scene = "new"
	}
	return `{"scene": "` + scene + `", "mood": "calm", "genres": ["Ambient"], "instruments": ["pad"], "bpm": 60, "music": "slow, sparse", "ambience": ""}`, nil
}

func TestScoreMusicSharesDescriptionAcrossSplitsAndCutsOnNewScene(t *testing.T) {
	ps := plainParagraphs(60)
	ps[45].Text = "Later that night, far away."
	llm := &scriptedMusicLLM{}
	out, remaining, err := NewClient(Config{}, llm).ScoreMusic(context.Background(), "B", "C", ps, nil)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("ScoreMusic() err=%v remaining=%d", err, len(remaining))
	}
	// Pass 1 never breaks the tone (each batch reports only its carried-over
	// region), so the chapter is one region split every 10 paragraphs.
	if len(out) != 6 {
		t.Fatalf("ScoreMusic() = %d regions, want 6", len(out))
	}
	// Two describe calls: regions 0-29 and 30-59.
	if llm.describes != 2 {
		t.Fatalf("describe calls = %d, want 2", llm.describes)
	}
	var cuts []int
	for _, r := range out {
		if r.Transition == "cut" {
			cuts = append(cuts, r.StartIdx)
		}
	}
	// The chapter's opening, then region 30 - its text contains paragraph
	// 45's scene change.
	if len(cuts) != 2 || cuts[0] != 0 || cuts[1] != 30 {
		t.Fatalf("cuts at %v, want [0 30]", cuts)
	}
}
