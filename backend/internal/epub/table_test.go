package epub

import (
	"reflect"
	"testing"
)

// tableRowsOf is each text block as "text" plus, for a table paragraph,
// its position.
type tableRowOf struct {
	Text  string
	Table *TablePos
}

func textBlocks(t *testing.T, doc string) []tableRowOf {
	t.Helper()
	_, _, blocks, _ := extractChapter([]byte(doc), nil, "", nil)
	var out []tableRowOf
	for _, b := range blocks {
		if b.Kind == BlockText {
			out = append(out, tableRowOf{b.Text, b.Table})
		}
	}
	return out
}

func texts(rows []tableRowOf) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Text
	}
	return out
}

func TestTableRowsAreParagraphs(t *testing.T) {
	got := textBlocks(t, `<html><head><title>Ignored</title></head><body><p>Before.</p>
<table><tbody>
<tr><td>Name</td><td>School</td><td>Mana</td></tr>
<tr><td>Summon Water</td><td>Water</td><td>10 mana</td></tr>
<tr><td>Icebolt</td><td></td><td><b>10</b> mana</td></tr>
</tbody></table><p>After.</p></body></html>`)

	want := []string{"Before.", "Name | School | Mana", "Summon Water | Water | 10 mana", "Icebolt | 10 mana", "After."}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("texts:\n got  %q\n want %q", texts(got), want)
	}
	if got[0].Table != nil || got[4].Table != nil {
		t.Errorf("paragraphs around the table are placed in it")
	}
	head := got[1].Table
	if head == nil || !head.Heading || head.Row != 0 || len(head.Cells) != 3 || !head.Cells[0].Header {
		t.Fatalf("heading row: %+v", head)
	}
	wantCells := []TableCell{
		{Start: 0, End: 7, Col: 0, ColSpan: 1, RowSpan: 1, Label: "Name"},
		{Start: 10, End: 17, Col: 2, ColSpan: 1, RowSpan: 1, Label: "Mana"},
	}
	if row := got[3].Table; row.Heading || row.Row != 2 || !reflect.DeepEqual(row.Cells, wantCells) {
		t.Errorf("row with an empty cell:\n got  %+v\n want %+v", row.Cells, wantCells)
	}
}

func TestTableEmphasisOffsetsFollowTheCell(t *testing.T) {
	_, _, blocks, _ := extractChapter([]byte(`<table><tr><td>Level</td><td><b>Nine</b></td></tr></table>`), nil, "", nil)
	if len(blocks) != 1 || blocks[0].Text != "Level | Nine" {
		t.Fatalf("blocks: %+v", blocks)
	}
	em := blocks[0].Emphasis
	if len(em) != 1 || em[0].Offset != 8 || em[0].Length != 4 || em[0].Replacement != "NINE" {
		t.Errorf("emphasis: %+v", em)
	}
}

func TestTableKeyRowHeadsTheRowUnderIt(t *testing.T) {
	got := textBlocks(t, `<table>
<tr><td colspan="2">Ring of Healing</td><td colspan="2">Further Description</td></tr>
<tr><td colspan="2">Details:</td><td colspan="2"><p>Heals twice per day.</p><p>Needs 11 Intelligence.</p></td></tr>
<tr><td>Rarity:</td><td>Magical:</td><td>Durability:</td><td>Charge:</td></tr>
<tr><td>Rare</td><td>Yes</td><td>72/100</td><td>2/s</td></tr>
<tr><td colspan="4">Bound to its wearer.</td></tr>
</table>`)

	want := []string{
		"Ring of Healing | Further Description", "Details:", "Heals twice per day.", "Needs 11 Intelligence.",
		"Rarity: | Magical: | Durability: | Charge:", "Rare | Yes | 72/100 | 2/s", "Bound to its wearer.",
	}
	if !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("texts:\n got  %q\n want %q", texts(got), want)
	}
	for i, r := range got {
		if r.Table == nil || r.Table.Table != 0 {
			t.Fatalf("%q isn't in the table: %+v", r.Text, r.Table)
		}
		if r.Table.Heading != (i == 4) {
			t.Errorf("%q: heading = %v", r.Text, r.Table.Heading)
		}
	}
	// A cell's own paragraphs each fill that cell.
	if c := got[3].Table; c.Row != 1 || !reflect.DeepEqual(c.Cells, []TableCell{{Start: 0, End: 22, Col: 2, ColSpan: 2, RowSpan: 1}}) {
		t.Errorf("paragraph in a cell: %+v", c)
	}
	var labels []string
	for _, c := range got[5].Table.Cells {
		labels = append(labels, c.Label)
	}
	if want := []string{"Rarity", "Magical", "Durability", "Charge"}; !reflect.DeepEqual(labels, want) {
		t.Errorf("labels = %q, want %q", labels, want)
	}
	if got[6].Table.Cells[0].Label != "" || got[0].Table.Cells[0].Label != "" {
		t.Errorf("a row laid out differently took the key row's labels")
	}
}

func TestTableHeadings(t *testing.T) {
	cases := []struct {
		name, doc string
		heading   []bool
	}{
		{"th row", `<table><tr><th>Name</th><th>Age</th></tr><tr><td>Sam</td><td>30</td></tr></table>`, []bool{true, false}},
		{"thead", `<table><thead><tr><td>Name</td><td>Age</td></tr></thead><tbody><tr><td>Sam</td><td>30</td></tr></tbody></table>`, []bool{true, false}},
		{"two columns of pairs", `<table><tr><td>Name</td><td>Jack</td></tr><tr><td>Class</td><td>None</td></tr></table>`, []bool{false, false}},
		{"numbers in the first row", `<table><tr><td>Strength</td><td>10</td><td>+2</td></tr><tr><td>Agility</td><td>12</td><td>+3</td></tr></table>`, []bool{false, false}},
		{"rowspan under the headings", `<table><tr><td>Skill</td><td>Sub-Skill</td><td>Level</td></tr><tr><td rowspan="2">Ranged</td><td>Archery</td><td>1</td></tr><tr><td>Knives</td><td>3</td></tr></table>`, []bool{true, false, false}},
		{"headings where the layout changes", `<table><tr><td colspan="3">Name: Jack</td></tr><tr><td>Stat</td><td>Points</td><td>Effect</td></tr><tr><td>Agility</td><td>12</td><td>Dodge</td></tr><tr><td>Luck</td><td>3</td><td>Loot</td></tr></table>`, []bool{false, true, false, false}},
		// A known limit: nothing tells these from headings.
		{"data rows under a title row", `<table><tr><td colspan="3">Blades</td></tr><tr><td>Sword</td><td>Rare</td><td>Steel</td></tr><tr><td>Axe</td><td>Common</td><td>Iron</td></tr><tr><td>Bow</td><td>Rare</td><td>Yew</td></tr></table>`, []bool{false, true, false, false}},
		{"headings with nothing under them", `<table><tr><th>Name</th><th>Age</th></tr></table>`, []bool{false}},
	}
	for _, c := range cases {
		got := textBlocks(t, c.doc)
		if len(got) != len(c.heading) {
			t.Errorf("%s: %d rows, want %d: %q", c.name, len(got), len(c.heading), texts(got))
			continue
		}
		for i, r := range got {
			if r.Table == nil || r.Table.Heading != c.heading[i] {
				t.Errorf("%s: row %q heading mismatch: %+v", c.name, r.Text, r.Table)
			}
		}
	}

	// A rowspan pushes the cells of the rows it covers to the right.
	got := textBlocks(t, cases[4].doc)
	if cells := got[2].Table.Cells; cells[0].Col != 1 || cells[0].Label != "Sub-Skill" || cells[1].Label != "Level" {
		t.Errorf("cells under a rowspan: %+v", cells)
	}
}

func TestSingleCellTableIsLayout(t *testing.T) {
	got := textBlocks(t, `<table><tr><td><p>One.</p><p>“Two,” she said.</p></td></tr></table>`)
	if want := []string{"One.", "“Two,”", "she said."}; !reflect.DeepEqual(texts(got), want) {
		t.Fatalf("texts = %q, want %q", texts(got), want)
	}
	for _, r := range got {
		if r.Table != nil {
			t.Errorf("%q is placed in a table", r.Text)
		}
	}
}

func TestTextTheWalkUsedToDrop(t *testing.T) {
	got := textBlocks(t, `<html><head><title>Not content</title><style>p{}</style></head><body>
<table><caption>Spells known</caption><tr><td>Icebolt</td><td>10 mana</td></tr></table>
<dl><dt>Term</dt><dd>Definition</dd></dl>
<div>Loose <i>text</i> <p>inner para</p> trailing</div>
<ul> <li>item</li> </ul>
</body></html>`)
	want := []string{"Spells known", "Icebolt | 10 mana", "Term", "Definition", "Loose text", "inner para", "trailing", "item"}
	if !reflect.DeepEqual(texts(got), want) {
		t.Errorf("texts:\n got  %q\n want %q", texts(got), want)
	}
}
