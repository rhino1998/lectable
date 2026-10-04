package epub

import (
	"archive/zip"
	"encoding/xml"
	"io"
	"path"
	"strings"

	"golang.org/x/net/html"
)

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
			text, _ := extractEmphasis(plainText(collectText(n)))
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
