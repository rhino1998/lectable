package store

import (
	"reflect"
	"testing"
)

func TestResolveGenerationTextReadsATableRow(t *testing.T) {
	cells := func(text string, labels ...string) []TableCell {
		var out []TableCell
		start := 0
		for i := 0; ; i++ {
			end := len(text)
			if j := indexFrom(text, " | ", start); j >= 0 {
				end = j
			}
			c := TableCell{Start: start, End: end, Col: i, ColSpan: 1, RowSpan: 1}
			if i < len(labels) {
				c.Label = labels[i]
			}
			out = append(out, c)
			if end == len(text) {
				return out
			}
			start = end + len(" | ")
		}
	}
	cases := []struct {
		name, text string
		labels     []string
		want       string
	}{
		{"cells become sentences", "Class: None | Renown: Unknown", nil, "Class: None. Renown: Unknown"},
		{"headings introduce cells", "Rare | Yes | 72/100", []string{"Rarity", "Magical", "Durability"}, "Rarity: Rare. Magical: Yes. Durability: 72/100"},
		{"a cell already ending a sentence", "Details: | Heals twice. | Rare", nil, "Details: Heals twice. Rare"},
		{"a column with no heading", "Icebolt | 10 mana", []string{"Name"}, "Name: Icebolt. 10 mana"},
	}
	for _, c := range cases {
		p := Paragraph{Text: c.text, Table: TablePos{Cells: cells(c.text, c.labels...)}}
		if got := p.ResolveGenerationText("", nil); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	// A heading goes outside the emphasis respelling of the cell it introduces.
	p := Paragraph{
		Text:     "Rare | Yes",
		Table:    TablePos{Cells: cells("Rare | Yes", "Rarity", "Magical")},
		Emphasis: PronunciationList{{Offset: 0, Length: 4, Replacement: "RARE"}},
	}
	if got, want := p.ResolveGenerationText("", nil), "Rarity: RARE. Magical: Yes"; got != want {
		t.Errorf("with emphasis: got %q, want %q", got, want)
	}

	// Cells that don't fit the text are ignored rather than applied.
	stale := Paragraph{Text: "short", Table: TablePos{Cells: []TableCell{{Start: 0, End: 40, Label: "Name"}}}}
	if got := stale.ResolveGenerationText("", nil); got != "short" {
		t.Errorf("stale cells: got %q", got)
	}
}

func indexFrom(s, sep string, from int) int {
	for i := from; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}

func TestHeadingRowIsUnvoiced(t *testing.T) {
	p := Paragraph{Text: "Name | Mana", Table: TablePos{Heading: true, Cells: []TableCell{{Start: 0, End: 4}, {Start: 7, End: 11}}}}
	if !p.Unvoiced() {
		t.Error("a heading row should be unvoiced")
	}
	p.Table.Heading = false
	if p.Unvoiced() {
		t.Error("an ordinary row should be voiced")
	}
}

func TestTablePosRoundTrips(t *testing.T) {
	s := openTestStore(t)
	want := TablePos{Table: 2, Row: 1, Cells: []TableCell{
		{Start: 0, End: 4, Col: 0, ColSpan: 1, RowSpan: 1, Label: "Rarity"},
		{Start: 7, End: 10, Col: 1, ColSpan: 2, RowSpan: 1, Header: true},
	}}
	bookID, _, err := s.CreateBook("Book", "Author", "en", "", "", 0, []ChapterInput{{Title: "Ch", Blocks: []BlockInput{
		{Kind: BlockText, Text: "Plain."},
		{Kind: BlockText, Text: "Rare | Yes", Table: want},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil || ch == nil {
		t.Fatalf("GetChapter: %v", err)
	}
	ps, err := s.ListParagraphsRaw(ch.ID)
	if err != nil || len(ps) != 2 {
		t.Fatalf("ListParagraphsRaw: %v, %d rows", err, len(ps))
	}
	if ps[0].Table.InTable() {
		t.Errorf("plain paragraph is in a table: %+v", ps[0].Table)
	}
	if !reflect.DeepEqual(ps[1].Table, want) {
		t.Errorf("table position:\n got  %+v\n want %+v", ps[1].Table, want)
	}
	got, err := s.GetParagraph(ps[1].ID)
	if err != nil || !reflect.DeepEqual(got.Table, want) {
		t.Errorf("GetParagraph table position: %+v (%v)", got, err)
	}
}
