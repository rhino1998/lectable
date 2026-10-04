package epub

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A play sets who speaks before each speech instead of quoting it:
//
//	THESEUS.<br/>Now, fair Hippolyta, ...        label on its own line
//	FREDDY. There's not one to be had.           inline, in capitals
//	<span class="smcap">Domin.</span> Come in.   in a marked span
//	MEPHISTOPHELES                               alone, the speech in the
//	                                             paragraphs after it
//
// Read as prose, none of it is dialogue (no quote marks) and the narrator
// reads every label aloud. applyPlay finds those labels, and when the book
// is mostly labelled speeches, rewrites each speech as IsQuote blocks
// carrying their Speaker, with the label kept as unspoken Block.Label.
// Directions inside a speech are split out: one saying what happens
// ("[He opens his umbrella and dashes off]") becomes a narration block,
// one saying how the line is delivered ("[Calling.]", "(delighted)") joins
// the label and isn't spoken. Paragraphs that are no one's speech - scene
// descriptions, "Enter Theseus", front matter - stay narration as parsed.

// classHint is what a paragraph's class says about it, where an epub
// bothers: Gutenberg's newer plays mark speeches "drama"/"dialog" and
// directions "stage"/"scenedesc".
type classHint int

const (
	hintNone classHint = iota
	hintSpeech
	hintDirection
)

func classHintOf(class string) classHint {
	for _, c := range strings.Fields(class) {
		switch c {
		case "drama", "dialog", "dialogue", "speech":
			return hintSpeech
		case "stage", "scenedesc", "stagedir", "direction":
			return hintDirection
		}
	}
	return hintNone
}

// Label forms - see the examples above.
const (
	formOwnLine = 'A'
	formInline  = 'B'
	formSpan    = 'C'
	formAlone   = 'D'
)

// playLabel is a speaker label read off the start of a paragraph.
type playLabel struct {
	key   string // the name upper-cased, to count and match by
	name  string // the character's name: "Mrs. Higgins"
	label string // as the source sets it: "MRS. HIGGINS.", "CYRANO (delighted):"
	form  rune
	// strong: set in capitals or a marked span, so recognizable even
	// when it occurs once ("Farewell." opening a speech isn't).
	strong bool
	rest   string // the speech after the label, still marked up; "" for formAlone
}

type playUnit struct {
	start, end int // the chapter's blocks[start:end]
	src        *blockSource
	plain      string
	label      *playLabel
	direction  bool
	accepted   bool
}

const (
	// minLabelCount is how often a label must recur to count as a speaker
	// on its own; a rarer one needs a recurring speaker beside it.
	minLabelCount = 3
	// A book is a play when at least minPlaySpeeches paragraphs, and
	// playSpeechPercent of all of them, start with a recurring label.
	// Measured on 12 Gutenberg plays (35-93%, front and back matter
	// included) and 5 novels (0-1.5%: LitRPG stat blocks, "NAME: ...").
	minPlaySpeeches   = 40
	playSpeechPercent = 15
	// minParenDirections is how many whole-paragraph "(...)" directions
	// make parentheses this book's direction marks - Shakespeare's
	// parentheses are spoken asides, Rostand's are directions.
	minParenDirections = 5
	maxLabelWords      = 5
)

// notSpeakers are capitalized paragraph openers that are structure, not
// people.
var notSpeakers = map[string]bool{
	"ACT": true, "SCENE": true, "CHAPTER": true, "PART": true, "BOOK": true, "VOLUME": true,
	"CURTAIN": true, "THE END": true, "FINIS": true, "END": true, "CONTENTS": true,
	"CHARACTERS": true, "PERSONS": true, "DRAMATIS PERSONAE": true, "DRAMATIS PERSONÆ": true,
	"TIME": true, "PLACE": true, "NOTE": true, "TABLEAU": true,
}

// honorifics end in a period that doesn't end the label: "MRS. HIGGINS."
var honorifics = map[string]bool{
	"MR": true, "MRS": true, "MS": true, "DR": true, "ST": true, "M": true, "MME": true, "MLLE": true,
	"REV": true, "COL": true, "CAPT": true, "GEN": true, "LT": true, "SGT": true, "PROF": true,
}

// applyPlay rewrites chapters as a play (see the top of this file) when
// mode says to, reporting whether it did, and drops every block's src.
func applyPlay(chapters []Chapter, mode PlayMode) bool {
	units := make([][]playUnit, len(chapters))
	if mode != PlayOff {
		for ci, ch := range chapters {
			units[ci] = playUnits(ch.Blocks)
		}
	}
	play := mode != PlayOff && rewritePlay(chapters, units, mode == PlayOn)
	for _, ch := range chapters {
		for i := range ch.Blocks {
			ch.Blocks[i].src = nil
		}
	}
	return play
}

// playUnits groups blocks back into source paragraphs (a block with src
// plus the Inline segments split off it) and reads each one's label.
func playUnits(blocks []Block) []playUnit {
	var units []playUnit
	for i := 0; i < len(blocks); i++ {
		if blocks[i].src == nil {
			continue
		}
		u := playUnit{start: i, end: i + 1, src: blocks[i].src}
		for u.end < len(blocks) && blocks[u.end].Kind == BlockText && blocks[u.end].Inline && blocks[u.end].src == nil {
			u.end++
		}
		u.plain = unmarked(u.src.raw)
		if !u.src.heading {
			u.label = parseLabel(u.src.raw)
		}
		units = append(units, u)
		i = u.end - 1
	}
	return units
}

func rewritePlay(chapters []Chapter, units [][]playUnit, force bool) bool {
	counts := map[string]int{}
	total := 0
	parenDirs, bracketDirs := 0, 0
	hinted := false
	for _, us := range units {
		for _, u := range us {
			if u.src.heading {
				continue
			}
			total++
			if u.label != nil {
				counts[u.label.key]++
			}
			if u.src.hint == hintSpeech {
				hinted = true
			}
			switch {
			case wrappedIn(u.plain, '(', ')'):
				parenDirs++
			case wrappedIn(u.plain, '[', ']'):
				bracketDirs++
			}
		}
	}
	speeches := 0
	forms := map[rune]int{}
	for _, us := range units {
		for _, u := range us {
			if u.label != nil && counts[u.label.key] >= minLabelCount {
				speeches++
				forms[u.label.form]++
			}
		}
	}
	if speeches == 0 || !force && (speeches < minPlaySpeeches || speeches*100 < playSpeechPercent*total) {
		return false
	}
	dominant := formOwnLine
	for f, n := range forms {
		if n > forms[dominant] {
			dominant = f
		}
	}
	style := playStyle{
		parenDirections: parenDirs >= minParenDirections && parenDirs > bracketDirs,
		// Without labels on each paragraph, a speech's later paragraphs
		// can only be told from a scene description by class - or, in a
		// book that sets labels apart from the speech, by assuming so.
		continueUnhinted: !hinted && (dominant == formOwnLine || dominant == formAlone),
	}

	for ci := range chapters {
		us := units[ci]
		for i := range us {
			u := &us[i]
			u.direction = u.label == nil && isDirection(*u, style)
			if u.label == nil {
				continue
			}
			// A label alone in its paragraph only counts in a book set
			// that way: elsewhere it is a "CURTAIN" or a cast list entry.
			if u.label.form == formAlone && dominant != formAlone {
				continue
			}
			u.accepted = counts[u.label.key] >= minLabelCount
		}
		// A character with a line or two: the same form as everyone
		// else's label, next to a recurring speaker's speech.
		for i := range us {
			u := &us[i]
			if u.label == nil || u.accepted || !u.label.strong || u.label.form != dominant {
				continue
			}
			if dominant == formAlone {
				// Nothing but the speech sits beside a label alone in its
				// paragraph, so it is a speaker when speech follows it - a
				// scene's name ("NIGHT") is followed by the first label.
				u.accepted = speechFollows(us, i)
			} else if recurringNeighbour(us, i, -1, counts) || recurringNeighbour(us, i, 1, counts) {
				u.accepted = true
			}
		}
		chapters[ci].Blocks = rewriteChapter(chapters[ci].Blocks, us, style)
	}
	return true
}

func recurringNeighbour(us []playUnit, i, step int, counts map[string]int) bool {
	for j := i + step; j >= 0 && j < len(us); j += step {
		if us[j].direction {
			continue
		}
		return us[j].label != nil && counts[us[j].label.key] >= minLabelCount && !us[j].src.heading
	}
	return false
}

// speechFollows reports whether the paragraph after us[i] (directions
// aside) is unlabelled text.
func speechFollows(us []playUnit, i int) bool {
	for j := i + 1; j < len(us); j++ {
		if us[j].direction {
			continue
		}
		return us[j].label == nil && !us[j].src.heading
	}
	return false
}

type playStyle struct {
	parenDirections  bool
	continueUnhinted bool
}

// rewriteChapter replaces each speech's blocks with its play blocks.
// Everything else - headings, directions, images, breaks, the title block
// Parse prepends - is kept as parsed.
func rewriteChapter(blocks []Block, units []playUnit, style playStyle) []Block {
	out := make([]Block, 0, len(blocks))
	speaker := ""      // whose speech an unlabelled paragraph continues
	pendingLabel := "" // a label alone in its paragraph, for the speech after it
	pos := 0
	for _, u := range units {
		for ; pos < u.start; pos++ {
			if blocks[pos].Kind == BlockText {
				speaker, pendingLabel = "", "" // the chapter's title block
			}
			out = append(out, blocks[pos])
		}
		pos = u.end
		keep := func() { out = append(out, blocks[u.start:u.end]...) }
		switch {
		case u.src.heading || endsPlay(u.plain):
			speaker, pendingLabel = "", ""
			keep()
		case u.accepted:
			speaker = u.label.name
			if u.label.form == formAlone {
				pendingLabel = u.label.label
				continue
			}
			out = append(out, speechBlocks(u.label.rest, speaker, u.label.label, style)...)
			pendingLabel = ""
		case u.direction:
			keep()
		case speaker != "" && (u.src.hint == hintSpeech || style.continueUnhinted && u.src.hint == hintNone):
			out = append(out, speechBlocks(u.src.raw, speaker, pendingLabel, style)...)
			pendingLabel = ""
		default:
			keep()
		}
	}
	return append(out, blocks[pos:]...)
}

var (
	entranceRe = regexp.MustCompile(`^(?i:enter|exit|exeunt|re-enter|curtain|scene)\b`)
	playEndRe  = regexp.MustCompile(`^(?:\*\*\*|(?i:the end|finis|curtain)\b)`)
	// deliveryRe matches a direction saying how a line is spoken rather
	// than what happens: "aside", "to Hermia", "smiling", "coldly".
	deliveryRe = regexp.MustCompile(`^(?i:aside|apart|aloud|to\s|\pL+ing\b|\pL+ly\b)`)
)

// endsPlay reports whether a paragraph ends the speeches before it rather
// than continuing them: "THE END", Gutenberg's "*** END OF THE PROJECT".
func endsPlay(plain string) bool { return playEndRe.MatchString(plain) }

// isDirection reports whether an unlabelled paragraph is a stage
// direction: classed as one, wholly bracketed or italic, or an entrance.
func isDirection(u playUnit, style playStyle) bool {
	if u.src.hint == hintDirection {
		return true
	}
	if wrappedIn(u.plain, '[', ']') || entranceRe.MatchString(u.plain) {
		return true
	}
	if wholeSpan(strings.TrimSpace(playMarks.Replace(u.src.raw)), italicStart, italicEnd) {
		return true
	}
	// Nothing but directions: "(He turns.)", "[Aside.] [Exit.]"
	for _, p := range splitDirections(playMarks.Replace(u.src.raw), style) {
		if !p.direction && hasWords(unmarked(p.raw)) {
			return false
		}
	}
	return true
}

// wrappedIn reports whether s is one open...close span, a trailing period
// aside: "[Exit Philostrate.]", "(Bellerose comes forward and bows.)".
func wrappedIn(s string, open, close rune) bool {
	s = strings.TrimSuffix(strings.TrimSpace(s), ".")
	if len(s) < 2 || rune(s[0]) != open || rune(s[len(s)-1]) != close {
		return false
	}
	return !strings.ContainsRune(s[1:len(s)-1], close)
}

// wholeSpan reports whether s is exactly one start...end marked span.
func wholeSpan(s string, start, end rune) bool {
	if !strings.HasPrefix(s, string(start)) || !strings.HasSuffix(strings.TrimRight(s, ". "), string(end)) {
		return false
	}
	return strings.Count(s, string(start)) == 1
}

// unmarked is raw as plain prose with every sentinel gone.
func unmarked(raw string) string {
	return normalizeWhitespace(strings.Map(func(r rune) rune {
		switch r {
		case lineBreak:
			return ' '
		case labelStart, labelEnd, boldStart, boldEnd, italicStart, italicEnd:
			return -1
		}
		return r
	}, raw))
}

// parseLabel reads the speaker label a paragraph starts with, nil if it
// has none. Whether the label is really a speaker's is rewritePlay's call:
// this only recognizes the shape.
func parseLabel(raw string) *playLabel {
	first, others, _ := strings.Cut(raw, string(lineBreak))
	for strings.TrimSpace(unmarked(first)) == "" && others != "" {
		first, others, _ = strings.Cut(others, string(lineBreak))
	}
	first = strings.TrimSpace(first)
	hasOthers := unmarked(others) != ""

	l := spanLabel(first)
	if l == nil {
		l = capsLabel(first)
	}
	if l == nil && hasOthers {
		l = ownLineLabel(first)
	}
	if l == nil {
		return nil
	}
	l.key = strings.ToUpper(l.name)
	if notSpeakers[l.key] || notSpeakers[firstWord(l.key)] || !hasName(l.key) {
		return nil
	}
	l.name = titleCase(l.name)
	l.rest = strings.TrimSpace(l.rest + " " + strings.ReplaceAll(others, string(lineBreak), " "))
	if unmarked(l.rest) == "" {
		l.form, l.rest = formAlone, ""
	}
	return l
}

func firstWord(s string) string {
	w, _, _ := strings.Cut(s, " ")
	return w
}

// hasName rejects what can't be a name: a lone letter or a Roman numeral
// ("I.", "IV.").
func hasName(key string) bool {
	letters := 0
	roman := true
	for _, r := range key {
		if unicode.IsLetter(r) {
			letters++
			if !strings.ContainsRune("IVXLC", r) {
				roman = false
			}
		}
	}
	return letters >= 2 && !roman
}

// spanLabel reads a label set in a marked span (small caps, a "charname"
// class, bold) at the start of line: `<span class="smcap">Domin.</span>`.
func spanLabel(line string) *playLabel {
	var end rune
	switch r, _ := utf8.DecodeRuneInString(line); r {
	case labelStart:
		end = labelEnd
	case boldStart:
		end = boldEnd
	default:
		return nil
	}
	start := utf8.RuneLen(end)
	i := strings.IndexRune(line, end)
	if i < 0 {
		return nil
	}
	inner := unmarked(line[start:i])
	name := strings.TrimSpace(strings.TrimRight(inner, ".: "))
	if name == "" || len(strings.Fields(name)) > maxLabelWords || strings.ContainsAny(name, ",;!?[]()\"“”") {
		return nil
	}
	if honorifics[strings.ToUpper(name)] { // "<b>MRS.</b> ..." isn't a whole name
		return nil
	}
	rest := line[i+utf8.RuneLen(end):]
	label := inner
	trimmed := strings.TrimLeft(rest, " ")
	attached, after := leadingDirection(trimmed)
	if attached != "" && strings.HasPrefix(strings.TrimLeft(after, " "), ":") {
		// "CYRANO (delighted):" - only a direction closed by the label's
		// own colon belongs to the label.
		label += " " + attached
		trimmed = strings.TrimLeft(after, " ")
	}
	if strings.HasPrefix(trimmed, ".") || strings.HasPrefix(trimmed, ":") {
		if !strings.HasSuffix(label, trimmed[:1]) {
			label += trimmed[:1]
		}
		trimmed = trimmed[1:]
	}
	return &playLabel{name: name, label: label, form: formSpan, strong: true, rest: trimmed}
}

// capsLabel reads a label set in capitals: "FREDDY. There's...", "THE
// DAUGHTER [in the space between] I'm...", "CYRANO (delighted):", or a
// whole line "MEPHISTOPHELES".
func capsLabel(line string) *playLabel {
	pos := 0
	var words []string
	terminated := false
scan:
	for pos < len(line) && len(words) < maxLabelWords+1 {
		rest := line[pos:]
		if d, _ := leadingDirection(rest); d != "" {
			break
		}
		word, _, _ := strings.Cut(rest, " ")
		core := word
		switch {
		case strings.HasSuffix(core, ":"):
			core = strings.TrimSuffix(core, ":")
			terminated = true
		case strings.HasSuffix(core, "."):
			core = strings.TrimSuffix(core, ".")
			next, _, _ := strings.Cut(strings.TrimLeft(rest[len(word):], " "), " ")
			if honorifics[core] && isCapsWord(strings.TrimRight(next, ".:")) {
				core += "."
			} else {
				terminated = true
			}
		}
		if !isCapsWord(strings.TrimSuffix(core, ".")) {
			return nil
		}
		words = append(words, core)
		pos += len(word)
		for pos < len(line) && line[pos] == ' ' {
			pos++
		}
		if terminated {
			break scan
		}
	}
	if len(words) == 0 || len(words) > maxLabelWords {
		return nil
	}
	rest := line[pos:]
	if !terminated {
		// "THE DAUGHTER [in the space...] I'm..." or "CYRANO (delighted):"
		if d, after := leadingDirection(rest); d != "" {
			after = strings.TrimLeft(after, " ")
			if strings.HasPrefix(after, ":") || strings.HasPrefix(after, ".") && unmarked(after[1:]) == "" {
				pos += len(rest) - len(after) + 1
				rest = after[1:]
			}
		} else if unmarked(rest) != "" {
			return nil
		}
	}
	form := formInline
	if unmarked(rest) == "" {
		form = formOwnLine
	}
	return &playLabel{name: strings.Join(words, " "), label: unmarked(line[:pos]), form: form, strong: true, rest: rest}
}

// ownLineLabel reads a label that is a whole line in ordinary case, the
// speech on the lines after it: "All.", "First Fairy."
func ownLineLabel(line string) *playLabel {
	text := unmarked(line)
	name := strings.TrimRight(text, ".:")
	if name == text || name == "" {
		return nil
	}
	words := strings.Fields(name)
	if len(words) > 3 {
		return nil
	}
	for _, w := range words {
		r, _ := utf8.DecodeRuneInString(w)
		if !unicode.IsUpper(r) || strings.ContainsAny(w, ",;!?[]()\"“”") {
			return nil
		}
	}
	return &playLabel{name: name, label: text, form: formOwnLine}
}

// isCapsWord reports whether w is a word of a name in capitals: letters
// all upper case, with digits, apostrophes and hyphens allowed.
func isCapsWord(w string) bool {
	letters := 0
	for _, r := range w {
		switch {
		case unicode.IsLetter(r):
			if !unicode.IsUpper(r) {
				return false
			}
			letters++
		case unicode.IsDigit(r), r == '\'', r == '’', r == '-':
		default:
			return false
		}
	}
	return letters > 0
}

// titleCase turns a label's name into a character's: "MRS. HIGGINS" and
// "lord goring" into "Mrs. Higgins" and "Lord Goring".
func titleCase(name string) string {
	var b strings.Builder
	upper := true
	for _, r := range name {
		if upper {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
		upper = r == ' ' || r == '-'
	}
	return b.String()
}

// leadingDirection splits off a bracketed or parenthesized direction s
// starts with (italic marks around it included), "" if there is none.
func leadingDirection(s string) (direction, rest string) {
	body := strings.TrimLeft(s, string(italicStart))
	if body == "" || body[0] != '[' && body[0] != '(' {
		return "", s
	}
	close := "]"
	if body[0] == '(' {
		close = ")"
	}
	i := strings.Index(body, close)
	if i < 0 {
		return "", s
	}
	rest = strings.TrimLeft(body[i+1:], string(italicEnd))
	return unmarked(body[:i+1]), rest
}

// speechPiece is a run of a speech: spoken words, or one direction.
type speechPiece struct {
	raw       string
	direction bool
}

// splitDirections cuts a speech at its directions: every [...] span, and
// (...) spans too when they are italic or parentheses are this book's
// direction marks. Italic marks wrapping a direction go with it.
func splitDirections(raw string, style playStyle) []speechPiece {
	var pieces []speechPiece
	add := func(s string, direction bool) {
		if s != "" {
			pieces = append(pieces, speechPiece{s, direction})
		}
	}
	pos := 0
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if c != '[' && c != '(' {
			continue
		}
		close := byte(']')
		if c == '(' {
			close = ')'
		}
		n := strings.IndexByte(raw[i:], close)
		if n < 0 {
			continue
		}
		start, end := i, i+n+1
		italicBefore := strings.HasSuffix(raw[:start], string(italicStart))
		italicInside := strings.HasPrefix(raw[start+1:], string(italicStart))
		if c == '(' && !style.parenDirections && !italicBefore && !italicInside {
			continue
		}
		if italicBefore {
			start -= utf8.RuneLen(italicStart)
		}
		end += len(raw[end:]) - len(strings.TrimLeft(raw[end:], string(italicEnd)))
		add(raw[pos:start], false)
		add(raw[start:end], true)
		pos, i = end, end-1
	}
	add(raw[pos:], false)
	return pieces
}

func hasWords(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

// speechBlocks turns one paragraph of speaker's speech (raw, its label
// already cut off) into blocks: the spoken runs as IsQuote blocks, each
// direction between them either a narration block or - when it only says
// how the next words are delivered - part of the next block's Label.
func speechBlocks(raw, speaker, label string, style playStyle) []Block {
	pieces := splitDirections(strings.ReplaceAll(raw, string(lineBreak), " "), style)
	spokenAfter := func(i int) bool {
		for _, p := range pieces[i+1:] {
			if !p.direction && hasWords(unmarked(p.raw)) {
				return true
			}
		}
		return false
	}
	var out []Block
	leading, afterDirection := true, false
	for i, p := range pieces {
		if p.direction {
			text := unmarked(p.raw)
			afterDirection = true
			inner := strings.TrimLeft(text, "[(")
			delivery := (leading || deliveryRe.MatchString(inner)) && !entranceRe.MatchString(inner)
			if delivery && spokenAfter(i) {
				label = strings.TrimSpace(label + " " + text)
				continue
			}
			out = append(out, Block{Kind: BlockText, Text: text, Inline: len(out) > 0})
			continue
		}
		text := strings.TrimSpace(playMarks.Replace(p.raw))
		if afterDirection {
			// "[drops her cloak]. Someone is coming now!"
			text = strings.TrimLeft(text, ".,;: ")
		}
		if !hasWords(unmarked(text)) {
			continue
		}
		clean, emphasis := extractEmphasis(normalizeWhitespace(text))
		out = append(out, Block{Kind: BlockText, Text: clean, IsQuote: true, Speaker: speaker, Label: label, Inline: len(out) > 0, Emphasis: emphasis})
		label = ""
		leading, afterDirection = false, false
	}
	return out
}
