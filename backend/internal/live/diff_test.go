package live

import (
	"encoding/json"
	"reflect"
	"testing"
)

// applyOps is a Go port of the frontend's applyOps (src/api/live.ts), used
// to check Diff's output round-trips: apply(prev, Diff(prev, next)) == next.
func applyOps(t *testing.T, doc any, opsJSON []byte) any {
	t.Helper()
	var ops []struct {
		Op    string            `json:"op"`
		Path  []any             `json:"path"`
		Value json.RawMessage   `json:"value"`
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(opsJSON, &ops); err != nil {
		t.Fatalf("unmarshal ops: %v", err)
	}
	for _, op := range ops {
		parentPath, last := op.Path, any(nil)
		if len(op.Path) > 0 {
			parentPath, last = op.Path[:len(op.Path)-1], op.Path[len(op.Path)-1]
		}
		var newVal any
		switch op.Op {
		case "set":
			v, _ := decode(op.Value)
			newVal = v
		case "arr":
			prev := get(doc, op.Path).([]any)
			out := []any{}
			for _, it := range op.Items {
				var pair [2]int
				if json.Unmarshal(it, &pair) == nil {
					out = append(out, prev[pair[0]:pair[1]]...)
					continue
				}
				var lit struct{ V json.RawMessage }
				if err := json.Unmarshal(it, &lit); err != nil {
					t.Fatalf("bad item %s", it)
				}
				v, _ := decode(lit.V)
				out = append(out, v)
			}
			newVal = out
		}
		if last == nil {
			doc = newVal
			continue
		}
		parent := get(doc, parentPath)
		switch p := parent.(type) {
		case map[string]any:
			if op.Op == "del" {
				delete(p, last.(string))
			} else {
				p[last.(string)] = newVal
			}
		case []any:
			p[int(last.(float64))] = newVal
		}
	}
	return doc
}

func get(doc any, path []any) any {
	for _, k := range path {
		switch d := doc.(type) {
		case map[string]any:
			doc = d[k.(string)]
		case []any:
			doc = d[int(k.(float64))]
		}
	}
	return doc
}

func mustDecode(t *testing.T, s string) any {
	t.Helper()
	v, err := decode([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDiffRoundTrip(t *testing.T) {
	cases := []struct {
		name       string
		prev, next string
		maxOps     int
	}{
		{"equal", `{"a":1}`, `{"a":1}`, 0},
		{"scalar field", `{"a":1,"b":"x"}`, `{"a":2,"b":"x"}`, 1},
		{"add and remove key", `{"a":1,"b":2}`, `{"a":1,"c":3}`, 2},
		{"root type change", `[1,2]`, `{"a":1}`, 1},
		{"null and false values", `{"a":1,"b":true}`, `{"a":null,"b":false}`, 2},
		{"nested paragraph status", `{"paragraphs":[{"idx":0,"s":"pending"},{"idx":1,"s":"pending"}]}`,
			`{"paragraphs":[{"idx":0,"s":"ready","url":"/a"},{"idx":1,"s":"pending"}]}`, 2},
		{"queue shift keyed", `{"q":[{"id":"a"},{"id":"b"},{"id":"c"}]}`, `{"q":[{"id":"b"},{"id":"c"},{"id":"d"}]}`, 1},
		{"keyed modify in place", `[{"id":"a","n":1},{"id":"b","n":1}]`, `[{"id":"a","n":1},{"id":"b","n":2}]`, 1},
		{"keyed reorder", `[{"id":"a"},{"id":"b"},{"id":"c"}]`, `[{"id":"c"},{"id":"a"},{"id":"b"}]`, 1},
		{"unkeyed append", `[1,2,3]`, `[1,2,3,4]`, 1},
		{"unkeyed middle insert", `[1,2,3]`, `[1,9,2,3]`, 1},
		{"unkeyed remove", `["a","b","c"]`, `["a","c"]`, 1},
		{"to empty", `[1,2]`, `[]`, 1},
		{"from empty", `[]`, `[{"id":"x"}]`, 1},
		{"duplicate ids fall back", `[{"id":"a"},{"id":"a"}]`, `[{"id":"a"}]`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prev, next := mustDecode(t, tc.prev), mustDecode(t, tc.next)
			ops := Diff(prev, next)
			if len(ops) > tc.maxOps {
				t.Errorf("got %d ops, want <= %d: %+v", len(ops), tc.maxOps, ops)
			}
			opsJSON, err := json.Marshal(ops)
			if err != nil {
				t.Fatal(err)
			}
			// Apply onto a fresh copy of prev using plain float64 numbers in
			// paths, as a JS client would see them.
			got := applyOps(t, mustDecode(t, tc.prev), opsJSON)
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(next)
			var g, w any
			json.Unmarshal(gotJSON, &g)
			json.Unmarshal(wantJSON, &w)
			if !reflect.DeepEqual(g, w) {
				t.Errorf("round trip mismatch\n got  %s\n want %s\n ops  %s", gotJSON, wantJSON, opsJSON)
			}
		})
	}
}

func TestDiffQueueShiftIsCompact(t *testing.T) {
	var prev, next []any
	for i := 0; i < 1000; i++ {
		prev = append(prev, map[string]any{"id": string(rune('a'+i%26)) + string(rune(i)), "n": json.Number("1")})
	}
	next = append(next, prev[1:]...)
	next = append(next, map[string]any{"id": "new", "n": json.Number("1")})
	ops, _ := json.Marshal(Diff(prev, next))
	if len(ops) > 200 {
		t.Errorf("queue shift patch is %d bytes, want a couple of index runs: %s", len(ops), ops)
	}
}
