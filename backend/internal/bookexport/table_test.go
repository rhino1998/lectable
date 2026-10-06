package bookexport

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// tableRow is a paragraph whose text is cells joined with " | ", each
// placed at (col, span).
func tableRow(id string, row int, text string, at ...[2]int) *Paragraph {
	p := &Paragraph{ID: id, Text: text, Table: store.TablePos{Table: 1, Row: row}}
	start := 0
	for i, cell := range strings.Split(text, " | ") {
		col, span := i, 1
		if i < len(at) {
			col, span = at[i][0], at[i][1]
		}
		p.Table.Cells = append(p.Table.Cells, store.TableCell{Start: start, End: start + len(cell), Col: col, ColSpan: span, RowSpan: 1})
		start += len(cell) + len(" | ")
	}
	return p
}

func TestEPUBTable(t *testing.T) {
	head := tableRow("h", 0, "Name | Mana")
	head.Table.Cells[0].Header, head.Table.Cells[1].Header = true, true
	spell := tableRow("s", 1, "Summon Water | 10 mana")
	spell.Audio = &Segment{Begin: 0, End: 2, Words: []Word{{Start: 0}, {Start: 0.5}, {Start: 1}, {Start: 1.5}}}
	details := tableRow("d", 2, "Details:", [2]int{0, 1})
	first := tableRow("a", 2, "Heals <twice>.", [2]int{1, 1})
	quote := tableRow("q", 2, "“Daily,”", [2]int{1, 1})
	quote.Inline = true
	second := tableRow("b", 2, "Needs wit.", [2]int{1, 1})

	ew := &epubWriter{opts: Options{WordLevel: true}}
	var body strings.Builder
	var narrated []string
	err := ew.table(&body, []*Paragraph{head, spell, details, first, quote, second}, func(p *Paragraph, words []wordSpan) error {
		narrated = append(narrated, p.ID)
		if p.ID == "s" && len(words) != 4 {
			t.Errorf("row words = %d, want 4 (the separator isn't one)", len(words))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := body.String()
	want := `<table class="table">
<tr id="p-h"><th>Name</th><th>Mana</th></tr>
<tr id="p-s"><td><span id="w-s-0">Summon</span> <span id="w-s-1">Water</span></td><td><span id="w-s-2">10</span> <span id="w-s-3">mana</span></td></tr>
<tr><td><p><span id="p-d">Details:</span></p></td><td><p><span id="p-a">Heals &lt;twice&gt;.</span> <span id="p-q">“Daily,”</span></p><p><span id="p-b">Needs wit.</span></p></td></tr>
</table>
`
	if got != want {
		t.Errorf("table:\n got  %s\n want %s", got, want)
	}
	if strings.Join(narrated, "") != "hsdaqb" {
		t.Errorf("narrated %q", narrated)
	}
	dec := xml.NewDecoder(strings.NewReader(got))
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("not well-formed: %v", err)
		}
	}
}
