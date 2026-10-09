package httpapi

import (
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/rhino1998/lectable/backend/internal/epub"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// A book imported from a website stops at a chapter the site lists but
// won't serve. When the reader has that part of the story as an epub - the
// published volume - it can fill in: the epub is uploaded for one check of
// the book's source, and each chapter the site (and the Wayback Machine)
// won't give is taken from it instead, in order, after which chapters from
// the site carry on. Nothing the book already has is touched, and chapters
// the site does serve still come from the site.
//
// An epub's chapters are matched to the site's by title: against the
// ebook titles the site's own list gives for a chapter where it does
// (websource.ChapterRef.Aliases - the web and ebook numbering differ), else
// against the chapter's title there.

// epubFill is an uploaded epub's chapters, offered for a book's missing
// ones. Not safe for concurrent use: one check owns it.
type epubFill struct {
	chapters []epub.Chapter
	// byKey lists the chapters with a given title key, in book order.
	byKey map[string][]int
	used  map[int]bool
	// order is the source's chapters in reading order and pos each one's
	// place in it (setNeighbours): how a chapter with no name to go by is
	// found among its neighbours.
	order []websource.ChapterRef
	pos   map[string]int
	// alike caches numberedAlike.
	alike *bool
	// openings is how chapters the book already has open, by source id
	// (addOpening), and opened how each of the epub's chapters does: a
	// chapter with no name to match on is recognised by its text instead.
	openings map[string]map[string]bool
	opened   []map[string]bool
	// byText caches which epub chapter each known opening is (-1: none).
	byText map[string]int
	// pinned is what the reader said outright: this chapter of the source
	// is that chapter of the epub. It outranks everything worked out.
	pinned map[string]int
}

func newEpubFill(book *epub.Book) *epubFill {
	f := &epubFill{
		chapters: book.Chapters, byKey: map[string][]int{}, used: map[int]bool{},
		openings: map[string]map[string]bool{}, byText: map[string]int{}, pinned: map[string]int{},
	}
	for i, ch := range book.Chapters {
		if k := titleKey(ch.Title); k != "" {
			f.byKey[k] = append(f.byKey[k], i)
		}
		var text strings.Builder
		for _, blk := range ch.Blocks {
			if blk.Kind == epub.BlockText && blk.Text != ch.Title && text.Len() < openingChars {
				text.WriteString(blk.Text + " ")
			}
		}
		f.opened = append(f.opened, shingles(text.String()))
	}
	return f
}

// openingChars is how much of a chapter's start is compared, and
// openingAlike how much of two openings' wording must be shared for them
// to be the same chapter - loose enough for a published edition's
// copy-edits, far above what two different chapters share.
const (
	openingChars = 700
	openingAlike = 0.45
)

// shingles is a text's set of three-word runs, lowercased and stripped of
// punctuation: what two editions of a passage have in common.
func shingles(text string) map[string]bool {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(words) > 110 {
		words = words[:110]
	}
	out := map[string]bool{}
	for i := 0; i+3 <= len(words); i++ {
		out[words[i]+" "+words[i+1]+" "+words[i+2]] = true
	}
	return out
}

// addOpening tells the fill how a chapter the book already has (or one
// read ahead) opens, so it can be found in the epub by its text.
func (f *epubFill) addOpening(sourceID, text string) {
	if set := shingles(text); len(set) >= 8 {
		f.openings[sourceID] = set
		delete(f.byText, sourceID)
	}
}

// textAnchor returns the epub chapter that opens the way the source's
// chapter id does, if exactly one clearly does.
func (f *epubFill) textAnchor(id string) (int, bool) {
	if i, ok := f.pinned[id]; ok {
		return i, true
	}
	if i, done := f.byText[id]; done {
		return i, i >= 0
	}
	want, known := f.openings[id]
	best, bestScore, second := -1, 0.0, 0.0
	if known {
		for i, have := range f.opened {
			if len(have) == 0 {
				continue
			}
			shared := 0
			for sh := range want {
				if have[sh] {
					shared++
				}
			}
			score := float64(shared) / float64(min(len(want), len(have)))
			switch {
			case score > bestScore:
				best, bestScore, second = i, score, bestScore
			case score > second:
				second = score
			}
		}
	}
	if bestScore < openingAlike || bestScore-second < 0.15 {
		best = -1
	}
	f.byText[id] = best
	return best, best >= 0
}

// chapterNumbering is what a chapter title opens with before its name:
// "Chapter 184:", "23.", "Ch. 5 -", "Book 3 Chapter 38".
var chapterNumbering = regexp.MustCompile(`(?i)^\s*(?:(?:book|volume|vol\.?|part)\s*\d+\s*[:,.\-–—]?\s*)?(?:chapter|ch\.?)?\s*\d+(?:\.\d+)?\s*[:.\-–—]?\s*`)

// numberWords are the words a title spells its number with ("Chapter
// Seventy Six - 076").
var numberWords = regexp.MustCompile(`(?i)\b(?:chapter|ch|zero|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundred|thousand|and)\b`)

var digits = regexp.MustCompile(`\d+`)

// volumeChapter is a title that numbers its chapter within a book:
// "Book 3 Chapter 38 Joyride" is book "3", chapter 38.
var volumeChapter = regexp.MustCompile(`(?i)^\s*(?:book|volume|vol\.?|part)\s*(\d+)\s*[:,.\-–—]?\s*(?:chapter|ch\.?)\s*(\d+)`)

// volumeOf is the book a title says its chapter is in, "" when it doesn't.
func volumeOf(title string) string {
	if m := volumeChapter.FindStringSubmatch(title); m != nil {
		return m[1]
	}
	return ""
}

// chapterNumber is the chapter number a title carries: the one it opens
// with ("Chapter 184: ..."), or, for a title that is nothing but its
// number, that ("Chapter Seventy Six - 076", "76").
func chapterNumber(title string) (int, bool) {
	if m := leadingNumber.FindStringSubmatch(title); m != nil {
		n, err := strconv.Atoi(m[2])
		return n, err == nil
	}
	// "Book 3 Chapter 38 Joyride": the chapter's number within its book.
	if m := volumeChapter.FindStringSubmatch(title); m != nil {
		n, err := strconv.Atoi(m[2])
		return n, err == nil
	}
	rest := numberWords.ReplaceAllString(title, "")
	for _, r := range rest {
		if unicode.IsLetter(r) {
			return 0, false
		}
	}
	all := digits.FindAllString(rest, -1)
	if len(all) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(all[len(all)-1])
	return n, err == nil
}

// titleKey reduces a chapter title to what two editions of it share. A
// published book renumbers its chapters (each volume starts at 1), so a
// chapter is known by its name: the title's letters and digits after its
// numbering - "Chapter 184: More Shady As We Go Along", "Book 2 Chapter
// 23 More Shady..." and "23. More Shady as We Go Along" are the same
// chapter. A title that is only a number ("1.02", "Chapter 12", "Chapter
// Seventy Six - 076") has nothing else, and is its number.
func titleKey(title string) string {
	key := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	// Nothing but a number, in figures or words?
	numberOnly := true
	for _, r := range numberWords.ReplaceAllString(title, "") {
		if unicode.IsLetter(r) {
			numberOnly = false
			break
		}
	}
	if !numberOnly {
		if name := key(chapterNumbering.ReplaceAllString(title, "")); len(name) >= 3 {
			return name
		}
	}
	if n, ok := chapterNumber(title); ok && numberOnly && !strings.Contains(title, ".") {
		return "#" + strconv.Itoa(n)
	}
	return strings.ReplaceAll(key(title), "chapter", "")
}

// cutShort is how long a title key must be before it can match as the
// start of a longer one: a site's address for a chapter cuts a long title
// off, and a title known only from the address is cut with it.
const cutShort = 12

// names are the titles ref goes by in an ebook.
func (f *epubFill) names(ref websource.ChapterRef) []string {
	if len(ref.Aliases) > 0 {
		return ref.Aliases
	}
	return []string{ref.Title}
}

// byName returns the epub chapters a title could be, in book order.
func (f *epubFill) byName(name string) []int {
	key := titleKey(name)
	if idxs := f.byKey[key]; len(idxs) > 0 {
		return idxs
	}
	if len(key) < cutShort {
		return nil
	}
	var idxs []int
	for other, list := range f.byKey {
		if strings.HasPrefix(other, key) {
			idxs = append(idxs, list...)
		}
	}
	sort.Ints(idxs)
	return idxs
}

// setNeighbours tells the fill the order the source's chapters come in, so
// that a chapter with no name to go by - "X", known only from a mangled
// address, or one known only to be missing ("Chapter 77") - can be found
// in the epub by the chapters around it.
func (f *epubFill) setNeighbours(chapters []websource.ChapterRef) {
	f.order = chapters
	f.pos = make(map[string]int, len(chapters))
	for i, ref := range chapters {
		f.pos[ref.ID] = i
	}
}

// anchorReach is how far along the source's chapters locate looks for one
// the epub has by name, to count from.
const anchorReach = 40

// named reports whether a title has a name the epub could know it by,
// rather than only a number or nothing.
func named(title string) bool {
	return len(titleKey(title)) >= 3 && !strings.HasPrefix(titleKey(title), "#")
}

// locate finds the epub chapters that stand for ref: by its titles, or -
// for a single title the epub doesn't have - by position, counting from
// chapters the epub is known to have (by name, or by opening with the
// same text - addOpening). With one known either side, they must sit
// exactly as far apart in the epub as in the source. With one on one side
// only, ref is counted on from it as long as the epub's numbering runs on
// unbroken. ok is false when the epub doesn't have it.
func (f *epubFill) locate(ref websource.ChapterRef) (groups [][]int, ok bool) {
	if i, ok := f.pinned[ref.ID]; ok {
		return [][]int{{i}}, true
	}
	names := f.names(ref)
	for _, name := range names {
		idxs := f.byName(name)
		// A bare number is only a match in an epub that numbers its
		// chapters as the source does, which the chapters around it show.
		if len(idxs) == 0 || (strings.HasPrefix(titleKey(name), "#") && !f.numberedAlike()) {
			groups = nil
			break
		}
		groups = append(groups, idxs)
	}
	if groups != nil {
		return groups, true
	}
	at, known := f.pos[ref.ID]
	if len(names) != 1 || !known {
		return nil, false
	}
	// The nearest chapters either side that the epub is known to have:
	// by a name only one of its chapters carries, or by how they open.
	anchor := func(step int) (dist, idx int, ok bool) {
		for d := 1; d <= anchorReach; d++ {
			i := at + step*d
			if i < 0 || i >= len(f.order) {
				break
			}
			if j, found := f.textAnchor(f.order[i].ID); found {
				return d, j, true
			}
			if t := f.order[i].Title; named(t) {
				if idxs := f.byName(t); len(idxs) == 1 {
					return d, idxs[0], true
				}
			}
		}
		return 0, 0, false
	}
	before, b, okBefore := anchor(-1)
	after, a, okAfter := anchor(1)
	switch {
	case okBefore && okAfter:
		// Both sides agree on how far apart they are.
		if a-b == before+after {
			return [][]int{{b + before}}, true
		}
	case okBefore:
		// An unbroken run on from a known chapter: the epub's own
		// numbering must run on with it, which is what stops the count
		// walking off the end of a volume into its back matter.
		if j := b + before; j < len(f.chapters) && f.runsOn(b, j) {
			return [][]int{{j}}, true
		}
	case okAfter:
		if j := a - after; j >= 0 && f.runsOn(j, a) {
			return [][]int{{j}}, true
		}
	}
	return nil, false
}

// notStory matches the titles of an ebook's front and back matter: a
// title that starts with one of these, not one that merely uses the word
// ("Time, Resources and Dedication" is a chapter).
var notStory = regexp.MustCompile(`(?i)^\W*(afterword|acknowledge?ments?|about the author|about \w+ press|also (by|in)|copyright|(table of )?contents|glossary|appendix|preview|sneak peek|thank you|dedication|title page|newsletter|patreon|information)\b`)

// runsOn reports whether the epub's chapters from i to j are one unbroken
// run of the story: numbered chapters count up by one, and nothing in
// between is front or back matter.
func (f *epubFill) runsOn(i, j int) bool {
	last, numbered := 0, false
	for k := i; k <= j; k++ {
		title := f.chapters[k].Title
		if notStory.MatchString(title) || len(f.opened[k]) == 0 {
			return false
		}
		n, ok := chapterNumber(title)
		if !ok {
			continue
		}
		if numbered && n != last+1 {
			return false
		}
		last, numbered = n, true
	}
	return true
}

// numberedAlike reports whether the epub numbers its chapters the way the
// source does: some chapter of the source with both a number and a name is
// in the epub under the same number. (A published volume usually starts
// again at 1; a few keep the serial's count.)
func (f *epubFill) numberedAlike() bool {
	if f.alike != nil {
		return *f.alike
	}
	same := false
	for _, ref := range f.order {
		n, ok := chapterNumber(ref.Title)
		if !ok || !named(ref.Title) {
			continue
		}
		for _, i := range f.byName(ref.Title) {
			if m, ok := chapterNumber(f.chapters[i].Title); ok {
				same = m == n
				f.alike = &same
				return same
			}
		}
	}
	// Nothing to compare by name. An epub whose chapters are only
	// numbered is the serial's own count if it starts past 1 (a later
	// volume that didn't start over); one starting at 1 could be either,
	// and a wrong chapter is worse than none.
	lowest := 0
	for _, ch := range f.chapters {
		if n, ok := chapterNumber(ch.Title); ok && (lowest == 0 || n < lowest) {
			lowest = n
		}
	}
	same = lowest > 1
	f.alike = &same
	return same
}

// has reports whether the epub has every chapter ref corresponds to.
func (f *epubFill) has(ref websource.ChapterRef) bool {
	_, ok := f.locate(ref)
	return ok
}

// take returns the epub's chapters standing in for ref, as chapters of
// the book's source carrying ref's id; ok is false when the epub doesn't
// have them all. None with ok true means they were already taken, for an
// earlier chapter of the site's that the same ebook chapter covers.
func (f *epubFill) take(ref websource.ChapterRef) (chapters []*websource.Chapter, ok bool) {
	groups, ok := f.locate(ref)
	if !ok {
		return nil, false
	}
	for _, idxs := range groups {
		for _, i := range idxs {
			if f.used[i] {
				continue
			}
			f.used[i] = true
			id := ""
			if len(chapters) == 0 {
				id = ref.ID
			}
			title := f.chapters[i].Title
			if len(groups) == 1 {
				title = f.titleFor(ref, title)
			}
			chapters = append(chapters, epubChapterAsSource(f.chapters[i], title, id, fmt.Sprintf("%s-fill%d", websource.FileKey(ref.ID), len(chapters))))
			break
		}
	}
	return chapters, true
}

// leadingNumber splits a title into what comes before its chapter number,
// the number, and the separator after it: "Chapter 184: Name" is
// "Chapter ", 184, ": ".
var leadingNumber = regexp.MustCompile(`(?i)^(\s*(?:chapter|ch\.?)?\s*)(\d+)(\s*[:.\-–—]?\s*)`)

// titleFor names an epub chapter standing in for ref the way the book's
// other chapters are named. The published book numbers its chapters from
// 1 in each volume ("70. Domineering, Territorial and Robust"); in the
// book it becomes the source's chapter 183, written like its neighbours
// ("Chapter 183: Domineering, Territorial and Robust"): the source's
// number - ref's own, or one more than the chapter before it when ref's
// title has none - with the epub's wording of the name. A source that
// doesn't number its chapters keeps the epub's title.
func (f *epubFill) titleFor(ref websource.ChapterRef, epubTitle string) string {
	name := strings.TrimSpace(chapterNumbering.ReplaceAllString(epubTitle, ""))
	if name == "" {
		// The epub only numbers it, in its own count: the source's title
		// is the one that fits the book - unless that count is the
		// source's own, and the chapter was found by it.
		for _, n := range f.names(ref) {
			if titleKey(n) == titleKey(epubTitle) {
				return epubTitle
			}
		}
		if strings.TrimSpace(ref.Title) != "" {
			return ref.Title
		}
		return epubTitle
	}
	// The source's own way of numbering this chapter, when its title has
	// one ("Book 3 Chapter 38 Joyride"), with the epub's wording after it.
	if own := chapterNumbering.FindString(ref.Title); strings.TrimSpace(own) != "" && named(ref.Title) {
		own = strings.TrimSpace(own)
		if last := own[len(own)-1]; last >= '0' && last <= '9' {
			own += ":"
		}
		return own + " " + name
	}
	at, known := f.pos[ref.ID]
	number, ok := chapterNumber(ref.Title)
	if !ok && known {
		// Counted on from the nearest numbered chapter before it.
		for d := 1; d <= anchorReach && at-d >= 0; d++ {
			if n, ok2 := chapterNumber(f.order[at-d].Title); ok2 {
				number, ok = n+d, true
				break
			}
		}
	}
	if !ok {
		return epubTitle
	}
	// Written as the nearest chapter that has punctuation after its number
	// is - a title known only from an address has lost its own.
	prefix, sep := "Chapter ", ": "
	styled := func(t string) bool {
		if m := leadingNumber.FindStringSubmatch(t); m != nil && strings.TrimSpace(m[3]) != "" {
			prefix, sep = m[1], m[3]
			return true
		}
		return false
	}
	if !styled(ref.Title) && known {
		for d := 1; d <= anchorReach; d++ {
			if at-d >= 0 && styled(f.order[at-d].Title) {
				break
			}
			if at+d < len(f.order) && styled(f.order[at+d].Title) {
				break
			}
		}
	}
	return prefix + strconv.Itoa(number) + sep + name
}

// epubChapterAsSource writes a parsed epub chapter back out as the markup
// a fetched chapter has, so it is stored, kept in the book's source epub
// and re-imported exactly like one: paragraphs (the segments a paragraph
// was split into rejoined), emphasis, table rows, images and scene breaks.
// id is the source chapter it stands for ("" for the second and later of
// several); fileKey names its files, since id needn't be unique; title is
// what the chapter is called in the book (its own title in the epub, also
// its opening line there, is dropped either way).
func epubChapterAsSource(ch epub.Chapter, title, id, fileKey string) *websource.Chapter {
	out := &websource.Chapter{ID: id, Title: title}
	if id == "" {
		// WriteEPUB tells chapters apart by id.
		out.ID = fileKey
	}
	var b strings.Builder
	inParagraph, table := false, -1
	closeParagraph := func() {
		if inParagraph {
			b.WriteString("</p>\n")
			inParagraph = false
		}
	}
	closeTable := func() {
		if table >= 0 {
			b.WriteString("</table>\n")
			table = -1
		}
	}
	titleSeen := false
	for _, blk := range ch.Blocks {
		switch blk.Kind {
		case epub.BlockImage:
			closeParagraph()
			closeTable()
			name := fmt.Sprintf("%s-%d%s", fileKey, len(out.Images), blk.ImageExt)
			out.Images = append(out.Images, websource.Image{Name: name, Data: blk.ImageData})
			b.WriteString(`<p><img src="../img/` + name + `" alt=""/></p>` + "\n")
		case epub.BlockBreak:
			closeParagraph()
			closeTable()
			b.WriteString("<hr/>\n")
		default:
			// The chapter's title is written by the epub itself.
			if !titleSeen {
				titleSeen = true
				if blk.Text == ch.Title {
					continue
				}
			}
			if t := blk.Table; t != nil {
				closeParagraph()
				if table != t.Table {
					closeTable()
					b.WriteString("<table>\n")
					table = t.Table
				}
				b.WriteString("<tr>")
				for _, c := range t.Cells {
					tag := "td"
					if c.Header {
						tag = "th"
					}
					fmt.Fprintf(&b, `<%s colspan="%d" rowspan="%d">%s</%s>`, tag, max(c.ColSpan, 1), max(c.RowSpan, 1),
						html.EscapeString(blk.Text[min(c.Start, len(blk.Text)):min(max(c.End, c.Start), len(blk.Text))]), tag)
				}
				b.WriteString("</tr>\n")
				continue
			}
			closeTable()
			if blk.Inline && inParagraph {
				b.WriteString(" ")
			} else {
				closeParagraph()
				b.WriteString("<p>")
				inParagraph = true
			}
			b.WriteString(emphasized(blk))
		}
	}
	closeParagraph()
	closeTable()
	out.Body = b.String()
	return out
}

// emphasized is a block's text as markup, its emphasized spans in <em>.
func emphasized(blk epub.Block) string {
	var spans []struct{ off, n int }
	for _, e := range blk.Emphasis {
		if e.Offset >= 0 && e.Length > 0 && e.Offset+e.Length <= len(blk.Text) {
			spans = append(spans, struct{ off, n int }{e.Offset, e.Length})
		}
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].off < spans[j].off })
	var b strings.Builder
	at := 0
	for _, sp := range spans {
		if sp.off < at {
			continue
		}
		b.WriteString(html.EscapeString(blk.Text[at:sp.off]))
		b.WriteString("<em>" + html.EscapeString(blk.Text[sp.off:sp.off+sp.n]) + "</em>")
		at = sp.off + sp.n
	}
	b.WriteString(html.EscapeString(blk.Text[at:]))
	return b.String()
}

// handleBackfillBookSource takes an epub (multipart "file") and checks the
// book's source with it at hand: chapters the site lists but won't serve
// are filled from the epub's, matched by title, and the rest come from the
// site as usual - in the background, like any check. Answers 202 with how
// many of the chapters the book is still missing the epub could stand in
// for; 422 if none, 409 for a book with no source or with a check already
// running.
func (s *Server) handleBackfillBookSource(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	b, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	src, err := s.Store.GetBookSource(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if src == nil {
		writeError(w, http.StatusConflict, "this book wasn't imported from a website, so it has no missing chapters to fill")
		return
	}
	reader, workID, ok := s.readerForBook(src)
	if !ok {
		writeError(w, http.StatusConflict, "this server can't read the site this book came from ("+src.Kind+")")
		return
	}
	if s.sourceBusy(bookID) {
		writeError(w, http.StatusConflict, "chapters are being fetched for this book right now - try again when that finishes")
		return
	}

	if err := r.ParseMultipartForm(200 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "could not parse upload: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()
	tmp, err := os.CreateTemp("", "backfill-*.epub")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to buffer upload")
		return
	}
	parsed, err := epub.ParseAs(tmp, size, epub.PlayOff)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not parse epub: "+err.Error())
		return
	}
	fill := newEpubFill(parsed)

	work, err := reader.site.Work(r.Context(), reader.fetch, workID)
	if err != nil {
		writeSourceError(w, reader.site, err, http.StatusBadGateway)
		return
	}
	haveIDs, err := s.Store.ChapterSourceIDs(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	have := make(map[string]bool, len(haveIDs))
	for _, id := range haveIDs {
		have[id] = true
	}
	plan, err := s.Store.GetBookSourcePlan(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	planned := plannedChapters(reader.site, work.Chapters, plan)
	fill.setNeighbours(planned)
	if err := s.knowBook(bookID, fill); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// "first": the reader names the epub chapter that is the first one
	// the book is missing, and the rest are counted on from it.
	if first := r.FormValue("first"); first != "" {
		at, err := strconv.Atoi(first)
		if err != nil || at < 0 || at >= len(fill.chapters) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("first must name one of the epub's %d chapters", len(fill.chapters)))
			return
		}
		for _, ref := range planned {
			if !have[ref.ID] {
				fill.pinned[ref.ID] = at
				break
			}
		}
	}
	// A chapter held because it couldn't be read is agreed to again when
	// the epub has it: that is what the epub is for.
	held := map[string]bool{}
	for _, p := range plan.Pending {
		if p.Unreadable {
			held[p.ID] = true
		}
	}
	matched, missing, released := 0, 0, 0
	for _, ref := range planned {
		if have[ref.ID] {
			continue
		}
		missing++
		if !fill.has(ref) {
			continue
		}
		matched++
		if held[ref.ID] {
			plan.Approved = append(plan.Approved, ref.ID)
			delete(held, ref.ID)
			released++
		}
	}
	if released > 0 {
		plan.Pending = slices.DeleteFunc(plan.Pending, func(p store.PlannedChapter) bool { return p.Unreadable && !held[p.ID] })
		if err := s.Store.SetBookSourcePlan(bookID, plan); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if missing == 0 {
		writeError(w, http.StatusUnprocessableEntity, "this book already has every chapter "+reader.site.Name()+" lists")
		return
	}
	if matched == 0 {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("none of this epub's %d chapters match the %d this book is missing (they are matched by chapter title, or by following on from chapters the book already has)", len(parsed.Chapters), missing))
		return
	}
	if !s.startSourceSync(bookID, fill) {
		writeError(w, http.StatusConflict, "chapters are being fetched for this book right now - try again when that finishes")
		return
	}
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: matched})
}

// knowBook tells fill how the chapters bookID already has open, so the
// epub's copies of them can be found by their text and the chapters after
// them counted on from there.
func (s *Server) knowBook(bookID string, fill *epubFill) error {
	openings, err := s.Store.ChapterOpenings(bookID, openingChars)
	if err != nil {
		return err
	}
	for _, o := range openings {
		for _, id := range strings.Fields(o.SourceID) {
			fill.addOpening(id, o.Text)
		}
	}
	return nil
}

// handleListEpubChapters reads an uploaded epub (multipart "file") and
// lists its chapters - title, how each opens - for choosing one by hand
// (handleMatchEpubChapter, or handleBackfillBookSource's "first"). With a
// "bookId" field, each also names the chapter of that book that opens
// with the same text, if one does. Nothing is stored.
func (s *Server) handleListEpubChapters(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "could not parse upload: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()
	fill, err := readEpubFill(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not parse epub: "+err.Error())
		return
	}
	inBook := map[int]int{}
	if bookID := r.FormValue("bookId"); bookID != "" {
		openings, err := s.Store.ChapterOpenings(bookID, openingChars)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, o := range openings {
			key := strconv.Itoa(o.Idx)
			fill.addOpening(key, o.Text)
			if i, ok := fill.textAnchor(key); ok {
				if _, taken := inBook[i]; !taken {
					inBook[i] = o.Idx
				}
			}
		}
	}
	resp := epubChaptersResponse{Chapters: make([]epubChapterDTO, 0, len(fill.chapters))}
	for i, ch := range fill.chapters {
		dto := epubChapterDTO{Index: i, Title: ch.Title}
		for _, blk := range ch.Blocks {
			if blk.Kind != epub.BlockText {
				continue
			}
			dto.Paragraphs++
			if blk.Text != ch.Title && len(dto.Opening) < 240 {
				dto.Opening += blk.Text + " "
			}
		}
		dto.Opening = strings.TrimSpace(dto.Opening)
		if idx, ok := inBook[i]; ok {
			dto.BookChapter = &idx
		}
		resp.Chapters = append(resp.Chapters, dto)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMatchEpubChapter replaces one chapter's content with a chapter of
// an uploaded epub chosen by hand (multipart "file", "epubChapter" its
// index as handleListEpubChapters lists it) - for a chapter that was
// filled from the wrong one, or whose published text is wanted instead.
// The chapter keeps its place, its source id and - unless "title" gives
// another - its title, so nothing else in the book moves. Like a
// re-import, everything derived from the old text goes (audio, speakers,
// passes) and the chapter's jobs are cancelled first (409 if one won't
// stop). For a book imported from a website the kept epub and the
// chapter's stored pages are updated too, so a later re-import or reparse
// gives the same text; an uploaded book's kept epub is left alone.
func (s *Server) handleMatchEpubChapter(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	b, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil || ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "could not parse upload: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()
	fill, err := readEpubFill(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not parse epub: "+err.Error())
		return
	}
	at, err := strconv.Atoi(r.FormValue("epubChapter"))
	if err != nil || at < 0 || at >= len(fill.chapters) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("epubChapter must name one of the epub's %d chapters", len(fill.chapters)))
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		title = ch.Title
	}

	sourceID := ""
	sourcedChapters, err := s.Store.SourcedChapters(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, sc := range sourcedChapters {
		if sc.Idx == idx {
			sourceID = sc.SourceID
		}
	}
	if sourceID != "" && s.sourceBusy(bookID) {
		writeError(w, http.StatusConflict, "chapters are being fetched for this book right now - try again when that finishes")
		return
	}

	var blocks []store.BlockInput
	var imageBytes [][]byte
	var asSource *websource.Chapter
	if sourceID != "" {
		// Stored the way a chapter fetched from the site is, so the kept
		// epub gives this same text back.
		asSource = epubChapterAsSource(fill.chapters[at], title, sourceID, websource.FileKey(sourceID))
		input, images, err := sourceChapterInput(websource.Meta{Title: b.Title}, asSource)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		title, blocks, imageBytes = input.Title, input.Blocks, images
	} else {
		blocks, imageBytes = chapterBlockInputs(fill.chapters[at])
	}
	paragraphs, status, err := s.swapChapterContent(r.Context(), bookID, ch, title, blocks, imageBytes)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if asSource != nil {
		if err := s.rewriteSourceEpub(bookID, []*websource.Chapter{asSource}); err != nil && !errors.Is(err, websource.ErrForeignEpub) {
			log.Printf("source: writing the matched chapter %q to the epub of %s: %v", title, bookID, err)
		}
		ref := websource.ChapterRef{ID: sourceID, Title: title}
		s.writeRawChapter(bookID, sourced{ch: asSource, raw: &websource.RawChapter{Ref: ref, Via: "epub", FetchedAt: time.Now().Unix(), Chapter: asSource}})
	}
	writeJSON(w, http.StatusOK, reimportChapterResponse{Title: title, Paragraphs: paragraphs})
}
