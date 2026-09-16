// Command jobswatch streams the Jobs dashboard's own live feed
// (GET /api/jobs/ws - see httpapi.handleJobsWS/buildJobsSnapshot) to the
// terminal as a readable diff instead of the full-snapshot-per-change JSON
// the frontend consumes, for exactly the kind of "what's actually happening
// to this task" debugging that otherwise means grepping server.log by hand
// for a task's own dedup key (e.g. "direction:b4e3cd5f...") across however
// many restarts have happened since - slow, and easy to miss a transition
// that happened between two log lines. Every task's own ID here is that
// same dedup key (jobs.QueueTask.ID docs this - "direction:<chapterID>",
// "generate:<characterID>", etc.), so a line printed here can be grepped
// straight out of server.log too, in either direction.
//
// Usage:
//
//	go run ./cmd/jobswatch                          # everything, live
//	go run ./cmd/jobswatch -book 4238153b            # one book only (prefix match)
//	go run ./cmd/jobswatch -kind speech_direction    # one kind only
//	go run ./cmd/jobswatch -grep "chapter 62"        # substring match over the whole printed line
//	go run ./cmd/jobswatch -raw                      # print each snapshot as raw JSON instead of diffing
//
// Reconnects automatically (with a short backoff) if the connection drops -
// including across a server restart, so it's safe to leave running while
// restarting ./server to watch whatever it does to already-queued/in-flight
// work (a real, recurring need: a restart's shutdown context cancellation
// is what several of this app's "task X failed: context canceled" log
// lines actually mean, and watching it happen live is far faster than
// reconstructing it after the fact from timestamps).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// task mirrors httpapi.queueTaskDTO field-for-field (JSON tags only - that
// type is unexported, so this is a deliberate copy, not an import) rather
// than importing internal/httpapi, which would drag its whole Server type
// (and everything *it* imports) into a small standalone debugging binary.
// Keep this in sync by hand if that DTO's shape ever changes.
type task struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Label        string `json:"label"`
	BookID       string `json:"bookId"`
	BookTitle    string `json:"bookTitle"`
	ChapterIdx   int    `json:"chapterIdx"`
	ChapterTitle string `json:"chapterTitle"`
	ParagraphIdx int    `json:"paragraphIdx"`
	Tier         string `json:"tier"`
	PresetID     string `json:"presetId"`
	Instruct     string `json:"instruct"`
	Attempt      int    `json:"attempt"`
}

// snapshot mirrors httpapi.jobsSnapshotDTO - see task's own doc comment.
type snapshot struct {
	InFlight []task `json:"inFlight"`
	Queued   []task `json:"queued"`
	Paused   bool   `json:"paused"`
}

// seen is one task's last-observed state, enough to describe every
// transition diffSnapshot cares about without re-deriving it from two
// whole snapshots each time.
type seen struct {
	task      task
	inFlight  bool
	firstSeen time.Time
}

func main() {
	addr := flag.String("addr", "ws://127.0.0.1:8080/api/jobs/ws", "jobs websocket URL (see backend's PORT env var if not the default 8080)")
	bookFilter := flag.String("book", "", "only show tasks whose bookId has this prefix")
	kindFilter := flag.String("kind", "", "only show tasks whose kind is in this comma-separated list (e.g. speech_direction,speaker_characterization)")
	grepFilter := flag.String("grep", "", "only show tasks whose printed line contains this substring (case-insensitive)")
	raw := flag.Bool("raw", false, "print each snapshot as raw JSON instead of diffing")
	full := flag.Bool("full", false, "on every change, also print the complete current queued/in-flight listing, not just the diff")
	flag.Parse()

	var kinds map[string]bool
	if *kindFilter != "" {
		kinds = map[string]bool{}
		for _, k := range strings.Split(*kindFilter, ",") {
			kinds[strings.TrimSpace(k)] = true
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	w := &watcher{
		addr:   *addr,
		book:   *bookFilter,
		kinds:  kinds,
		grep:   strings.ToLower(*grepFilter),
		raw:    *raw,
		full:   *full,
		known:  map[string]seen{},
		paused: false,
	}
	w.run(ctx)
}

type watcher struct {
	addr  string
	book  string
	kinds map[string]bool
	grep  string
	raw   bool
	full  bool

	known       map[string]seen
	pausedKnown bool
	paused      bool
}

func (w *watcher) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := w.connectAndStream(ctx); err != nil && ctx.Err() == nil {
			logf("disconnected: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (w *watcher) connectAndStream(ctx context.Context) error {
	logf("connecting to %s ...", w.addr)
	conn, _, err := websocket.Dial(ctx, w.addr, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	logf("connected")

	// Every reconnect starts from a blank slate - a snapshot right after
	// dialing is the queue's real current state, not a diff against
	// whatever this process last saw before the connection dropped, which
	// could be stale by an arbitrary amount (including a full server
	// restart in between, see this command's own doc comment).
	w.known = map[string]seen{}
	w.pausedKnown = false

	for {
		var snap snapshot
		if err := wsjson.Read(ctx, conn, &snap); err != nil {
			return err
		}
		w.handleSnapshot(snap)
	}
}

func (w *watcher) handleSnapshot(snap snapshot) {
	if w.raw {
		b, _ := json.Marshal(snap)
		fmt.Println(string(b))
		return
	}

	filtered := func(ts []task) []task {
		out := make([]task, 0, len(ts))
		for _, t := range ts {
			if w.book != "" && !strings.HasPrefix(t.BookID, w.book) {
				continue
			}
			if w.kinds != nil && !w.kinds[t.Kind] {
				continue
			}
			if w.grep != "" && !strings.Contains(strings.ToLower(describe(t, false)), w.grep) {
				continue
			}
			out = append(out, t)
		}
		return out
	}
	inFlight := filtered(snap.InFlight)
	queued := filtered(snap.Queued)

	if !w.pausedKnown || w.paused != snap.Paused {
		logf("queue %s", map[bool]string{true: "PAUSED", false: "resumed"}[snap.Paused])
		w.paused = snap.Paused
		w.pausedKnown = true
	}

	now := time.Now()
	next := map[string]seen{}
	for _, t := range inFlight {
		next[t.ID] = seen{task: t, inFlight: true, firstSeen: now}
	}
	for _, t := range queued {
		next[t.ID] = seen{task: t, inFlight: false, firstSeen: now}
	}
	// Preserve firstSeen across snapshots for anything still present, so
	// "how long has this actually been sitting there" stays meaningful
	// instead of resetting on every single message.
	for id, ns := range next {
		if old, ok := w.known[id]; ok {
			ns.firstSeen = old.firstSeen
			next[id] = ns
		}
	}

	for id, ns := range next {
		old, existed := w.known[id]
		switch {
		case !existed:
			status := "queued"
			if ns.inFlight {
				status = "dispatched"
			}
			logf("+ %-10s %s", status, describe(ns.task, true))
		case old.inFlight != ns.inFlight && ns.inFlight:
			logf("> dispatched %s (queued %s)", describe(ns.task, true), time.Since(old.firstSeen).Round(time.Second))
		case old.task.Attempt != ns.task.Attempt:
			logf("! retry attempt=%d %s", ns.task.Attempt, describe(ns.task, true))
		case old.task.Tier != ns.task.Tier:
			logf("^ promoted %s -> %s  %s", old.task.Tier, ns.task.Tier, describe(ns.task, true))
		}
	}
	for id, old := range w.known {
		if _, ok := next[id]; !ok {
			age := time.Since(old.firstSeen).Round(time.Second)
			status := "gone"
			if old.inFlight {
				status = "finished/failed"
			} else {
				status = "gone (canceled, dispatched, or completed while unobserved)"
			}
			logf("- %-10s %s  (was present %s)", status, describe(old.task, true), age)
		}
	}
	w.known = next

	if w.full && (len(inFlight) > 0 || len(queued) > 0) {
		fmt.Println("  --- current state ---")
		for _, t := range inFlight {
			fmt.Println("  [in-flight] " + describe(t, true))
		}
		for _, t := range queued {
			fmt.Println("  [queued]    " + describe(t, true))
		}
		fmt.Println("  ---------------------")
	}
}

// describe renders one task as a single readable line, deliberately
// omitting any field that's meaningless for this task's own kind (see
// httpapi.queueTaskDTO's own field-by-field doc comments on which those
// are) rather than printing a confusing zero value for it.
func describe(t task, withID bool) string {
	var parts []string
	parts = append(parts, "["+t.Kind+"]")
	if t.Label != "" {
		parts = append(parts, strconv.Quote(t.Label))
	}
	if t.ChapterTitle != "" {
		parts = append(parts, fmt.Sprintf("ch%d(%s)", t.ChapterIdx, strconv.Quote(t.ChapterTitle)))
	} else if t.ChapterIdx != 0 {
		parts = append(parts, fmt.Sprintf("ch%d", t.ChapterIdx))
	}
	if t.ParagraphIdx != 0 {
		parts = append(parts, fmt.Sprintf("para%d", t.ParagraphIdx))
	}
	parts = append(parts, "tier="+t.Tier)
	if t.Attempt > 0 {
		parts = append(parts, fmt.Sprintf("attempt=%d", t.Attempt))
	}
	if t.BookTitle != "" {
		parts = append(parts, "book="+strconv.Quote(t.BookTitle))
	}
	if withID {
		parts = append(parts, "id="+t.ID)
	}
	return strings.Join(parts, " ")
}

func logf(format string, args ...any) {
	log.Printf(format, args...)
}
