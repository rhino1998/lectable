package jobs

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"
)

// durationSamples is how many of a kind's most recent runs kindDurations
// averages over.
const durationSamples = 20

// kindDurations remembers how long each kind of task has been taking: the
// last few successful runs per kind, a paused pass's stretches added up
// as one run. It backs the Jobs views' "time left" estimate for
// everything that isn't narration (see Manager.WorkLeft), which is
// measured by text length instead (narrationThroughput). The zero value is
// ready to use.
type kindDurations struct {
	mu     sync.Mutex
	recent map[Kind][]time.Duration // oldest first
}

func (d *kindDurations) record(kind Kind, ran time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.recent == nil {
		d.recent = map[Kind][]time.Duration{}
	}
	runs := append(d.recent[kind], ran)
	if len(runs) > durationSamples {
		runs = runs[len(runs)-durationSamples:]
	}
	d.recent[kind] = runs
}

// typical is the mean of kind's recent runs, ok false if it hasn't run
// since startup.
func (d *kindDurations) typical(kind Kind) (time.Duration, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	runs := d.recent[kind]
	if len(runs) == 0 {
		return 0, false
	}
	var sum time.Duration
	for _, r := range runs {
		sum += r
	}
	return sum / time.Duration(len(runs)), true
}

// KindWork is one kind's share of the work a set of tasks still holds.
type KindWork struct {
	Kind  Kind
	Tasks int
	// Typical is how long one task of this kind has been taking lately, 0
	// if none has finished since startup.
	Typical time.Duration
	// Left estimates the wall-clock time until all of them are done:
	// Tasks x Typical over how many of the kind's pool run at once. 0
	// when Typical is.
	Left time.Duration
}

// WorkLeft sums tasks by kind with an estimate of how long each kind's
// share will take, most first. Narration (measured by Throughput instead)
// and group rows (preprocessing phases and bulk groups, which only wait on
// other tasks) are left out.
func (m *Manager) WorkLeft(tasks []QueueTask) []KindWork {
	counts := map[Kind]int{}
	for _, t := range tasks {
		if t.Kind == KindVoiceClone || t.Kind == KindVoiceDesign || strings.HasPrefix(string(t.Kind), "pipeline_") {
			continue
		}
		counts[t.Kind]++
	}
	out := make([]KindWork, 0, len(counts))
	for kind, n := range counts {
		w := KindWork{Kind: kind, Tasks: n}
		if typical, ok := m.durations.typical(kind); ok {
			w.Typical = typical
			w.Left = time.Duration(n) * typical / time.Duration(max(poolCapacity[poolFor(kind)], 1))
		}
		out = append(out, w)
	}
	slices.SortFunc(out, func(a, b KindWork) int {
		return cmp.Or(cmp.Compare(b.Left, a.Left), cmp.Compare(b.Tasks, a.Tasks), cmp.Compare(a.Kind, b.Kind))
	})
	return out
}
