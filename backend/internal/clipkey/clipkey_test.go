package clipkey

import (
	"reflect"
	"testing"
)

// takes runs TakeAt over a chapter given as one line id per paragraph
// ("" for a paragraph that is never the same line as anything).
func takes(chapter []string) []int {
	out := make([]int, len(chapter))
	for i := range chapter {
		out[i] = TakeAt(i, func(a, b int) bool { return chapter[a] != "" && chapter[a] == chapter[b] })
	}
	return out
}

// spaced is a chapter with line id at each of idxs and nothing between.
func spaced(id string, idxs ...int) []string {
	chapter := make([]string, idxs[len(idxs)-1]+1)
	for _, i := range idxs {
		chapter[i] = id
	}
	return chapter
}

func pick(all []int, idxs ...int) []int {
	out := make([]int, len(idxs))
	for i, idx := range idxs {
		out[i] = all[idx]
	}
	return out
}

func TestTakeAt(t *testing.T) {
	tests := []struct {
		name    string
		chapter []string
		want    []int
	}{
		{"no repeats", []string{"a", "b", "c"}, []int{0, 0, 0}},
		{"adjacent run", []string{"a", "a", "a"}, []int{0, 1, 2}},
		{"interleaved", []string{"a", "b", "a", "b"}, []int{0, 0, 1, 1}},
		{"unkeyed paragraphs never match", []string{"", "", ""}, []int{0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := takes(tt.chapter); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("takes = %v, want %v", got, tt.want)
			}
		})
	}

	spacedTests := []struct {
		name string
		idxs []int
		want []int
	}{
		{"at the window", []int{0, RunWindow}, []int{0, 1}},
		{"past the window", []int{0, RunWindow + 1}, []int{0, 0}},
		{"chain longer than the window", []int{0, 4, 8, 12}, []int{0, 1, 2, 3}},
		{"gap restarts the count", []int{0, 1, 20, 22}, []int{0, 1, 0, 1}},
	}
	for _, tt := range spacedTests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pick(takes(spaced("a", tt.idxs...)), tt.idxs...); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("takes at %v = %v, want %v", tt.idxs, got, tt.want)
			}
		})
	}
}

// An edit before a run must not change the takes inside it.
func TestTakeAtLocal(t *testing.T) {
	chapter := spaced("a", 40, 42, 45)
	alone := pick(takes(chapter), 40, 42, 45)
	chapter[0], chapter[1], chapter[2] = "a", "x", "a"
	if after := pick(takes(chapter), 40, 42, 45); !reflect.DeepEqual(after, alone) {
		t.Errorf("takes after earlier lines = %v, want %v", after, alone)
	}
}

func TestKey(t *testing.T) {
	base := Recipe{
		CloneModel: "higgs", Reference: "preset/angry", Language: "English",
		Text: "Fritz said.", AlignText: "Fritz said.",
	}
	if got, again := base.Key(0), base.Key(0); got != again || len(got) != 32 {
		t.Fatalf("Key(0) = %q then %q, want one stable 32-character key", got, again)
	}
	seen := map[string]string{base.Key(0): "base", base.Key(1): "take 1"}
	if len(seen) != 2 {
		t.Fatal("Key(0) and Key(1) collide")
	}
	variants := map[string]Recipe{}
	for name, edit := range map[string]func(*Recipe){
		"clone model":    func(r *Recipe) { r.CloneModel = "pocket" },
		"reference":      func(r *Recipe) { r.Reference = "preset/sad" },
		"language":       func(r *Recipe) { r.Language = "German" },
		"clone instruct": func(r *Recipe) { r.CloneInstruct = "gruff" },
		"text":           func(r *Recipe) { r.Text = "Fritz sed." },
		"align text":     func(r *Recipe) { r.AlignText = "Fritz  said." },
	} {
		r := base
		edit(&r)
		variants[name] = r
	}
	for name, r := range variants {
		if prev, dup := seen[r.Key(0)]; dup {
			t.Errorf("changing %s gives the same key as %s", name, prev)
		}
		seen[r.Key(0)] = name
	}
}

// Fields are length-prefixed, so text can't slide between them.
func TestKeyFieldBoundaries(t *testing.T) {
	a := Recipe{Language: "ab", CloneInstruct: "c"}
	b := Recipe{Language: "a", CloneInstruct: "bc"}
	if a.Key(0) == b.Key(0) {
		t.Error("recipes differing only in where a field ends share a key")
	}
}
