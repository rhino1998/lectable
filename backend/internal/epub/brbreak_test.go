package epub

import (
	"reflect"
	"testing"
)

func TestExtractChapterDoubleBrSplitsParagraphs(t *testing.T) {
	raw := []byte(`<html><body><h1>Chapter 57</h1><p>He was woken by a small shake.<br /><br />“Hello,” Fritz whispered politely.<br/> <br/>“Hello,” <em>she</em> replied softly.<br /><br />* * *<br /><br />One line<br />still the same paragraph.</p></body></html>`)
	_, _, blocks, _ := extractChapter(raw, nil, "", nil)

	type got struct {
		Kind   BlockKind
		Text   string
		Inline bool
		Quote  bool
	}
	var out []got
	for _, b := range blocks {
		out = append(out, got{b.Kind, b.Text, b.Inline, b.IsQuote})
	}
	want := []got{
		{BlockText, "Chapter 57", false, false},
		{BlockText, "He was woken by a small shake.", false, false},
		{BlockText, "“Hello,”", false, true},
		{BlockText, "Fritz whispered politely.", true, false},
		{BlockText, "“Hello,”", false, true},
		{BlockText, "she replied softly.", true, false},
		{BlockBreak, "", false, false},
		{BlockText, "One line still the same paragraph.", false, false},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("blocks mismatch:\n got  %+v\n want %+v", out, want)
	}
	// Emphasis still lands on the right piece.
	if len(blocks[5].Emphasis) != 1 {
		t.Errorf("want one emphasis span on %q, got %+v", blocks[5].Text, blocks[5].Emphasis)
	}
}
