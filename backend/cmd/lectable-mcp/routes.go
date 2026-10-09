package main

import (
	"fmt"
	"sort"
	"strings"
)

// apiRoute is one backend route as cmd/apigen read it from its handler
// (routes_gen.go). Query, Body and Response are TypeScript-style shapes.
type apiRoute struct {
	Method, Path string
	Name         string // the handler, less "handle": getBook
	Doc          string
	Query        string   // "scope: string; all: string", every one optional
	Body         string   // the JSON body's shape, "" for none
	Files        []string // multipart file fields, for an upload
	Response     string   // "" for a 204, "file" for a binary body
	Errors       []string // error codes it can answer with
}

func (r apiRoute) key() string { return r.Method + " " + r.Path }

// matches reports whether a concrete path ("/api/books/abc/voice") is one
// this route's pattern serves.
func (r apiRoute) matches(path string) bool {
	want := strings.Split(r.Path, "/")
	got := strings.Split(path, "/")
	if len(want) != len(got) {
		return false
	}
	for i, seg := range want {
		if strings.HasPrefix(seg, "{") {
			if got[i] == "" {
				return false
			}
			continue
		}
		if seg != got[i] {
			return false
		}
	}
	return true
}

// findRoute is the route serving method and path. A literal segment beats
// a parameter ("/chapters/insert" over "/chapters/{idx}"), as in ServeMux.
// With none, methods lists what the path does accept.
func findRoute(method, path string) (route *apiRoute, methods []string) {
	best := -1
	for i := range apiRoutes {
		r := &apiRoutes[i]
		if !r.matches(path) {
			continue
		}
		if r.Method != method {
			methods = append(methods, r.Method)
			continue
		}
		if literal := strings.Count(r.Path, "/") - strings.Count(r.Path, "{"); literal > best {
			best, route = literal, r
		}
	}
	return route, methods
}

func firstSentence(doc string) string {
	doc = strings.Join(strings.Fields(doc), " ")
	// A handler's doc starts with its own name; that's noise here.
	if name, rest, ok := strings.Cut(doc, " "); ok && strings.HasPrefix(name, "handle") {
		doc = rest
	}
	if i := strings.Index(doc, ". "); i >= 0 {
		doc = doc[:i+1]
	}
	return clip(doc, 160)
}

func (r apiRoute) summary() string {
	if s := firstSentence(r.Doc); s != "" {
		return r.key() + " - " + s
	}
	return r.key()
}

func (r apiRoute) detail() string {
	var b strings.Builder
	b.WriteString(r.key() + "  (" + r.Name + ")\n")
	if r.Query != "" {
		b.WriteString("  query (all optional): " + r.Query + "\n")
	}
	if r.Body != "" {
		b.WriteString("  body: " + r.Body + "\n")
	}
	if len(r.Files) > 0 {
		b.WriteString("  upload (multipart file fields): " + strings.Join(r.Files, ", ") + "\n")
	}
	switch r.Response {
	case "":
		b.WriteString("  response: none (204)\n")
	case "file":
		b.WriteString("  response: a file (api_get saves it and gives the path)\n")
	default:
		b.WriteString("  response: " + r.Response + "\n")
	}
	if len(r.Errors) > 0 {
		b.WriteString("  error codes: " + strings.Join(r.Errors, ", ") + "\n")
	}
	if doc := strings.TrimSpace(r.Doc); doc != "" {
		b.WriteString("  " + strings.ReplaceAll(doc, "\n", "\n  ") + "\n")
	}
	return b.String()
}

// routesDetailLimit is how many matches are still shown in full.
const routesDetailLimit = 5

// searchRoutes lists the routes matching every word of query (in the
// method, path, handler name or doc), in full when there are few.
func searchRoutes(query string, detail bool) string {
	words := strings.Fields(strings.ToLower(query))
	var hits []apiRoute
	for _, r := range apiRoutes {
		hay := strings.ToLower(r.key() + " " + r.Name + " " + r.Doc)
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if ok {
			hits = append(hits, r)
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("no route matches %q (of %d) - try fewer or other words; docs/api.md has the prose", query, len(apiRoutes))
	}
	var b strings.Builder
	if detail || len(hits) <= routesDetailLimit {
		for _, r := range hits {
			b.WriteString(r.detail() + "\n")
		}
		b.WriteString("More on what each does: backend/docs/api.md (grep the path).")
		return b.String()
	}
	fmt.Fprintf(&b, "%d routes", len(hits))
	if len(words) > 0 {
		fmt.Fprintf(&b, " match %q", query)
	}
	b.WriteString(" - narrow the query (or pass detail) for params and bodies:\n")
	for _, r := range hits {
		if len(words) == 0 {
			b.WriteString(r.key() + "\n") // the whole table: paths only
			continue
		}
		b.WriteString(r.summary() + "\n")
	}
	return b.String()
}

func sortedStrings(s []string) []string {
	sort.Strings(s)
	return s
}
