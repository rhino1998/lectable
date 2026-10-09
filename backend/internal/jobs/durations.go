package jobs

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/sched"
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

// groupTimeLeft estimates how long members - the tasks a group is still
// waiting for - take if they are all the queue does: each kind's count x
// its typical run time over how many of its pool run at once, as WorkLeft
// has it, and narration's text at narrationRate (characters a second
// across every slot, 0 if unmeasured). Kinds are summed, since they share
// the same models. partial reports members it had no figure for (a kind
// that hasn't finished a task since startup, a group inside the group).
func (m *Manager) groupTimeLeft(members []sched.Task, narrationRate float64) (left time.Duration, partial bool) {
	counts := map[Kind]int{}
	chars := 0
	for _, member := range members {
		t, ok := member.(*task)
		switch {
		case !ok:
			partial = true
		case t.kind == KindVoiceClone || t.kind == KindVoiceDesign:
			chars += taskChars(t)
		default:
			counts[t.kind]++
		}
	}
	if chars > 0 {
		if narrationRate > 0 {
			left += time.Duration(float64(chars) / narrationRate * float64(time.Second))
		} else {
			partial = true
		}
	}
	for kind, n := range counts {
		typical, ok := m.durations.typical(kind)
		if !ok {
			partial = true
			continue
		}
		left += time.Duration(n) * typical / time.Duration(max(poolCapacity[poolFor(kind)], 1))
	}
	return left, partial
}
