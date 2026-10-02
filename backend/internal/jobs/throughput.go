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
// Each completion's audio length is kept too, for the same run's speed as
// a multiple of realtime (see Manager.RealtimeFactor). The zero value is
// ready to use.
type narrationThroughput struct {
	mu      sync.Mutex
	samples []throughputSample // oldest first
}

type throughputSample struct {
	at    time.Time
	chars int
	audio time.Duration // the rendered clip's length, 0 if unknown
}

func (nt *narrationThroughput) record(at time.Time, chars int, audio time.Duration) {
	if chars <= 0 {
		return
	}
	nt.mu.Lock()
	defer nt.mu.Unlock()
	nt.samples = append(nt.samples, throughputSample{at: at, chars: chars, audio: audio})
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
	charsPerSec, _, ok = nt.measure(now, true)
	return charsPerSec, ok
}

// realtime is rate in seconds of audio rendered per wall-clock second -
// 12 means an hour of narration takes five minutes. ok is also false when
// the run's clips had no measurable length.
func (nt *narrationThroughput) realtime(now time.Time) (factor float64, ok bool) {
	_, factor, ok = nt.measure(now, true)
	return factor, ok && factor > 0
}

// lastRate is rate without the staleness cutoff: the most recent run's
// speed even after it ended - a "typical speed" for estimating work that
// hasn't been queued yet. Forgotten once a new run's first completion
// prunes the old run past throughputWindow.
func (nt *narrationThroughput) lastRate(now time.Time) (charsPerSec float64, ok bool) {
	charsPerSec, _, ok = nt.measure(now, false)
	return charsPerSec, ok
}

func (nt *narrationThroughput) measure(now time.Time, freshOnly bool) (charsPerSec, realtime float64, ok bool) {
	nt.mu.Lock()
	defer nt.mu.Unlock()
	n := len(nt.samples)
	if n == 0 || (freshOnly && now.Sub(nt.samples[n-1].at) > throughputGap) {
		return 0, 0, false
	}
	start := n - 1
	for start > 0 && nt.samples[start].at.Sub(nt.samples[start-1].at) <= throughputGap {
		start--
	}
	run := nt.samples[start:]
	span := run[len(run)-1].at.Sub(run[0].at)
	if len(run) < throughputMinSamples || span < throughputMinSpan {
		return 0, 0, false
	}
	// The first completion only marks when the run's clock starts; the
	// characters (and audio) counted are the ones finished after it.
	chars := 0
	var audio time.Duration
	for _, s := range run[1:] {
		chars += s.chars
		audio += s.audio
	}
	return float64(chars) / span.Seconds(), audio.Seconds() / span.Seconds(), true
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

// LastThroughput is Throughput for the most recent run even once it has
// ended (see narrationThroughput.lastRate).
func (m *Manager) LastThroughput() (charsPerSec float64, ok bool) {
	return m.throughput.lastRate(time.Now())
}

// RealtimeFactor is Throughput as a multiple of realtime: seconds of
// narration audio rendered per wall-clock second, every generation slot
// together (see narrationThroughput.realtime).
func (m *Manager) RealtimeFactor() (factor float64, ok bool) {
	return m.throughput.realtime(time.Now())
}
