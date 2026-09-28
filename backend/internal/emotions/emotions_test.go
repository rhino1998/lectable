package emotions

import (
	"regexp"
	"testing"
)

var idRe = regexp.MustCompile(`^[a-z]+$`)

func TestAllEntriesComplete(t *testing.T) {
	if len(All) == 0 {
		t.Fatal("All is empty")
	}
	ids := map[string]bool{}
	labels := map[string]bool{}
	for _, e := range All {
		if !idRe.MatchString(e.ID) {
			t.Errorf("id %q: must be lowercase letters (it's a stored value and a file name)", e.ID)
		}
		if ids[e.ID] {
			t.Errorf("duplicate id %q", e.ID)
		}
		ids[e.ID] = true
		if labels[e.Label] {
			t.Errorf("duplicate label %q", e.Label)
		}
		labels[e.Label] = true
		if e.Label == "" || e.Description == "" || e.Instruction == "" || e.RefLine == "" {
			t.Errorf("%q: every field must be set", e.ID)
		}
	}
}

func TestGetAndValid(t *testing.T) {
	for _, e := range All {
		got, ok := Get(e.ID)
		if !ok || got.ID != e.ID {
			t.Errorf("Get(%q) = %v, %v", e.ID, got.ID, ok)
		}
		if !Valid(e.ID) {
			t.Errorf("Valid(%q) = false", e.ID)
		}
	}
	for _, id := range []string{Neutral, "neutral", "WARM", "bogus"} {
		if Valid(id) {
			t.Errorf("Valid(%q) = true, want false", id)
		}
		if _, ok := Get(id); ok {
			t.Errorf("Get(%q) ok, want !ok", id)
		}
	}
}
