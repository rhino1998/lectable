// Package direction is a dialogue line's free-form speech direction, for a
// clone model that takes direction as text (voices.DeliveryDirected - Fish
// Audio Drama 3): a style for the whole line, point tags at a spot in it,
// and tags scoped to a run of its words. Drama 3 reads all three in the
// text it is given (its technical report, Appendix C):
//
//	[icy calm, as if you already accepted it] Fine.     a style prefix
//	Oh, you didn't. [sighs] Fine.                       a point tag
//	Promise me <whisper>you won't tell her</whisper>.   a scoped tag
//
// A direction is stored apart from the line's text (store.Paragraph.
// Direction), anchored by rune offsets into it, and only written into the
// generation text (Insertions) - the words a reader sees, the aligner
// times and the completeness check hears are never touched, the same
// invariant internal/deliverytags keeps for Higgs's tags.
//
// It replaces, for such a model, the fixed emotion labels' reference-clip
// variants (internal/emotions); see backend/docs/drama3-direction-plan.md.
// A leaf package beside deliverytags, so speakerattr (parsing the LLM's
// annotated lines), store (resolving generation text) and httpapi (the
// reader's override) share one parser.
package direction

import (
	"crypto/sha1"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rhino1998/lectable/backend/internal/deliverytags"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
)

// PointTags are the point tags a direction may use: Table 10 of the Drama 3
// report ([pause] [gasps] [clears throat] [hesitates]), the README's and
// the first port test's [sighs]/[laughs], and the report's [coughs], plus a
// few more vocal events of the same kind. A tag outside the list is
// dropped when parsed - an unknown one risks being read aloud. Widen it
// from what generation's spoken-tag check logs, not by guesswork. The
// fish_drama3 port's chunker keeps a copy (audio.cpp session.cpp
// is_point_tag) to tell a style it repeats on every chunk from a point tag
// it leaves in place - change both together.
var PointTags = []string{
	"pause", "sighs", "laughs", "chuckles", "gasps", "sobs", "sniffs",
	"groans", "coughs", "clears throat", "hesitates", "scoffs", "gulps",
}

// SpanTags are the scoped tags a direction may use (Table 10).
var SpanTags = []string{"whisper", "slow", "emphasis"}

// MaxStyleWords caps a style's length: a short delivery note
// ("dry, barely holding back a laugh"), not a paragraph of acting notes.
const MaxStyleWords = 12

// maxStyleChars bounds a style that is few words but long ones.
const maxStyleChars = 120

// Mark is a point tag before rune At of the text.
type Mark struct {
	At  int    `json:"at"`
	Tag string `json:"tag"`
}

// Span is a scoped tag over runes [Start, End) of the text.
type Span struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Tag   string `json:"tag"`
}

// Direction is one line's speech direction. The zero value is no
// direction. For is TextHash of the text the offsets were written against:
// a direction whose text has since changed (an edit, a reimport) no longer
// applies (Current).
type Direction struct {
	Style string `json:"style,omitempty"`
	Marks []Mark `json:"marks,omitempty"`
	Spans []Span `json:"spans,omitempty"`
	For   string `json:"for,omitempty"`
}

// Empty reports whether d directs nothing.
func (d Direction) Empty() bool {
	return d.Style == "" && len(d.Marks) == 0 && len(d.Spans) == 0
}

// Current reports whether d was written for text.
func (d Direction) Current(text string) bool {
	return !d.Empty() && d.For == TextHash(text)
}

// Equal reports whether a and b direct the same way (For aside).
func (a Direction) Equal(b Direction) bool {
	return a.Style == b.Style && slices.Equal(a.Marks, b.Marks) && slices.Equal(a.Spans, b.Spans)
}

// TextHash is what Direction.For holds for text.
func TextHash(text string) string {
	sum := sha1.Sum([]byte(text))
	return hex.EncodeToString(sum[:8])
}

// Scan implements sql.Scanner over the paragraphs.direction JSON column.
func (d *Direction) Scan(v any) error {
	*d = Direction{}
	var raw []byte
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		return fmt.Errorf("direction: scan %T", v)
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, d)
}

// Value implements driver.Valuer.
func (d Direction) Value() (driver.Value, error) {
	if d.Empty() {
		return "{}", nil
	}
	b, err := json.Marshal(d)
	return string(b), err
}

// isPointTag/isSpanTag report membership, ignoring case and spacing.
func isPointTag(tag string) bool { return slices.Contains(PointTags, tag) }
func isSpanTag(tag string) bool  { return slices.Contains(SpanTags, tag) }

// normTag folds a tag as written ("Clears  Throat") to its list form.
func normTag(tag string) string {
	return strings.Join(strings.Fields(strings.ToLower(tag)), " ")
}

// CleanStyle normalizes a style: trimmed, whitespace collapsed, no
// brackets or tag markup, no trailing period. "" for one that breaks
// the rules (too long, or a line break - Drama 3's controls are one short
// span each).
func CleanStyle(style string) string {
	if strings.ContainsAny(style, "[]<>\n|") {
		return ""
	}
	style = strings.Join(strings.Fields(style), " ")
	style = strings.TrimRight(style, ".;")
	if len(strings.Fields(style)) > MaxStyleWords || len(style) > maxStyleChars {
		return ""
	}
	return style
}

// ErrTextChanged is Parse's answer when annotated's words aren't text's.
var ErrTextChanged = errors.New("direction: annotated line changes the text")

// Parse reads annotated - text with direction written into it the way
// Drama 3 reads it ("[dry, amused] Oh, you didn't. [sighs] Fine.") - into
// a Direction anchored to text. A bracket at the very start is the style
// unless it is a known point tag; a bracket anywhere else is a point tag.
// Unknown point and span tags are dropped (along with the span's words'
// markup, never the words), as is an over-long style. Annotated's letters
// and digits, in order, must be text's (case aside) - punctuation,
// quote marks and spacing may differ, since an LLM often drops a line's
// quote marks - else ErrTextChanged. A span left unclosed, closed out of
// order, or nested is an error too.
func Parse(text, annotated string) (Direction, error) {
	type control struct {
		pos   int // index into the stripped text's alnum sequence: before alnum #pos
		kind  byte
		tag   string
		start bool
	}
	var (
		d        Direction
		controls []control
		alnums   []rune
	)
	s := strings.TrimSpace(annotated)
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		switch r {
		case '[':
			end := strings.IndexByte(s, ']')
			if end < 0 {
				return Direction{}, fmt.Errorf("direction: unclosed [ in %q", annotated)
			}
			inner := s[1:end]
			if strings.Contains(text, s[:end+1]) {
				// The book's own bracket ("[Skill: Fireball]"): words.
				s = s[1:]
				continue
			}
			s = s[end+1:]
			tag := normTag(inner)
			switch {
			case isPointTag(tag):
				controls = append(controls, control{pos: len(alnums), kind: 'p', tag: tag})
			case len(alnums) == 0 && d.Style == "":
				d.Style = CleanStyle(inner)
			}
			continue
		case '<':
			end := strings.IndexByte(s, '>')
			if end < 0 {
				return Direction{}, fmt.Errorf("direction: unclosed < in %q", annotated)
			}
			if strings.Contains(text, s[:end+1]) {
				s = s[1:]
				continue
			}
			inner := strings.TrimSpace(s[1:end])
			s = s[end+1:]
			closing := strings.HasPrefix(inner, "/")
			tag := normTag(strings.TrimPrefix(inner, "/"))
			if isSpanTag(tag) {
				controls = append(controls, control{pos: len(alnums), kind: 's', tag: tag, start: !closing})
			}
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			alnums = append(alnums, unicode.ToLower(r))
		}
		s = s[size:]
	}

	// textAlnum[i] is the rune index in text of its i'th letter or digit.
	var textAlnum []int
	var textRunes []rune
	for i, r := range []rune(text) {
		textRunes = append(textRunes, r)
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if len(textAlnum) >= len(alnums) || unicode.ToLower(r) != alnums[len(textAlnum)] {
				return Direction{}, ErrTextChanged
			}
			textAlnum = append(textAlnum, i)
		}
	}
	if len(textAlnum) != len(alnums) {
		return Direction{}, ErrTextChanged
	}

	var open *Span
	for _, c := range controls {
		switch c.kind {
		case 'p':
			d.Marks = append(d.Marks, Mark{At: beforeWord(textRunes, textAlnum, c.pos), Tag: c.tag})
		case 's':
			if c.start {
				if open != nil {
					return Direction{}, fmt.Errorf("direction: <%s> inside <%s>", c.tag, open.Tag)
				}
				open = &Span{Start: beforeWord(textRunes, textAlnum, c.pos), Tag: c.tag}
				continue
			}
			if open == nil || open.Tag != c.tag {
				return Direction{}, fmt.Errorf("direction: </%s> closes nothing", c.tag)
			}
			open.End = afterWord(textRunes, textAlnum, c.pos)
			if open.End > open.Start {
				d.Spans = append(d.Spans, *open)
			}
			open = nil
		}
	}
	if open != nil {
		return Direction{}, fmt.Errorf("direction: <%s> never closed", open.Tag)
	}
	// Point tags at one spot keep one copy each.
	d.Marks = slices.CompactFunc(d.Marks, func(a, b Mark) bool { return a == b })
	if !d.Empty() {
		d.For = TextHash(text)
	}
	return d, nil
}

// beforeWord places a control that came before alnum #i of the text: at
// the start of that alnum's word, backed up over any opening punctuation
// (a quote mark) glued to it - so "[sighs] “Fine" anchors before the “.
// Past the last alnum, it's the end of the text.
func beforeWord(runes []rune, alnum []int, i int) int {
	if i >= len(alnum) {
		return len(runes)
	}
	at := alnum[i]
	for at > 0 && !unicode.IsSpace(runes[at-1]) && !isWordRune(runes[at-1]) {
		at--
	}
	// Inside a word ("don[pause]'t") the control moves to the word's start.
	for at > 0 && isWordRune(runes[at-1]) {
		at--
	}
	return at
}

// afterWord places a closing control that came after alnum #i-1: at the
// end of that alnum's word.
func afterWord(runes []rune, alnum []int, i int) int {
	if i == 0 {
		return 0
	}
	at := alnum[i-1] + 1
	for at < len(runes) && isWordRune(runes[at]) {
		at++
	}
	return at
}

// isWordRune is a letter, digit, or an apostrophe/hyphen inside a word.
func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == '’' || r == '-'
}

// Annotate writes d into text the way Parse reads it back - what the
// reader's editor shows and the pass's prompt quotes.
func (d Direction) Annotate(text string) string {
	runes := []rune(text)
	ins := d.runeInsertions(len(runes))
	var b strings.Builder
	if d.Style != "" {
		b.WriteString("[" + d.Style + "] ")
	}
	pos := 0
	for _, in := range ins {
		b.WriteString(string(runes[pos:in.at]))
		b.WriteString(in.text)
		pos = in.at
	}
	b.WriteString(string(runes[pos:]))
	return b.String()
}

type runeInsertion struct {
	at   int
	text string
}

// runeInsertions lists d's marks and span tags in text order, as rendered:
// a point tag followed by a space ("[sighs] Fine"), or preceded by one at
// the end of the line; span tags hugging their words. A close sorts before
// a mark or open at the same spot.
func (d Direction) runeInsertions(n int) []runeInsertion {
	type keyed struct {
		runeInsertion
		order int
	}
	var all []keyed
	for _, sp := range d.Spans {
		if sp.Start < 0 || sp.End > n || sp.Start >= sp.End {
			continue
		}
		all = append(all,
			keyed{runeInsertion{sp.Start, "<" + sp.Tag + ">"}, 2},
			keyed{runeInsertion{sp.End, "</" + sp.Tag + ">"}, 0})
	}
	for _, m := range d.Marks {
		if m.At < 0 || m.At > n {
			continue
		}
		text := "[" + m.Tag + "] "
		if m.At == n {
			text = " [" + m.Tag + "]"
		}
		all = append(all, keyed{runeInsertion{m.At, text}, 1})
	}
	slices.SortStableFunc(all, func(a, b keyed) int {
		if a.at != b.at {
			return a.at - b.at
		}
		return a.order - b.order
	})
	out := make([]runeInsertion, len(all))
	for i, k := range all {
		out[i] = k.runeInsertion
	}
	return out
}

// Insertions writes d into a generation text as deliverytags insertions
// (byte offsets into text), for pronounce.Apply to compose with subs - the
// line's own substitutions. The style comes first, at offset 0. Apply
// drops an insertion strictly inside a substitution, which would unbalance
// a span, so one there is moved to the substitution's edge: an open tag or
// point tag to its start, a close to its end.
func (d Direction) Insertions(text string, subs []pronounce.Substitution) []deliverytags.Insertion {
	if d.Empty() {
		return nil
	}
	var out []deliverytags.Insertion
	if d.Style != "" {
		out = append(out, deliverytags.Insertion{Offset: 0, Tag: "[" + d.Style + "] "})
	}
	runes := []rune(text)
	byteAt := make([]int, len(runes)+1)
	off := 0
	for i, r := range runes {
		byteAt[i] = off
		off += utf8.RuneLen(r)
	}
	byteAt[len(runes)] = off
	for _, in := range d.runeInsertions(len(runes)) {
		at := byteAt[in.at]
		closing := strings.HasPrefix(in.text, "</")
		for _, s := range subs {
			if at > s.Offset && at < s.Offset+s.Length {
				if closing {
					at = s.Offset + s.Length
				} else {
					at = s.Offset
				}
			}
		}
		out = append(out, deliverytags.Insertion{Offset: at, Tag: in.text})
	}
	return out
}

// Strip removes every direction control from a generation text, leaving
// the words - for a retry that drops a line's direction, and for checks
// comparing speech against what was meant to be said.
func Strip(s string) string {
	var b strings.Builder
	for len(s) > 0 {
		switch s[0] {
		case '[':
			if end := strings.IndexByte(s, ']'); end > 0 {
				s = strings.TrimLeft(s[end+1:], " ")
				continue
			}
		case '<':
			if end := strings.IndexByte(s, '>'); end > 0 && !strings.HasPrefix(s, "<|") {
				s = s[end+1:]
				continue
			}
		}
		b.WriteByte(s[0])
		s = s[1:]
	}
	return strings.TrimSpace(b.String())
}

// StripTags removes point and span tags from a generation text but keeps
// a leading style - the first step down generation's retry ladder.
func StripTags(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") {
		if end := strings.IndexByte(s, ']'); end > 0 && !isPointTag(normTag(s[1:end])) {
			return s[:end+1] + " " + Strip(s[end+1:])
		}
	}
	return Strip(s)
}

// LiteralBracketSubs removes the book's own square and angle brackets
// from a line bound for a directed model ("[Skill: Fireball]" is read
// "Skill: Fireball"), which would otherwise take them for direction.
func LiteralBracketSubs(text string) []pronounce.Substitution {
	var out []pronounce.Substitution
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '[', ']', '<', '>':
			out = append(out, pronounce.Substitution{Offset: i, Length: 1, Replacement: ""})
		}
	}
	return out
}

// CarryAcross mends a directed generation text cut in two (left then
// right) by something that knows nothing of direction - generation's
// bisect retry: the style prefix is repeated at the head of right, and a
// span open at the cut is closed at the end of left and reopened at the
// head of right. Text without direction comes back unchanged.
func CarryAcross(left, right string) (string, string) {
	var head string
	if t := strings.TrimSpace(left); strings.HasPrefix(t, "[") {
		if end := strings.IndexByte(t, ']'); end > 0 && !isPointTag(normTag(t[1:end])) {
			head = t[:end+1] + " "
		}
	}
	if open := openSpan(left); open != "" {
		left = strings.TrimRight(left, " ") + "</" + open + ">"
		right = "<" + open + ">" + strings.TrimLeft(right, " ")
	}
	return left, head + right
}

// openSpan is the span tag left open at the end of s, or "".
func openSpan(s string) string {
	open := ""
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			return open
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			return open
		}
		inner := s[i+1 : i+end]
		s = s[i+end+1:]
		switch tag := normTag(strings.TrimPrefix(inner, "/")); {
		case !isSpanTag(tag):
		case strings.HasPrefix(inner, "/"):
			open = ""
		default:
			open = tag
		}
	}
}
