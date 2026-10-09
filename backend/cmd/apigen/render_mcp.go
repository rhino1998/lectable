package main

import (
	"fmt"
	"go/format"
	"go/types"
	"strconv"
	"strings"
)

// mcpShapeDepth is how many levels of nested structs a route's body is
// written out to before a type is left as its name.
const mcpShapeDepth = 3

// renderMCPRoutes writes cmd/lectable-mcp's route catalog: what its
// `routes` tool searches and its `api_get`/`api_call` tools check a
// request against. A body is spelled out field by field (a model calling
// the route has no type declarations to look a name up in); a response is
// left as its type's name, since the caller sees the value itself.
func (g *gen) renderMCPRoutes(routes []*route) ([]byte, error) {
	var b strings.Builder
	b.WriteString("// " + headerNote + "\n\npackage main\n\n")
	b.WriteString("var apiRoutes = []apiRoute{\n")
	for _, r := range routes {
		b.WriteString("\t{\n")
		fmt.Fprintf(&b, "\t\tMethod: %q,\n\t\tPath: %q,\n\t\tName: %q,\n", r.method, r.path, r.name)
		if doc := strings.TrimSpace(r.doc); doc != "" {
			fmt.Fprintf(&b, "\t\tDoc: %s,\n", strconv.Quote(doc))
		}
		if len(r.query) > 0 {
			var qs []string
			for _, p := range r.query {
				qs = append(qs, p.name+": "+g.mcpType(p.typ, 0))
			}
			fmt.Fprintf(&b, "\t\tQuery: %q,\n", strings.Join(qs, "; "))
		}
		if r.body != nil {
			fmt.Fprintf(&b, "\t\tBody: %q,\n", g.mcpType(r.body, mcpShapeDepth))
		}
		if len(r.multipart) > 0 {
			fmt.Fprintf(&b, "\t\tFiles: %#v,\n", r.multipart)
		}
		resp := ""
		switch r.kind {
		case respJSON:
			resp = g.tsType(r.resp)
		case respBinary:
			resp = "file"
		}
		if resp != "" {
			fmt.Fprintf(&b, "\t\tResponse: %q,\n", resp)
		}
		if len(r.errorCodes) > 0 {
			fmt.Fprintf(&b, "\t\tErrors: %#v,\n", r.errorCodes)
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n")
	return format.Source([]byte(b.String()))
}

// mcpType is tsType with structs written out as their fields, depth
// levels down, and enums as their values.
func (g *gen) mcpType(t types.Type, depth int) string {
	switch t := types.Unalias(t).(type) {
	case *types.Pointer:
		return g.mcpType(t.Elem(), depth)
	case *types.Slice:
		if b, ok := t.Elem().(*types.Basic); ok && b.Kind() == types.Byte {
			return "string"
		}
		return tsArray(g.mcpType(t.Elem(), depth))
	case *types.Array:
		return tsArray(g.mcpType(t.Elem(), depth))
	case *types.Map:
		return "Record<string, " + g.mcpType(t.Elem(), depth) + ">"
	case *types.Named:
		d := g.decls[t.Obj()]
		switch {
		case d == nil:
		case d.isEnum:
			quoted := make([]string, len(d.enum))
			for i, v := range d.enum {
				quoted[i] = tsString(v)
			}
			return strings.Join(quoted, " | ")
		case depth > 0:
			return "{ " + strings.Join(g.mcpFields(d, depth-1), "; ") + " }"
		}
	}
	return g.tsType(t)
}

func (g *gen) mcpFields(d *decl, depth int) []string {
	var out []string
	for _, e := range d.embeds {
		out = append(out, g.mcpFields(g.decls[g.byName[e]], depth)...)
	}
	for _, f := range d.fields {
		t := f.tsType
		if t == "" {
			t = g.mcpType(f.typ, depth)
		}
		opt := ""
		if _, isPtr := f.typ.(*types.Pointer); f.optional || isPtr {
			opt = "?"
		}
		out = append(out, tsKey(f.json)+opt+": "+t)
	}
	return out
}
