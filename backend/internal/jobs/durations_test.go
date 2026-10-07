package jobs

import (
	"testing"
	"time"
)

func TestKindDurationsTypical(t *testing.T) {
	var d kindDurations
	if _, ok := d.typical(KindSummary); ok {
		t.Fatal("typical with no runs reported ok")
	}
	d.record(KindSummary, 10*time.Second)
	d.record(KindSummary, 30*time.Second)
	if got, ok := d.typical(KindSummary); !ok || got != 20*time.Second {
		t.Errorf("typical = %v, %v, want 20s", got, ok)
	}
	// Only the most recent durationSamples runs count.
	for range durationSamples {
		d.record(KindSummary, time.Minute)
	}
	if got, _ := d.typical(KindSummary); got != time.Minute {
		t.Errorf("typical after %d newer runs = %v, want 1m", durationSamples, got)
	}
}

func TestWorkLeft(t *testing.T) {
	m := &Manager{}
	m.durations.record(KindSummary, time.Minute)
	tasks := []QueueTask{
		{Kind: KindSummary}, {Kind: KindSummary}, {Kind: KindAmbience},
		{Kind: KindVoiceClone}, {Kind: "pipeline_summary"},
	}
	got := m.WorkLeft(tasks)
	if len(got) != 2 {
		t.Fatalf("WorkLeft = %+v, want summary and ambience only", got)
	}
	want := 2 * time.Minute / time.Duration(poolCapacity[poolFor(KindSummary)])
	if got[0].Kind != KindSummary || got[0].Tasks != 2 || got[0].Typical != time.Minute || got[0].Left != want {
		t.Errorf("summary = %+v, want 2 tasks, 1m typical, %v left", got[0], want)
	}
	if got[1].Kind != KindAmbience || got[1].Tasks != 1 || got[1].Left != 0 {
		t.Errorf("ambience = %+v, want 1 task with no estimate", got[1])
	}
}
