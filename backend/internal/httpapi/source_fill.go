package httpapi

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"unicode"

	"github.com/rhino1998/lectable/backend/internal/epub"
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
}

func newEpubFill(book *epub.Book) *epubFill {
	f := &epubFill{chapters: book.Chapters, byKey: map[string][]int{}, used: map[int]bool{}}
	for i, ch := range book.Chapters {
		if k := titleKey(ch.Title); k != "" {
			f.byKey[k] = append(f.byKey[k], i)
		}
	}
	return f
}

// titleKey reduces a chapter title to what two ways of writing it share:
// its letters and digits, without the word "chapter" - "Chapter 1.02" and
// "1.02" are the same chapter, "1.02" and "1.20" are not.
func titleKey(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return strings.ReplaceAll(b.String(), "chapter", "")
}

// names are the titles ref goes by in an ebook.
func (f *epubFill) names(ref websource.ChapterRef) []string {
	if len(ref.Aliases) > 0 {
		return ref.Aliases
	}
	return []string{ref.Title}
}

// has reports whether the epub has every chapter ref corresponds to.
func (f *epubFill) has(ref websource.ChapterRef) bool {
	for _, name := range f.names(ref) {
		if len(f.byKey[titleKey(name)]) == 0 {
			return false
		}
	}
	return true
}

// take returns the epub's chapters standing in for ref, as chapters of
// the book's source carrying ref's id; ok is false when the epub doesn't
// have them all. None with ok true means they were already taken, for an
// earlier chapter of the site's that the same ebook chapter covers.
func (f *epubFill) take(ref websource.ChapterRef) (chapters []*websource.Chapter, ok bool) {
	if !f.has(ref) {
		return nil, false
	}
	for _, name := range f.names(ref) {
		for _, i := range f.byKey[titleKey(name)] {
			if f.used[i] {
				continue
			}
			f.used[i] = true
			id := ""
			if len(chapters) == 0 {
				id = ref.ID
			}
			chapters = append(chapters, epubChapterAsSource(f.chapters[i], id, fmt.Sprintf("%s-fill%d", websource.FileKey(ref.ID), len(chapters))))
			break
		}
	}
	return chapters, true
}

// epubChapterAsSource writes a parsed epub chapter back out as the markup
// a fetched chapter has, so it is stored, kept in the book's source epub
// and re-imported exactly like one: paragraphs (the segments a paragraph
// was split into rejoined), emphasis, table rows, images and scene breaks.
// id is the source chapter it stands for ("" for the second and later of
// several); fileKey names its files, since id needn't be unique.
func epubChapterAsSource(ch epub.Chapter, id, fileKey string) *websource.Chapter {
	out := &websource.Chapter{ID: id, Title: ch.Title}
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
	if _, running := s.sourceSyncs.Load(bookID); running {
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
	matched, missing := 0, 0
	for _, ref := range work.Chapters {
		if have[ref.ID] {
			continue
		}
		missing++
		if fill.has(ref) {
			matched++
		}
	}
	if missing == 0 {
		writeError(w, http.StatusUnprocessableEntity, "this book already has every chapter "+reader.site.Name()+" lists")
		return
	}
	if matched == 0 {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("none of this epub's %d chapters match the %d this book is missing (they are matched by chapter title)", len(parsed.Chapters), missing))
		return
	}
	if !s.startSourceSync(bookID, fill) {
		writeError(w, http.StatusConflict, "chapters are being fetched for this book right now - try again when that finishes")
		return
	}
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: matched})
}
