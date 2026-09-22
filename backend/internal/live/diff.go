package live

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
)

// Op is one step of a patch transforming a topic's previous JSON value into
// its new one. Values are the generic decode of the topic's JSON (maps,
// slices, json.Number, string, bool, nil), and Path addresses into that
// structure by object key (string) or array index (int). Three kinds:
//
//   - "set": replace the value at Path (or add it, for a new object key)
//     with Value. An empty Path replaces the whole document.
//   - "del": remove the object key at Path.
//   - "arr": rebuild the array at Path from Items, each either a [start,
//     end) pair of indices into the *previous* array (a run of elements
//     carried over unchanged) or {"v": value} (a new or changed element).
//     Emitted instead of per-index ops whenever elements were inserted,
//     removed or reordered, so e.g. a job queue shifting by one costs a
//     couple of index pairs rather than a rewrite of every element.
//
// Ops apply in order; the frontend's applyOps (src/api/live.ts) is the
// reference consumer.
type Op struct {
	Kind  string
	Path  []any
	Value any
	Items []ArrItem
}

// ArrItem is one entry of an "arr" op - see Op.
type ArrItem struct {
	// Start/End (End > Start) reference prev[Start:End]; otherwise Value
	// is a literal element.
	Start, End int
	Value      any
}

func (o Op) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"op":`)
	writeJSON(&buf, o.Kind)
	buf.WriteString(`,"path":`)
	path := o.Path
	if path == nil {
		path = []any{}
	}
	writeJSON(&buf, path)
	switch o.Kind {
	case "set":
		buf.WriteString(`,"value":`)
		writeJSON(&buf, o.Value)
	case "arr":
		buf.WriteString(`,"items":[`)
		for i, it := range o.Items {
			if i > 0 {
				buf.WriteByte(',')
			}
			if it.End > it.Start {
				writeJSON(&buf, [2]int{it.Start, it.End})
			} else {
				buf.WriteString(`{"v":`)
				writeJSON(&buf, it.Value)
				buf.WriteByte('}')
			}
		}
		buf.WriteByte(']')
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func writeJSON(buf *bytes.Buffer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// Every value here came out of json.Unmarshal in the first place.
		panic(err)
	}
	buf.Write(b)
}

// decode parses raw into the generic value form Diff works on, keeping
// numbers as json.Number so re-encoding a carried-over value is byte-exact.
func decode(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// Diff returns the ops that turn prev into next (both generic decodes -
// see decode). An empty result means they're equal.
func Diff(prev, next any) []Op {
	var ops []Op
	diffInto(&ops, nil, prev, next)
	return ops
}

func diffInto(ops *[]Op, path []any, prev, next any) {
	switch n := next.(type) {
	case map[string]any:
		if p, ok := prev.(map[string]any); ok {
			diffObject(ops, path, p, n)
			return
		}
	case []any:
		if p, ok := prev.([]any); ok {
			diffArray(ops, path, p, n)
			return
		}
	default:
		if reflect.DeepEqual(prev, next) {
			return
		}
	}
	*ops = append(*ops, Op{Kind: "set", Path: clonePath(path), Value: next})
}

func diffObject(ops *[]Op, path []any, prev, next map[string]any) {
	for k := range prev {
		if _, ok := next[k]; !ok {
			*ops = append(*ops, Op{Kind: "del", Path: appendPath(path, k)})
		}
	}
	for k, nv := range next {
		pv, ok := prev[k]
		if !ok {
			*ops = append(*ops, Op{Kind: "set", Path: appendPath(path, k), Value: nv})
			continue
		}
		diffInto(ops, appendPath(path, k), pv, nv)
	}
}

func diffArray(ops *[]Op, path []any, prev, next []any) {
	prevIDs, prevKeyed := elementIDs(prev)
	nextIDs, nextKeyed := elementIDs(next)
	keyed := prevKeyed && nextKeyed

	sameShape := len(prev) == len(next)
	if sameShape && keyed {
		for i := range prevIDs {
			if prevIDs[i] != nextIDs[i] {
				sameShape = false
				break
			}
		}
	}
	if sameShape {
		for i := range next {
			diffInto(ops, appendPath(path, i), prev[i], next[i])
		}
		return
	}

	var items []ArrItem
	ref := func(i int) {
		if n := len(items); n > 0 && items[n-1].End > items[n-1].Start && items[n-1].End == i {
			items[n-1].End++
			return
		}
		items = append(items, ArrItem{Start: i, End: i + 1})
	}
	lit := func(v any) { items = append(items, ArrItem{Value: v}) }

	if keyed {
		at := make(map[string]int, len(prevIDs))
		for i, id := range prevIDs {
			at[id] = i
		}
		for j, id := range nextIDs {
			if i, ok := at[id]; ok && reflect.DeepEqual(prev[i], next[j]) {
				ref(i)
			} else {
				lit(next[j])
			}
		}
	} else {
		pre := 0
		for pre < len(prev) && pre < len(next) && reflect.DeepEqual(prev[pre], next[pre]) {
			pre++
		}
		suf := 0
		for suf < len(prev)-pre && suf < len(next)-pre &&
			reflect.DeepEqual(prev[len(prev)-1-suf], next[len(next)-1-suf]) {
			suf++
		}
		for i := 0; i < pre; i++ {
			ref(i)
		}
		for _, v := range next[pre : len(next)-suf] {
			lit(v)
		}
		for i := len(prev) - suf; i < len(prev); i++ {
			ref(i)
		}
	}
	*ops = append(*ops, Op{Kind: "arr", Path: clonePath(path), Items: items})
}

// elementIDs reports each element's string "id" field, and whether every
// element has one and they're all distinct - the precondition for matching
// elements by identity rather than position.
func elementIDs(arr []any) ([]string, bool) {
	ids := make([]string, len(arr))
	seen := make(map[string]struct{}, len(arr))
	for i, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		id, ok := m["id"].(string)
		if !ok {
			return nil, false
		}
		if _, dup := seen[id]; dup {
			return nil, false
		}
		seen[id] = struct{}{}
		ids[i] = id
	}
	return ids, true
}

func appendPath(path []any, k any) []any {
	out := make([]any, len(path)+1)
	copy(out, path)
	out[len(path)] = k
	return out
}

func clonePath(path []any) []any {
	if path == nil {
		return []any{}
	}
	return append([]any(nil), path...)
}

// Apply is the inverse of Diff for a client holding a topic's value:
// applies a patch message's raw "ops" array to doc (a generic decode as
// produced by Decode), returning the new value. Containers along each
// changed path are copied, never mutated in place. The frontend's
// src/api/live.ts applyOps is the same algorithm.
func Apply(doc any, opsJSON []byte) (any, error) {
	var ops []struct {
		Op    string            `json:"op"`
		Path  []any             `json:"path"`
		Value json.RawMessage   `json:"value"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(opsJSON, &ops); err != nil {
		return nil, err
	}
	for _, op := range ops {
		var err error
		doc, err = applyAt(doc, op.Op, op.Path, op.Value, op.Items)
		if err != nil {
			return nil, err
		}
	}
	return doc, nil
}

// Decode parses raw JSON into the generic form Apply works on.
func Decode(raw []byte) (any, error) { return decode(raw) }

func applyAt(node any, kind string, path []any, value json.RawMessage, items []json.RawMessage) (any, error) {
	if len(path) == 0 {
		switch kind {
		case "set":
			return decode(value)
		case "arr":
			prev, ok := node.([]any)
			if !ok {
				return nil, fmt.Errorf("live: arr op on %T", node)
			}
			out := []any{}
			for _, it := range items {
				var pair [2]int
				if json.Unmarshal(it, &pair) == nil {
					if pair[0] < 0 || pair[1] > len(prev) || pair[0] > pair[1] {
						return nil, fmt.Errorf("live: arr range %v out of bounds", pair)
					}
					out = append(out, prev[pair[0]:pair[1]]...)
					continue
				}
				var lit struct {
					V json.RawMessage `json:"v"`
				}
				if err := json.Unmarshal(it, &lit); err != nil {
					return nil, err
				}
				v, err := decode(lit.V)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}
		return nil, fmt.Errorf("live: %s op with empty path", kind)
	}
	switch n := node.(type) {
	case map[string]any:
		k, ok := path[0].(string)
		if !ok {
			return nil, fmt.Errorf("live: non-string key %v into object", path[0])
		}
		out := make(map[string]any, len(n)+1)
		for kk, vv := range n {
			out[kk] = vv
		}
		if kind == "del" && len(path) == 1 {
			delete(out, k)
			return out, nil
		}
		v, err := applyAt(n[k], kind, path[1:], value, items)
		if err != nil {
			return nil, err
		}
		out[k] = v
		return out, nil
	case []any:
		f, ok := path[0].(float64)
		i := int(f)
		if !ok || i < 0 || i >= len(n) {
			return nil, fmt.Errorf("live: bad array index %v", path[0])
		}
		out := append([]any(nil), n...)
		v, err := applyAt(n[i], kind, path[1:], value, items)
		if err != nil {
			return nil, err
		}
		out[i] = v
		return out, nil
	}
	return nil, fmt.Errorf("live: path through %T", node)
}
