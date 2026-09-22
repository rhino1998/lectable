package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Wire protocol - JSON text frames in both directions.
//
// Client -> server:
//
//	{"type":"subscribe","id":"<client-chosen>","topic":"<kind>","params":{...}}
//	{"type":"unsubscribe","id":"<same id>"}
//
// id is opaque to the server and just tags every message about that
// subscription; subscribing again with an id already in use replaces the
// old subscription.
//
// Server -> client:
//
//	{"type":"snapshot","id":"...","data":<full value>}
//	{"type":"patch","id":"...","ops":[<Op>...]}
//	{"type":"error","id":"...","status":404,"error":"book not found"}
//
// A subscription always starts with a snapshot or an error. A patch
// applies to the value the client currently holds for that id. After an
// error the next message for the id (if the topic recovers) is a fresh
// snapshot, never a patch.
type clientMessage struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	Topic  string          `json:"topic"`
	Params json.RawMessage `json:"params"`
}

// Client is one WebSocket connection's subscriptions and outbound queue.
type Client struct {
	hub  *Hub
	out  chan []byte
	done chan struct{}
	once sync.Once

	mu   sync.Mutex
	subs map[string]*topic // subscription id -> topic
}

func (h *Hub) newClient() *Client {
	return &Client{
		hub:  h,
		out:  make(chan []byte, h.cfg.ClientBuffer),
		done: make(chan struct{}),
		subs: make(map[string]*topic),
	}
}

// ServeWS upgrades r to a WebSocket and serves the protocol above until
// either side closes it.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, opts *websocket.AcceptOptions) {
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	// Snapshots of a whole chapter (every paragraph's text and word
	// timings) can be large; the default 32KiB read limit only matters for
	// what the client sends, which is tiny, but keep it explicit.
	conn.SetReadLimit(64 << 10)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	c := h.newClient()
	defer c.closeAll()

	go func() {
		defer cancel()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var msg clientMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				c.sendError("", http.StatusBadRequest, "invalid message: "+err.Error())
				continue
			}
			switch msg.Type {
			case "subscribe":
				c.subscribe(msg.ID, msg.Topic, msg.Params)
			case "unsubscribe":
				c.unsubscribe(msg.ID)
			default:
				c.sendError(msg.ID, http.StatusBadRequest, fmt.Sprintf("unknown message type %q", msg.Type))
			}
		}
	}()

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			conn.Close(websocket.StatusPolicyViolation, "client too slow; reconnect")
			return
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		case msg := <-c.out:
			if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
				return
			}
		}
	}
}

func (c *Client) subscribe(id, kind string, params json.RawMessage) {
	resolve, ok := c.hub.resolvers[kind]
	if !ok {
		c.sendError(id, http.StatusBadRequest, fmt.Sprintf("unknown topic %q", kind))
		return
	}
	def, err := resolve(params)
	if err != nil {
		status, msg := errorStatus(err)
		if _, ok := err.(*Error); !ok {
			status = http.StatusBadRequest
		}
		c.sendError(id, status, msg)
		return
	}
	c.unsubscribe(id)

	t := c.hub.acquire(def)
	c.mu.Lock()
	c.subs[id] = t
	c.mu.Unlock()

	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.built {
		// First subscriber: build inline rather than waiting on the refresh
		// loop, so the snapshot arrives immediately. rebuild would deadlock
		// on t.mu, so this is its no-subscribers-yet special case.
		v, err := def.Build()
		if err != nil {
			t.built, t.ok = true, false
			t.errStatus, t.errMsg = errorStatus(err)
		} else if raw, err := json.Marshal(v); err != nil {
			t.built, t.ok = true, false
			t.errStatus, t.errMsg = http.StatusInternalServerError, err.Error()
		} else if val, err := decode(raw); err != nil {
			t.built, t.ok = true, false
			t.errStatus, t.errMsg = http.StatusInternalServerError, err.Error()
		} else {
			t.built, t.ok, t.raw, t.val = true, true, raw, val
		}
	}
	t.subs[subRef{c, id}] = struct{}{}
	if t.ok {
		c.sendSnapshot(id, t.raw)
	} else {
		c.sendError(id, t.errStatus, t.errMsg)
	}
}

func (c *Client) unsubscribe(id string) {
	c.mu.Lock()
	t, ok := c.subs[id]
	delete(c.subs, id)
	c.mu.Unlock()
	if !ok {
		return
	}
	t.mu.Lock()
	delete(t.subs, subRef{c, id})
	t.mu.Unlock()
	c.hub.release(t)
}

func (c *Client) closeAll() {
	c.mu.Lock()
	ids := make([]string, 0, len(c.subs))
	for id := range c.subs {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.unsubscribe(id)
	}
}

// send queues msg without blocking; a full queue means the connection
// can't keep up, so it's dropped (done closed) rather than stalling every
// other subscriber's fan-out behind it or silently losing a patch, which
// would corrupt the client's copy. The client reconnects and resubscribes.
func (c *Client) send(msg []byte) {
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.out <- msg:
	default:
		c.once.Do(func() { close(c.done) })
	}
}

func (c *Client) sendSnapshot(id string, raw []byte) {
	c.send(envelope("snapshot", id, "data", raw))
}

func (c *Client) sendPatch(id string, ops []byte) {
	c.send(envelope("patch", id, "ops", ops))
}

func (c *Client) sendError(id string, status int, message string) {
	msg, _ := json.Marshal(struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Status int    `json:"status"`
		Error  string `json:"error"`
	}{"error", id, status, message})
	c.send(msg)
}

// envelope builds {"type":typ,"id":id,field:raw} around an already-encoded
// payload, so a patch computed once per topic isn't re-marshaled per
// subscriber.
func envelope(typ, id, field string, raw []byte) []byte {
	idJSON := strconv.Quote(id)
	if b, err := json.Marshal(id); err == nil {
		idJSON = string(b)
	}
	buf := make([]byte, 0, len(raw)+len(idJSON)+len(typ)+len(field)+32)
	buf = append(buf, `{"type":"`...)
	buf = append(buf, typ...)
	buf = append(buf, `","id":`...)
	buf = append(buf, idJSON...)
	buf = append(buf, `,"`...)
	buf = append(buf, field...)
	buf = append(buf, `":`...)
	buf = append(buf, raw...)
	buf = append(buf, '}')
	return buf
}
