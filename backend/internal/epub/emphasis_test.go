package epub

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// bodyNode parses a small HTML fragment and returns its <body> node, for
// exercising collectText the same way extractChapter's own DOM walk does.
func bodyNode(t *testing.T, fragment string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader("<html><body>" + fragment + "</body></html>"))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	var body *html.Node
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "body" {
			body = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if body == nil {
		t.Fatal("no <body> found")
	}
	return body
}

func TestCollectTextAndExtractEmphasis(t *testing.T) {
	cases := []struct {
		name         string
		html         string
		wantText     string
		wantReplaced []string // Replacement strings, in order
	}{
		{
			name:         "bold only",
			html:         "I <b>never</b> said that.",
			wantText:     "I never said that.",
			wantReplaced: []string{"NEVER"},
		},
		{
			name:         "italic only",
			html:         "I <em>never</em> said that.",
			wantText:     "I never said that.",
			wantReplaced: []string{`"never"`},
		},
		{
			name:         "strong and i tags too",
			html:         "<strong>Stop</strong> right there, <i>please</i>.",
			wantText:     "Stop right there, please.",
			wantReplaced: []string{"STOP", `"please"`},
		},
		{
			name:         "nested bold and italic - both treatments merge",
			html:         "It was <strong><em>never</em></strong> going to work.",
			wantText:     "It was never going to work.",
			wantReplaced: []string{`"NEVER"`},
		},
		{
			name:         "nested same category collapses to one span",
			html:         "<b>very <b>truly</b> important</b> news.",
			wantText:     "very truly important news.",
			wantReplaced: []string{"VERY TRULY IMPORTANT"},
		},
		{
			name:         "no emphasis at all",
			html:         "Just an ordinary sentence.",
			wantText:     "Just an ordinary sentence.",
			wantReplaced: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := collectText(bodyNode(t, tc.html))
			clean, subs := extractEmphasis(raw)
			if clean != tc.wantText {
				t.Errorf("clean text = %q, want %q", clean, tc.wantText)
			}
			if len(subs) != len(tc.wantReplaced) {
				t.Fatalf("got %d substitutions %+v, want %d (%v)", len(subs), subs, len(tc.wantReplaced), tc.wantReplaced)
			}
			for i, sub := range subs {
				if sub.Replacement != tc.wantReplaced[i] {
					t.Errorf("substitution %d replacement = %q, want %q", i, sub.Replacement, tc.wantReplaced[i])
				}
				// Offset/Length must correctly index into clean.
				if got := clean[sub.Offset : sub.Offset+sub.Length]; got != strings.Trim(tc.wantReplaced[i], `"`) && !strings.EqualFold(got, strings.Trim(tc.wantReplaced[i], `"`)) {
					t.Errorf("substitution %d spans %q in clean text, want it to cover the original (case-insensitive) of %q", i, got, tc.wantReplaced[i])
				}
			}
		})
	}
}

// TestExtractEmphasisNoOrphanSentinels guards against a malformed/unpaired
// sentinel (should never happen in practice - collectText always writes
// matched pairs) leaking a raw Private Use Area character into the
// cleaned text.
func TestExtractEmphasisNoOrphanSentinels(t *testing.T) {
	broken := string(boldStart) + "oops"
	clean, subs := extractEmphasis(broken)
	if clean != "oops" {
		t.Errorf("clean = %q, want %q (sentinel stripped, no crash)", clean, "oops")
	}
	if len(subs) != 0 {
		t.Errorf("expected no substitutions for an unpaired sentinel, got %+v", subs)
	}
}
