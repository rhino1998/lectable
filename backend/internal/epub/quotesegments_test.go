package epub

import (
	"strings"
	"testing"
)

func TestSplitQuoteSegments(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []quoteSegment
	}{
		{
			name: "straight quotes",
			text: `"The bridge is out," Sam said.`,
			want: []quoteSegment{
				{Text: `"The bridge is out,"`, IsQuote: true},
				{Text: "Sam said."},
			},
		},
		{
			name: "curly quotes",
			text: "“The bridge is out,” Sam said.",
			want: []quoteSegment{
				{Text: "“The bridge is out,”", IsQuote: true},
				{Text: "Sam said."},
			},
		},
		{
			name: "guillemets",
			text: "«The bridge is out,» Sam said.",
			want: []quoteSegment{
				{Text: "«The bridge is out,»", IsQuote: true},
				{Text: "Sam said."},
			},
		},
		{
			name: "html-escaped angle brackets decode to literal < >",
			// golang.org/x/net/html decodes &lt;/&gt; entities into literal
			// '<'/'>' runes before this function ever sees the text - see
			// collectText - so this is what the source epub's own
			// `&lt;The bridge is out,&gt; Sam said.` actually looks like
			// by the time it reaches splitQuoteSegments.
			text: "<The bridge is out,> Sam said.",
			want: []quoteSegment{
				{Text: "<The bridge is out,>", IsQuote: true},
				{Text: "Sam said."},
			},
		},
		{
			name: "angle brackets mid-sentence stay narration when not dialogue-shaped",
			// <off> doesn't end on a dialogueTerminators rune, so the span
			// fails looksLikeDialogue and the whole sentence comes back
			// as one unsplit non-quote segment, quote marks and all - see
			// splitQuoteSegments' own doc comment.
			text: "It felt <off> to leave without saying goodbye.",
			want: []quoteSegment{
				{Text: "It felt <off> to leave without saying goodbye."},
			},
		},
		{
			name: "no quotes at all",
			text: "Just an ordinary sentence.",
			want: []quoteSegment{
				{Text: "Just an ordinary sentence."},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitQuoteSegments(tc.text)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d segments %+v, want %d %+v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("segment %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestSplitQuoteSegmentsWithEmphasis exercises the real pipeline order
// (collectText, wrapping bold/italic descendants in boldStart/boldEnd/
// italicStart/italicEnd PUA sentinels, runs BEFORE splitQuoteSegments -
// see extractChapter's own call site) with the new «»/<> quote styles
// wrapping emphasized text, confirming the sentinel pair - and the
// emphasis it marks - survives quote-span splitting intact: neither new
// quote-char set collides with the 0xE000-0xE003 sentinel range, and a
// quote span's own boundaries never fall inside a sentinel pair for this
// well-formed input, so extractEmphasis still finds exactly the bold/
// italic span it should on whichever segment ends up containing it.
func TestSplitQuoteSegmentsWithEmphasis(t *testing.T) {
	cases := []struct {
		name         string
		html         string
		wantQuote    string // quote segment's clean text, after extractEmphasis
		wantReplaced string
	}{
		{
			name:         "guillemets wrapping bold",
			html:         "«<b>Stop</b>,» he shouted.",
			wantQuote:    "«Stop,»",
			wantReplaced: "STOP",
		},
		{
			name: "html-escaped angle brackets wrapping italic",
			html: "&lt;<em>Stop</em>,&gt; he said.",
			// collectText decodes &lt;/&gt; into literal '<'/'>' runes
			// (see splitQuoteSegments' own test above) - indistinguishable
			// at this point from a source epub that used them directly.
			wantQuote:    "<Stop,>",
			wantReplaced: `"Stop"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := collectText(bodyNode(t, tc.html))
			segs := splitQuoteSegments(raw)

			// Mirror extractChapter's own call site: extractEmphasis runs
			// per final segment, whether or not it's a quote.
			var gotQuoteText string
			var gotQuoteSubs []string
			found := false
			for _, seg := range segs {
				clean, subs := extractEmphasis(seg.Text)

				// No PUA sentinel may ever leak into text the reader/TTS
				// worker actually sees, in ANY segment - a malformed
				// split could just as easily strand one in the
				// narration side as the quote side.
				if strings.ContainsAny(clean, string([]rune{boldStart, boldEnd, italicStart, italicEnd})) {
					t.Errorf("segment %+v leaked a raw emphasis sentinel into clean text %q", seg, clean)
				}

				if seg.IsQuote {
					found = true
					gotQuoteText = clean
					for _, s := range subs {
						gotQuoteSubs = append(gotQuoteSubs, s.Replacement)
					}
				}
			}
			if !found {
				t.Fatalf("no IsQuote segment found in %+v (raw = %q)", segs, raw)
			}
			if gotQuoteText != tc.wantQuote {
				t.Errorf("clean quote text = %q, want %q", gotQuoteText, tc.wantQuote)
			}
			if len(gotQuoteSubs) != 1 || gotQuoteSubs[0] != tc.wantReplaced {
				t.Errorf("quote segment substitutions = %v, want [%q]", gotQuoteSubs, tc.wantReplaced)
			}
		})
	}
}
