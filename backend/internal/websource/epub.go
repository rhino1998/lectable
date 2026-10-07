package websource

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"path"
	"strings"
)

// A fetched work is kept as an epub - the book's source epub, like an
// uploaded one - with one spine file per chapter, so the ordinary parser
// reads it and a single chapter can be re-imported from it later. Its
// index (indexPath) lists the chapters by their site ids, which is what
// lets WriteEPUB add newly published chapters to an existing file.
const (
	indexPath   = "META-INF/lectable-source.json"
	contentDir  = "OEBPS/"
	chapterDir  = "ch/"
	imageDir    = "img/"
	epubMIME    = "application/epub+zip"
	xhtmlMIME   = "application/xhtml+xml"
	containerXM = `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
`
)

// ErrForeignEpub is an epub this package didn't write (no index) - a
// book's kept source replaced by an uploaded file - which can't be added
// to.
var ErrForeignEpub = errors.New("not an epub written from a website")

// Meta is the book-level half of a written epub.
type Meta struct {
	// Kind and WorkID name the source and the work there (Source.Kind,
	// Work.ID).
	Kind   string
	WorkID string
	URL    string
	Title  string
	Author string
	// Cover is the cover image and CoverExt its extension (".jpg"); both
	// empty for none. When adding to an existing epub, an empty Cover
	// keeps the one it has.
	Cover    []byte
	CoverExt string
}

type epubIndex struct {
	Kind string `json:"kind,omitempty"`
	// WorkID keeps the name it had when Royal Road was the only source.
	WorkID    string         `json:"fictionId"`
	URL       string         `json:"url"`
	Title     string         `json:"title"`
	Author    string         `json:"author"`
	CoverFile string         `json:"coverFile,omitempty"`
	Chapters  []indexChapter `json:"chapters"`
}

type indexChapter struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// File is the chapter's file name without its extension (FileKey of
	// its id); absent in an index from when that was always the id.
	File   string   `json:"file,omitempty"`
	Images []string `json:"images,omitempty"`
}

func (c indexChapter) file() string {
	if c.File != "" {
		return c.File
	}
	return c.ID
}

// FileKey turns a chapter id into a name safe for a file in the epub: a
// source's ids may be paths ("2017/03/03/1-00"). Letters, digits, '-' and
// '_' are kept and anything else becomes '_'; ids that differ only in
// those characters would collide, which no source's do.
func FileKey(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '_'
	}, id)
}

// WriteEPUB writes an epub of prev's chapters (nil for a new book)
// followed by chapters, in that order; a chapter prev already has is
// skipped. prev's chapter and image entries are copied as they are, not
// recompressed.
func WriteEPUB(w io.Writer, meta Meta, prev *zip.Reader, chapters []*Chapter) error {
	return writeEPUB(w, meta, prev, chapters, false)
}

// RewriteEPUB writes prev again with each of chapters in place of the
// chapter of prev that has its id - for when chapters have been cleaned up
// afresh from their stored pages. Chapters prev doesn't have are ignored;
// its other chapters, cover and details are kept.
func RewriteEPUB(w io.Writer, prev *zip.Reader, chapters []*Chapter) error {
	return writeEPUB(w, Meta{}, prev, chapters, true)
}

func writeEPUB(w io.Writer, meta Meta, prev *zip.Reader, chapters []*Chapter, replace bool) error {
	idx := epubIndex{Kind: meta.Kind, WorkID: meta.WorkID, URL: meta.URL, Title: meta.Title, Author: meta.Author}
	prevFiles := map[string]*zip.File{}
	have := map[string]bool{}
	if prev != nil {
		for _, f := range prev.File {
			prevFiles[f.Name] = f
		}
		old, err := readIndex(prevFiles)
		if err != nil {
			return err
		}
		idx.Chapters = old.Chapters
		idx.CoverFile = old.CoverFile
		if replace {
			idx.Kind, idx.WorkID, idx.URL, idx.Title, idx.Author = old.Kind, old.WorkID, old.URL, old.Title, old.Author
		}
		for _, c := range old.Chapters {
			have[c.ID] = true
		}
	}

	zw := zip.NewWriter(w)
	// The mimetype entry comes first and uncompressed, as the format asks.
	mt, err := zw.CreateHeader(&zip.FileHeader{Name: "mimetype", Method: zip.Store})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(mt, epubMIME); err != nil {
		return err
	}
	write := func(name string, data []byte) error {
		f, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		return err
	}
	if err := write("META-INF/container.xml", []byte(containerXM)); err != nil {
		return err
	}

	switch {
	case len(meta.Cover) > 0 && meta.CoverExt != "":
		idx.CoverFile = "cover" + meta.CoverExt
		if err := write(contentDir+idx.CoverFile, meta.Cover); err != nil {
			return err
		}
	case idx.CoverFile != "":
		if err := copyEntry(zw, prevFiles, contentDir+idx.CoverFile); err != nil {
			idx.CoverFile = ""
		}
	}
	fresh := map[string]*Chapter{}
	if replace {
		for _, ch := range chapters {
			fresh[ch.ID] = ch
		}
	}
	for i, c := range idx.Chapters {
		if ch := fresh[c.ID]; ch != nil {
			ic := indexChapter{ID: ch.ID, Title: ch.Title, File: c.file()}
			if err := write(contentDir+chapterDir+ic.File+".xhtml", chapterXHTML(ch)); err != nil {
				return err
			}
			for _, img := range ch.Images {
				if err := write(contentDir+imageDir+img.Name, img.Data); err != nil {
					return err
				}
				ic.Images = append(ic.Images, img.Name)
			}
			idx.Chapters[i] = ic
			continue
		}
		if err := copyEntry(zw, prevFiles, contentDir+chapterDir+c.file()+".xhtml"); err != nil {
			return fmt.Errorf("chapter %q: %w", c.Title, err)
		}
		for _, name := range c.Images {
			if err := copyEntry(zw, prevFiles, contentDir+imageDir+name); err != nil {
				return fmt.Errorf("chapter %q: %w", c.Title, err)
			}
		}
	}
	for _, ch := range chapters {
		if have[ch.ID] || replace {
			continue
		}
		have[ch.ID] = true
		ic := indexChapter{ID: ch.ID, Title: ch.Title, File: FileKey(ch.ID)}
		if err := write(contentDir+chapterDir+ic.File+".xhtml", chapterXHTML(ch)); err != nil {
			return err
		}
		for _, img := range ch.Images {
			if err := write(contentDir+imageDir+img.Name, img.Data); err != nil {
				return err
			}
			ic.Images = append(ic.Images, img.Name)
		}
		idx.Chapters = append(idx.Chapters, ic)
	}

	if err := write(contentDir+"nav.xhtml", navXHTML(idx)); err != nil {
		return err
	}
	if err := write(contentDir+"content.opf", packageOPF(idx)); err != nil {
		return err
	}
	raw, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	if err := write(indexPath, raw); err != nil {
		return err
	}
	return zw.Close()
}

// ChapterIDs lists the site ids of an epub's chapters, in order.
func ChapterIDs(zr *zip.Reader) ([]string, error) {
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	idx, err := readIndex(files)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(idx.Chapters))
	for i, c := range idx.Chapters {
		ids[i] = c.ID
	}
	return ids, nil
}

func readIndex(files map[string]*zip.File) (*epubIndex, error) {
	f, ok := files[indexPath]
	if !ok {
		return nil, ErrForeignEpub
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var idx epubIndex
	if err := json.NewDecoder(io.LimitReader(rc, 64<<20)).Decode(&idx); err != nil {
		return nil, fmt.Errorf("read epub index: %w", err)
	}
	return &idx, nil
}

func copyEntry(zw *zip.Writer, files map[string]*zip.File, name string) error {
	f, ok := files[name]
	if !ok {
		return fmt.Errorf("%s missing from the existing epub", name)
	}
	return zw.Copy(f)
}

// chapterXHTML is one chapter's spine file. The title is always there as
// an <h1>, so a chapter with no text of its own (only a note, or images)
// still parses as a chapter and the epub's chapter count stays the
// fiction's.
func chapterXHTML(ch *Chapter) []byte {
	var b bytes.Buffer
	title := html.EscapeString(ch.Title)
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n<!DOCTYPE html>\n")
	b.WriteString(`<html xmlns="http://www.w3.org/1999/xhtml"><head><title>` + title + "</title></head><body>\n")
	b.WriteString("<h1>" + title + "</h1>\n")
	b.WriteString(ch.Body)
	b.WriteString("\n</body></html>\n")
	return b.Bytes()
}

func navXHTML(idx epubIndex) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n<!DOCTYPE html>\n")
	b.WriteString(`<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops"><head><title>` + html.EscapeString(idx.Title) + "</title></head><body>\n")
	b.WriteString(`<nav epub:type="toc"><ol>` + "\n")
	for _, c := range idx.Chapters {
		fmt.Fprintf(&b, `<li><a href="%s%s.xhtml">%s</a></li>`+"\n", chapterDir, c.file(), html.EscapeString(c.Title))
	}
	b.WriteString("</ol></nav>\n</body></html>\n")
	return b.Bytes()
}

func packageOPF(idx epubIndex) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id">` + "\n")
	b.WriteString(`<metadata xmlns:dc="http://purl.org/dc/elements/1.1/">` + "\n")
	fmt.Fprintf(&b, "<dc:identifier id=\"id\">%s:%s</dc:identifier>\n", html.EscapeString(idx.Kind), html.EscapeString(idx.WorkID))
	fmt.Fprintf(&b, "<dc:title>%s</dc:title>\n", html.EscapeString(idx.Title))
	if idx.Author != "" {
		fmt.Fprintf(&b, "<dc:creator>%s</dc:creator>\n", html.EscapeString(idx.Author))
	}
	b.WriteString("<dc:language>en</dc:language>\n")
	if idx.URL != "" {
		fmt.Fprintf(&b, "<dc:source>%s</dc:source>\n", html.EscapeString(idx.URL))
	}
	if idx.CoverFile != "" {
		b.WriteString(`<meta name="cover" content="cover"/>` + "\n")
	}
	b.WriteString("</metadata>\n<manifest>\n")
	b.WriteString(`<item id="nav" href="nav.xhtml" media-type="` + xhtmlMIME + `" properties="nav"/>` + "\n")
	if idx.CoverFile != "" {
		fmt.Fprintf(&b, `<item id="cover" href="%s" media-type="%s" properties="cover-image"/>`+"\n", idx.CoverFile, imageMIME(idx.CoverFile))
	}
	// Item ids are positions: a chapter's own id needn't be a valid one.
	for n, c := range idx.Chapters {
		fmt.Fprintf(&b, `<item id="c%d" href="%s%s.xhtml" media-type="%s"/>`+"\n", n, chapterDir, c.file(), xhtmlMIME)
		for m, name := range c.Images {
			fmt.Fprintf(&b, `<item id="c%di%d" href="%s%s" media-type="%s"/>`+"\n", n, m, imageDir, name, imageMIME(name))
		}
	}
	b.WriteString("</manifest>\n<spine>\n")
	for n := range idx.Chapters {
		fmt.Fprintf(&b, `<itemref idref="c%d"/>`+"\n", n)
	}
	b.WriteString("</spine>\n</package>\n")
	return b.Bytes()
}

func imageMIME(name string) string {
	switch path.Ext(name) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
