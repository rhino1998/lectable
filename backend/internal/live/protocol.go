package live

import "encoding/json"

// The protocol's message shapes (see the Client doc comment). Named so
// cmd/apigen can generate the clients' copies; the snapshot/patch
// envelopes are built by hand in envelope() to avoid re-marshaling a
// shared payload per subscriber, so these document them rather than
// encode them.

// SubscribeMessage starts (or restarts) a subscription.
type SubscribeMessage struct {
	Type string `json:"type" tstype:"'subscribe'"`
	// ID is chosen by the client and echoed on every reply.
	ID     string          `json:"id"`
	Topic  string          `json:"topic"`
	Params json.RawMessage `json:"params"`
}

// UnsubscribeMessage ends a subscription.
type UnsubscribeMessage struct {
	Type string `json:"type" tstype:"'unsubscribe'"`
	ID   string `json:"id"`
}

// SnapshotMessage carries a topic's full current value.
type SnapshotMessage struct {
	Type string          `json:"type" tstype:"'snapshot'"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

// PatchMessage carries the changes since the value the client holds.
type PatchMessage struct {
	Type string `json:"type" tstype:"'patch'"`
	ID   string `json:"id"`
	// Ops' wire shape comes from Op.MarshalJSON.
	Ops []Op `json:"ops" tstype:"LiveOp[]" kttype:"List<JsonElement>"`
}

// ErrorMessage reports a subscription that couldn't be served (bad
// params, missing book, ...). The next message for the id, if any, is a
// fresh snapshot.
type ErrorMessage struct {
	Type   string `json:"type" tstype:"'error'"`
	ID     string `json:"id"`
	Status int    `json:"status"`
	Error  string `json:"error"`
}
