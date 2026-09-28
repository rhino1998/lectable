package epub

import (
	"archive/zip"
	"path"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"golang.org/x/net/html"
)

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
			pieces := strings.Split(text, string(paragraphBreak))
			if len(pieces) > 1 {
				for _, piece := range pieces {
					piece = strings.TrimSpace(piece)
					if piece == "" {
						continue
					}
					if isSceneBreakText(piece) {
						blocks = append(blocks, Block{Kind: BlockBreak})
						continue
					}
					blockIdx := 0
					for _, seg := range splitQuoteSegments(piece) {
						cleanText, emphasis := extractEmphasis(seg.Text)
						blocks = append(blocks, Block{Kind: BlockText, Text: cleanText, Inline: blockIdx > 0, IsQuote: seg.IsQuote, Emphasis: emphasis})
						blockIdx++
					}
				}
				text = strings.Join(strings.Fields(strings.ReplaceAll(text, string(paragraphBreak), " ")), " ")
				if title == "" && headingTags[n.Data] && text != "" {
					cleanTitle, _ := extractEmphasis(text)
					title = cleanTitle
					titleFromHeading = true
				}
				return
			}
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

// paragraphBreak is another Private Use Area sentinel, written by
// collectText in place of the second of two consecutive <br>s (with only
// whitespace between them). Some epubs put a whole scene inside one <p>
// and separate its real paragraphs with <br/><br/> - flattened to spaces,
// every turn of dialogue in that scene became one giant paragraph, which
// both reads as a wall of text and tells speakerattr those turns are one
// continuous speaker's beat. extractChapter splits a block's text on this
// sentinel into separate paragraphs. A single <br> stays a line break (a
// space), so verse and addresses laid out with one <br> per line are
// unaffected.
var paragraphBreak = rune(0xE004)

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
	// lastBr is b.Len() just after the most recent single <br>'s space, or
	// -1 - a second <br> with only whitespace written since becomes a
	// paragraphBreak instead (see its own doc comment).
	lastBr := -1
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			b.WriteString(n.Data)
		case html.ElementNode:
			if n.Data == "br" {
				if lastBr >= 0 && strings.TrimSpace(b.String()[lastBr:]) == "" {
					b.WriteRune(paragraphBreak)
					lastBr = -1
					return
				}
				b.WriteByte(' ')
				lastBr = b.Len()
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
