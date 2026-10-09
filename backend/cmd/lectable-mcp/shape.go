package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// selectPath picks part of a decoded JSON value by a dot path: a field
// name steps into an object, a number indexes an array, and * maps the
// rest of the path over every element ("queued.*.kind").
func selectPath(v any, path string) (any, error) {
	if path == "" {
		return v, nil
	}
	seg, rest, _ := strings.Cut(path, ".")
	switch t := v.(type) {
	case map[string]any:
		child, ok := t[seg]
		if !ok {
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			return nil, fmt.Errorf("no field %q (have %s)", seg, strings.Join(sortedStrings(keys), ", "))
		}
		return selectPath(child, rest)
	case []any:
		if seg == "*" {
			out := make([]any, 0, len(t))
			for _, e := range t {
				c, err := selectPath(e, rest)
				if err != nil {
					return nil, err
				}
				out = append(out, c)
			}
			return out, nil
		}
		i, err := strconv.Atoi(seg)
		if err != nil {
			return nil, fmt.Errorf("%q steps into an array of %d: use an index or *", seg, len(t))
		}
		if i < 0 || i >= len(t) {
			return nil, fmt.Errorf("index %d outside an array of %d", i, len(t))
		}
		return selectPath(t[i], rest)
	}
	return nil, fmt.Errorf("%q steps into a value that has no fields", seg)
}

// shrinkLevels are tried in order until the rendered value fits: how many
// elements of each array and characters of each string are kept.
var shrinkLevels = []struct{ items, chars int }{
	{200, 4000}, {50, 1000}, {20, 400}, {8, 200}, {3, 100},
}

// renderJSON renders a decoded JSON value in at most maxChars, cutting
// long arrays and strings (and saying so) rather than the text's tail, so
// what comes back is still the shape of the whole answer.
func renderJSON(v any, maxChars int) string {
	out := marshal(v)
	if len(out) <= maxChars {
		return out
	}
	for _, l := range shrinkLevels {
		out = marshal(shrink(v, l.items, l.chars))
		if len(out) <= maxChars {
			return out + fmt.Sprintf("\n[shortened to fit %d chars: arrays to %d items, strings to %d chars - narrow it with select, or raise max_chars]", maxChars, l.items, l.chars)
		}
	}
	return clip(out, maxChars) + fmt.Sprintf("\n[cut at %d chars even with arrays and strings shortened - narrow it with select]", maxChars)
}

func marshal(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimRight(b.String(), "\n")
}

func shrink(v any, items, chars int) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = shrink(e, items, chars)
		}
		return out
	case []any:
		n := min(len(t), items)
		out := make([]any, 0, n+1)
		for _, e := range t[:n] {
			out = append(out, shrink(e, items, chars))
		}
		if n < len(t) {
			out = append(out, fmt.Sprintf("… %d more (of %d)", len(t)-n, len(t)))
		}
		return out
	case string:
		return clip(t, chars)
	}
	return v
}

// clip cuts s to about n bytes on a rune boundary, noting how much went.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("…(+%d chars)", len(s)-cut)
}

// oneLine is clip for text shown on a single line of a listing.
func oneLine(s string, n int) string {
	return clip(strings.Join(strings.Fields(s), " "), n)
}
