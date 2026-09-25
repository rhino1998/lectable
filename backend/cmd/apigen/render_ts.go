package main

import (
	"fmt"
	"go/types"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

func (g *gen) tsType(t types.Type) string {
	switch special(t) {
	case "time":
		return "string"
	case "raw":
		return "unknown"
	}
	switch t := types.Unalias(t).(type) {
	case *types.Basic:
		switch {
		case t.Info()&types.IsString != 0:
			return "string"
		case t.Info()&types.IsBoolean != 0:
			return "boolean"
		case t.Info()&types.IsNumeric != 0:
			return "number"
		}
	case *types.Pointer:
		return g.tsType(t.Elem()) + " | null"
	case *types.Slice:
		if b, ok := t.Elem().(*types.Basic); ok && b.Kind() == types.Byte {
			return "string"
		}
		return tsArray(g.tsType(t.Elem()))
	case *types.Array:
		return tsArray(g.tsType(t.Elem()))
	case *types.Map:
		return "Record<string, " + g.tsType(t.Elem()) + ">"
	case *types.Interface:
		return "unknown"
	case *types.Named:
		if d, ok := g.decls[t.Obj()]; ok && d != nil {
			return d.name
		}
		return g.tsType(t.Underlying())
	}
	panic(fmt.Sprintf("apigen: no TS mapping for %s", t))
}

func tsArray(elem string) string {
	if strings.ContainsAny(elem, " |") {
		return "(" + elem + ")[]"
	}
	return elem + "[]"
}

// tsIdent matches the type names in a tstype override (quoted literals
// start with a quote, so they never match).
var tsIdent = regexp.MustCompile(`\b[A-Z][A-Za-z0-9_]*\b`)

func tsKey(k string) string {
	for i, r := range k {
		if !unicode.IsLetter(r) && r != '_' && r != '$' && (i == 0 || !unicode.IsDigit(r)) {
			return "'" + k + "'"
		}
	}
	return k
}

func enumConstName(d *decl) string { return upperSnake(plural(d.name)) }

func (g *gen) renderTS(cat *catalogData, topics []liveTopic, routes []*route) []byte {
	var b strings.Builder
	b.WriteString("// " + headerNote + "\n\n")
	imports := map[string]bool{}
	for _, d := range g.decls {
		for _, f := range d.fields {
			for _, id := range tsIdent.FindAllString(f.tsType, -1) {
				if o, ok := g.byName[id]; !ok || g.decls[o].tsHand {
					imports[id] = true
				}
			}
		}
	}
	if len(imports) > 0 {
		var names []string
		for n := range imports {
			names = append(names, n)
		}
		sort.Strings(names)
		b.WriteString("import type { " + strings.Join(names, ", ") + " } from './types'\n\n")
	}

	b.WriteString("// ---- Enums ----\n")
	for _, d := range sortedDecls(g) {
		if !d.isEnum {
			continue
		}
		b.WriteString("\n")
		writeDoc(&b, "", d.doc)
		quoted := make([]string, len(d.enum))
		for i, v := range d.enum {
			quoted[i] = tsString(v)
		}
		b.WriteString("export const " + enumConstName(d) + " = [" + strings.Join(quoted, ", ") + "] as const\n")
		b.WriteString("export type " + d.name + " = (typeof " + enumConstName(d) + ")[number]\n")
	}

	b.WriteString("\n// ---- Catalog ----\n")
	for _, l := range cat.lists {
		b.WriteString("\n")
		writeDoc(&b, "", l.doc)
		b.WriteString("export const " + l.name + " = [\n")
		v := reflect.ValueOf(l.value)
		for i := 0; i < v.Len(); i++ {
			b.WriteString("  " + tsLiteral(v.Index(i)) + ",\n")
		}
		b.WriteString("] as const satisfies readonly " + g.elemName(l) + "[]\n")
		b.WriteString("export type " + l.idType + " = (typeof " + l.name + ")[number]['id']\n")
	}
	for _, s := range cat.scalars {
		b.WriteString("\n")
		writeDoc(&b, "", s.doc)
		b.WriteString("export const " + s.name + ": " + s.typ + " = " + tsString(s.value) + "\n")
	}

	b.WriteString("\n// ---- Types ----\n")
	for _, d := range sortedDecls(g) {
		if d.tsHand || d.isEnum {
			continue
		}
		b.WriteString("\n")
		writeDoc(&b, "", d.doc)
		b.WriteString("export interface " + d.name)
		if len(d.embeds) > 0 {
			b.WriteString(" extends " + strings.Join(d.embeds, ", "))
		}
		b.WriteString(" {\n")
		for _, f := range d.fields {
			writeDoc(&b, "  ", f.doc)
			t := f.tsType
			if t == "" {
				t = g.tsType(f.typ)
			}
			opt := ""
			if _, isPtr := f.typ.(*types.Pointer); f.optional || (isPtr && f.tsType == "") {
				opt = "?"
			}
			b.WriteString("  " + tsKey(f.json) + opt + ": " + t + "\n")
		}
		b.WriteString("}\n")
	}

	b.WriteString("\n// ---- Live topics ----\n\n")
	writeDoc(&b, "", "Every live topic (GET /api/events - see live.ts): the params it's subscribed with and the value it carries.")
	b.WriteString("export interface LiveTopics {\n")
	for _, t := range topics {
		writeDoc(&b, "  ", t.doc)
		b.WriteString("  " + tsKey(t.name) + ": { params: " + tsTopicParams(t.params) + "; data: " + g.tsType(t.data) + " }\n")
	}
	b.WriteString("}\n")

	b.WriteString("\n// ---- Routes ----\n\n")
	writeDoc(&b, "", "Every REST route, keyed \"<METHOD> <path>\": its path and query params, body, response (void for a 204, Blob for a file), and the errorResponse codes it can answer with.")
	b.WriteString("export interface Routes {\n")
	for _, r := range routes {
		writeDoc(&b, "  ", r.doc)
		b.WriteString("  " + tsString(r.key()) + ": {\n")
		b.WriteString("    path: " + g.tsParams(r.pathParams, false) + "\n")
		b.WriteString("    query: " + g.tsParams(r.query, true) + "\n")
		body := "never"
		switch {
		case len(r.multipart) > 0:
			body = "FormData"
		case r.body != nil:
			body = g.tsType(r.body)
		}
		b.WriteString("    body: " + body + "\n")
		resp := "void"
		switch r.kind {
		case respJSON:
			resp = g.tsType(r.resp)
		case respBinary:
			resp = "Blob"
		}
		b.WriteString("    response: " + resp + "\n")
		codes := "never"
		if len(r.errorCodes) > 0 {
			var qs []string
			for _, c := range r.errorCodes {
				qs = append(qs, tsString(c))
			}
			codes = strings.Join(qs, " | ")
		}
		b.WriteString("    errorCode: " + codes + "\n")
		b.WriteString("  }\n")
	}
	b.WriteString("}\n\n")
	writeDoc(&b, "", "How each route's response body is read.")
	b.WriteString("export const ROUTE_RESPONSE_KINDS = {\n")
	for _, r := range routes {
		b.WriteString("  " + tsString(r.key()) + ": " + tsString(string(r.kind)) + ",\n")
	}
	b.WriteString("} as const satisfies Record<keyof Routes, 'json' | 'none' | 'binary'>\n")
	return []byte(b.String())
}

func tsTopicParams(ps []topicParam) string {
	if len(ps) == 0 {
		return "Record<never, never>"
	}
	var parts []string
	for _, p := range ps {
		t := "string"
		if p.typ == "int" {
			t = "number"
		}
		parts = append(parts, tsKey(p.name)+": "+t)
	}
	return "{ " + strings.Join(parts, "; ") + " }"
}

// tsParams renders path/query params. An untyped query param (plain
// string on the Go side) also takes a number or boolean, since
// URLSearchParams stringifies them.
func (g *gen) tsParams(ps []param, optional bool) string {
	if len(ps) == 0 {
		return "Record<never, never>"
	}
	var parts []string
	for _, p := range ps {
		t := g.tsType(p.typ)
		if optional && t == "string" {
			t = "string | number | boolean"
		}
		opt := ""
		if optional {
			opt = "?"
		}
		parts = append(parts, tsKey(p.name)+opt+": "+t)
	}
	return "{ " + strings.Join(parts, "; ") + " }"
}
