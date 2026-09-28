package jobs

import (
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/store"
)

func TestNarrationThroughput(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	var nt narrationThroughput
	if _, ok := nt.rate(t0); ok {
		t.Fatal("empty: want !ok")
	}

	// 10s apart, 100 chars each: the first sample only starts the clock.
	for i := range 4 {
		nt.record(t0.Add(time.Duration(i)*10*time.Second), 100)
	}
	got, ok := nt.rate(t0.Add(30 * time.Second))
	if !ok || got != 10 {
		t.Fatalf("rate = %v, %v; want 10 chars/s", got, ok)
	}

	// Stale once the run ends - but still the last known speed.
	later := t0.Add(30*time.Second + throughputGap + time.Second)
	if _, ok := nt.rate(later); ok {
		t.Fatal("after a long idle gap: want !ok")
	}
	if got, ok := nt.lastRate(later); !ok || got != 10 {
		t.Fatalf("lastRate = %v, %v; want the ended run's 10 chars/s", got, ok)
	}

	// A new run after a gap is measured on its own, not averaged across
	// the idle stretch.
	t1 := t0.Add(time.Hour)
	for i := range 3 {
		nt.record(t1.Add(time.Duration(i)*20*time.Second), 400)
	}
	got, ok = nt.rate(t1.Add(40 * time.Second))
	if !ok || got != 20 {
		t.Fatalf("new run rate = %v, %v; want 20 chars/s", got, ok)
	}
	if len(nt.samples) != 3 {
		t.Fatalf("kept %d samples, want the old run pruned past the window", len(nt.samples))
	}
}

func TestNarrationThroughputNeedsEnough(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	var nt narrationThroughput
	nt.record(t0, 100)
	nt.record(t0.Add(time.Second), 100)
	nt.record(t0.Add(2*time.Second), 100)
	if _, ok := nt.rate(t0.Add(2 * time.Second)); ok {
		t.Fatal("3 samples over 2s: want !ok (span too short to trust)")
	}
	nt.record(t0, 0) // ignored
	if len(nt.samples) != 3 {
		t.Fatalf("zero-char sample was recorded")
	}
}

func TestTaskChars(t *testing.T) {
	p := func(s string) store.Paragraph { return store.Paragraph{Text: s} }
	if n := taskChars(&task{kind: KindVoiceClone, paragraph: p("héllo")}); n != 5 {
		t.Errorf("single = %d, want 5 (runes)", n)
	}
	if n := taskChars(&task{kind: KindVoiceDesign, paragraph: p("ab"), mergeParagraphs: []store.Paragraph{p("ab"), p("cde")}}); n != 5 {
		t.Errorf("merged = %d, want 5", n)
	}
	if n := taskChars(&task{kind: KindSpeakerAttribution, paragraph: p("abc")}); n != 0 {
		t.Errorf("non-narration = %d, want 0", n)
	}
}
