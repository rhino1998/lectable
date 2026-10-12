package speakerattr

import "testing"

func TestCaseFoldPoolMergesCaseOnlyVariants(t *testing.T) {
	got := caseFoldPool([]string{"The attendant", "the attendant"})
	if got["The attendant"] != "The attendant" || got["the attendant"] != "The attendant" {
		t.Fatalf("caseFoldPool() = %v, want both variants folded to the capitalized spelling", got)
	}
}

func TestCaseFoldPoolPrefersCapitalizedSpelling(t *testing.T) {
	got := caseFoldPool([]string{"guard", "Guard"})
	if got["guard"] != "Guard" || got["Guard"] != "Guard" {
		t.Fatalf("caseFoldPool() = %v, want both variants folded to \"Guard\"", got)
	}
}

func TestCaseFoldPoolLeavesDistinctNamesAlone(t *testing.T) {
	got := caseFoldPool([]string{"Toby", "Jane"})
	if got["Toby"] != "Toby" || got["Jane"] != "Jane" {
		t.Fatalf("caseFoldPool() = %v, want distinct names left untouched", got)
	}
}

// TestCanonMapFromPoolComposesCaseFoldWithWholeNameMerge confirms the two
// merge passes compose correctly: a case-only pair ("the attendant"/"The
// attendant") folds together the same run a whole-name-containment pair
// ("vesna"/"Captain Vesna Thorne") still does, in one pool.
func TestCanonMapFromPoolComposesCaseFoldWithWholeNameMerge(t *testing.T) {
	pool := map[string]bool{
		"vesna":                true,
		"Captain Vesna Thorne": true,
		"the attendant":        true,
		"The attendant":        true,
	}
	got := canonMapFromPool(pool, nil)
	if got["vesna"] != "Captain Vesna Thorne" || got["Captain Vesna Thorne"] != "Captain Vesna Thorne" {
		t.Fatalf("canonMapFromPool() = %v, want vesna/Captain Vesna Thorne merged", got)
	}
	if got["the attendant"] != "The attendant" || got["The attendant"] != "The attendant" {
		t.Fatalf("canonMapFromPool() = %v, want the attendant/The attendant merged to the capitalized spelling", got)
	}
}

// TestCanonicalizeSpeakerNamesIsCaseInsensitive is canonicalizeSpeakerNames'
// own end-to-end version of the case-fold behavior: two lines attributed
// with different capitalizations of the same role collapse to one speaker.
func TestCanonicalizeSpeakerNamesIsCaseInsensitive(t *testing.T) {
	raw := map[int]string{
		0: "The attendant",
		1: "the attendant",
	}
	got := canonicalizeSpeakerNames(nil, raw, nil)
	if got[0] != got[1] {
		t.Fatalf("canonicalizeSpeakerNames() = %v, want both lines attributed to the same speaker", got)
	}
}

func TestCanonicalizeSpeakerNamesIgnoresApostropheStyle(t *testing.T) {
	got := canonicalizeSpeakerNames([]string{"Ve’delina"}, map[int]string{0: "Ve'delina", 1: "Ve’delina", 2: "Olesm"}, nil)
	if got[0] != got[1] {
		t.Fatalf("apostrophe variants kept apart: %q, %q", got[0], got[1])
	}
	if got[2] != "Olesm" {
		t.Fatalf("unrelated name changed: %q", got[2])
	}
	if !containsWholeName("Wall Lord Ve’delina", "Ve'delina") {
		t.Fatal("containsWholeName should ignore the apostrophe's style")
	}
}

func TestSpeakerNameResolverIgnoresApostropheStyle(t *testing.T) {
	r := NewSpeakerNameResolver([]string{"Ve’delina"}, map[string][]string{"Ve’delina": {"The Wall Lord's daughter"}})
	for _, written := range []string{"Ve'delina", "ve‘delina", "Ve'delina's", "Ve’delina’s", "the Wall Lord’s daughter"} {
		if got, ok := r.Resolve(written); !ok || got != "Ve’delina" {
			t.Errorf("Resolve(%q) = %q, %v", written, got, ok)
		}
	}
	if got := r.Canonical("Ve'delina"); got != "Ve’delina" {
		t.Errorf("Canonical = %q", got)
	}
}
