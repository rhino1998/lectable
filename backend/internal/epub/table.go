package epub

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"golang.org/x/net/html"
)

// CellSeparator joins a table row's cells into one paragraph's Text. A
// client that knows about TablePos lays the cells out as a grid and never
// shows it; anything else (search, an LLM prompt, an older client) reads
// "Icebolt | Water | 10 mana". Generation replaces it with a sentence
// break (store.Paragraph.ResolveGenerationText).
const CellSeparator = " | "

// TablePos places a paragraph in a source <table>. A table is read row by
// row: a row's cells are joined into one paragraph (CellSeparator), so
// "Icebolt", "Water", "10 mana" are one clip rather than three fragments,
// and Cells says where each one sits in the text. A cell holding blocks of
// its own (several <p>s) keeps them as separate paragraphs instead, each
// with one cell covering its whole text.
type TablePos struct {
	// Table tells a chapter's tables apart; consecutive paragraphs sharing
	// it are one table.
	Table int
	// Row is the row's index in its table.
	Row int
	// Cells is in column order.
	Cells []TableCell
	// Heading marks a row of column headings that the rows under it speak
	// as each cell's Label: said there, the row itself is silent.
	Heading bool
}

// TableCell is one cell's span of its paragraph's Text.
type TableCell struct {
	// Start and End are byte offsets into the paragraph's Text.
	Start, End int
	// Col is the first grid column the cell occupies; ColSpan and RowSpan
	// are at least 1.
	Col, ColSpan, RowSpan int
	// Header marks a heading cell: a <th>, or a cell of a heading row.
	Header bool
	// Label is the heading of this cell's column, for a cell under a
	// heading row - spoken before the cell ("Mana: 10 mana"), never shown.
	Label string
}

// maxSpan bounds a colspan/rowspan attribute: a malformed epub shouldn't
// get to allocate a million-column grid.
const maxSpan = 100

type tableCell struct {
	node             *html.Node
	col              int
	colSpan, rowSpan int
	th               bool
	blocks           bool // holds block elements of its own
}

type tableRow struct {
	cells []tableCell
	head  bool // inside <thead>
}

type table struct {
	rows []tableRow
	// other is what the table holds besides rows - a <caption> - read as
	// ordinary blocks ahead of it.
	other []*html.Node
}

// readTable reads a <table>'s rows and cells, working out each cell's
// grid column from the spans before and above it. A table nested in a
// cell is left for that cell's own walk.
func readTable(n *html.Node) table {
	var t table
	// taken[c] is how many more rows a rowspan from above occupies
	// column c for.
	var taken []int
	addRow := func(tr *html.Node, head bool) {
		row := tableRow{head: head}
		col := 0
		for c := tr.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode || (c.Data != "td" && c.Data != "th") {
				continue
			}
			for col < len(taken) && taken[col] > 0 {
				col++
			}
			cell := tableCell{
				node: c, col: col,
				colSpan: spanAttr(c, "colspan"), rowSpan: spanAttr(c, "rowspan"),
				th: c.Data == "th", blocks: containsBlockDescendant(c) || containsTable(c),
			}
			for i := 0; i < cell.colSpan; i++ {
				for len(taken) <= col+i {
					taken = append(taken, 0)
				}
				taken[col+i] = cell.rowSpan
			}
			col += cell.colSpan
			row.cells = append(row.cells, cell)
		}
		for i := range taken {
			if taken[i] > 0 {
				taken[i]--
			}
		}
		t.rows = append(t.rows, row)
	}
	var section func(n *html.Node, head bool)
	section = func(n *html.Node, head bool) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			switch c.Data {
			case "tr":
				addRow(c, head)
			case "thead":
				section(c, true)
			case "tbody", "tfoot":
				section(c, false)
			case "colgroup", "col":
			default:
				t.other = append(t.other, c)
			}
		}
	}
	section(n, false)
	return t
}

func spanAttr(n *html.Node, name string) int {
	v, err := strconv.Atoi(strings.TrimSpace(attr(n, name)))
	if err != nil || v < 1 {
		return 1
	}
	return min(v, maxSpan)
}

func containsTable(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == "table" || containsTable(c) {
			return true
		}
	}
	return false
}

// isGrid reports whether t is a table worth laying out as one. A single
// cell is a layout wrapper - some epubs put a whole chapter in one - and
// reads as the ordinary blocks inside it.
func (t table) isGrid() bool {
	n := 0
	for _, r := range t.rows {
		n += len(r.cells)
	}
	return n >= 2
}

// cellText is a leaf cell's text as one line, with its emphasis.
func cellText(n *html.Node) (string, []pronounce.Substitution) {
	raw := strings.ReplaceAll(collectText(n), string(paragraphBreak), " ")
	return extractEmphasis(plainText(raw))
}

// headings finds t's heading rows - rows whose cells name the columns of
// the rows under them - and returns, per row, whether it is one and the
// column headings (by grid column) that apply to it. texts is each leaf
// cell's text.
//
// Markup says so when it can (<thead>, a row of <th>), but most epubs set
// headings in plain <td>s, so two shapes are also recognized:
//   - the first row of a table three or more columns wide, when it reads
//     like headings (a few words a cell, no numbers, no "Key:") - a
//     two-column table is as likely "Name | Jack" over "Class | None";
//   - anywhere, a row of "Key:" cells over a row laid out the same
//     ("Rarity: | Durability:" over "Rare | 72/100"), heading only the
//     rows that match it;
//   - likewise a row that reads like headings where the table's layout
//     changes - a character sheet's "Stat | Points | Effect" under its
//     two-column summary.
func (t table) headings(texts [][]string) (heading []bool, labels []map[int]string) {
	heading = make([]bool, len(t.rows))
	labels = make([]map[int]string, len(t.rows))
	keyed := make([]bool, len(t.rows)) // a "Key:" row
	top := -1
	for i, r := range t.rows {
		allTH := len(r.cells) > 0
		for _, c := range r.cells {
			allTH = allTH && c.th
		}
		if !r.head && !allTH {
			break
		}
		top = i
	}
	if top < 0 && t.firstRowHeads(texts) {
		top = 0
	}
	if top >= 0 && top < len(t.rows)-1 {
		heading[top] = true
	}
	for i := 0; i < len(t.rows)-1; i++ {
		if !heading[i] && i > top && (isKeyRow(t.rows[i], texts[i]) || t.startsSection(i, texts[i])) && sameLayout(t.rows[i], t.rows[i+1]) {
			heading[i], keyed[i] = true, true
		}
	}
	cur := -1
	for i, r := range t.rows {
		switch {
		case heading[i]:
			cur = i
		case cur >= 0 && keyed[cur] && !sameLayout(t.rows[cur], r):
			cur = -1
		case cur >= 0:
			labels[i] = map[int]string{}
			for j, c := range t.rows[cur].cells {
				labels[i][c.col] = strings.TrimSpace(strings.TrimRight(texts[cur][j], ": "))
			}
		}
	}
	return heading, labels
}

// firstRowHeads reports whether the first row reads as column headings
// with nothing in the markup saying so.
func (t table) firstRowHeads(texts [][]string) bool {
	if len(t.rows) < 2 || len(t.rows[0].cells) < 3 {
		return false
	}
	width := 0
	for _, r := range t.rows {
		for _, c := range r.cells {
			width = max(width, c.col+c.colSpan)
		}
	}
	if width != len(t.rows[0].cells) {
		return false
	}
	for j, c := range t.rows[0].cells {
		if c.colSpan != 1 || c.rowSpan != 1 || c.blocks || !looksLikeHeading(texts[0][j]) {
			return false
		}
	}
	return true
}

// startsSection reports whether row i reads like headings and is laid out
// unlike the row above it: the first row of a differently shaped part of
// the table.
func (t table) startsSection(i int, texts []string) bool {
	r := t.rows[i]
	if i == 0 || len(r.cells) < 3 || sameLayout(t.rows[i-1], r) {
		return false
	}
	for j, c := range r.cells {
		if c.blocks || !looksLikeHeading(texts[j]) {
			return false
		}
	}
	return true
}

// isKeyRow reports whether every cell of r is a short "Key:".
func isKeyRow(r tableRow, texts []string) bool {
	if len(r.cells) < 2 {
		return false
	}
	for j, c := range r.cells {
		if c.blocks || !strings.HasSuffix(texts[j], ":") || len(strings.Fields(texts[j])) > maxHeadingWords {
			return false
		}
	}
	return true
}

// sameLayout reports whether two rows' cells occupy the same columns.
func sameLayout(a, b tableRow) bool {
	if len(a.cells) != len(b.cells) {
		return false
	}
	for i := range a.cells {
		if a.cells[i].col != b.cells[i].col || a.cells[i].colSpan != b.cells[i].colSpan {
			return false
		}
	}
	return true
}

// maxHeadingWords is the longest a column heading runs.
const maxHeadingWords = 4

func looksLikeHeading(text string) bool {
	if text == "" || strings.HasSuffix(text, ":") || len(strings.Fields(text)) > maxHeadingWords {
		return false
	}
	return !strings.ContainsFunc(text, unicode.IsDigit)
}
