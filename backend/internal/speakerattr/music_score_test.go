package speakerattr

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// scriptedMusicLLM answers every describe call with a fixed description, marking "new" scene only for regions whose
// text contains "Later".
type scriptedMusicLLM struct {
	mu        sync.Mutex
	describes int
}

func (s *scriptedMusicLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
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
	out, remaining, err := NewClient(Config{}, llm).ScoreMusic(context.Background(), "B", "C", nil, nil, ps, nil)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("ScoreMusic() err=%v remaining=%d", err, len(remaining))
	}
	// With no summary scenes the chapter is one region split every 10
	// paragraphs.
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

// sameAmbienceLLM answers every describe call after the first with
// "ambience": "same".
type sameAmbienceLLM struct{ calls int }

func (s *sameAmbienceLLM) LLMGenerate(_ context.Context, _, _ string, _ float32, _ int) (string, error) {
	s.calls++
	ambience := "harbor at night, lapping water"
	if s.calls > 1 {
		ambience = "Same."
	}
	return `{"scene": "same", "mood": "calm", "genres": ["Ambient"], "instruments": ["pad"], "bpm": "60", "music": "slow, sparse", "ambience": "` + ambience + `"}`, nil
}

func TestDescribeMusicRegionSameKeepsPreviousAmbience(t *testing.T) {
	c := NewClient(Config{}, &sameAmbienceLLM{})
	ps := plainParagraphs(5)
	first, err := c.describeMusicRegion(context.Background(), "B", "C", nil, nil, ps, musicDescribeContext{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.describeMusicRegion(context.Background(), "B", "C", nil, nil, ps, musicDescribeContext{prev: &first, transition: "continuation"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Ambience == "" || second.Ambience != first.Ambience || second.rawAmbience != first.rawAmbience {
		t.Fatalf("ambience = %q after %q, want it kept", second.Ambience, first.Ambience)
	}
	if !strings.HasSuffix(second.Prompt, "60 BPM") {
		t.Fatalf("prompt = %q, want a bpm given as a string read", second.Prompt)
	}
	// With nothing before it, "same" is no ambience rather than a prompt.
	alone, err := NewClient(Config{}, &sameAmbienceLLM{calls: 1}).describeMusicRegion(context.Background(), "B", "C", nil, nil, ps, musicDescribeContext{})
	if err != nil || alone.Ambience != "" {
		t.Fatalf("ambience = %q err=%v, want none", alone.Ambience, err)
	}
}
