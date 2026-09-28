package epub

import (
	"encoding/xml"
	"io"
	"strings"
)

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
