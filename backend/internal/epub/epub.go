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

	"github.com/rhino1998/lectable/backend/internal/pronounce"
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

// maxEntrySize caps how much of one archive entry readZipFile will
// decompress - far above any real chapter, image or package document, but
// low enough that a malformed epub or zip bomb fails instead of exhausting
// memory. The header's declared size isn't trusted: the read itself is
// limited.
const maxEntrySize = 64 << 20

func readZipFile(files map[string]*zip.File, name string) ([]byte, error) {
	f, ok := files[name]
	if !ok {
		return nil, fmt.Errorf("not found in archive: %s", name)
	}
	if f.UncompressedSize64 > maxEntrySize {
		return nil, fmt.Errorf("%s: %d bytes uncompressed, over the %d-byte limit", name, f.UncompressedSize64, maxEntrySize)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, maxEntrySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEntrySize {
		return nil, fmt.Errorf("%s: over the %d-byte uncompressed limit", name, maxEntrySize)
	}
	return data, nil
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
