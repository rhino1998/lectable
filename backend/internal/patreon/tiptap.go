package patreon

import (
	"encoding/json"
	"fmt"
	"html"
	"strings"
)

// A post written in Patreon's current editor is stored as a Tiptap
// (ProseMirror) document: a tree of nodes, text nodes carrying marks.
//
//	{"type":"doc","content":[{"type":"paragraph","content":[
//	  {"type":"text","marks":[{"type":"bold"}],"text":"Chapter 12"}]}]}

type tiptapNode struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Attrs   map[string]any `json:"attrs"`
	Marks   []tiptapMark   `json:"marks"`
	Content []tiptapNode   `json:"content"`
}

type tiptapMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs"`
}

// tiptapHTML renders a Tiptap document as HTML. A node type it doesn't
// know is rendered as its children, so its text is never lost.
func tiptapHTML(raw string) (string, error) {
	var doc tiptapNode
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return "", fmt.Errorf("its text isn't the document Patreon's editor writes: %w", err)
	}
	var b strings.Builder
	writeTiptap(&b, doc)
	return b.String(), nil
}

// tiptapBlocks are the node types that are one HTML element.
var tiptapBlocks = map[string]string{
	"paragraph": "p", "blockquote": "blockquote", "bulletList": "ul", "orderedList": "ol",
	"listItem": "li", "codeBlock": "pre",
}

// tiptapMarks are the marks that are one HTML element.
var tiptapMarks = map[string]string{
	"bold": "strong", "italic": "em", "underline": "u", "strike": "s", "code": "code",
	"superscript": "sup", "subscript": "sub",
}

func writeTiptap(b *strings.Builder, n tiptapNode) {
	children := func() {
		for _, c := range n.Content {
			writeTiptap(b, c)
		}
	}
	switch n.Type {
	case "text":
		var closing []string
		for _, m := range n.Marks {
			if tag, ok := tiptapMarks[m.Type]; ok {
				b.WriteString("<" + tag + ">")
				closing = append(closing, tag)
			} else if href, _ := m.Attrs["href"].(string); m.Type == "link" && href != "" {
				b.WriteString(`<a href="` + html.EscapeString(href) + `">`)
				closing = append(closing, "a")
			}
		}
		b.WriteString(html.EscapeString(n.Text))
		for i := len(closing) - 1; i >= 0; i-- {
			b.WriteString("</" + closing[i] + ">")
		}
	case "hardBreak":
		b.WriteString("<br/>")
	case "horizontalRule":
		b.WriteString("<hr/>")
	case "heading":
		level := 2
		if l, ok := n.Attrs["level"].(float64); ok && l >= 1 && l <= 6 {
			level = int(l)
		}
		fmt.Fprintf(b, "<h%d>", level)
		children()
		fmt.Fprintf(b, "</h%d>", level)
	case "image":
		src, _ := n.Attrs["src"].(string)
		if src == "" {
			src, _ = n.Attrs["url"].(string)
		}
		if src != "" {
			alt, _ := n.Attrs["alt"].(string)
			b.WriteString(`<p><img src="` + html.EscapeString(src) + `" alt="` + html.EscapeString(alt) + `"/></p>`)
		}
	default:
		tag, ok := tiptapBlocks[n.Type]
		if !ok {
			children()
			return
		}
		b.WriteString("<" + tag + ">")
		children()
		b.WriteString("</" + tag + ">")
	}
}
