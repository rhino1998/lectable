package bookexport

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"mime"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/oggopus"
)

// The EPUB side of an export, OCF container layout:
//
//	mimetype                    first, stored, no extra fields
//	META-INF/container.xml
//	OEBPS/package.opf
//	OEBPS/nav.xhtml, style.css
//	OEBPS/text/c00000.xhtml     one per chapter, generated from the paragraph rows
//	OEBPS/smil/c00000.smil      its Media Overlay
//	OEBPS/audio/<chapter id>/<voice id>/<idx>.opus
//	OEBPS/images/<chapter id>/<image id>.<ext>, images/cover.<ext>
//
// Chapter text is generated rather than taken from the source epub: the
// paragraph rows don't map one-to-one onto its elements (a quote split
// out of a sentence is its own row), and every row needs an id for the
// overlay to point at. Each row is a <span id="p-<paragraph id>">, inline
// rows sharing their visual paragraph's <p>.
const (
	oebps          = "OEBPS/"
	opusMediaType  = "audio/ogg; codecs=opus"
	activeClass    = "-epub-media-overlay-active"
	playbackActive = "-epub-media-overlay-playing"
)

// Progress reports how many of total bytes of source files have been
// written.
type Progress func(done, total int64)

// WriteEPUB writes b as an EPUB 3 with Media Overlays to w, with a full
// export's lectable data under META-INF/lectable/. now stamps the
// package's modification date.
func WriteEPUB(ctx context.Context, w io.Writer, b *Book, opts Options, now time.Time, progress Progress) error {
	ew := &epubWriter{zw: zip.NewWriter(w), book: b, opts: opts, now: now.UTC(), progress: progress}
	for _, c := range b.Chapters {
		for _, blk := range c.Content {
			switch {
			case blk.Image != nil:
				ew.total += blk.Image.Size
			case blk.Paragraph != nil && blk.Paragraph.Audio != nil && !opts.Latents:
				ew.clipSizes(blk.Paragraph.Audio.Clip)
			}
		}
	}
	if b.Cover != nil {
		ew.total += b.Cover.Size
	}
	for _, f := range b.Lectable.Files {
		ew.total += f.Size
	}
	if err := ew.write(ctx); err != nil {
		return err
	}
	return ew.zw.Close()
}

type epubWriter struct {
	zw       *zip.Writer
	book     *Book
	opts     Options
	now      time.Time
	progress Progress
	done     int64
	total    int64
	counted  map[*Clip]bool

	// manifest collects package.opf <item>s as resources are written.
	items []opfItem
	// files maps every data file written to where it went.
	files   []ManifestFile
	written map[string]bool // archive paths already written
}

type opfItem struct {
	id, href, mediaType, properties, overlay string
	duration                                 float64 // media:duration, overlays only
}

func (ew *epubWriter) clipSizes(c *Clip) {
	if ew.counted == nil {
		ew.counted = map[*Clip]bool{}
	}
	if !ew.counted[c] {
		ew.counted[c] = true
		ew.total += c.Size
	}
}

func (ew *epubWriter) write(ctx context.Context) error {
	if err := ew.mimetype(); err != nil {
		return err
	}
	if err := ew.text("META-INF/container.xml", containerXML); err != nil {
		return err
	}
	if err := ew.text(oebps+"style.css", styleCSS); err != nil {
		return err
	}
	ew.items = append(ew.items, opfItem{id: "css", href: "style.css", mediaType: "text/css"})

	if c := ew.book.Cover; c != nil {
		href := "images/cover" + c.Ext
		if err := ew.copyFile(ctx, oebps+href, c, zip.Store); err != nil {
			return err
		}
		ew.items = append(ew.items, opfItem{id: "cover-image", href: href, mediaType: imageMediaType(c.Ext), properties: "cover-image"})
	}

	for i := range ew.book.Chapters {
		if err := ew.chapter(ctx, &ew.book.Chapters[i]); err != nil {
			return err
		}
	}
	if err := ew.text(oebps+"nav.xhtml", ew.nav()); err != nil {
		return err
	}
	ew.items = append(ew.items, opfItem{id: "nav", href: "nav.xhtml", mediaType: "application/xhtml+xml", properties: "nav"})
	if err := ew.text(oebps+"package.opf", ew.opf()); err != nil {
		return err
	}
	return ew.lectable(ctx)
}

// mimetype is the OCF's first entry: stored, with sizes in the local
// header (no data descriptor) and no extra field, so "mimetype" +
// "application/epub+zip" sit at fixed offsets for readers that sniff them.
func (ew *epubWriter) mimetype() error {
	data := []byte("application/epub+zip")
	fw, err := ew.zw.CreateRaw(&zip.FileHeader{
		Name: "mimetype", Method: zip.Store, CRC32: crc32.ChecksumIEEE(data),
		CompressedSize64: uint64(len(data)), UncompressedSize64: uint64(len(data)),
	})
	if err != nil {
		return err
	}
	_, err = fw.Write(data)
	return err
}

func (ew *epubWriter) text(name, body string) error {
	return ew.bytes(name, []byte(body), zip.Deflate, time.Time{})
}

func (ew *epubWriter) bytes(name string, data []byte, method uint16, mod time.Time) error {
	if mod.IsZero() {
		mod = ew.now
	}
	fw, err := ew.zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: mod})
	if err != nil {
		return err
	}
	_, err = fw.Write(data)
	return err
}

// copyFile copies f into the archive at name (once - a shared clip is
// written by its first segment), keeping its mtime (audio URLs and
// Android's offline sync version clips by it), and records where it
// belongs in the library.
func (ew *epubWriter) copyFile(ctx context.Context, name string, f *File, method uint16) error {
	if ew.written == nil {
		ew.written = map[string]bool{}
	}
	if ew.written[name] {
		return nil
	}
	ew.written[name] = true
	if err := ctx.Err(); err != nil {
		return err
	}
	src, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer src.Close()
	fw, err := ew.zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: f.ModTime})
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, src); err != nil {
		return fmt.Errorf("%s: %w", f.Path, err)
	}
	ew.advance(f.Size)
	ew.files = append(ew.files, ManifestFile{Archive: name, Data: f.DataPath, ModTime: f.ModTime})
	return nil
}

func (ew *epubWriter) advance(n int64) {
	ew.done += n
	if ew.progress != nil {
		ew.progress(ew.done, ew.total)
	}
}

// clip writes c's audio, transcoding a legacy WAV to Opus. Returns its
// href from OEBPS.
func (ew *epubWriter) clip(ctx context.Context, c *Clip) (string, error) {
	href := "audio/" + strings.TrimPrefix(c.DataPath, "audio/"+ew.book.ID+"/")
	name := oebps + href
	if !c.Legacy {
		return href, ew.copyFile(ctx, name, &c.File, zip.Store)
	}
	if ew.written[name] {
		return href, nil
	}
	if ew.written == nil {
		ew.written = map[string]bool{}
	}
	ew.written[name] = true
	wav, err := os.ReadFile(c.Path)
	if err != nil {
		return "", err
	}
	data, err := oggopus.EncodeWAV(wav)
	if err != nil {
		return "", fmt.Errorf("transcode %s: %w", c.Path, err)
	}
	if err := ew.bytes(name, data, zip.Store, c.ModTime); err != nil {
		return "", err
	}
	ew.advance(c.Size)
	ew.files = append(ew.files, ManifestFile{Archive: name, Data: c.DataPath, ModTime: c.ModTime})
	return href, nil
}

func chapterName(c *Chapter) string { return fmt.Sprintf("c%05d", c.Idx) }

func chapterTitle(c *Chapter) string {
	if strings.TrimSpace(c.Title) != "" {
		return c.Title
	}
	return "Chapter " + strconv.Itoa(c.Idx+1)
}

// chapter writes c's text, its overlay (if any of it has audio), and its
// clips and images.
func (ew *epubWriter) chapter(ctx context.Context, c *Chapter) error {
	name := chapterName(c)
	textHref := "text/" + name + ".xhtml"
	var body, smil strings.Builder
	var duration float64
	open := false // inside a <p>
	closeP := func() {
		if open {
			body.WriteString("</p>\n")
			open = false
		}
	}
	for _, blk := range c.Content {
		switch blk.Kind {
		case BlockImage:
			closeP()
			href := "images/" + strings.TrimPrefix(blk.Image.DataPath, "images/"+ew.book.ID+"/")
			if err := ew.copyFile(ctx, oebps+href, blk.Image, zip.Store); err != nil {
				return err
			}
			ew.items = append(ew.items, opfItem{id: itemID("img", href), href: href, mediaType: imageMediaType(blk.Image.Ext)})
			fmt.Fprintf(&body, "<figure class=\"image\"><img src=\"../%s\" alt=\"\"/></figure>\n", xmlEscape(href))
		case BlockBreak:
			closeP()
			body.WriteString("<hr class=\"break\"/>\n")
		case BlockParagraph:
			p := blk.Paragraph
			if !p.Inline || !open {
				closeP()
				body.WriteString("<p>")
				open = true
			} else {
				body.WriteString(" ")
			}
			words := ew.wordSpans(p)
			id := "p-" + p.ID
			fmt.Fprintf(&body, "<span id=\"%s\">", id)
			if words == nil {
				body.WriteString(xmlEscape(p.Text))
			} else {
				writeWordSpans(&body, p, words)
			}
			body.WriteString("</span>")
			if p.Audio == nil || ew.opts.Latents {
				continue // a latent transfer's clips travel as data instead
			}
			href, err := ew.clip(ctx, p.Audio.Clip)
			if err != nil {
				return err
			}
			duration += p.Audio.End - p.Audio.Begin
			writeOverlay(&smil, textHref, href, p, words)
		}
	}
	closeP()

	title := chapterTitle(c)
	xhtml := fmt.Sprintf(xhtmlTemplate, xmlEscape(ew.lang()), xmlEscape(ew.lang()), xmlEscape(title), body.String())
	if err := ew.text(oebps+textHref, xhtml); err != nil {
		return err
	}
	item := opfItem{id: name, href: textHref, mediaType: "application/xhtml+xml"}
	if smil.Len() > 0 {
		smilHref := "smil/" + name + ".smil"
		doc := fmt.Sprintf(smilTemplate, "../"+textHref, smil.String())
		if err := ew.text(oebps+smilHref, doc); err != nil {
			return err
		}
		item.overlay = "mo-" + name
		ew.items = append(ew.items, opfItem{id: item.overlay, href: smilHref, mediaType: "application/smil+xml", duration: duration})
	}
	ew.items = append(ew.items, item)
	return nil
}

var wordRe = regexp.MustCompile(`\S+`)

// wordSpan is one whitespace-delimited word of a paragraph's text and its
// overlay range, [begin, end) in the clip's timeline.
type wordSpan struct {
	from, to   int // byte offsets into Text
	begin, end float64
}

// wordSpans splits p for a word-level overlay, or nil to keep it
// paragraph-level: word-level off, no audio, or an alignment that doesn't
// match the text word for word (the reader's own highlighter falls back
// the same way - frontend ParagraphText.wordStartTimes). Each word runs
// from its aligned start to the next word's, the first from the segment's
// start and the last to its end, so the ranges tile the segment. With
// PhraseSeconds the words are then grouped into phrases (groupPhrases).
func (ew *epubWriter) wordSpans(p *Paragraph) []wordSpan {
	if !ew.opts.WordLevel || p.Audio == nil {
		return nil
	}
	seg := p.Audio
	locs := wordRe.FindAllStringIndex(p.Text, -1)
	if len(locs) < 2 || len(locs) != len(seg.Words) {
		return nil
	}
	clamp := func(t float64) float64 { return min(max(t, seg.Begin), seg.End) }
	out := make([]wordSpan, len(locs))
	for i, loc := range locs {
		begin := clamp(seg.Words[i].Start)
		if i == 0 {
			begin = seg.Begin
		}
		end := seg.End
		if i+1 < len(locs) {
			end = clamp(seg.Words[i+1].Start)
		}
		out[i] = wordSpan{from: loc[0], to: loc[1], begin: begin, end: end}
	}
	if ew.opts.PhraseSeconds > 0 {
		out = groupPhrases(p.Text, out, ew.opts.PhraseSeconds)
		if len(out) < 2 {
			return nil // one phrase is just the paragraph
		}
	}
	return out
}

// phraseBreakRe matches a word ending a clause - where a phrase would
// rather end.
var phraseBreakRe = regexp.MustCompile(`[.,;:!?…—–)\]"”’]$`)

// groupPhrases merges consecutive words into phrases at least minSeconds
// long. A phrase ends at the first word that brings it to minSeconds, or
// earlier - once it's half that long - after a word ending a clause; a
// short last phrase joins the one before it.
func groupPhrases(text string, words []wordSpan, minSeconds float64) []wordSpan {
	var out []wordSpan
	cur := words[0]
	for _, w := range words[1:] {
		long := cur.end-cur.begin >= minSeconds
		clause := cur.end-cur.begin >= minSeconds/2 && phraseBreakRe.MatchString(text[cur.from:cur.to])
		if long || clause {
			out = append(out, cur)
			cur = w
			continue
		}
		cur.to, cur.end = w.to, w.end
	}
	if n := len(out); n > 0 && cur.end-cur.begin < minSeconds/2 {
		out[n-1].to, out[n-1].end = cur.to, cur.end
	} else {
		out = append(out, cur)
	}
	return out
}

func writeWordSpans(b *strings.Builder, p *Paragraph, words []wordSpan) {
	cursor := 0
	for i, w := range words {
		b.WriteString(xmlEscape(p.Text[cursor:w.from]))
		fmt.Fprintf(b, "<span id=\"w-%s-%d\">%s</span>", p.ID, i, xmlEscape(p.Text[w.from:w.to]))
		cursor = w.to
	}
	b.WriteString(xmlEscape(p.Text[cursor:]))
}

// writeOverlay adds p's overlay entry: one <par> for the paragraph, or a
// <seq> of one <par> per word. A word the aligner squeezed to nothing
// (clamped past the clip's end) gets no entry - it's still in the text.
func writeOverlay(b *strings.Builder, textHref, audioHref string, p *Paragraph, words []wordSpan) {
	seg := p.Audio
	audio := func(begin, end float64) string {
		return fmt.Sprintf("<audio src=\"../%s\" clipBegin=\"%s\" clipEnd=\"%s\"/>", xmlEscape(audioHref), clock(begin), clock(end))
	}
	if words == nil {
		fmt.Fprintf(b, "<par id=\"par-%s\"><text src=\"../%s#p-%s\"/>%s</par>\n", p.ID, textHref, p.ID, audio(seg.Begin, seg.End))
		return
	}
	fmt.Fprintf(b, "<seq id=\"seq-%s\" epub:textref=\"../%s#p-%s\">\n", p.ID, textHref, p.ID)
	for i, w := range words {
		if w.end <= w.begin {
			continue
		}
		fmt.Fprintf(b, "<par id=\"par-%s-%d\"><text src=\"../%s#w-%s-%d\"/>%s</par>\n", p.ID, i, textHref, p.ID, i, audio(w.begin, w.end))
	}
	b.WriteString("</seq>\n")
}

// clock is a SMIL clock value in seconds, millisecond precision.
func clock(t float64) string { return strconv.FormatFloat(t, 'f', 3, 64) + "s" }

// mediaDuration is a SMIL full clock value, as media:duration wants.
func mediaDuration(t float64) string {
	ms := int64(t*1000 + 0.5)
	return fmt.Sprintf("%d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

func (ew *epubWriter) lang() string {
	if ew.book.Language != "" {
		return ew.book.Language
	}
	return "en"
}

func (ew *epubWriter) title() string {
	if label := ChapterLabel(ew.opts.Chapters); label != "" {
		return ew.book.Title + " (" + label + ")"
	}
	return ew.book.Title
}

func (ew *epubWriter) nav() string {
	var b strings.Builder
	for i := range ew.book.Chapters {
		c := &ew.book.Chapters[i]
		fmt.Fprintf(&b, "<li><a href=\"text/%s.xhtml\">%s</a></li>\n", chapterName(c), xmlEscape(chapterTitle(c)))
	}
	return fmt.Sprintf(navTemplate, xmlEscape(ew.lang()), xmlEscape(ew.lang()), xmlEscape(ew.title()), b.String())
}

func (ew *epubWriter) opf() string {
	var meta, manifest, spine strings.Builder
	id := "urn:lectable:" + ew.book.ID
	if !ew.opts.Full() {
		id += ":" + ew.opts.ID()
	}
	fmt.Fprintf(&meta, "<dc:identifier id=\"bookid\">%s</dc:identifier>\n", xmlEscape(id))
	fmt.Fprintf(&meta, "<dc:title>%s</dc:title>\n", xmlEscape(ew.title()))
	if ew.book.Author != "" {
		fmt.Fprintf(&meta, "<dc:creator>%s</dc:creator>\n", xmlEscape(ew.book.Author))
	}
	fmt.Fprintf(&meta, "<dc:language>%s</dc:language>\n", xmlEscape(ew.lang()))
	fmt.Fprintf(&meta, "<meta property=\"dcterms:modified\">%s</meta>\n", ew.now.Format("2006-01-02T15:04:05Z"))
	if ew.book.SeriesName != "" {
		fmt.Fprintf(&meta, "<meta property=\"belongs-to-collection\" id=\"series\">%s</meta>\n", xmlEscape(ew.book.SeriesName))
		meta.WriteString("<meta refines=\"#series\" property=\"collection-type\">series</meta>\n")
		if ew.book.SeriesIndex > 0 {
			fmt.Fprintf(&meta, "<meta refines=\"#series\" property=\"group-position\">%s</meta>\n", strconv.FormatFloat(ew.book.SeriesIndex, 'f', -1, 64))
		}
	}
	if ew.book.Cover != nil {
		meta.WriteString("<meta name=\"cover\" content=\"cover-image\"/>\n")
	}
	var total float64
	for _, it := range ew.items {
		if it.mediaType == "application/smil+xml" {
			fmt.Fprintf(&meta, "<meta property=\"media:duration\" refines=\"#%s\">%s</meta>\n", it.id, mediaDuration(it.duration))
			total += it.duration
		}
	}
	if total > 0 {
		fmt.Fprintf(&meta, "<meta property=\"media:duration\">%s</meta>\n", mediaDuration(total))
		fmt.Fprintf(&meta, "<meta property=\"media:active-class\">%s</meta>\n", activeClass)
		fmt.Fprintf(&meta, "<meta property=\"media:playback-active-class\">%s</meta>\n", playbackActive)
	}

	for _, it := range ew.items {
		fmt.Fprintf(&manifest, "<item id=\"%s\" href=\"%s\" media-type=\"%s\"", it.id, xmlEscape(it.href), it.mediaType)
		if it.properties != "" {
			fmt.Fprintf(&manifest, " properties=\"%s\"", it.properties)
		}
		if it.overlay != "" {
			fmt.Fprintf(&manifest, " media-overlay=\"%s\"", it.overlay)
		}
		manifest.WriteString("/>\n")
	}
	for _, f := range ew.files {
		if !strings.HasPrefix(f.Archive, oebps+"audio/") {
			continue
		}
		href := strings.TrimPrefix(f.Archive, oebps)
		fmt.Fprintf(&manifest, "<item id=\"%s\" href=\"%s\" media-type=\"%s\"/>\n", itemID("a", href), xmlEscape(href), opusMediaType)
	}
	for i := range ew.book.Chapters {
		fmt.Fprintf(&spine, "<itemref idref=\"%s\"/>\n", chapterName(&ew.book.Chapters[i]))
	}
	return fmt.Sprintf(opfTemplate, xmlEscape(ew.lang()), meta.String(), manifest.String(), spine.String())
}

// lectable writes META-INF/lectable/: the rows and lectable-only files of
// a full export, and the manifest (every export has one).
func (ew *epubWriter) lectable(ctx context.Context) error {
	lb := &ew.book.Lectable
	m := Manifest{
		Format: manifestFormat, Version: manifestVersion, ExportedAt: ew.now,
		BookID: ew.book.ID, Title: ew.book.Title, Full: ew.opts.Full(), WordLevel: ew.opts.WordLevel,
		Latents: ew.opts.Latents, Voices: lb.Voices,
	}
	for i := range ew.book.Chapters {
		m.Chapters = append(m.Chapters, ew.book.Chapters[i].Idx)
	}
	if ew.opts.Full() || ew.opts.Latents {
		for _, table := range slices.Sorted(maps.Keys(lb.Rows)) {
			var buf bytes.Buffer
			for _, row := range lb.Rows[table] {
				buf.Write(row)
				buf.WriteByte('\n')
			}
			if err := ew.bytes(dbDir+table+".jsonl", buf.Bytes(), zip.Deflate, time.Time{}); err != nil {
				return err
			}
			m.Tables = append(m.Tables, table)
		}
		for i := range lb.Files {
			f := &lb.Files[i]
			method := zip.Deflate
			if strings.HasSuffix(f.Path, ".opus") || strings.HasSuffix(f.Path, ".epub") {
				method = zip.Store
			}
			if err := ew.copyFile(ctx, lectableDataDir+f.DataPath, f, method); err != nil {
				return err
			}
		}
		m.Files = ew.files
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return ew.bytes(manifestPath, data, zip.Deflate, time.Time{})
}

// itemID turns a resource href into a manifest item id (an XML NCName).
func itemID(prefix, href string) string {
	var b strings.Builder
	b.WriteString(prefix + "-")
	for _, r := range href {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func imageMediaType(ext string) string {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".webp":
		return "image/webp"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// xmlEscape escapes s for XML text or an attribute value, dropping
// characters XML 1.0 can't contain at all.
func xmlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"':
			b.WriteString("&quot;")
		case r == '\t' || r == '\n' || r == '\r' || r >= 0x20 && r != 0xFFFE && r != 0xFFFF && (r < 0xD800 || r > 0xDFFF):
			b.WriteRune(r)
		}
	}
	return b.String()
}

const containerXML = `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/package.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
`

const opfTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="bookid" xml:lang="%s">
<metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
%s</metadata>
<manifest>
%s</manifest>
<spine>
%s</spine>
</package>
`

const xhtmlTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" lang="%s" xml:lang="%s">
<head>
<meta charset="utf-8"/>
<title>%s</title>
<link rel="stylesheet" type="text/css" href="../style.css"/>
</head>
<body>
<section epub:type="chapter">
%s</section>
</body>
</html>
`

const smilTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<smil xmlns="http://www.w3.org/ns/SMIL" xmlns:epub="http://www.idpf.org/2007/ops" version="3.0">
<body>
<seq id="chapter" epub:textref="%s" epub:type="chapter">
%s</seq>
</body>
</smil>
`

const navTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" lang="%s" xml:lang="%s">
<head>
<meta charset="utf-8"/>
<title>%s</title>
</head>
<body>
<nav epub:type="toc" id="toc">
<ol>
%s</ol>
</nav>
</body>
</html>
`

const styleCSS = `body { line-height: 1.5; }
p { margin: 0 0 0.9em; text-indent: 0; }
hr.break { border: none; text-align: center; margin: 1.2em 0; }
hr.break::after { content: "* * *"; }
figure.image { margin: 1em 0; text-align: center; }
figure.image img { max-width: 100%; }
.` + activeClass + ` { background-color: rgba(255, 213, 79, 0.45); border-radius: 2px; }
`
