package epub

import "testing"

func TestIsSceneBreakText(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"asterisks with spaces", "* * *", true},
		{"bare asterisks", "***", true},
		{"asterism", "⁂", true},
		{"dash rule", "-=-=-=-=-", true},
		{"bullets", "• • •", true},
		{"single dash", "-", true},
		{"too many glyphs", "*************", false}, // > maxSceneBreakGlyphs
		{"empty", "", false},
		{"ordinary short narration", "No.", false},
		{"ordinary short narration 2", "Wait.", false},
		{"number", "42", false},
		{"mixed glyph and letter", "* Chapter *", false},
		{"em dash sentence", "— he said.", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isSceneBreakText(c.text); got != c.want {
				t.Errorf("isSceneBreakText(%q) = %v, want %v", c.text, got, c.want)
			}
		})
	}
}

func TestExtractChapterSceneBreaks(t *testing.T) {
	raw := []byte(`<html><body>
		<p>The door closed behind her.</p>
		<hr/>
		<p>* * *</p>
		<p>Morning came too soon.</p>
	</body></html>`)

	_, _, blocks, _ := extractChapter(raw, nil, "", nil)

	var kinds []BlockKind
	for _, b := range blocks {
		kinds = append(kinds, b.Kind)
	}
	want := []BlockKind{BlockText, BlockBreak, BlockBreak, BlockText}
	if len(kinds) != len(want) {
		t.Fatalf("got %v blocks, want kinds %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("block %d kind = %q, want %q", i, kinds[i], want[i])
		}
	}
}
