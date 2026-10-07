package royalroad

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/net/html"
)

// Everything that depends on the site's markup is in this file. What each
// function relies on is said where it is read; in short:
//
//   - a fiction page states its metadata three times over - a schema.org
//     "Book" JSON-LD block, <meta>/<link rel="canonical"> tags, and the
//     visible header (.fic-header) - and its chapter list twice: a script
//     assigning window.chapters a JSON array, and table#chapters;
//   - a chapter page holds the chapter's text in one element classed
//     "chapter-content", with the author's notes, ads and navigation in
//     boxes beside it, never inside;
//   - the notices the site mixes into the text are ordinary elements whose
//     class a <style> block on the same page sets to display: none.

// parseFiction reads a fiction page fetched from pageURL (after
// redirects): title, author, cover, canonical URL and the chapter list in
// reading order. ID is left to the caller.
func parseFiction(page []byte, pageURL string) (*Fiction, error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, fmt.Errorf("parse fiction page: %w", err)
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil, fmt.Errorf("fiction page URL: %w", err)
	}
	p := readFictionPage(doc)

	chapters, found, err := p.chapterList(base)
	if err != nil {
		return nil, err
	}
	if !found {
		if !p.isFiction {
			return nil, errors.New("not a Royal Road fiction page")
		}
		return nil, errors.New("the fiction page has no chapter list")
	}

	f := &Fiction{
		Title:    firstNonEmpty(p.book.name, p.meta["og:title"], p.meta["twitter:title"], p.headerTitle, p.docTitle),
		Author:   firstNonEmpty(p.book.author, p.meta["books:author"], p.headerAuthor, p.meta["twitter:creator"]),
		URL:      pageURL,
		Chapters: chapters,
	}
	if f.Title == "" {
		return nil, errors.New("the fiction page has no title")
	}
	for _, raw := range []string{p.canonical, p.meta["og:url"], p.book.url} {
		if u := resolve(base, raw); u != "" {
			f.URL = u
			break
		}
	}
	for _, raw := range []string{p.book.image, p.meta["og:image"], p.meta["twitter:image"], p.headerCover} {
		u := resolve(base, raw)
		if u == "" {
			continue
		}
		// The first one stated decides: a fiction without a cover names
		// the site's placeholder everywhere a cover would be.
		if !isPlaceholderCover(u) {
			f.CoverURL = u
		}
		break
	}
	return f, nil
}

// fictionPage is what one walk over a fiction page collects.
type fictionPage struct {
	// meta is every <meta property|name=... content=...>, first one wins.
	meta      map[string]string
	canonical string // <link rel="canonical">
	docTitle  string // <title>, without the site's " | Royal Road"
	book      struct{ name, author, url, image string }

	// The visible header: <div class="... fic-header"> holds the cover
	// (<img data-type="cover">), the title (<h1>) and "by <a
	// href="/profile/N">author</a>".
	headerTitle, headerAuthor, headerCover string

	// chaptersJSON is the array a script assigns to window.chapters;
	// table is <table id="chapters">.
	chaptersJSON []byte
	table        *html.Node

	isFiction bool
}

var chaptersAssign = regexp.MustCompile(`window\.chapters\s*=\s*`)

func readFictionPage(doc *html.Node) *fictionPage {
	p := &fictionPage{meta: map[string]string{}}
	var walk func(n *html.Node, inHeader bool)
	walk = func(n *html.Node, inHeader bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "meta":
				key := attr(n, "property")
				if key == "" {
					key = attr(n, "name")
				}
				if _, ok := p.meta[key]; !ok && key != "" {
					p.meta[key] = strings.TrimSpace(attr(n, "content"))
				}
			case "link":
				if strings.EqualFold(attr(n, "rel"), "canonical") && p.canonical == "" {
					p.canonical = attr(n, "href")
				}
			case "title":
				if p.docTitle == "" {
					t, _, _ := strings.Cut(textOf(n), " | Royal Road")
					p.docTitle = t
				}
			case "script":
				p.readScript(n)
				return
			case "table":
				if attr(n, "id") == "chapters" && p.table == nil {
					p.table = n
				}
			case "h1":
				if inHeader && p.headerTitle == "" {
					p.headerTitle = textOf(n)
				}
			case "a":
				if inHeader && p.headerAuthor == "" && strings.HasPrefix(attr(n, "href"), "/profile/") {
					p.headerAuthor = textOf(n)
				}
			case "img":
				if inHeader && p.headerCover == "" && attr(n, "data-type") == "cover" {
					p.headerCover = attr(n, "src")
				}
			}
			if hasClass(n, "fic-header") {
				inHeader = true
				p.isFiction = true
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inHeader)
		}
	}
	walk(doc, false)
	if p.meta["og:type"] == "books.book" || p.book.name != "" {
		p.isFiction = true
	}
	return p
}

// readScript picks up the two scripts that carry data: the JSON-LD block
// describing the fiction as a schema.org Book, and the one assigning
// window.chapters.
func (p *fictionPage) readScript(n *html.Node) {
	if n.FirstChild == nil {
		return
	}
	src := n.FirstChild.Data
	if strings.EqualFold(strings.TrimSpace(attr(n, "type")), "application/ld+json") {
		var ld struct {
			Type   any    `json:"@type"`
			Name   string `json:"name"`
			URL    string `json:"url"`
			Image  any    `json:"image"`
			Author any    `json:"author"`
		}
		if json.Unmarshal([]byte(src), &ld) != nil || ld.Type != "Book" || p.book.name != "" {
			return
		}
		p.book.name = strings.Join(strings.Fields(ld.Name), " ")
		p.book.url = ld.URL
		p.book.image, _ = ld.Image.(string)
		if a, ok := ld.Author.(map[string]any); ok {
			name, _ := a["name"].(string)
			p.book.author = strings.Join(strings.Fields(name), " ")
		}
		return
	}
	if p.chaptersJSON != nil {
		return
	}
	if loc := chaptersAssign.FindStringIndex(src); loc != nil {
		// The array runs on to wherever its JSON ends; the script goes on
		// with other assignments after it.
		var raw json.RawMessage
		if json.NewDecoder(strings.NewReader(src[loc[1]:])).Decode(&raw) == nil && len(raw) > 0 && raw[0] == '[' {
			p.chaptersJSON = raw
		}
	}
}

var chapterPath = regexp.MustCompile(`/chapter/(\d+)(?:/|$)`)

// chapterList reads the chapter list, in the order the page gives it
// (reading order). found is false when the page has none at all; a list
// that is there but empty is a fiction with no chapters published.
//
// The script's array is preferred: each entry carries the chapter's id
// and whether an anonymous visitor can read it ("isUnlocked"), which the
// table does not say. Seen on every fiction looked at, volumes or not:
//
//	window.chapters = [{"id":301778,"title":"1. Good Morning Brother",
//	  "order":0,"isUnlocked":true,
//	  "url":"/fiction/21220/mother-of-learning/chapter/301778/1-good-morning-brother", ...}, ...];
//
// The table (<table id="chapters">, one <tr data-url="..."> per chapter,
// the title in its first link) lists the same chapters in the same order
// and is the fallback.
func (p *fictionPage) chapterList(base *url.URL) (list []ChapterRef, found bool, err error) {
	if p.chaptersJSON != nil {
		var entries []struct {
			ID         json.Number `json:"id"`
			Title      string      `json:"title"`
			URL        string      `json:"url"`
			IsUnlocked *bool       `json:"isUnlocked"`
		}
		if err := json.Unmarshal(p.chaptersJSON, &entries); err != nil {
			return nil, false, fmt.Errorf("read the fiction page's chapter list: %w", err)
		}
		list = make([]ChapterRef, 0, len(entries))
		for i, e := range entries {
			ref := ChapterRef{
				ID:       e.ID.String(),
				Title:    strings.Join(strings.Fields(e.Title), " "),
				URL:      resolve(base, e.URL),
				Unlocked: e.IsUnlocked == nil || *e.IsUnlocked,
			}
			if !isDecimal(ref.ID) || ref.URL == "" {
				return nil, false, fmt.Errorf("chapter list entry %d (%q) has no id or URL", i+1, ref.Title)
			}
			list = append(list, ref)
		}
		return list, true, nil
	}
	if p.table == nil {
		return nil, false, nil
	}
	list = []ChapterRef{}
	var rows func(n *html.Node) error
	rows = func(n *html.Node) error {
		if n.Type == html.ElementNode && n.Data == "tr" {
			link := find(n, func(c *html.Node) bool { return c.Data == "a" && attr(c, "href") != "" })
			raw := attr(n, "data-url")
			if raw == "" && link != nil {
				raw = attr(link, "href")
			}
			if raw == "" {
				return nil // the heading row
			}
			ref := ChapterRef{URL: resolve(base, raw), Unlocked: true}
			if m := chapterPath.FindStringSubmatch(raw); m != nil {
				ref.ID = m[1]
			}
			if link != nil {
				ref.Title = textOf(link)
			}
			if ref.ID == "" || ref.URL == "" {
				return fmt.Errorf("chapter table row %d (%q) has no chapter id in its link %q", len(list)+1, ref.Title, raw)
			}
			list = append(list, ref)
			return nil
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := rows(c); err != nil {
				return err
			}
		}
		return nil
	}
	if err := rows(p.table); err != nil {
		return nil, false, err
	}
	return list, true, nil
}

// isPlaceholderCover reports whether a cover URL is the site's stand-in
// for a fiction without one. Real covers are uploads on the site's CDN
// (royalroadcdn.com/public/covers-...); the stand-in is one of the site's
// own static assets, /dist/img/nocover-new-min.png.
func isPlaceholderCover(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return true
	}
	return strings.HasPrefix(parsed.Path, "/dist/") || strings.Contains(parsed.Path, "nocover")
}

// cleanChapter finds the chapter's own text in a chapter page and strips
// it, in place, to what the author wrote as the chapter. It returns the
// element whose children are that text and the <img> elements left in it.
// title is the chapter's title in the chapter list.
//
// The text is the children of the one element classed "chapter-content"
// (<div class="chapter-inner chapter-content">). The author's notes
// (<div class="portlet solid author-note-portlet">, before and/or after),
// ads, navigation and comments are its siblings or further out, so taking
// only this element leaves them all behind. A page without it - a chapter
// the site won't show - is an error; an element with nothing in it is a
// chapter that is only a note, and is returned empty.
func cleanChapter(page []byte, title string) (content *html.Node, images []*html.Node, err error) {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, nil, fmt.Errorf("parse chapter page: %w", err)
	}
	content = find(doc, func(n *html.Node) bool { return hasClass(n, "chapter-content") })
	if content == nil {
		content = find(doc, func(n *html.Node) bool { return hasClass(n, "chapter-inner") })
	}
	if content == nil {
		return nil, nil, errors.New("the page has no chapter text (the chapter may be locked or removed)")
	}

	hidden := hiddenClasses(doc)
	strip(content, hidden)
	dropNavigation(content)
	cleanAttrs(content)
	unwrapCells(content)
	dropTitleLine(content, title)
	prune(content)

	var collect func(n *html.Node)
	collect = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.Data == "img" {
				images = append(images, c)
			}
			collect(c)
		}
	}
	collect(content)
	return content, images, nil
}

// hiddenClasses returns the classes the page's own <style> blocks hide.
//
// The site mixes a notice or two into every chapter's text ("Stolen
// content warning: this content belongs on Royal Road. ..."), as
//
//	<span class="cmRlZjZh..."><br>notice<br></span>
//
// between two paragraphs, and keeps a browser from showing it with a
// <style> block in the page's <head>:
//
//	.cmRlZjZh...{ display: none; speak: never; }
//
// The class name is different on every page load and looks just like the
// ones real paragraphs carry (<p class="cnNmMDQx...">), so the style rule
// is the only thing that tells them apart. This reads every rule whose
// selector is a single class and whose declarations end up with display:
// none; rules inside @media and the like are skipped, since they hide
// only on some screens.
func hiddenClasses(doc *html.Node) map[string]bool {
	var css strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "style" {
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.TextNode {
					css.WriteString(c.Data)
					css.WriteByte('\n')
				}
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	hidden := map[string]bool{}
	rest := cssComment.ReplaceAllString(css.String(), " ")
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			break
		}
		// A stray "}" or ";" before the selector belongs to whatever came
		// before it.
		prelude := rest[:open]
		if i := strings.LastIndexAny(prelude, "};"); i >= 0 {
			prelude = prelude[i+1:]
		}
		rest = rest[open+1:]
		if strings.HasPrefix(strings.TrimSpace(prelude), "@") {
			// Skip the at-rule's whole block, nested rules included.
			depth := 1
			i := 0
			for ; i < len(rest) && depth > 0; i++ {
				switch rest[i] {
				case '{':
					depth++
				case '}':
					depth--
				}
			}
			rest = rest[i:]
			continue
		}
		body := rest
		if end := strings.IndexByte(rest, '}'); end >= 0 {
			body, rest = rest[:end], rest[end+1:]
		} else {
			rest = ""
		}
		if !displayNone(body) {
			continue
		}
		for _, sel := range strings.Split(prelude, ",") {
			if sel = strings.TrimSpace(sel); simpleClass.MatchString(sel) {
				hidden[sel[1:]] = true
			}
		}
	}
	return hidden
}

var (
	cssComment  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	simpleClass = regexp.MustCompile(`^\.[A-Za-z0-9_-]+$`)
)

// displayNone reports whether a CSS declaration list leaves display set
// to none (the last display declaration wins).
func displayNone(declarations string) bool {
	none := false
	for _, d := range strings.Split(declarations, ";") {
		name, value, ok := strings.Cut(d, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "display") {
			continue
		}
		value = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.ToLower(value)), "!important"))
		none = value == "none"
	}
	return none
}

// notText are elements that are never part of a chapter's text.
var notText = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
	"link": true, "meta": true, "base": true, "title": true, "head": true,
	"iframe": true, "frame": true, "object": true, "embed": true, "applet": true,
	"video": true, "audio": true, "source": true, "track": true, "canvas": true,
	"svg": true, "math": true, "map": true, "area": true,
	"form": true, "input": true, "button": true, "select": true, "textarea": true,
	"option": true, "dialog": true,
}

// siteBoxes are the classes of the site's own boxes: every one (author's
// note, ad, comment list) is a "portlet", and a note's text sits in an
// "author-note". None has been seen inside the chapter text; this only
// keeps one out if the site ever moves it there.
var siteBoxes = []string{"portlet", "author-note-portlet", "author-note"}

// kept are the attributes that still mean something once the text has
// left the site; classes, ids, inline styles, data-*, sizes and event
// handlers all go.
var kept = map[string]bool{"href": true, "src": true, "alt": true, "colspan": true, "rowspan": true}

// strip removes from n's descendants everything that isn't chapter text:
// scripts, forms and the like, what the page hides, the site's own boxes,
// and the frame of a spoiler (its text stays). Attributes are left for
// cleanAttrs, since telling navigation apart needs the links whole.
func strip(n *html.Node, hidden map[string]bool) {
	var next *html.Node
	for c := n.FirstChild; c != nil; c = next {
		next = c.NextSibling
		switch c.Type {
		case html.TextNode:
			continue
		case html.ElementNode:
		default: // comments, doctypes
			remove(c)
			continue
		}
		if notText[c.Data] || isHidden(c, hidden) || hasAnyClass(c, siteBoxes) {
			remove(c)
			continue
		}
		if c.Data == "a" {
			if addr, ok := protectedEmail(c); ok {
				// Text follows in the same place, so nothing is lost.
				n.InsertBefore(&html.Node{Type: html.TextNode, Data: addr}, c)
				remove(c)
				continue
			}
		}
		strip(c, hidden)
		switch {
		case hasClass(c, "spoiler"):
			spoilerHeading(c)
		case hasClass(c, "spoiler-new"):
			spoilerCaption(c)
		}
	}
}

// cleanAttrs drops every attribute not in kept from n's descendants, and
// each <img> that has nothing to fetch.
func cleanAttrs(n *html.Node) {
	var next *html.Node
	for c := n.FirstChild; c != nil; c = next {
		next = c.NextSibling
		if c.Type != html.ElementNode {
			continue
		}
		if c.Data == "img" && !usableSrc(attr(c, "src")) {
			remove(c)
			continue
		}
		attrs := c.Attr[:0]
		for _, a := range c.Attr {
			if a.Namespace != "" || !kept[a.Key] {
				continue
			}
			// A link the reader can't follow from the book keeps its text
			// only.
			if a.Key == "href" && !isWebURL(a.Val) {
				continue
			}
			attrs = append(attrs, a)
		}
		c.Attr = attrs
		cleanAttrs(c)
	}
}

// isHidden reports whether an element is one a reader of the page never
// sees: one of its classes is hidden by the page's style block (see
// hiddenClasses), its own style says display: none, or it has the hidden
// attribute.
//
// The body of a collapsed spoiler is the exception. It is served with
// display: none too, but in its own style attribute, as the child of a
// "spoilerContent" box - and the reader opens it with the button beside
// it, so it is chapter text (see spoilerHeading). The notices the site
// mixes in are never marked that way: they are hidden through a class
// and the page's style block, and nothing on the page shows them. In the
// chapters looked at, spoiler bodies were the only elements of chapter
// text with display: none in their own style.
func isHidden(n *html.Node, hidden map[string]bool) bool {
	inline := false
	for _, a := range n.Attr {
		switch a.Key {
		case "class":
			for _, class := range strings.Fields(a.Val) {
				if hidden[class] {
					return true
				}
			}
		case "style":
			inline = inline || displayNone(a.Val)
		case "hidden":
			inline = true
		}
	}
	return inline && !hasClass(n, "spoiler-inner") && (n.Parent == nil || !hasClass(n.Parent, "spoilerContent"))
}

// spoilerHeading removes a spoiler's frame, keeping what the author
// wrote. Part of a chapter - a status sheet, a glossary - can be folded
// away behind a button; the page comes with it folded, in one of two
// forms (the first from chapters written on the site's old forum):
//
//	<div class='spoiler'><div class='smalltext' style='margin-bottom:2px'><b>Spoiler</b> :
//	  <input type='button' value='Show' class='spoilerButton'></div>
//	<div class='spoilerContent'><div style='display: none;'> ...the text... </div></div></div>
//
//	<div class="spoiler">
//	<div class="smalltext" style="margin-bottom: 2px"><strong>Spoiler</strong>: V&amp;V stats
//	  <input class="spoilerButton" type="button" value="Show"></div>
//	<div class="spoilerContent">
//	<div class="spoiler-inner" style="display: none"> ...the text... </div></div></div>
//
// The body is kept by isHidden and the button is gone as a form control.
// Of the heading (.smalltext), the "Spoiler" label is the site's and what
// follows the colon the author's title - which is itself "Spoiler" when
// they gave none. So the label and colon go, and with them the whole
// heading unless a title of the author's own is left. A heading the
// author rewrote entirely has no such label and stays as it is.
func spoilerHeading(spoiler *html.Node) {
	var heading *html.Node
	for c := spoiler.FirstChild; c != nil; c = c.NextSibling {
		if hasClass(c, "smalltext") {
			heading = c
			break
		}
	}
	if heading == nil {
		return
	}
	label := heading.FirstChild
	for label != nil && label.Type == html.TextNode && strings.TrimSpace(label.Data) == "" {
		label = label.NextSibling
	}
	if label == nil || label.Type != html.ElementNode || (label.Data != "b" && label.Data != "strong") || !strings.EqualFold(textOf(label), "Spoiler") {
		return
	}
	if after := label.NextSibling; after != nil && after.Type == html.TextNode {
		after.Data = strings.TrimLeft(strings.TrimPrefix(strings.TrimLeft(after.Data, " \t\n\u00a0"), ":"), " \t\n\u00a0")
	}
	remove(label)
	if title := textOf(heading); title == "" || strings.EqualFold(title, "Spoiler") {
		remove(heading)
	}
}

// spoilerCaption handles the newer form of spoiler, which the page holds
// unfolded and a script folds in the browser:
//
//	<div class="spoiler-new" data-class="spoiler" data-caption="Spoiler"> ...the text... </div>
//
// Its heading is the data-caption attribute, "Spoiler" unless the author
// gave a title; a title of the author's own becomes a paragraph before
// the text.
func spoilerCaption(spoiler *html.Node) {
	caption := strings.Join(strings.Fields(attr(spoiler, "data-caption")), " ")
	if caption == "" || strings.EqualFold(caption, "Spoiler") {
		return
	}
	p := &html.Node{Type: html.ElementNode, Data: "p"}
	p.AppendChild(&html.Node{Type: html.TextNode, Data: caption})
	spoiler.InsertBefore(p, spoiler.FirstChild)
}

// dropNavigation removes the links from one chapter to the next that
// authors wrote into the text themselves, before the site had buttons for
// it. They survive in chapters from the site's forum days, pointing at
// the old address and the forum threads the chapters then were, as a line
// of their own:
//
//	<br /><a href="http://www.royalroadl.com/forum/showthread.php?tid=6324" target="_blank">Next Chapter</a>
//	<br /><a href="http://www.royalroadl.com/fiction/494" target="_blank">Review</a>
//
//	<a href="...viewtopic.php?f=64&t=97"><<Previous</a> <a href="...viewtopic.php?f=64&t=99">Next>></a>
//
// or dressed up as a dialogue box:
//
//	<table><tbody><tr><td colspan='4'>Continue Misfortune?</td></tr>
//	<td width="25%"><a href="http://www.royalroadl.com/forum/viewtopic.php?f=161&t=1281" target="_blank">Previous</a></td>
//	<td colspan='2'><a href="http://www.royalroadl.com/forum/viewforum.php?f=161" target="_blank">Exit</a></td>
//	<td width="25%"><a href="http://www.royalroadl.com/forum/viewtopic.php?f=161&t=1293" target="_blank">Next</a></td></tbody></table>
//
// A line (what stands between two line breaks or block boundaries) goes
// when it has links, every one of them is a navigation link (navLink),
// and nothing but punctuation stands outside them. A table goes when it
// is such a box (navTable). A sentence that links to another chapter, a
// link to anywhere else, and a list of chapters by title all stay.
func dropNavigation(content *html.Node) {
	var (
		links, outside []*html.Node
		prose          bool
	)
	flush := func() {
		if len(links) > 0 && !prose {
			for _, n := range append(links, outside...) {
				remove(n)
			}
		}
		links, outside, prose = links[:0], outside[:0], false
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		var next *html.Node
		for c := n.FirstChild; c != nil; c = next {
			next = c.NextSibling
			switch {
			case c.Type == html.TextNode:
				if strings.IndexFunc(c.Data, isWordRune) >= 0 {
					prose = true
				} else {
					outside = append(outside, c)
				}
			case c.Type != html.ElementNode:
			case c.Data == "br":
				flush()
			case c.Data == "table" && navTable(c):
				flush()
				remove(c)
			case blocks[c.Data]:
				flush()
				walk(c)
				flush()
			case c.Data == "a" && attr(c, "href") != "":
				if navLink(c, navWords) {
					links = append(links, c)
				} else {
					prose = true
				}
			case c.Data == "img":
				prose = true
			default:
				walk(c)
			}
		}
	}
	walk(content)
	flush()
}

// navWords are the words navigation links are made of, and navKeys the
// ones of which a link must have at least one: "Next Chapter >>",
// "Previous", "Back to Main Page", "Table of Contents & Updates",
// "Review? :)" (the fiction's page, where reviews are left).
var (
	navWords = map[string]bool{
		"previous": true, "prev": true, "next": true, "chapter": true, "back": true, "to": true,
		"the": true, "main": true, "page": true, "index": true, "exit": true, "table": true,
		"of": true, "contents": true, "toc": true, "updates": true, "review": true, "first": true, "last": true,
	}
	navKeys = map[string]bool{
		"previous": true, "prev": true, "next": true, "main": true, "index": true, "exit": true,
		"contents": true, "toc": true, "review": true, "first": true, "last": true,
	}
	// boxWords adds the answers of a box asking whether to go on ("Yes"
	// is the next chapter, "No" the one before).
	boxWords = map[string]bool{"yes": true, "no": true}

	// bbCode is forum markup left unconverted in a link's text:
	// "[p=center]Next Chapter[/p]".
	bbCode = regexp.MustCompile(`\[/?[A-Za-z]+(?:=[^\]]*)?\]`)
)

// navLink reports whether a link is navigation: it points at a story
// page of the site (storyLink) and its text is made of words only
// (navigation words, at least one of navKeys; in a box also boxWords).
func navLink(a *html.Node, words map[string]bool) bool {
	return storyLink(attr(a, "href")) && navText(textOf(a), words)
}

func navText(text string, words map[string]bool) bool {
	key := false
	fields := strings.FieldsFunc(strings.ToLower(bbCode.ReplaceAllString(text, " ")), func(r rune) bool { return !unicode.IsLetter(r) })
	for _, w := range fields {
		switch {
		case navKeys[w] || (len(fields) == 1 && boxWords[w] && words[w]):
			key = true
		case !words[w]:
			return false
		}
	}
	return key
}

// storyLink reports whether a link points at a fiction or chapter of the
// site: /fiction/... on royalroad.com, or on royalroadl.com, its address
// until 2019, where chapters were also threads of the forum
// (/forum/showthread.php?tid=N, /forum/viewtopic.php?f=N&t=N, and
// /forum/viewforum.php?f=N or forumdisplay.php for a fiction's index).
func storyLink(href string) bool {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return false
	}
	switch strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") {
	case "", "royalroad.com", "royalroadl.com":
	default:
		return false
	}
	return strings.HasPrefix(u.Path, "/fiction/") || strings.HasPrefix(u.Path, "/forum/")
}

// navTable reports whether a table is a navigation box: it has links,
// each a navigation link or a "Yes"/"No" alone in its cell, and besides
// them at most one cell of other text - the box's caption ("Onwards?") -
// and cells naming a direction without a link (the first chapter's
// "Previous", or a "No" that leads nowhere).
func navTable(table *html.Node) bool {
	both := map[string]bool{}
	for w := range navWords {
		both[w] = true
	}
	for w := range boxWords {
		both[w] = true
	}
	links, captions := 0, 0
	ok := true
	var cells func(n *html.Node)
	cells = func(n *html.Node) {
		for c := n.FirstChild; c != nil && ok; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			if c.Data != "td" && c.Data != "th" && c.Data != "caption" {
				cells(c)
				continue
			}
			if find(c, func(d *html.Node) bool { return d.Data == "td" || d.Data == "th" }) != nil {
				cells(c)
				continue
			}
			cellLinks := 0
			var inside func(n *html.Node)
			inside = func(n *html.Node) {
				for d := n.FirstChild; d != nil && ok; d = d.NextSibling {
					switch {
					case d.Type == html.TextNode:
						if cellLinks >= 0 && strings.IndexFunc(d.Data, isWordRune) >= 0 {
							if cellLinks > 0 {
								ok = false
							}
							cellLinks = -1 // a cell of text
						}
					case d.Type != html.ElementNode:
					case d.Data == "a" && attr(d, "href") != "":
						if cellLinks < 0 || !navLink(d, both) {
							ok = false
							return
						}
						cellLinks++
					case d.Data == "img":
						ok = false
					default:
						inside(d)
					}
				}
			}
			inside(c)
			switch {
			case cellLinks > 0:
				links += cellLinks
			case cellLinks < 0 && !navText(textOf(c), both):
				captions++
				if len(textOf(c)) > 80 {
					ok = false
				}
			}
		}
	}
	cells(table)
	return ok && links > 0 && captions <= 1
}

// protectedEmail decodes an e-mail address the site's CDN has replaced in
// the page. Anything shaped like an address is served as
//
//	<a href="/cdn-cgi/l/email-protection" class="__cf_email__"
//	   data-cfemail="51333035...">[email&#160;protected]</a>
//
// and put back by a script in the browser: data-cfemail is hex, its first
// byte a key every later byte is XORed with. Left alone, the text would
// read "[email protected]".
func protectedEmail(a *html.Node) (string, bool) {
	raw, err := hex.DecodeString(attr(a, "data-cfemail"))
	if err != nil || len(raw) < 2 {
		return "", false
	}
	out := make([]byte, len(raw)-1)
	for i, b := range raw[1:] {
		out[i] = b ^ raw[0]
	}
	return string(out), true
}

// usableSrc reports whether an <img src> names something that can be
// fetched: a web address, absolute or relative to the page.
func usableSrc(src string) bool {
	src = strings.TrimSpace(src)
	if src == "" {
		return false
	}
	u, err := url.Parse(src)
	if err != nil {
		return false
	}
	if u.Scheme == "" {
		return u.Host != "" || u.Path != ""
	}
	return isWebURL(src)
}

func isWebURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// unwrapCells takes a table cell's text out of the single paragraph the
// site's editor wraps it in:
//
//	<td style="width: 25%"><p class="cnMzOD..."><span style="font-weight: 400">Passive Skill</span></p></td>
//
// A cell holding a block reads as a paragraph of its own, where a cell of
// plain text is read with the rest of its row; a stat table is nearly
// always the former on the site and meant as the latter. A cell with
// several paragraphs keeps them.
func unwrapCells(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		for c.Data == "td" || c.Data == "th" {
			var only *html.Node
			single := true
			for k := c.FirstChild; k != nil && single; k = k.NextSibling {
				switch {
				case k.Type == html.TextNode && strings.TrimSpace(k.Data) == "":
				case k.Type == html.ElementNode && (k.Data == "p" || k.Data == "div") && only == nil:
					only = k
				default:
					single = false
				}
			}
			if !single || only == nil {
				break
			}
			for k := only.FirstChild; k != nil; k = only.FirstChild {
				only.RemoveChild(k)
				c.InsertBefore(k, only)
			}
			c.RemoveChild(only)
		}
		unwrapCells(c)
	}
}

// blocks are the elements that start a line of their own.
var blocks = map[string]bool{
	"p": true, "div": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"blockquote": true, "pre": true, "center": true, "section": true, "article": true, "aside": true,
	"header": true, "footer": true, "figure": true, "figcaption": true, "details": true, "summary": true,
	"ul": true, "ol": true, "li": true, "dl": true, "dt": true, "dd": true, "hr": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true, "td": true, "th": true, "caption": true,
}

// dropTitleLine removes the chapter's first line of text when it only
// repeats the chapter's title - many authors open the text with
//
//	<p style="text-align: center"><strong>Chapter 001<br></strong><strong>Good Morning Brother</strong></p>
//
// under a chapter listed as "1. Good Morning Brother", and the title is
// already read out once. A title set over two lines ("Chapter 1", then
// "Warm Spots") loses both.
func dropTitleLine(content *html.Node, title string) {
	line := firstLine(content)
	if line == nil {
		return
	}
	match := titleRepeat(lineText(line), title)
	if match == noRepeat {
		return
	}
	remove(line)
	if match != repeatsNumber {
		return
	}
	// Only the number so far: the name may follow on a line of its own.
	if line = firstLine(content); line != nil && titleRepeat(lineText(line), title) == repeatsName {
		remove(line)
	}
}

// firstLine is the innermost block holding the chapter's first text, or
// that text itself when it stands loose in content; nil when there is no
// text, or when its block isn't a line of its own (a table cell, or a
// block with further blocks inside it).
func firstLine(content *html.Node) *html.Node {
	first := find(content, func(n *html.Node) bool { return false }, func(n *html.Node) bool {
		return strings.TrimSpace(n.Data) != ""
	})
	if first == nil {
		return nil
	}
	line := first
	for n := first; n != content; n = n.Parent {
		line = n
		if n.Type == html.ElementNode && blocks[n.Data] {
			break
		}
	}
	if line.Type != html.ElementNode {
		return line
	}
	if line.Data == "td" || line.Data == "th" || line.Data == "li" || line.Data == "caption" {
		return nil
	}
	nested := find(line, func(n *html.Node) bool { return blocks[n.Data] || n.Data == "img" })
	if nested != nil {
		return nil
	}
	return line
}

// lineText is a line's text with each <br> read as a space.
func lineText(n *html.Node) string {
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
		case n.Type == html.ElementNode && n.Data == "br":
			b.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

type repeat int

const (
	noRepeat      repeat = iota
	repeatsTitle         // the whole title
	repeatsNumber        // "Chapter 12" of "Chapter 12: The Bridge"
	repeatsName          // "The Bridge" of "Chapter 12: The Bridge"
)

// titleRepeat says whether a line of chapter text is the chapter's title
// again. Both are compared as words - case, punctuation, the word
// "Chapter" and leading zeros aside - and split into numbers and names:
//
//   - the names must be the title's names, all of them in order, or (two
//     words or more) the ones the title ends with: "Refuse the Chain"
//     under "GB - Chapter 1: Refuse The Chain";
//   - numbers the line and the title both give must agree, so "Chapter 2
//     ..." never passes for chapter 1;
//   - a line of numbers alone must give the title's numbers.
//
// It errs towards keeping the line: quotation marks, or a full stop or
// other sentence ending the title doesn't end with, mark it as prose even
// when the words match ("Run." under a chapter called "Run").
func titleRepeat(line, title string) repeat {
	line, title = strings.TrimSpace(line), strings.TrimSpace(title)
	if line == "" || title == "" {
		return noRepeat
	}
	if strings.EqualFold(line, strings.Join(strings.Fields(title), " ")) {
		return repeatsTitle
	}
	lineNames, lineNums := titleWords(line)
	titleNames, titleNums := titleWords(title)

	if len(lineNames) == 0 {
		if len(lineNums) > 0 && endsWith(titleNums, lineNums) {
			if len(titleNames) == 0 {
				return repeatsTitle
			}
			return repeatsNumber
		}
		return noRepeat
	}
	if strings.ContainsAny(line, "\"“”„«»") {
		return noRepeat
	}
	if last := lastRune(line); strings.ContainsRune(".!?…", last) && last != lastRune(title) {
		return noRepeat
	}
	names := equal(lineNames, titleNames) || (len(lineNames) >= 2 && endsWith(titleNames, lineNames))
	if !names {
		return noRepeat
	}
	switch {
	case len(lineNums) == 0:
		if len(titleNums) == 0 {
			return repeatsTitle
		}
		return repeatsName
	case len(titleNums) == 0, endsWith(titleNums, lineNums), endsWith(lineNums, titleNums):
		return repeatsTitle
	}
	return noRepeat
}

// titleWords splits text into its words, lowercased, with apostrophes
// dropped ("Life’s" and "Lifes" are one word): numbers (leading zeros
// trimmed) apart from the rest, and the label "chapter" in neither.
func titleWords(s string) (names, nums []string) {
	var word []rune
	flush := func() {
		if len(word) == 0 {
			return
		}
		w := string(word)
		word = word[:0]
		switch {
		case isDecimal(w):
			if w = strings.TrimLeft(w, "0"); w == "" {
				w = "0"
			}
			nums = append(nums, w)
		case w == "chapter" || w == "chap" || w == "ch":
		default:
			names = append(names, w)
		}
	}
	for _, r := range s {
		switch {
		case isWordRune(r):
			word = append(word, unicode.ToLower(r))
		case r == '\'' || r == '’' || r == 'ʼ':
		default:
			flush()
		}
	}
	flush()
	return names, nums
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

func lastRune(s string) rune {
	var last rune
	for _, r := range s {
		last = r
	}
	return last
}

func equal(a, b []string) bool { return len(a) == len(b) && endsWith(a, b) }

// endsWith reports whether list ends with tail.
func endsWith(list, tail []string) bool {
	if len(tail) > len(list) {
		return false
	}
	for i, w := range tail {
		if list[len(list)-len(tail)+i] != w {
			return false
		}
	}
	return true
}

// prunable are the containers dropped once nothing is left in them - what
// held only a hidden notice, a removed image or the title line.
var prunable = map[string]bool{
	"p": true, "div": true, "span": true, "a": true, "em": true, "strong": true, "b": true, "i": true,
	"u": true, "s": true, "small": true, "sup": true, "sub": true, "font": true, "center": true,
	"blockquote": true, "section": true, "figure": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true,
}

// prune removes n's descendants that hold no text, image, rule or table
// cell. Line breaks alone don't keep one. Of an inline one, the space or
// line break inside stays in its place: the space between two words is
// often set apart in an element of its own (<em>I</em><span> </span><em>assure</em>).
func prune(n *html.Node) {
	var next *html.Node
	for c := n.FirstChild; c != nil; c = next {
		next = c.NextSibling
		if c.Type != html.ElementNode {
			continue
		}
		prune(c)
		if !prunable[c.Data] {
			continue
		}
		holds := find(c, func(d *html.Node) bool { return d.Data != "br" && !prunable[d.Data] }, func(t *html.Node) bool {
			return strings.TrimSpace(t.Data) != ""
		})
		if holds != nil {
			continue
		}
		if !blocks[c.Data] {
			for k := c.FirstChild; k != nil; k = c.FirstChild {
				c.RemoveChild(k)
				n.InsertBefore(k, c)
			}
		}
		remove(c)
	}
}

// attr is an element's attribute value, "" if absent.
func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name && a.Namespace == "" {
			return a.Val
		}
	}
	return ""
}

// remove detaches n from its parent.
func remove(n *html.Node) {
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

func hasClass(n *html.Node, class string) bool {
	if n.Type != html.ElementNode {
		return false
	}
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

func hasAnyClass(n *html.Node, classes []string) bool {
	for _, c := range classes {
		if hasClass(n, c) {
			return true
		}
	}
	return false
}

// find returns the first descendant of n, in document order, that is an
// element matching element or - when text is given - a text node matching
// it.
func find(n *html.Node, element func(*html.Node) bool, text ...func(*html.Node) bool) *html.Node {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.ElementNode:
			if element(c) {
				return c
			}
		case html.TextNode:
			if len(text) > 0 && text[0](c) {
				return c
			}
		}
		if got := find(c, element, text...); got != nil {
			return got
		}
	}
	return nil
}

// textOf is an element's text, whitespace-normalized.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// resolve makes raw absolute against base; "" if it is empty or isn't a
// web address.
func resolve(base *url.URL, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := base.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.String()
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.Join(strings.Fields(v), " "); v != "" {
			return v
		}
	}
	return ""
}
