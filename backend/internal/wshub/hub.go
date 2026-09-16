// Package wshub fans out paragraph status changes to reader clients over
// WebSocket, so the frontend learns a paragraph went pending -> generating
// -> ready the moment it happens instead of discovering it on the next
// poll. It carries no other state - the REST API remains the source of
// truth for everything else (chapter content, position, etc.); this is
// purely a low-latency notification channel layered on top of it.
package wshub

import (
	"encoding/json"
	"sync"
)

// ParagraphUpdate is one paragraph's status change. Mirrors the paragraph
// fields httpapi's chapter DTO already exposes, plus which chapter it
// belongs to, so a client can patch an already-loaded chapter in place
// instead of refetching it.
type ParagraphUpdate struct {
	ChapterIdx      int     `json:"chapterIdx"`
	ParagraphIdx    int     `json:"paragraphIdx"`
	AudioStatus     string  `json:"audioStatus"`
	AudioError      string  `json:"audioError,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	AudioURL        string  `json:"audioUrl,omitempty"`
	// AudioPointerSeconds mirrors paragraphDTO's own field of the same
	// name (see its doc comment) - set only for a scare-quote merge group
	// pointer, whose AudioURL resolves to another paragraph's shared clip.
	AudioPointerSeconds float64 `json:"audioPointerSeconds,omitempty"`
	// Words carries word-level alignment (raw [{text,start,end}]
	// JSON - see ttsworker.Manager.Align) once it's ready. Omitted (not just "[]")
	// for updates that aren't about alignment - "generating" or "error" -
	// so the frontend's merge leaves whatever word timing a paragraph
	// already had untouched rather than wiping it on an unrelated status
	// change.
	Words json.RawMessage `json:"words,omitempty"`
}

// Hub fans updates for a book out to whichever clients currently have that
// book's reader open.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan ParagraphUpdate]struct{} // bookID -> subscriber channels
}

func New() *Hub {
	return &Hub{subs: make(map[string]map[chan ParagraphUpdate]struct{})}
}

// Subscribe registers a new listener for bookID's updates; call Unsubscribe
// with the same channel when the connection closes. The channel is
// buffered so a slow reader can't block generation - Publish drops an
// update for a subscriber whose buffer is already full rather than
// waiting, which is fine here since the client refetches full state on
// (re)connect anyway, so a dropped push is only ever seen a bit late, not
// missed entirely.
func (h *Hub) Subscribe(bookID string) chan ParagraphUpdate {
	ch := make(chan ParagraphUpdate, 64)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.subs[bookID] == nil {
		h.subs[bookID] = make(map[chan ParagraphUpdate]struct{})
	}
	h.subs[bookID][ch] = struct{}{}
	return ch
}

func (h *Hub) Unsubscribe(bookID string, ch chan ParagraphUpdate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs[bookID], ch)
	if len(h.subs[bookID]) == 0 {
		delete(h.subs, bookID)
	}
}

func (h *Hub) Publish(bookID string, u ParagraphUpdate) {
	h.mu.Lock()
	subs := h.subs[bookID]
	chans := make([]chan ParagraphUpdate, 0, len(subs))
	for ch := range subs {
		chans = append(chans, ch)
	}
	h.mu.Unlock()

	for _, ch := range chans {
		select {
		case ch <- u:
		default:
		}
	}
}
