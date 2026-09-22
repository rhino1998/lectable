// Package live pushes server state to clients over one WebSocket as it
// changes, replacing client-side polling. A client subscribes to *topics*
// - named categories of state such as "books" or "chapter" plus the
// parameters that pick one instance (a book id, a chapter index) - and
// receives the topic's full current value once ("snapshot"), then a
// "patch" (see Op) every time that value changes afterward.
//
// Topics are plain builder functions (typically the same code a REST GET
// handler uses) plus a list of dependencies naming what can change them:
// "db:<table>" for a store table (see store.Store.OnChange) or any other
// name a producer passes to Invalidate (e.g. "jobs" for the in-memory job
// queue). An invalidation marks every active topic depending on it dirty;
// one background loop rebuilds dirty topics, compares the result with what
// subscribers last saw, and fans out a patch only if something actually
// changed. A topic's value is computed once per change no matter how many
// connections subscribe to it, and topics nobody subscribes to cost
// nothing.
package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"slices"
	"sync"
	"time"
)

// AnyDep, passed to Invalidate, dirties every active topic regardless of
// its declared dependencies.
const AnyDep = "*"

// Topic is one resolved instance of a topic kind - see Resolver.
type Topic struct {
	// Key identifies this instance canonically: two subscriptions whose
	// Key matches share one computed value.
	Key string
	// Deps are the invalidation names this topic's value depends on.
	Deps []string
	// MinInterval throttles how often an expensive topic rebuilds under a
	// steady stream of invalidations - a dirty topic rebuilt less than
	// MinInterval ago waits out the remainder. Zero means no throttle
	// beyond the hub's own debounce.
	MinInterval time.Duration
	// Build computes the topic's current value, which must marshal to
	// JSON. A returned *Error is reported to subscribers with its status;
	// any other error as a 500.
	Build func() (any, error)
}

// Resolver turns a subscribe request's params into a Topic, or returns an
// error (reported as a 400 unless it's an *Error) for malformed params.
type Resolver func(params json.RawMessage) (*Topic, error)

// Error is a Build/Resolver failure carrying an HTTP-style status, so a
// client can tell "that book doesn't exist" (404) apart from a server
// fault.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Config tunes a Hub; zero values pick the defaults noted per field.
type Config struct {
	// Debounce is how long the refresh loop waits after the first
	// invalidation before rebuilding, so a burst of writes (a transaction
	// per paragraph, a job finishing and the next starting) coalesces into
	// one rebuild. Default 100ms.
	Debounce time.Duration
	// Resync, if positive, invalidates every topic on this interval as a
	// safety net for state that changes without an Invalidate call.
	// Default 0 (off) - cmd/server sets it.
	Resync time.Duration
	// ClientBuffer is how many outbound messages may queue per connection
	// before it's considered stuck and dropped (the client reconnects and
	// resubscribes, getting fresh snapshots). Default 256.
	ClientBuffer int
}

type Hub struct {
	cfg       Config
	resolvers map[string]Resolver

	mu     sync.Mutex
	topics map[string]*topic
	dirty  map[string]bool // pending invalidation names since the last refresh pass
	wake   chan struct{}
}

func New(cfg Config) *Hub {
	if cfg.Debounce <= 0 {
		cfg.Debounce = 100 * time.Millisecond
	}
	if cfg.ClientBuffer <= 0 {
		cfg.ClientBuffer = 256
	}
	return &Hub{
		cfg:       cfg,
		resolvers: make(map[string]Resolver),
		topics:    make(map[string]*topic),
		dirty:     make(map[string]bool),
		wake:      make(chan struct{}, 1),
	}
}

// Register adds a topic kind clients can subscribe to by name. Must be
// called before Run/any connection is served.
func (h *Hub) Register(kind string, r Resolver) {
	h.resolvers[kind] = r
}

// Invalidate marks every active topic depending on any of deps dirty (all
// of them, for AnyDep). Never blocks - safe to call from a store write or
// while holding unrelated locks.
func (h *Hub) Invalidate(deps ...string) {
	if len(deps) == 0 {
		return
	}
	h.mu.Lock()
	for _, d := range deps {
		h.dirty[d] = true
	}
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// Run drives the refresh loop until ctx is canceled.
func (h *Hub) Run(ctx context.Context) {
	var resync <-chan time.Time
	if h.cfg.Resync > 0 {
		t := time.NewTicker(h.cfg.Resync)
		defer t.Stop()
		resync = t.C
	}
	var retry *time.Timer
	var retryC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-resync:
			h.Invalidate(AnyDep)
			continue
		case <-h.wake:
		case <-retryC:
			retryC = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.cfg.Debounce):
		}
		if wait := h.refresh(); wait > 0 {
			if retry == nil {
				retry = time.NewTimer(wait)
			} else {
				retry.Reset(wait)
			}
			retryC = retry.C
		}
	}
}

// refresh folds pending invalidations into each topic's dirty flag, then
// rebuilds every dirty topic whose MinInterval has elapsed. Returns how
// long until the soonest still-throttled topic may rebuild (0 if none).
func (h *Hub) refresh() time.Duration {
	var wait time.Duration
	var topics []*topic
	now := time.Now()

	h.mu.Lock()
	dirty := h.dirty
	h.dirty = make(map[string]bool)
	for _, t := range h.topics {
		if len(dirty) > 0 && (dirty[AnyDep] || slices.ContainsFunc(t.def.Deps, func(d string) bool { return dirty[d] })) {
			t.dirty = true
		}
		if !t.dirty {
			continue
		}
		if remaining := t.def.MinInterval - now.Sub(t.lastBuild); remaining > 0 {
			if wait == 0 || remaining < wait {
				wait = remaining
			}
			continue
		}
		t.dirty = false
		t.lastBuild = now
		topics = append(topics, t)
	}
	h.mu.Unlock()

	for _, t := range topics {
		t.rebuild()
	}
	return wait
}

// topic is a Topic's shared live state plus every subscription to it.
type topic struct {
	def Topic

	// Guarded by Hub.mu: subscriptions holding this topic, whether an
	// invalidation is still waiting to be rebuilt, and when the refresh
	// loop last rebuilt it (for MinInterval).
	refs      int
	dirty     bool
	lastBuild time.Time

	mu   sync.Mutex // serializes rebuilds with subscriber add/remove
	subs map[subRef]struct{}
	// Current value as last sent: either ok (raw/val set) or failed
	// (errStatus/errMsg set). built is false until the first Build.
	built     bool
	ok        bool
	raw       []byte
	val       any
	errStatus int
	errMsg    string
}

type subRef struct {
	c  *Client
	id string
}

// rebuild recomputes the topic and pushes whatever changed to every
// subscriber. Caller must not hold t.mu.
func (t *topic) rebuild() {
	t.mu.Lock()
	defer t.mu.Unlock()
	v, err := t.def.Build()
	if err != nil {
		status, msg := errorStatus(err)
		if t.built && !t.ok && t.errStatus == status && t.errMsg == msg {
			return
		}
		t.built, t.ok, t.raw, t.val, t.errStatus, t.errMsg = true, false, nil, nil, status, msg
		for s := range t.subs {
			s.c.sendError(s.id, status, msg)
		}
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		log.Printf("live: marshal %s: %v", t.def.Key, err)
		return
	}
	if t.built && t.ok && bytes.Equal(raw, t.raw) {
		return
	}
	val, err := decode(raw)
	if err != nil {
		log.Printf("live: decode %s: %v", t.def.Key, err)
		return
	}
	wasOK := t.built && t.ok
	prev := t.val
	t.built, t.ok, t.raw, t.val = true, true, raw, val
	if !wasOK {
		for s := range t.subs {
			s.c.sendSnapshot(s.id, raw)
		}
		return
	}
	ops, err := json.Marshal(Diff(prev, val))
	if err != nil {
		log.Printf("live: marshal patch %s: %v", t.def.Key, err)
		return
	}
	for s := range t.subs {
		s.c.sendPatch(s.id, ops)
	}
}

func errorStatus(err error) (int, string) {
	var le *Error
	if errors.As(err, &le) {
		return le.Status, le.Message
	}
	return http.StatusInternalServerError, err.Error()
}

// acquire returns the shared topic for def.Key, creating it if needed, and
// takes a reference on it. Paired with release.
func (h *Hub) acquire(def *Topic) *topic {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.topics[def.Key]
	if !ok {
		t = &topic{def: *def, subs: make(map[subRef]struct{})}
		h.topics[def.Key] = t
	}
	t.refs++
	return t
}

func (h *Hub) release(t *topic) {
	h.mu.Lock()
	defer h.mu.Unlock()
	t.refs--
	if t.refs <= 0 {
		delete(h.topics, t.def.Key)
	}
}
