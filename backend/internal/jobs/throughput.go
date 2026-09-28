package jobs

import (
	"sync"
	"time"
	"unicode/utf8"
)

// throughputWindow bounds how far back narrationThroughput looks, and
// throughputGap is the longest pause between two completions it still
// treats as one continuous run - a rate measured across an idle stretch
// (queue drained, paused, worker restarting) would badly underestimate how
// fast work actually goes once it's flowing.
const (
	throughputWindow     = 10 * time.Minute
	throughputGap        = 2 * time.Minute
	throughputMinSamples = 3
	throughputMinSpan    = 20 * time.Second
)

// narrationThroughput measures how fast narration actually renders, in
// characters of paragraph text per wall-clock second across every
// generation slot together (so concurrency and batching are baked in),
// from recent successful KindVoiceClone/KindVoiceDesign completions. It
// backs the Jobs views' "time left" estimates (see Manager.Throughput).
// The zero value is ready to use.
type narrationThroughput struct {
	mu      sync.Mutex
	samples []throughputSample // oldest first
}

type throughputSample struct {
	at    time.Time
	chars int
}

func (nt *narrationThroughput) record(at time.Time, chars int) {
	if chars <= 0 {
		return
	}
	nt.mu.Lock()
	defer nt.mu.Unlock()
	nt.samples = append(nt.samples, throughputSample{at: at, chars: chars})
	cut := 0
	for cut < len(nt.samples) && at.Sub(nt.samples[cut].at) > throughputWindow {
		cut++
	}
	nt.samples = nt.samples[cut:]
}

// rate returns characters per second over the most recent continuous run
// of completions, ok false until there's enough of one to measure (or once
// the last completion is older than throughputGap - the run is over, and
// whatever starts next may run at a different pace).
func (nt *narrationThroughput) rate(now time.Time) (charsPerSec float64, ok bool) {
	nt.mu.Lock()
	defer nt.mu.Unlock()
	n := len(nt.samples)
	if n == 0 || now.Sub(nt.samples[n-1].at) > throughputGap {
		return 0, false
	}
	start := n - 1
	for start > 0 && nt.samples[start].at.Sub(nt.samples[start-1].at) <= throughputGap {
		start--
	}
	run := nt.samples[start:]
	span := run[len(run)-1].at.Sub(run[0].at)
	if len(run) < throughputMinSamples || span < throughputMinSpan {
		return 0, false
	}
	// The first completion only marks when the run's clock starts; the
	// characters counted are the ones finished after it.
	chars := 0
	for _, s := range run[1:] {
		chars += s.chars
	}
	return float64(chars) / span.Seconds(), true
}

// taskChars is how much paragraph text a narration task renders - every
// member of a merged group, else its one paragraph; 0 for any other kind.
func taskChars(t *task) int {
	if t.kind != KindVoiceClone && t.kind != KindVoiceDesign {
		return 0
	}
	if len(t.mergeParagraphs) > 0 {
		n := 0
		for _, p := range t.mergeParagraphs {
			n += utf8.RuneCountInString(p.Text)
		}
		return n
	}
	return utf8.RuneCountInString(t.paragraph.Text)
}

// Throughput reports recent narration speed in characters per second (see
// narrationThroughput), ok false when there's no recent continuous run to
// measure.
func (m *Manager) Throughput() (charsPerSec float64, ok bool) {
	return m.throughput.rate(time.Now())
}
