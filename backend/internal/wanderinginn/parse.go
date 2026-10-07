package wanderinginn

import (
	"bytes"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/websource"
	"golang.org/x/net/html"
)

// volume is one volume of the table of contents.
type volume struct {
	id       string  // "vol-3", the wrapper's own id
	number   float64 // 3
	title    string  // "Volume 3"
	coverURL string  // the volume's first published book's cover, "" if none
	chapters []websource.ChapterRef
}

var (
	volumeID  = regexp.MustCompile(`^vol-(\d+)$`)
	thumbSize = regexp.MustCompile(`-\d+x\d+(\.[A-Za-z0-9]+)$`)
)

// parseTOC reads the table-of-contents page. Each volume is a
//
//	<div id="vol-3" class="volume-wrapper">
//
// holding its name (h2.volume-title), cards for the published books made
// from it (img.book-cover - a thumbnail, "...-150x150.jpg", of an image
// stored without that suffix), and one div.chapter-entry per chapter,
// whose "web serial" cell (div.body-web) links to the chapter; the other
// cells say where the chapter falls in the audiobook and ebook - the
// ebook cell (div.body-ebook) names the ebook's chapter or, one to a line,
// chapters ("1.00<br>1.01"), which become the chapter's Aliases. A
// chapter's id is its address's path (2017/03/03/rw1-00): the list gives
// nothing else that stays put.
//
// Until 2023 the page was plain text instead (as the Internet Archive
// still has it): inside div.entry-content, a paragraph "Volume 3" in bold
// and after it paragraphs of chapter links - see parsePlainTOC.
func parseTOC(page []byte, pageURL string) ([]volume, error) {
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, err
	}
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, err
	}
	var vols []volume
	walk(doc, func(n *html.Node) bool {
		m := volumeID.FindStringSubmatch(attr(n, "id"))
		if m == nil || !hasClass(n, "volume-wrapper") {
			return true
		}
		v := volume{id: m[0], title: "Volume " + m[1]}
		v.number, _ = strconv.ParseFloat(m[1], 64)
		seen := map[string]bool{}
		walk(n, func(c *html.Node) bool {
			switch {
			case hasClass(c, "volume-title"):
				if t := squash(textOf(c)); t != "" {
					v.title = t
				}
			case c.Data == "img" && hasClass(c, "book-cover") && v.coverURL == "":
				if u, err := base.Parse(attr(c, "src")); err == nil && attr(c, "src") != "" {
					v.coverURL = thumbSize.ReplaceAllString(u.String(), "$1")
				}
			case hasClass(c, "body-web"):
				link := first(c, func(a *html.Node) bool { return a.Data == "a" && attr(a, "href") != "" })
				if link == nil {
					return false
				}
				u, err := base.Parse(attr(link, "href"))
				if err != nil {
					return false
				}
				id := strings.Trim(u.Path, "/")
				title := squash(textOf(link))
				if id == "" || title == "" || seen[id] {
					return false
				}
				seen[id] = true
				u.Fragment = ""
				v.chapters = append(v.chapters, websource.ChapterRef{ID: id, Title: title, URL: u.String(), Unlocked: true})
				return false
			case hasClass(c, "body-ebook"):
				// The row's ebook cell comes after its web cell.
				if n := len(v.chapters); n > 0 && v.chapters[n-1].Aliases == nil {
					v.chapters[n-1].Aliases = lines(c)
				}
				return false
			}
			return true
		})
		if len(v.chapters) > 0 {
			vols = append(vols, v)
		}
		return false
	})
	if len(vols) == 0 {
		vols = parsePlainTOC(doc, base)
	}
	if len(vols) == 0 {
		return nil, errors.New("no volumes on The Wandering Inn's table of contents (has the page changed?)")
	}
	return vols, nil
}

var plainVolume = regexp.MustCompile(`^Volume (\d+)$`)

// parsePlainTOC reads the table of contents as it was until 2023: every
// chapter link under div.entry-content belongs to the volume whose bold
// "Volume N" paragraph came last before it.
func parsePlainTOC(doc *html.Node, base *url.URL) []volume {
	content := first(doc, func(n *html.Node) bool { return n.Data == "div" && hasClass(n, "entry-content") })
	if content == nil {
		return nil
	}
	var vols []volume
	seen := map[string]bool{}
	walk(content, func(n *html.Node) bool {
		switch n.Data {
		case "strong", "b":
			if m := plainVolume.FindStringSubmatch(squash(textOf(n))); m != nil {
				v := volume{id: "vol-" + m[1], title: m[0]}
				v.number, _ = strconv.ParseFloat(m[1], 64)
				vols = append(vols, v)
			}
			return false
		case "a":
			u, err := base.Parse(attr(n, "href"))
			if err != nil || len(vols) == 0 {
				return false
			}
			id := strings.Trim(u.Path, "/")
			title := squash(textOf(n))
			if !chapterPath.MatchString(id) || title == "" || seen[id] {
				return false
			}
			seen[id] = true
			u.Fragment = ""
			v := &vols[len(vols)-1]
			v.chapters = append(v.chapters, websource.ChapterRef{ID: id, Title: title, URL: u.String(), Unlocked: true})
			return false
		}
		return true
	})
	kept := vols[:0]
	for _, v := range vols {
		if len(v.chapters) > 0 {
			kept = append(kept, v)
		}
	}
	return kept
}

// lines returns an element's text split at its <br>s, blanks dropped.
func lines(n *html.Node) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if t := squash(cur.String()); t != "" {
			out = append(out, t)
		}
		cur.Reset()
	}
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			cur.WriteString(n.Data)
		case n.Type == html.ElementNode && n.Data == "br":
			flush()
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	flush()
	return out
}

// dropTags never carry chapter text.
var dropTags = map[string]bool{
	"script": true, "style": true, "iframe": true, "noscript": true, "form": true, "input": true,
	"button": true, "link": true, "meta": true, "object": true, "embed": true, "svg": true,
	"video": true, "audio": true,
}

// keepAttrs are the attributes the epub parser reads or a table needs;
// the rest (image metadata, text colors) is dropped.
var keepAttrs = map[string]bool{"href": true, "src": true, "alt": true, "colspan": true, "rowspan": true}

// cleanChapter finds a chapter page's text and strips it to the story. The
// text is the page's <article class="twi-article"> - or, on a page from
// before the 2026 redesign, its div.entry-content, which also held the
// "share this" and "like this" widgets (div.sharedaddy, #jp-post-flair).
// A chapter for patrons only has a div.patreon-protected-post in its
// place, or in earlier years a password form (form.post-password-form):
// websource.ErrLocked. Within the text:
//
//   - the last lines are a rule and a paragraph of "Previous Chapter" /
//     "Next Chapter" links, which go;
//   - an author's note closes many chapters: a paragraph opening with a
//     bold "Author's Note:", running to the end and usually followed by
//     reader art. It goes too, art included - it isn't the story, and reads
//     badly aloud. A note at the head of a chapter (rare) is kept: nothing
//     marks where it ends.
//
// Returns the element whose children are the chapter and the <img>
// elements left in it.
func cleanChapter(page []byte) (content *html.Node, images []*html.Node, err error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, nil, err
	}
	content = first(doc, func(n *html.Node) bool { return n.Data == "article" && hasClass(n, "twi-article") })
	if content == nil {
		if first(doc, func(n *html.Node) bool {
			return hasClass(n, "patreon-protected-post") || hasClass(n, "post-password-form")
		}) != nil {
			return nil, nil, websource.ErrLocked
		}
		content = first(doc, func(n *html.Node) bool { return n.Data == "div" && hasClass(n, "entry-content") })
	}
	if content == nil {
		return nil, nil, errors.New("no chapter text on the page (has the site changed?)")
	}
	// The older pages' widgets, inside the text's own element.
	for {
		widget := first(content, func(n *html.Node) bool {
			return n != content && (hasClass(n, "sharedaddy") || hasClass(n, "wpcnt") || attr(n, "id") == "jp-post-flair")
		})
		if widget == nil {
			break
		}
		detach(widget)
	}

	dropNavigation(content)
	dropAuthorsNote(content)

	var doomed []*html.Node
	walk(content, func(n *html.Node) bool {
		if n == content {
			return true
		}
		if dropTags[n.Data] || (n.Data == "img" && attr(n, "src") == "") {
			doomed = append(doomed, n)
			return false
		}
		if n.Data == "img" {
			images = append(images, n)
		}
		kept := n.Attr[:0]
		for _, a := range n.Attr {
			if keepAttrs[a.Key] {
				kept = append(kept, a)
			}
		}
		n.Attr = kept
		return true
	})
	for _, n := range doomed {
		detach(n)
	}
	return content, images, nil
}

// dropNavigation removes the chapter's closing "Previous Chapter / Next
// Chapter" paragraph and the rule above it. Only a trailing paragraph
// whose text is nothing but those links counts.
func dropNavigation(content *html.Node) {
	last := lastElement(content)
	if last == nil || !isNavigation(last) {
		return
	}
	prev := prevElement(last)
	detach(last)
	for prev != nil && (prev.Data == "hr" || squash(textOf(prev)) == "" && first(prev, isImage) == nil) {
		p := prevElement(prev)
		detach(prev)
		prev = p
	}
}

func isNavigation(n *html.Node) bool {
	if first(n, func(a *html.Node) bool { return a.Data == "a" }) == nil {
		return false
	}
	text := strings.ToLower(squash(textOf(n)))
	for _, w := range []string{"previous chapter", "next chapter"} {
		text = strings.ReplaceAll(text, w, "")
	}
	return strings.TrimSpace(text) == ""
}

var noteOpening = regexp.MustCompile(`(?i)^author['’]s notes?\b`)

// dropAuthorsNote removes a closing author's note: from the first
// paragraph in the chapter's second half that opens with a bold "Author's
// Note" through the end.
func dropAuthorsNote(content *html.Node) {
	var blocks []*html.Node
	for c := content.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			blocks = append(blocks, c)
		}
	}
	for i, b := range blocks {
		if i < len(blocks)/2 {
			continue
		}
		bold := first(b, func(n *html.Node) bool { return n.Data == "strong" || n.Data == "b" })
		if bold == nil || !noteOpening.MatchString(squash(textOf(bold))) || !noteOpening.MatchString(squash(textOf(b))) {
			continue
		}
		for n := b; n != nil; {
			next := n.NextSibling
			detach(n)
			n = next
		}
		// The blank paragraphs and rule that set the note off.
		for last := lastElement(content); last != nil && (last.Data == "hr" || squash(textOf(last)) == "" && first(last, isImage) == nil); last = lastElement(content) {
			detach(last)
		}
		return
	}
}

func isImage(n *html.Node) bool { return n.Data == "img" }

func lastElement(parent *html.Node) *html.Node {
	for c := parent.LastChild; c != nil; c = c.PrevSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

func prevElement(n *html.Node) *html.Node {
	for c := n.PrevSibling; c != nil; c = c.PrevSibling {
		if c.Type == html.ElementNode {
			return c
		}
	}
	return nil
}

// walk visits every element under n (n included), depth first; fn
// returning false skips that element's children.
func walk(n *html.Node, fn func(*html.Node) bool) {
	if n.Type == html.ElementNode && !fn(n) {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

// first returns the first element under n (n included) that match accepts.
func first(n *html.Node, match func(*html.Node) bool) *html.Node {
	var found *html.Node
	walk(n, func(c *html.Node) bool {
		if found == nil && match(c) {
			found = c
		}
		return found == nil
	})
	return found
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	for c := range strings.FieldsSeq(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}

// squash collapses whitespace, non-breaking spaces included.
func squash(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, " ", " ")), " ")
}

func detach(n *html.Node) {
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}
