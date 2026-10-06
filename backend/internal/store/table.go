package store

import (
	"database/sql/driver"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/deliverytags"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
)

// TablePos places a paragraph in a source <table> - see
// internal/epub.TablePos, which this mirrors. The zero value (no Cells) is
// a paragraph outside any table. Stored as JSON in paragraphs.table_pos.
type TablePos struct {
	Table   int         `json:"table"`
	Row     int         `json:"row"`
	Cells   []TableCell `json:"cells,omitempty"`
	Heading bool        `json:"heading,omitempty"`
}

// TableCell is one cell's span of its paragraph's Text: byte offsets
// [Start, End), the grid column it starts in and how far it spans.
type TableCell struct {
	Start   int    `json:"start"`
	End     int    `json:"end"`
	Col     int    `json:"col"`
	ColSpan int    `json:"colSpan"`
	RowSpan int    `json:"rowSpan"`
	Header  bool   `json:"header,omitempty"`
	Label   string `json:"label,omitempty"`
}

func (t *TablePos) Scan(v any) error { return scanJSON(v, t) }
func (t TablePos) Value() (driver.Value, error) {
	if !t.InTable() {
		return "{}", nil
	}
	return jsonValue(t)
}

// InTable reports whether the paragraph sits in a table at all.
func (t TablePos) InTable() bool { return len(t.Cells) > 0 }

// spoken is how a table row is read aloud, as edits to its text: each
// separator between cells becomes a sentence break, and a cell under a
// heading row is introduced by its column's heading ("Rarity: Rare.
// Durability: 72/100."). The headings are extra words the text doesn't
// have - forced alignment still runs on the plain text, which stretches
// the neighbouring word over them, and the completeness check compares
// against the generation text too (jobs.extraWordsIn). Nothing for a
// paragraph outside a table, or cells that don't fit text.
func (t TablePos) spoken(text string) ([]deliverytags.Insertion, []pronounce.Substitution) {
	var insertions []deliverytags.Insertion
	var subs []pronounce.Substitution
	prevEnd := 0
	for i, c := range t.Cells {
		if c.Start < prevEnd || c.End < c.Start || c.End > len(text) {
			return nil, nil
		}
		if i > 0 {
			sep := ". "
			if strings.ContainsAny(text[max(prevEnd-1, 0):prevEnd], ".!?:;,…") {
				sep = " "
			}
			subs = append(subs, pronounce.Substitution{Offset: prevEnd, Length: c.Start - prevEnd, Replacement: sep})
		}
		if c.Label != "" && c.End > c.Start {
			insertions = append(insertions, deliverytags.Insertion{Offset: c.Start, Tag: c.Label + ": "})
		}
		prevEnd = c.End
	}
	return insertions, subs
}
