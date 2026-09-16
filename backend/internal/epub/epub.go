// Package epub parses .epub files into plain-text chapters and paragraphs,
// suitable for feeding one paragraph at a time to a TTS engine, plus any
// inline images found in each chapter's markup.
package epub

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"golang.org/x/net/html"
)

// BlockKind distinguishes the two kinds of content a chapter can contain,
// in original document order.
type BlockKind string

const (
	BlockText  BlockKind = "text"
	BlockImage BlockKind = "image"
	// BlockBreak is a scene/section break - a source epub's own <hr/>, or a
	// paragraph containing nothing but scene-break glyphs ("* * *", "***",
	// a lone "⁂" asterism, etc. - see isSceneBreakText). Carries no Text of
	// its own: unlike BlockText, there is nothing to narrate or display
	// beyond a visual/audible break, so it never becomes a paragraph row at
	// all (see store.Store.CreateBook) - it's positioned among the
	// surrounding content exactly like BlockImage is, just with no bytes.
	BlockBreak BlockKind = "break"
)

type Block struct {
	Kind BlockKind
	Text string // BlockText

	// Inline marks a BlockText block as a split-out continuation of the
	// immediately preceding BlockText block, rather than the start of a
	// new visual paragraph - set when a single source paragraph's text is
	// split into alternating narration/quoted-dialogue segments (see
	// splitQuoteSegments) so each segment can be speaker-attributed and
	// voiced independently (internal/speakerattr, internal/narration)
	// while the reader still sees one continuous paragraph. Always false
	// for a segment that starts a (possibly single-segment) paragraph.
	Inline bool

	// IsQuote marks a BlockText block as one of splitQuoteSegments' actual
	// quoted-dialogue spans, as opposed to a narration/description segment
	// (including the surrounding prose of a paragraph that was never split
	// at all - unsplit text is never a quote span by definition). Only a
	// block with IsQuote true may ever be attributed to a character name
	// by internal/speakerattr - narration/description about a character
	// is not that character speaking, and httpapi.attributeChapter
	// enforces this regardless of what the model itself decides (see its
	// own doc comment).
	IsQuote bool

	// Emphasis is this block's own structural emphasis - epub markup
	// (<em>/<i>/<strong>/<b>) found in the source, resolved to
	// generation-time substitutions (see extractEmphasis's own doc
	// comment for why a substitution - respelling in ALL CAPS - rather
	// than a delivery tag: no wired clone model exposes a dedicated
	// stress/emphasis control token). Offset/Length are byte offsets into
	// this same Block's own Text, the same coordinate space
	// pronounce.Substitution already uses elsewhere. nil for a block with
	// no emphasized span (the common case).
	Emphasis []pronounce.Substitution

	ImageData []byte // BlockImage
	ImageExt  string // BlockImage, e.g. ".jpg" (lowercase, includes the dot)
}

type Chapter struct {
	Title  string
	Blocks []Block
}

type Book struct {
	Title          string
	Author         string
	Language       string
	CoverData      []byte
	CoverMediaType string
	SeriesName     string  // "" if the epub isn't part of a series
	SeriesIndex    float64 // 0 if unknown; can be fractional (e.g. a 2.5 novella)
	Chapters       []Chapter
}

type manifestItem struct {
	Href       string
	MediaType  string
	Properties string
}

// Parse reads an .epub file from r (of the given size) and extracts
// metadata, chapter content (text and inline images), and a cover image if
// present.
func Parse(r io.ReaderAt, size int64) (*Book, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("open epub as zip: %w", err)
	}

	files := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		files[f.Name] = f
	}

	opfPath, err := findOPFPath(files)
	if err != nil {
		return nil, err
	}
	opfData, err := readZipFile(files, opfPath)
	if err != nil {
		return nil, fmt.Errorf("read package document: %w", err)
	}
	opfDir := path.Dir(opfPath)

	pkg, err := parsePackage(opfData)
	if err != nil {
		return nil, fmt.Errorf("parse package document: %w", err)
	}

	book := &Book{
		Title:       firstNonEmpty(pkg.title, "Untitled"),
		Author:      pkg.author,
		Language:    pkg.language,
		SeriesName:  pkg.seriesName,
		SeriesIndex: parseSeriesIndex(pkg.seriesIndex),
	}

	if coverItem, ok := resolveCover(pkg); ok {
		coverPath := resolveHref(opfDir, coverItem.Href)
		if data, err := readZipFile(files, coverPath); err == nil {
			book.CoverData = data
			book.CoverMediaType = coverItem.MediaType
		}
	}

	tocEntries := loadTocEntries(pkg, files, opfDir)
	tocByPath := groupTocEntries(tocEntries)
	hasToc := len(tocEntries) > 0

	// appendChapter applies the title-fallback and narrate-the-title rules
	// shared by every path below, then adds the chapter.
	appendChapter := func(title string, blocks []Block) {
		if title == "" {
			title = fmt.Sprintf("Chapter %d", len(book.Chapters)+1)
		}
		// The title only ends up as spoken content on its own when it came
		// from a heading inside the body (extractChapter already emitted
		// that heading as a normal text block) or is a TOC entry's own
		// title, which is never in the blocks at all - prepend it as one,
		// unless the first spoken text already *is* the title (avoids
		// saying it twice).
		if !startsWithTitle(blocks, title) {
			blocks = append([]Block{{Kind: BlockText, Text: title}}, blocks...)
		}
		book.Chapters = append(book.Chapters, Chapter{Title: title, Blocks: blocks})
	}

	for _, idref := range pkg.spine {
		item, ok := pkg.manifest[idref]
		if !ok {
			continue
		}
		if strings.Contains(item.Properties, "nav") {
			continue
		}
		if !strings.Contains(item.MediaType, "html") && !strings.Contains(item.MediaType, "xml") {
			continue
		}
		itemPath := resolveHref(opfDir, item.Href)
		raw, err := readZipFile(files, itemPath)
		if err != nil {
			continue // skip unreadable spine items rather than failing the whole book
		}
		itemDir := path.Dir(itemPath)

		entries := tocByPath[itemPath]
		anchorIDs := make(map[string]bool, len(entries))
		for _, e := range entries {
			if e.Anchor != "" {
				anchorIDs[e.Anchor] = true
			}
		}

		title, titleFromHeading, blocks, boundaries := extractChapter(raw, files, itemDir, anchorIDs)
		if len(blocks) == 0 {
			continue
		}

		switch {
		case len(entries) == 0:
			// Some books split each real chapter across several spine files
			// (one per print page, or a chapter plus its footnotes as a
			// separate file) with no TOC entry of their own, and no heading
			// either - fold those into whatever chapter came before them
			// instead of inventing a new, oddly-named chapter for each one.
			// A file with its own heading is kept as its own chapter even
			// without a TOC entry, since some TOCs only go down to
			// Part/Act level while the book's actual chapters are still
			// real, independent spine files.
			if hasToc && !titleFromHeading && len(book.Chapters) > 0 {
				last := &book.Chapters[len(book.Chapters)-1]
				last.Blocks = append(last.Blocks, blocks...)
				continue
			}
			appendChapter(title, blocks)

		case len(entries) == 1 && entries[0].Anchor == "":
			// The common case: exactly one TOC entry, covering the whole file.
			appendChapter(entries[0].Title, blocks)

		default:
			// Multiple TOC entries share this one file - e.g. an Act/Part
			// whose own entry points at the top of the file, with its
			// Chapter 1, 2, 3... nested underneath as anchors into the
			// *same* file. Split the extracted blocks at each anchor's
			// position and give each segment its own chapter.
			type split struct {
				start    int
				title    string
				anchored bool // true if this came from a #anchor, not the whole-file entry
			}
			var splits []split
			for _, e := range entries {
				if e.Anchor == "" {
					splits = append(splits, split{start: 0, title: e.Title})
					continue
				}
				if idx, found := boundaries[e.Anchor]; found {
					splits = append(splits, split{start: idx, title: e.Title, anchored: true})
				}
				// An anchor that isn't actually in the document (malformed
				// epub) is just dropped rather than inventing a chapter at
				// a made-up position.
			}
			if len(splits) == 0 {
				appendChapter(title, blocks)
				break
			}
			// Sort by position; when a whole-file entry (e.g. "Act One") and
			// an anchored one (e.g. its "Chapter 1") land on the very same
			// position - no lead-in content of its own before the first
			// chapter starts - the anchored, more specific title wins the
			// slot and the whole-file entry contributes nothing, rather than
			// the reverse (which would silently swallow "Chapter 1"'s name).
			sort.SliceStable(splits, func(i, j int) bool {
				if splits[i].start != splits[j].start {
					return splits[i].start < splits[j].start
				}
				return splits[i].anchored && !splits[j].anchored
			})
			deduped := splits[:1]
			for _, s := range splits[1:] {
				if s.start != deduped[len(deduped)-1].start {
					deduped = append(deduped, s)
				}
			}
			splits = deduped

			if splits[0].start > 0 {
				// Content before the first real split point isn't under any
				// of this file's TOC titles - fold it into the previous
				// overall chapter if there is one, else just let the first
				// segment start from the top of the file instead.
				if len(book.Chapters) > 0 {
					last := &book.Chapters[len(book.Chapters)-1]
					last.Blocks = append(last.Blocks, blocks[:splits[0].start]...)
				} else {
					splits[0].start = 0
				}
			}

			for i, s := range splits {
				end := len(blocks)
				if i+1 < len(splits) {
					end = splits[i+1].start
				}
				appendChapter(s.title, blocks[s.start:end])
			}
		}
	}

	if len(book.Chapters) == 0 {
		return nil, fmt.Errorf("no readable chapters found in epub")
	}

	return book, nil
}

func firstNonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// parseSeriesIndex parses a raw series-position string (calibre:series_index
// content, or an EPUB3 group-position value) into a float, tolerating
// fractional positions (e.g. "2.5" for a novella between two entries).
// Returns 0 if s is empty or not a number.
func parseSeriesIndex(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// startsWithTitle reports whether the first text block (skipping any
// leading images) is exactly the chapter title already.
func startsWithTitle(blocks []Block, title string) bool {
	for _, b := range blocks {
		if b.Kind != BlockText {
			continue
		}
		return b.Text == title
	}
	return false
}

func readZipFile(files map[string]*zip.File, name string) ([]byte, error) {
	f, ok := files[name]
	if !ok {
		return nil, fmt.Errorf("not found in archive: %s", name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// resolveHref joins an OPF-relative or spine-relative href (a URI, possibly
// percent-encoded and possibly carrying a #fragment) against the directory
// it was declared in, and returns a cleaned zip-entry-style path.
func resolveHref(baseDir, href string) string {
	p, _ := resolveHrefWithFragment(baseDir, href)
	return p
}

// resolveHrefWithFragment is resolveHref but also returns the #fragment
// (without the '#'), e.g. for a TOC link into the middle of a shared file.
func resolveHrefWithFragment(baseDir, href string) (string, string) {
	if h, err := url.PathUnescape(href); err == nil {
		href = h
	}
	fragment := ""
	if i := strings.IndexByte(href, '#'); i >= 0 {
		fragment = href[i+1:]
		href = href[:i]
	}
	return path.Clean(path.Join(baseDir, href)), fragment
}

func findOPFPath(files map[string]*zip.File) (string, error) {
	data, err := readZipFile(files, "META-INF/container.xml")
	if err != nil {
		return "", fmt.Errorf("read container.xml: %w", err)
	}
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("parse container.xml: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "rootfile" {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == "full-path" {
				return a.Value, nil
			}
		}
	}
	return "", fmt.Errorf("container.xml has no rootfile element")
}

type packageDoc struct {
	title       string
	author      string
	language    string
	manifest    map[string]manifestItem
	spine       []string
	coverID     string // EPUB2-style <meta name="cover" content="ID">
	ncxID       string // EPUB2-style <spine toc="ID">, pointing at the NCX manifest item
	seriesName  string // resolved from either calibre:series or an EPUB3 collection; see resolveSeries
	seriesIndex string // raw, possibly-fractional position string; parsed in Parse via parseSeriesIndex
}

// rawMeta is a <meta> element as read off the wire, before any
// interpretation - either the EPUB2/Calibre name="..." content="..." form,
// or the EPUB3 property="..." [id="..."] [refines="#other-id"]>text</meta>
// form. Both forms are collected uniformly and resolved afterward (see
// resolveSeries) since refines can point at an id seen earlier OR later in
// document order.
type rawMeta struct {
	name     string
	content  string
	property string
	id       string
	refines  string // target id, with any leading '#' stripped
	text     string // element text content (EPUB3 form)
}

func parsePackage(data []byte) (*packageDoc, error) {
	pkg := &packageDoc{manifest: make(map[string]manifestItem)}
	dec := xml.NewDecoder(strings.NewReader(string(data)))

	// stack of enclosing element local-names, e.g. ["package","metadata"]
	var stack []string
	inside := func(name string) bool {
		for _, s := range stack {
			if s == name {
				return true
			}
		}
		return false
	}

	var textTarget *string
	var textBuf strings.Builder

	var metas []rawMeta
	var curMeta *rawMeta // non-nil while inside a <meta>...</meta> element
	var metaTextBuf strings.Builder

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			switch {
			case inside("metadata") && (t.Name.Local == "title" || t.Name.Local == "creator" || t.Name.Local == "language"):
				textBuf.Reset()
				switch t.Name.Local {
				case "title":
					textTarget = &pkg.title
				case "creator":
					textTarget = &pkg.author
				case "language":
					textTarget = &pkg.language
				}
			case inside("metadata") && t.Name.Local == "meta":
				var m rawMeta
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "name":
						m.name = a.Value
					case "content":
						m.content = a.Value
					case "property":
						m.property = a.Value
					case "id":
						m.id = a.Value
					case "refines":
						m.refines = strings.TrimPrefix(a.Value, "#")
					}
				}
				if m.name == "cover" {
					pkg.coverID = m.content
				}
				metas = append(metas, m)
				curMeta = &metas[len(metas)-1]
				metaTextBuf.Reset()
			case t.Name.Local == "spine":
				for _, a := range t.Attr {
					if a.Name.Local == "toc" {
						pkg.ncxID = a.Value
					}
				}
			case inside("manifest") && t.Name.Local == "item":
				var id, href, mediaType, props string
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "id":
						id = a.Value
					case "href":
						href = a.Value
					case "media-type":
						mediaType = a.Value
					case "properties":
						props = a.Value
					}
				}
				if id != "" {
					pkg.manifest[id] = manifestItem{Href: href, MediaType: mediaType, Properties: props}
				}
			case inside("spine") && t.Name.Local == "itemref":
				var idref, linear string
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "idref":
						idref = a.Value
					case "linear":
						linear = a.Value
					}
				}
				if idref != "" && linear != "no" {
					pkg.spine = append(pkg.spine, idref)
				}
			}
		case xml.CharData:
			if textTarget != nil {
				textBuf.Write(t)
			}
			if curMeta != nil {
				metaTextBuf.Write(t)
			}
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			if textTarget != nil && (t.Name.Local == "title" || t.Name.Local == "creator" || t.Name.Local == "language") {
				if *textTarget == "" {
					*textTarget = strings.TrimSpace(textBuf.String())
				}
				textTarget = nil
			}
			if curMeta != nil && t.Name.Local == "meta" {
				curMeta.text = strings.TrimSpace(metaTextBuf.String())
				curMeta = nil
			}
		}
	}
	pkg.seriesName, pkg.seriesIndex = resolveSeries(metas)
	return pkg, nil
}

// resolveSeries extracts series name/position from a book's <meta>
// elements. Two conventions are supported: the Calibre convention
// (<meta name="calibre:series" content="..."/> and
// ...series_index="..."/>), which is checked first since it's by far the
// more common convention among real-world epubs, and the EPUB3 standard
// "belongs-to-collection" convention
// (<meta property="belongs-to-collection" id="c1">Name</meta> refined by
// <meta refines="#c1" property="collection-type">series</meta> and
// <meta refines="#c1" property="group-position">2</meta>). A collection
// with no collection-type refinement is treated as a series too, since
// that's the overwhelmingly common case in practice.
func resolveSeries(metas []rawMeta) (name, index string) {
	for _, m := range metas {
		switch m.name {
		case "calibre:series":
			name = m.content
		case "calibre:series_index":
			index = m.content
		}
	}
	if name != "" {
		return name, index
	}

	for _, m := range metas {
		if m.property != "belongs-to-collection" || m.id == "" || m.text == "" {
			continue
		}
		isSeries := true
		var position string
		for _, r := range metas {
			if r.refines != m.id {
				continue
			}
			switch r.property {
			case "collection-type":
				isSeries = r.text == "series"
			case "group-position":
				position = r.text
			}
		}
		if isSeries {
			return m.text, position
		}
	}
	return "", ""
}

func resolveCover(pkg *packageDoc) (manifestItem, bool) {
	for _, item := range pkg.manifest {
		if strings.Contains(item.Properties, "cover-image") {
			return item, true
		}
	}
	if pkg.coverID != "" {
		if item, ok := pkg.manifest[pkg.coverID]; ok && strings.HasPrefix(item.MediaType, "image/") {
			return item, true
		}
	}
	return manifestItem{}, false
}

// TocEntry is one link from the epub's own table of contents, in document
// (reading) order, with its target file and (if it points partway into a
// file shared with other entries, e.g. an Act containing several chapters)
// anchor fragment kept separate rather than collapsed together.
type TocEntry struct {
	Path   string // resolved file path, fragment stripped
	Anchor string // fragment, "" if the link points at the top of the file
	Title  string
}

// loadTocEntries returns every entry from the epub's own table of contents,
// in document order, preferring the EPUB3 nav document and falling back to
// the EPUB2 NCX. Returns nil if neither is present or parseable - callers
// just get no overrides. The full tree is flattened; grouping by Path is
// left to the caller, since what matters is "which entries share a file"
// and their relative order, not literal nesting depth.
func loadTocEntries(pkg *packageDoc, files map[string]*zip.File, opfDir string) []TocEntry {
	for _, item := range pkg.manifest {
		if !strings.Contains(item.Properties, "nav") {
			continue
		}
		navPath := resolveHref(opfDir, item.Href)
		data, err := readZipFile(files, navPath)
		if err != nil {
			continue
		}
		if entries := parseNavToc(data, path.Dir(navPath)); len(entries) > 0 {
			return entries
		}
	}

	if pkg.ncxID != "" {
		if item, ok := pkg.manifest[pkg.ncxID]; ok {
			ncxPath := resolveHref(opfDir, item.Href)
			if data, err := readZipFile(files, ncxPath); err == nil {
				if entries := parseNcxToc(data, path.Dir(ncxPath)); len(entries) > 0 {
					return entries
				}
			}
		}
	}

	return nil
}

// groupTocEntries buckets entries by target file, preserving each bucket's
// internal document order (so a whole-file "Act One" entry, if present,
// sorts before its nested "Chapter 1", "Chapter 2", ... entries into the
// same file).
func groupTocEntries(entries []TocEntry) map[string][]TocEntry {
	grouped := make(map[string][]TocEntry)
	for _, e := range entries {
		grouped[e.Path] = append(grouped[e.Path], e)
	}
	return grouped
}

// parseNavToc extracts every link from an EPUB3 navigation document, using
// ONLY the <nav> explicitly marked epub:type="toc" - never guessing at an
// untyped one. A nav document commonly also carries a "page-list" nav
// (print-page-number links, whose link text is short print-pagination
// codes, not chapter names) and/or a "landmarks" nav; picking either of
// those by accident produces exactly the kind of garbage titles this
// function exists to avoid. No properly marked toc nav means no entries,
// which is safer than guessing wrong.
//
// Nested <ol> sub-lists (an Act containing its own Chapter links) are
// walked fully, each becoming its own entry - the "which one wins" problem
// only existed for the case that's actually still handled here, an <a>
// pointing at the same *file* as an ancestor (a true sub-section, not a
// sibling chapter): entries keep document order, so a shared-file conflict
// is resolved later by treating the outer (earlier) one as position 0 and
// letting the caller split blocks[0:] between them.
func parseNavToc(data []byte, navDir string) []TocEntry {
	doc, err := html.Parse(strings.NewReader(string(data)))
	if err != nil {
		return nil
	}

	var toc *html.Node
	var findToc func(n *html.Node)
	findToc = func(n *html.Node) {
		if toc != nil {
			return
		}
		if n.Type == html.ElementNode && n.Data == "nav" && strings.Contains(attr(n, "epub:type"), "toc") {
			toc = n
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			findToc(c)
		}
	}
	findToc(doc)
	if toc == nil {
		return nil
	}

	var entries []TocEntry
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" {
			href := attr(n, "href")
			text, _ := extractEmphasis(normalizeWhitespace(collectText(n)))
			if href != "" && text != "" {
				p, frag := resolveHrefWithFragment(navDir, href)
				entries = append(entries, TocEntry{Path: p, Anchor: frag, Title: text})
			}
			return // an <a>'s own children are just its link text, not more structure
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(toc)
	return entries
}

// parseNcxToc extracts every link from an EPUB2 NCX document's <navMap>,
// in document order, at any nesting depth. Each <navPoint> is tracked with
// its own frame on a stack so a nested navPoint's src/label capture can't
// clobber its still-open parent's (a flat set of shared variables would
// break here, since a child's closing tag - and so its own contribution to
// the result - is always processed before its parent's).
func parseNcxToc(data []byte, ncxDir string) []TocEntry {
	type frame struct {
		src   string
		label strings.Builder
	}

	var entries []TocEntry
	var stack []*frame
	var capturing *frame // the frame currently inside its <navLabel><text>

	dec := xml.NewDecoder(strings.NewReader(string(data)))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return entries
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "navPoint":
				stack = append(stack, &frame{})
			case "content":
				if len(stack) > 0 {
					top := stack[len(stack)-1]
					if top.src == "" {
						for _, a := range t.Attr {
							if a.Name.Local == "src" {
								top.src = a.Value
							}
						}
					}
				}
			case "text":
				if len(stack) > 0 && capturing == nil {
					top := stack[len(stack)-1]
					if top.label.Len() == 0 {
						capturing = top
					}
				}
			}
		case xml.CharData:
			if capturing != nil {
				capturing.label.Write(t)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "text":
				capturing = nil
			case "navPoint":
				if len(stack) > 0 {
					top := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					if top.src != "" {
						if title := normalizeWhitespace(top.label.String()); title != "" {
							p, frag := resolveHrefWithFragment(ncxDir, top.src)
							entries = append(entries, TocEntry{Path: p, Anchor: frag, Title: title})
						}
					}
				}
			}
		}
	}
	return entries
}

var blockTags = map[string]bool{
	"p": true, "div": true, "li": true, "blockquote": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"pre": true, "td": true, "figcaption": true,
}

var headingTags = map[string]bool{"h1": true, "h2": true, "h3": true}

// extractChapter parses an XHTML document and returns a best-effort title
// (the first heading found), whether that title actually came from a
// heading in the body (as opposed to <title> or nothing), and a slice of
// content blocks (text and inline images) in document order, with text
// tags stripped and whitespace normalized. Image blocks carry the actual
// decoded image bytes, read from the zip relative to itemDir (the chapter
// file's own directory, which is what "src" is relative to - not
// necessarily the OPF's directory).
//
// titleFromHeading matters beyond just picking a title: a spine file with
// its own heading is strong evidence it's a real, independent chapter even
// if the epub's table of contents doesn't happen to list it individually
// (some TOCs only go down to Part/Act level) - see its use in Parse.
//
// anchorIDs is the set of TOC-fragment ids that share this file (e.g. an
// Act split into several TOC-linked Chapters by id) - boundaries reports,
// for each one actually found, the index into `blocks` where its section
// starts, so Parse can split a single spine file into multiple chapters.
func extractChapter(
	raw []byte, files map[string]*zip.File, itemDir string, anchorIDs map[string]bool,
) (title string, titleFromHeading bool, blocks []Block, boundaries map[string]int) {
	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return "", false, nil, nil
	}
	boundaries = map[string]int{}

	addImage := func(src string) {
		if src == "" {
			return
		}
		imgPath := resolveHref(itemDir, src)
		data, err := readZipFile(files, imgPath)
		if err != nil {
			return // skip images that can't be read rather than failing the chapter
		}
		ext := strings.ToLower(path.Ext(imgPath))
		if ext == "" {
			return
		}
		blocks = append(blocks, Block{Kind: BlockImage, ImageData: data, ImageExt: ext})
	}

	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && len(anchorIDs) > 0 {
			id := attr(n, "id")
			if id == "" {
				id = attr(n, "name") // old-style <a name="..."> anchors
			}
			if id != "" && anchorIDs[id] {
				if _, seen := boundaries[id]; !seen {
					boundaries[id] = len(blocks)
				}
			}
		}
		if n.Type == html.ElementNode && n.Data == "img" {
			addImage(attr(n, "src"))
			return
		}
		if n.Type == html.ElementNode && n.Data == "hr" {
			// A real scene-break element. Previously fell all the way
			// through this walk with no match (hr is in neither blockTags
			// nor a handled leaf case) and no children to recurse into,
			// silently vanishing - the reader never got so much as a pause
			// where the source epub marked a scene change.
			blocks = append(blocks, Block{Kind: BlockBreak})
			return
		}
		if n.Type == html.ElementNode && blockTags[n.Data] {
			if containsBlockDescendant(n) {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				return
			}
			// A leaf block can still directly contain <img> alongside (or
			// instead of) text, e.g. a full-page illustration wrapped in a
			// <div> or <p>. Emit any such images first, then the text.
			for _, src := range findImageSrcs(n) {
				addImage(src)
			}
			text := collectText(n)
			if text != "" {
				if isSceneBreakText(text) {
					// A scene break spelled out as text rather than a real
					// <hr/> - "* * *", "***", a lone "⁂" asterism, and so
					// on. Previously sailed straight through as an ordinary
					// BlockText paragraph, so the TTS worker read it
					// literally aloud ("star star star" for "* * *").
					blocks = append(blocks, Block{Kind: BlockBreak})
					return
				}
				blockIdx := 0
				for _, seg := range splitQuoteSegments(text) {
					cleanText, emphasis := extractEmphasis(seg.Text)
					blocks = append(blocks, Block{Kind: BlockText, Text: cleanText, Inline: blockIdx > 0, IsQuote: seg.IsQuote, Emphasis: emphasis})
					blockIdx++
				}
				if title == "" && headingTags[n.Data] {
					// Discard any emphasis spans here - a heading used as
					// a chapter title has nothing to apply them to.
					cleanTitle, _ := extractEmphasis(text)
					title = cleanTitle
					titleFromHeading = true
				}
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if title == "" {
		title = findTitleTag(doc)
	}
	return title, titleFromHeading, blocks, boundaries
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func findImageSrcs(n *html.Node) []string {
	var out []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "img" {
			if src := attr(n, "src"); src != "" {
				out = append(out, src)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

func containsBlockDescendant(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && blockTags[c.Data] {
			return true
		}
		if containsBlockDescendant(c) {
			return true
		}
	}
	return false
}

// boldTags/italicTags are the epub markup elements this app treats as a
// structural emphasis signal, split into the two conventional prose
// categories - <strong>/<b> and <em>/<i> - since each gets a different
// generation-time treatment (see extractEmphasis's own doc comment):
// bold respells in ALL CAPS, italic wraps in "scare quotes". No wired
// clone model exposes a dedicated stress/emphasis control token
// (confirmed against Higgs Audio v3 TTS's own 43-tag vocabulary - see
// internal/speakerattr's validSentenceTags/validInlineTags), so there's
// no delivery tag to reach for here the way there is for shouting/
// surprise/etc.
var boldTags = map[string]bool{"strong": true, "b": true}
var italicTags = map[string]bool{"em": true, "i": true}

// boldStart/boldEnd/italicStart/italicEnd are Unicode Private Use Area
// sentinels collectText wraps around a bold/italic element's own text
// content with - guaranteed never to collide with real book text, and
// treated as ordinary, non-whitespace, non-quote/sentence-boundary runes
// by every text-processing step between here and extractEmphasis (see its
// own doc comment), so a sentinel pair survives splitQuoteSegments intact,
// still wrapping the same words, regardless of how that function slices/
// rejoins the surrounding text. Written as rune()
// conversions rather than literal rune constants so this source file
// stays plain ASCII.
var (
	boldStart   = rune(0xE000)
	boldEnd     = rune(0xE001)
	italicStart = rune(0xE002)
	italicEnd   = rune(0xE003)
)

// collectText flattens n's own text content, wrapping any boldTags/
// italicTags descendant's content in the matching sentinel pair
// (extracted later by extractEmphasis, once per final split chunk - see
// this function's own call site) - each category's own nesting depth is
// tracked independently, so nested markup of the *same* category
// (<b>very <b>truly</b> important</b>) emits just one flat, non-nested
// sentinel pair around the whole nested range (the semantically correct
// outcome - the entire range shares that treatment either way) rather
// than two improperly-nested pairs extractEmphasis would have to unwind.
// Bold and italic nest independently of each other
// (<strong><em>word</em></strong>) - see extractEmphasis's own doc
// comment for how the two are then merged into one "both" treatment
// rather than one silently overriding the other.
func collectText(n *html.Node) string {
	var b strings.Builder
	boldDepth, italicDepth := 0, 0
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
		case html.ElementNode:
			if n.Data == "br" {
				b.WriteByte(' ')
				return
			}
			if boldTags[n.Data] || italicTags[n.Data] {
				bold := boldTags[n.Data]
				if bold && boldDepth == 0 {
					b.WriteRune(boldStart)
				} else if !bold && italicDepth == 0 {
					b.WriteRune(italicStart)
				}
				if bold {
					boldDepth++
				} else {
					italicDepth++
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					walk(c)
				}
				if bold {
					boldDepth--
				} else {
					italicDepth--
				}
				if bold && boldDepth == 0 {
					b.WriteRune(boldEnd)
				} else if !bold && italicDepth == 0 {
					b.WriteRune(italicEnd)
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return normalizeWhitespace(b.String())
}

// emphasisSpan is one bold and/or italic range found by extractEmphasis's
// own scan, keyed by its [start, end) byte range in the cleaned output
// text so an exact-range bold+italic overlap (the common "both" case -
// see extractEmphasis's own doc comment) merges into one entry instead of
// two separately-tracked spans.
type emphasisSpan struct {
	bold, italic bool
}

// extractEmphasis scans text for boldStart/boldEnd/italicStart/italicEnd-
// wrapped spans and returns text with every sentinel removed
// (byte-identical to the original source wording - this is what ends up
// as the reader's own on-screen paragraph text) plus one
// pronounce.Substitution per span found, applied only at generation time
// (store.Paragraph.ResolveGenerationText, via pronounce.Apply) exactly
// like a pronunciation fix, never touching the text returned here:
//   - Bold only: respelled in upper case.
//   - Italic only: wrapped in "scare quotes".
//   - Both (an exact-range overlap, e.g. <strong><em>word</em></strong>):
//     both at once - upper case, then quoted - rather than letting one
//     silently win the way two independent, identically-ranged
//     substitutions would (pronounce.Apply drops the second of two
//     overlapping edits rather than corrupting the output).
//
// Called once per quote/narration segment (see this function's own call
// site in extractChapter), so offsets are computed directly against that
// segment's own final text in one pass - no cross-boundary offset math
// needed even though splitQuoteSegments may have sliced and rejoined the
// text a sentinel pair traveled through beforehand. Safe
// against a malformed/unpaired sentinel (should never happen - collectText
// always writes both of a pair, matched by construction): an unpaired
// start sentinel with no matching end is simply dropped with no
// substitution recorded, same as a paragraph with no emphasis at all.
func extractEmphasis(text string) (string, []pronounce.Substitution) {
	type key struct{ start, end int }
	spans := make(map[key]emphasisSpan)
	var order []key
	record := func(start, end int, bold bool) {
		if end <= start {
			return
		}
		k := key{start, end}
		sp, ok := spans[k]
		if !ok {
			order = append(order, k)
		}
		if bold {
			sp.bold = true
		} else {
			sp.italic = true
		}
		spans[k] = sp
	}

	var b strings.Builder
	boldOpen, italicOpen := -1, -1
	for _, r := range text {
		switch r {
		case boldStart:
			boldOpen = b.Len()
		case boldEnd:
			if boldOpen >= 0 {
				record(boldOpen, b.Len(), true)
				boldOpen = -1
			}
		case italicStart:
			italicOpen = b.Len()
		case italicEnd:
			if italicOpen >= 0 {
				record(italicOpen, b.Len(), false)
				italicOpen = -1
			}
		default:
			b.WriteRune(r)
		}
	}
	clean := b.String()

	var subs []pronounce.Substitution
	for _, k := range order {
		sp := spans[k]
		inner := clean[k.start:k.end]
		replacement := inner
		if sp.bold {
			replacement = strings.ToUpper(replacement)
		}
		if sp.italic {
			replacement = `"` + replacement + `"`
		}
		subs = append(subs, pronounce.Substitution{
			Offset:      k.start,
			Length:      k.end - k.start,
			Replacement: replacement,
		})
	}
	return clean, subs
}

// sceneBreakGlyphs is the closed set of characters a prose scene/section
// break is conventionally spelled out with when an epub doesn't use a real
// <hr/> element - a handful of repeated symbols, e.g. "* * *", "***",
// "-=-=-=-", or a single "⁂" asterism (the dedicated Unicode character for
// exactly this purpose). Deliberately contains no letter or digit, so
// isSceneBreakText can never misidentify even a very short real line of
// narration ("No.", "Wait.") as a break.
var sceneBreakGlyphs = map[rune]bool{
	'*': true, '#': true, '~': true, '-': true, '_': true, '=': true, '+': true,
	'•': true, '·': true, '∙': true, '○': true, '◦': true, '§': true,
	'‡': true, '†': true, '❦': true, '❧': true, '⁂': true, '⁕': true,
	'♦': true, '♠': true, '❈': true, '✦': true, '✧': true, '⋅': true,
}

// maxSceneBreakGlyphs bounds how many marker characters a candidate line
// can contain before isSceneBreakText gives up and treats it as ordinary
// text instead - real breaks are always a short, repeated run ("* * *",
// "-=-=-=-="), so this stays generous while still refusing to swallow an
// implausibly long line.
const maxSceneBreakGlyphs = 12

// isSceneBreakText reports whether text (already whitespace-normalized by
// collectText, and possibly still carrying boldStart/boldEnd/italicStart/
// italicEnd sentinels - ignored here, since a scene-break line is never
// meaningfully bold/italic) consists of nothing but sceneBreakGlyphs. See
// that var's own doc comment for why this can only ever be a false
// negative (missing a break formatted with an unlisted glyph), never a
// false positive against real narration.
func isSceneBreakText(text string) bool {
	n := 0
	for _, r := range text {
		switch {
		case r == boldStart || r == boldEnd || r == italicStart || r == italicEnd:
			continue
		case unicode.IsSpace(r):
			continue
		case sceneBreakGlyphs[r]:
			n++
		default:
			return false
		}
	}
	return n > 0 && n <= maxSceneBreakGlyphs
}

func normalizeWhitespace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

// quoteOpeners/quoteClosers are the double-quote characters this app
// recognizes as bounding spoken dialogue - straight ASCII " (which serves
// as both opener and closer), curly “ ” (which don't), guillemets « »
// (French/Russian/Spanish/Italian/Greek-style quotes, also directional),
// and angle brackets < > - directional the same way, and only reachable
// here as literal runes via a source epub's own &lt;/&gt; entities (a raw
// `<` in XHTML text would otherwise open a tag, so golang.org/x/net/html
// only ever hands collectText a literal `<`/`>` rune after decoding an
// entity), making them unambiguous to treat as quote marks rather than
// markup. Single quotes are deliberately excluded: they're far more often
// an apostrophe (contraction, possessive) than nested dialogue, and
// getting that wrong would wrongly fragment ordinary narration.
const (
	quoteOpeners = "\"“«<"
	quoteClosers = "\"”»>"
)

// quoteSegment is one piece of splitQuoteSegments' output - its text plus
// whether it's an actual quoted-dialogue span (IsQuote) as opposed to a
// narration/description segment. See Block.IsQuote for why this
// distinction is kept all the way through to attribution.
type quoteSegment struct {
	Text    string
	IsQuote bool
}

// dialogueTerminators are the runes a genuine quoted line of spoken
// dialogue almost always ends on, immediately before its closing quote
// mark: a full stop, exclamation, question mark, ellipsis, em dash
// (interrupted speech), or comma (dialogue continuing into a tag right
// after, e.g. `"Ahem," I said`). See looksLikeDialogue.
const dialogueTerminators = ".!?,…—"

// looksLikeDialogue reports whether the quoted span runes[start:end]
// (quote marks included, per span's own doc comment) plausibly represents
// spoken dialogue rather than a scare-quoted word/phrase or an in-text
// mention (`the "downward" stairway`, `felt "right" to leave`,
// `crossed "become the captain of my own ship" off my list`) - real false
// positives pulled from actual books during development, where a
// syntactically-detected quote span was neither dialogue nor
// attributable to any speaker, but a purely syntactic splitter (and, for
// what it's worth, BookNLP's own trained quote-span model - checked
// against the same chapters) both flag it as one anyway. The check itself
// is narrow and cheap: the last non-whitespace rune immediately before
// the closing quote mark, tested against dialogueTerminators. A
// scare-quote/mention sits mid-sentence with the surrounding prose
// continuing right past the closing quote, so it essentially never ends
// on one of these; a genuine spoken line, however short, is essentially
// always punctuated as a complete utterance (`"Amateurs . . ."`,
// `"Damn!"`, `"Ahem,"`) - including books that write long stretches of
// bare, untagged back-and-forth dialogue, since this check has nothing to
// do with whether a tag is present, only with how the quoted text itself
// ends. Deliberately one-directional - splitQuoteSegments only ever calls
// this to decide whether to keep a syntactically-found span as a quote,
// never to invent one text doesn't otherwise have - so it can only trade
// away a few false positives, never cost real dialogue recall.
func looksLikeDialogue(runes []rune, start, end int) bool {
	inner := strings.TrimRightFunc(string(runes[start+1:end-1]), unicode.IsSpace)
	if inner == "" {
		return false
	}
	last, _ := utf8.DecodeLastRuneInString(inner)
	return strings.ContainsRune(dialogueTerminators, last)
}

// splitQuoteSegments splits text into alternating narration and quoted-
// dialogue segments on quote-mark boundaries (quoteOpeners/quoteClosers -
// straight ", curly “ ”, guillemets « », and angle brackets < >), each
// segment including its own quote marks, so a paragraph like `”The bridge
// is out,” Sam said.` becomes [{`”The bridge is out,”`, true}, {`Sam
// said.`, false}] - two separately attributable/voiceable units (see
// internal/speakerattr, internal/narration) that the reader still sees as
// one paragraph (see Block.Inline). A straight " toggles in/out of a quote
// (it's both opener and closer); every other pair only opens on its own
// left-hand rune and only closes on its own right-hand rune. Unbalanced or
// quote-free text comes back as a single unsplit, non-quote segment -
// splitting only happens where a quote clearly opens and closes within the
// same paragraph, never on a guess. A span that opens and closes cleanly
// but fails looksLikeDialogue is dropped from spans entirely (not kept as
// its own non-quote segment) - its text simply merges back into whichever
// narration surrounds it, exactly as if no quote marks had been there at
// all, rather than fragmenting one flowing sentence into extra
// single-word paragraphs/audio clips.
func splitQuoteSegments(text string) []quoteSegment {
	type span struct{ start, end int } // rune offsets, end exclusive, quote marks included

	runes := []rune(text)
	var spans []span
	inQuote := false
	quoteStart := 0
	for i, ch := range runes {
		switch {
		case !inQuote && strings.ContainsRune(quoteOpeners, ch):
			inQuote = true
			quoteStart = i
		case inQuote && strings.ContainsRune(quoteClosers, ch):
			if looksLikeDialogue(runes, quoteStart, i+1) {
				spans = append(spans, span{quoteStart, i + 1})
			}
			inQuote = false
		}
	}
	if len(spans) == 0 {
		return []quoteSegment{{Text: text}}
	}

	var segments []quoteSegment
	pos := 0
	for _, sp := range spans {
		if sp.start > pos {
			if s := strings.TrimSpace(string(runes[pos:sp.start])); s != "" {
				segments = append(segments, quoteSegment{Text: s})
			}
		}
		segments = append(segments, quoteSegment{Text: string(runes[sp.start:sp.end]), IsQuote: true})
		pos = sp.end
	}
	if pos < len(runes) {
		if s := strings.TrimSpace(string(runes[pos:])); s != "" {
			segments = append(segments, quoteSegment{Text: s})
		}
	}
	// segments always has at least one element here (each span is
	// unconditionally appended above), so no "did we actually split"
	// fallback is needed - unlike the len(spans)==0 case above, there's
	// nothing to discard: a paragraph that's entirely one quote with no
	// surrounding narration (e.g. a standalone `"I don't know what to
	// do."` paragraph) correctly comes back as a single IsQuote=true
	// segment, not the plain-text fallback. An earlier version of this
	// function returned []quoteSegment{{Text: text}} (silently dropping
	// IsQuote) whenever len(segments) <= 1, which - since a whole-paragraph
	// quote always produces exactly one segment - meant every standalone
	// dialogue paragraph with no narration tag lost its IsQuote flag
	// entirely, forcing it to be treated as narration by every downstream
	// IsQuote-gated behavior (attribution's own prompt hints, and
	// httpapi.attributeChapter's deterministic is_quote filter, which
	// would then force its speaker back to "Narrator" regardless of what
	// the model correctly identified it as).
	return segments
}

func findTitleTag(doc *html.Node) string {
	var found string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if found != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "title" && n.FirstChild != nil {
			found = normalizeWhitespace(n.FirstChild.Data)
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}
