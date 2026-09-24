package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
)

// pronunciationCandidate is one regex-detectable span whose pronunciation
// is worth fixing - either genuinely ambiguous (multiple plausible
// readings, needs an LLM call to pick one - see ResolvePronunciation's own
// "ambiguous" split) or a single fixed misreading regex alone can already
// correct (no LLM involvement at all). The abbreviation entries below are
// always exact-case (never (?i)) and require the abbreviation's own
// capitalization exactly as written ("Dr.", not "dr.") - these are
// proper-abbreviation spellings that essentially never occur any other
// way in real prose, so requiring exact case is a cheap, meaningful guard
// against the kind of false positive the laughter/humming heuristics
// (sfxheuristics.go) had to work around for common words.
type pronunciationCandidate struct {
	re *regexp.Regexp
	// spanGroup is which of re's own regex submatch groups (0 = the whole
	// match) marks the actual text to substitute - only ever non-zero for
	// a pattern that needs to match more context than it actually
	// replaces (see noPattern below: RE2 has no lookahead, so seeing "a
	// digit follows" requires matching that digit too, even though only
	// "No." itself should ever be replaced).
	spanGroup int
	// candidates is a fixed, shared reading list for every match of re -
	// used for every pattern except one whose correct reading depends on
	// the actual text matched (see dynamicCandidates).
	candidates []string
	// dynamicCandidates, when non-nil, computes this specific match's own
	// reading list from its own submatch groups (index 0 is the whole
	// match, 1+ are re's own capture groups, in order) instead of using
	// the static candidates field above - needed when the correct answer
	// depends on the actual text matched (a date's own day/month numbers,
	// or matching the original's own capitalization). Returning nil
	// discards this match entirely - no substitution offered, TTS reads
	// the original text exactly as written - rather than ever guessing at
	// a reading that doesn't actually parse (e.g. "13/45" isn't a
	// plausible calendar date).
	//
	// candidates[0] (static or dynamic) is always the more common/default
	// reading - findPronunciationTerms/ResolvePronunciation fall back to
	// it whenever the LLM's own choice can't be trusted for any reason,
	// and a term with only one candidate resolves directly to it without
	// ever reaching the LLM at all (see ResolvePronunciation).
	dynamicCandidates func(groups []string) []string
	// senses describes what each candidate means, index-aligned with the
	// candidate list - what the LLM is actually shown and asked to pick
	// between (see pronunciationBatch). Measured on real chapters, the
	// model can't reliably bind a bare respelling like "winned"/"wined" to
	// the sense it stands for, so every multi-candidate entry needs one.
	senses []string
	// kind names a homograph (its lowercase spelling) for
	// resolveHomographByGrammar's deterministic rules; "" for everything
	// else.
	kind string
}

var pronunciationCandidates = []pronunciationCandidate{
	{re: regexp.MustCompile(`\bDr\.`), candidates: []string{"Doctor", "Drive"}, senses: []string{
		"Doctor - a title before a person's name (Dr. Chen)",
		"Drive - part of a road name or address (Mulholland Dr.)",
	}},
	{re: regexp.MustCompile(`\bSt\.`), candidates: []string{"Street", "Saint"}, senses: []string{
		"Street - part of a road name or address (Main St., 42nd St.)",
		"Saint - before a saint's or a place's name (St. Andrews, St. Louis)",
	}},
	{re: regexp.MustCompile(`\bFt\.`), candidates: []string{"Fort", "Feet"}, senses: []string{
		"Fort - part of a place name (Ft. Worth)",
		"Feet - a measurement after a number (6 Ft. tall)",
	}},
	{re: regexp.MustCompile(`\bMt\.`), candidates: []string{"Mount", "Mountain"}, senses: []string{
		"Mount - before a mountain's name (Mt. Rainier)",
		"Mountain - only where \"Mount\" would read strangely",
	}},
	// "No." only when immediately followed by a number ("No. 5", "Room
	// No. 12") - RE2 has no lookahead, so the digit(s) have to be part of
	// the match itself even though they're never part of the
	// substitution (spanGroup: 1 - just "No.", never the number after
	// it). Requiring a following number is what keeps this from matching
	// an ordinary "No." ending a sentence - real prose essentially never
	// follows that with a bare number.
	{re: regexp.MustCompile(`(No\.)\s*\d+`), spanGroup: 1, candidates: []string{"Number", "No"}, senses: []string{
		"Number - labels a numbered item (Room No. 5)",
		"No - the word \"no\", a refusal or negation that a number just happens to follow",
	}},
	// "C'mon"/"c'mon" - straight apostrophe ('), right single curly quote
	// (’, U+2019, the typographically "correct" smart-quote apostrophe),
	// or left single curly quote (‘, U+2018) - epub smart-quote
	// conversion isn't always direction-aware for an apostrophe sitting
	// right after a single capital letter like "C", so a left curly quote
	// shows up here often enough in the wild to be worth matching too.
	// Higgs routinely mispronounces this contraction, but it only ever
	// means one thing, so there's nothing to disambiguate: cmonCandidates
	// below always returns exactly one reading, case-matched to how it
	// was actually written, and this term resolves without ever reaching
	// the LLM (see ResolvePronunciation).
	{re: regexp.MustCompile(`(?i)\bC['’‘]mon\b`), dynamicCandidates: cmonCandidates},
	// A short, closed set of narrator-abbreviation expansions with no real
	// second reading to disambiguate (unlike Dr./St./Ft./Mt. above, which
	// each stand for two genuinely different dictionary words) - a TTS
	// front-end's own text normalizer can still misfire on these (spelling
	// out each letter, or mangling "etc." into something that sounds like
	// "etsy"), so - like C'mon above - each resolves to exactly one reading
	// and never reaches the LLM. Exact-case, lowercase only: the
	// overwhelmingly common way each is actually written in running prose.
	{re: regexp.MustCompile(`\betc\.`), candidates: []string{"et cetera"}},
	{re: regexp.MustCompile(`\be\.g\.`), candidates: []string{"for example"}},
	{re: regexp.MustCompile(`\bi\.e\.`), candidates: []string{"that is"}},
	{re: regexp.MustCompile(`\bvs\.`), candidates: []string{"versus"}},
	{re: regexp.MustCompile(`\bapprox\.`), candidates: []string{"approximately"}},
	{re: regexp.MustCompile(`\bJr\.`), candidates: []string{"Junior"}},
	{re: regexp.MustCompile(`\bSr\.`), candidates: []string{"Senior"}},
	// A capitalized name immediately followed by a Roman numeral ("Henry
	// VIII", "Elizabeth II") - genuinely ambiguous the same way a bare
	// "M/D" date is: TTS has no reliable way to read "VIII" as anything but
	// individual letters, but respelling it as an ordinal is only correct
	// when it's actually a person's own regnal/ordinal number, not a
	// heading/label/version number ("World War II", "Chapter IV", "ACT
	// II") - romanNumeralCandidates reads a denylisted heading word's
	// numeral as a plain cardinal instead ("Act two").
	{re: romanNumeralRE, dynamicCandidates: romanNumeralCandidates, senses: []string{
		"leave as written - the numeral isn't a person's regnal number",
		"a monarch's or person's regnal number, read as \"the Nth\" (Henry VIII)",
	}},
	// Numbers - including the "M/D" fraction-or-date choice - aren't in
	// this table: findNumberTerms scans them separately, since telling
	// "3/4" apart from the "3/4" inside "3/4/2024" or "A3/4" needs the
	// neighbouring characters, which RE2 (no lookaround) can't check here.

	// Classic English homographs - identical spelling, different
	// pronunciation depending on sense/part of speech - joining the
	// abbreviation entries above under the same "regex finds it, LLM
	// disambiguates by context" shape. Unlike the abbreviations above,
	// there's no second dictionary spelling for either sense (there's only
	// one way to spell "lead" the metal, or "wind" the weather) - so BOTH
	// candidates are explicit respellings, never the word's own bare
	// original spelling: leaving either candidate as the ambiguous original
	// risks TTS guessing wrong regardless of which sense the model picks.
	// Each respelling is a different, real (or, where no real homophone
	// exists, an invented-but-phonetically-obvious) word that happens to be
	// an exact - or near-exact - homophone of that sense. candidates[0]
	// (homographCandidates' own first argument) is always the more common
	// sense in ordinary narrative prose - the default a term the model
	// can't confidently place from context falls back to.
	//
	// Deliberately limited to homographs a simple respelling can actually
	// fix: classic noun/verb stress-shift pairs (record/produce/object/
	// present/conduct/permit and the like, where both senses are already
	// spelled identically and only *stress* moves) have no fix available
	// through plain text substitution and are left out. Also deliberately
	// excludes any homograph whose dominant sense is extremely
	// high-frequency in ordinary prose (e.g. "does" as the auxiliary verb,
	// which would flood every batch with a term that's overwhelmingly
	// never the rare "female deer" plural) - these five are all common
	// enough to be worth fixing but not so ubiquitous that sending every
	// occurrence to the model would meaningfully bloat batch size.
	//
	// - "read": "reed" (present/infinitive - "I read books", "will you
	//   read this") vs "red" (past tense - "she read it yesterday",
	//   already an exact real-word homophone).
	// - "lead": "leed" (verb/front-position - "lead the charge", "in the
	//   lead"; no real-word homophone exists, so this is a plain phonetic
	//   respelling) vs "led" (the metal, or past tense of "to lead" - "a
	//   lead pipe", "she had led them", already an exact real-word
	//   homophone).
	// - "wind": "winned" (the weather noun - no real homophone spells the
	//   short-i sound cleanly, so this is an invented respelling, the same
	//   shape as "leed") vs "wined" (the verb - "wind the clock", "the road
	//   will wind", already an exact real-word homophone of "to wine" +
	//   past tense).
	// - "tear": "tare" (the verb, "to rip" - a real, if less common,
	//   English word - a plant/weed, or a packaging-weight term - that
	//   happens to be an exact homophone) vs "tier" (the noun, a teardrop -
	//   already an exact real-word homophone).
	// - "bow": "beau" (weapon/ribbon/knot sense - a real word, a suitor,
	//   that happens to be an exact homophone) vs "bough" (bending at the
	//   waist, or a ship's own front - both share the same sound, unlike
	//   the weapon/ribbon sense - already an exact real-word homophone, a
	//   tree branch).
	{re: regexp.MustCompile(`\b[Rr]ead\b`), dynamicCandidates: homographCandidates("reed", "red"), kind: "read", senses: []string{
		"present tense, infinitive, command, or the noun \"a good read\" (can read, to read, will read, read this!) - sounds like REED",
		"past tense or past participle (he read it yesterday, had read, it read:, well-read) - sounds like RED",
	}},
	{re: regexp.MustCompile(`\b[Ll]ead\b`), dynamicCandidates: homographCandidates("leed", "led"), kind: "lead", senses: []string{
		"to guide, go first, or be ahead; a head start (lead the way, will lead, in the lead, follow her lead) - sounds like LEED",
		"the heavy metal (lead pipe, heavy as lead), or a misspelling of the past tense \"led\" (yesterday he lead them) - sounds like LED",
	}},
	{re: regexp.MustCompile(`\b[Ww]ind\b`), dynamicCandidates: homographCandidates("winned", "wined"), kind: "wind", senses: []string{
		"moving air, a breeze, breath, or anything named after it (the wind blew, Wind Strike, second wind) - rhymes with PINNED",
		"to twist, coil, crank, or meander (wind the clock, wind up, the road winds) - rhymes with FIND",
	}},
	{re: regexp.MustCompile(`\b[Tt]ear\b`), dynamicCandidates: homographCandidates("tare", "tier"), kind: "tear", senses: []string{
		"to rip, or a rip/hole (tear it apart, a tear in the fabric) - rhymes with BARE",
		"a teardrop, or eyes filling with tears (a tear rolled down, tear-streaked, her eyes tear up) - rhymes with FEAR",
	}},
	{re: regexp.MustCompile(`\b[Bb]ow\b`), dynamicCandidates: homographCandidates("beau", "bough"), kind: "bow", senses: []string{
		"a weapon for shooting arrows, or a ribbon knot (drew his bow, bow and arrow, tied a bow) - rhymes with GO",
		"bending at the waist, or the front of a ship (took a bow, a stiff bow, the ship's bow) - rhymes with COW",
	}},
}

// caseMatch title-cases alt to match sample's own leading letter -
// capitalized if sample starts with an uppercase letter, unchanged
// (already lowercase) otherwise. Shared by homographCandidates below
// (mirroring cmonCandidates' own inline version of the same check above)
// so a respelled substitution never looks jarringly mismatched in case
// from the word it's replacing.
func caseMatch(sample, alt string) string {
	r := []rune(sample)
	if len(r) == 0 || !unicode.IsUpper(r[0]) {
		return alt
	}
	return strings.ToUpper(alt[:1]) + alt[1:]
}

// homographCandidates builds a dynamicCandidates function for the common
// shape every homograph entry above shares: two case-matched respellings,
// one per sense, never the word's own bare original spelling - see the
// pronunciationCandidates list's own doc comment for why neither candidate
// is left unrespelled. defaultSpelling (candidate 0) is the more common
// sense in ordinary narrative prose, offered whenever the model can't
// confidently place the term from context; otherSpelling is candidate 1.
// One factory replaces five otherwise near-identical one-line closures
// (readCandidates, leadCandidates, ... before this).
func homographCandidates(defaultSpelling, otherSpelling string) func(groups []string) []string {
	return func(groups []string) []string {
		return []string{caseMatch(groups[0], defaultSpelling), caseMatch(groups[0], otherSpelling)}
	}
}

// monthNames/dayOrdinals back dateCandidates - index 0 is an unused
// sentinel (month/day numbers are always >= 1), so monthNames[3] ==
// "March" and dayOrdinals[4] == "fourth" directly, no off-by-one
// adjustment needed at the call site.
var monthNames = [...]string{
	"", "January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December",
}

var dayOrdinals = [...]string{
	"",
	"first", "second", "third", "fourth", "fifth", "sixth", "seventh", "eighth", "ninth", "tenth",
	"eleventh", "twelfth", "thirteenth", "fourteenth", "fifteenth", "sixteenth", "seventeenth", "eighteenth", "nineteenth", "twentieth",
	"twenty-first", "twenty-second", "twenty-third", "twenty-fourth", "twenty-fifth",
	"twenty-sixth", "twenty-seventh", "twenty-eighth", "twenty-ninth", "thirtieth",
	"thirty-first",
}

// dateReading spells a bare "M/D" token as a calendar date ("March the
// fourth"), or returns "" when it isn't a plausible one (month outside 1-12,
// day outside 1-31, or either part longer than two digits) - see
// findNumberTerms, which offers it as a fraction token's second candidate.
func dateReading(tok string) string {
	m := fractionRE.FindStringSubmatch(tok)
	if m == nil || len(m[1]) > 2 || len(m[2]) > 2 {
		return ""
	}
	month, _ := strconv.Atoi(m[1])
	day, _ := strconv.Atoi(m[2])
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return ""
	}
	return fmt.Sprintf("%s the %s", monthNames[month], dayOrdinals[day])
}

// romanNumeralPattern is every multi-character Roman numeral from 2 up to
// len(dayOrdinals)-1 (31, comfortably past any real regnal number), built
// once at init from intToRoman rather than hand-listed. Single-character
// numerals (I, V, X, L, C, D, M) are deliberately excluded entirely: each
// collides too heavily with ordinary English on sight alone (the pronoun
// "I", "V"/"X" as a grade/placeholder/generic label, and so on) to trust
// as a Roman numeral without more context than a regex can cheaply check -
// the same "false negative over false positive" call dateCandidates makes
// for an implausible M/D pair, applied here by construction instead of by
// a runtime check. A real but rare cost: "Henry V" is missed since "V"
// alone never matches.
var romanNumeralPattern = buildRomanNumeralPattern()

func buildRomanNumeralPattern() string {
	var parts []string
	for n := len(dayOrdinals) - 1; n >= 2; n-- {
		if r := intToRoman(n); len(r) > 1 {
			parts = append(parts, r)
		}
	}
	return strings.Join(parts, "|")
}

// intToRoman is the standard greedy subtractive-notation algorithm,
// correct for any n in the small range this package ever calls it with
// (buildRomanNumeralPattern only ever asks for 2..31).
func intToRoman(n int) string {
	vals := []struct {
		v int
		s string
	}{{10, "X"}, {9, "IX"}, {5, "V"}, {4, "IV"}, {1, "I"}}
	var b strings.Builder
	for _, vs := range vals {
		for n >= vs.v {
			b.WriteString(vs.s)
			n -= vs.v
		}
	}
	return b.String()
}

// romanToInt parses a Roman numeral matched by romanNumeralPattern back
// into its integer value - always well-formed input here (the pattern only
// ever matches an exact numeral it itself generated), but written as a
// general subtractive-notation parser rather than a lookup table.
func romanToInt(s string) (int, bool) {
	vals := map[byte]int{'I': 1, 'V': 5, 'X': 10, 'L': 50, 'C': 100, 'D': 500, 'M': 1000}
	total := 0
	for i := 0; i < len(s); i++ {
		v, ok := vals[s[i]]
		if !ok {
			return 0, false
		}
		if i+1 < len(s) {
			if next, ok2 := vals[s[i+1]]; ok2 && next > v {
				total -= v
				continue
			}
		}
		total += v
	}
	return total, true
}

// romanNumeralRE matches a capitalized word immediately followed by one of
// romanNumeralPattern's own numerals - group 1 is the word, group 2 the
// numeral. Compiled once at init, after romanNumeralPattern is built.
var romanNumeralRE = regexp.MustCompile(`\b([A-Z][a-zA-Z]+) (` + romanNumeralPattern + `)\b`)

// romanNumeralNonNames is a denylist of common capitalized words that often
// precede a Roman numeral in ordinary prose without it being a person's own
// ordinal - a title/heading/grouping number instead, read as a cardinal
// ("World War II", "Chapter IV", "ACT II" - wrong here would be the jarring
// "World War the Second"). Keys are title-cased; see romanNumeralCandidates.
var romanNumeralNonNames = map[string]bool{
	"War": true, "Act": true, "Chapter": true, "Part": true, "Book": true,
	"Volume": true, "Level": true, "Section": true, "World": true,
	"Round": true, "Game": true, "Episode": true, "Season": true,
	"Table": true, "Figure": true, "Appendix": true, "Verse": true,
	"Scene": true, "Movement": true, "Step": true, "Phase": true,
	"Group": true, "Class": true, "Grade": true, "Type": true,
	"Model": true, "Version": true, "Note": true, "Category": true,
	"Super": true,
}

// romanNumeralCandidates is the Name-plus-Roman-numeral pattern's own
// dynamicCandidates - groups[1] is the leading word, groups[2] its Roman
// numeral (see romanNumeralRE). A leading word on the romanNumeralNonNames
// list (matched case-insensitively, so a play's "ACT II" heading counts)
// labels a heading/grouping number, read as a plain cardinal - one
// candidate, no LLM call: "ACT II" -> "Act two", "World War II" -> "World
// War two". An all-caps leading word is title-cased so the TTS front-end
// doesn't spell it out letter by letter. Anything else is a possible
// regnal number: candidate 0 (the default) is the unchanged original text,
// candidate 1 the ordinal respelling ("Henry VIII" -> "Henry the eighth") -
// reusing dayOrdinals rather than a second lookup table, since the two
// passes need the exact same word for the same number.
func romanNumeralCandidates(groups []string) []string {
	val, ok := romanToInt(groups[2])
	if !ok || val < 1 || val >= len(dayOrdinals) {
		return nil
	}
	word := groups[1]
	titled := strings.ToUpper(word[:1]) + strings.ToLower(word[1:])
	if romanNumeralNonNames[titled] {
		if word == strings.ToUpper(word) {
			word = titled
		}
		return []string{fmt.Sprintf("%s %s", word, numberWords(int64(val)))}
	}
	return []string{groups[0], fmt.Sprintf("%s the %s", word, dayOrdinals[val])}
}

// cmonCandidates is the C'mon pattern's own dynamicCandidates - always
// exactly one reading ("Come on"/"come on"), case-matched to whichever
// casing the contraction was actually written in (capitalized at a
// sentence's own start, lowercase mid-sentence) rather than hardcoding
// one. Always returns exactly one candidate, so this term is resolved
// directly and never actually reaches the LLM - see ResolvePronunciation.
func cmonCandidates(groups []string) []string {
	r := []rune(groups[0])
	if len(r) == 0 {
		return nil
	}
	if unicode.IsUpper(r[0]) {
		return []string{"Come on"}
	}
	return []string{"come on"}
}

// pronunciationTerm is one detected span within a single batch, with an
// id unique within that batch (see findPronunciationTerms) used to
// correlate the LLM's own {id, choice} response back to it - only ever
// assigned to a term ResolvePronunciation actually sends to the model
// (see its own "ambiguous" split; a single-candidate term never gets an
// LLM round trip, so its id is never used for that purpose). Offset/
// Length are byte offsets into that paragraph's own real,
// ParagraphInput.Text - never into oneLine(p.Text) (used only for this
// pass's own "Lines:" display text, for readability - see
// pronunciationBatch) - because Offset/Length ultimately become a
// pronounce.Substitution applied against store.Paragraph.Text itself
// (ResolveGenerationText), the same coordinate space delivery-tag
// insertions already use.
type pronunciationTerm struct {
	ID           int
	ParagraphIdx int
	Offset       int
	Length       int
	Text         string
	Candidates   []string
	// Senses/Kind are copied from the matching pronunciationCandidate (or
	// set by findNumberTerms) - see its own fields.
	Senses []string
	Kind   string
}

// findPronunciationTerms scans every paragraph in batch (against its own
// real Text, not the display-only oneLine variant - see pronunciationTerm's
// own doc comment) for pronunciationCandidates matches, returning them in
// (ParagraphIdx, Offset) order with sequential ids starting from 0 - this
// pass never needs ids unique beyond one batch, unlike, say, a paragraph
// Idx, which must stay unique chapter-wide. A match whose own candidates
// (static or dynamic) come back empty is silently skipped, never
// appended - see pronunciationCandidate.dynamicCandidates' own doc
// comment for why that happens (a "M/D" pair that isn't a plausible
// date, for instance) and why it's always safe: nothing spoken changes
// for a term that's never even offered as a substitution.
func findPronunciationTerms(batch []ParagraphInput) []pronunciationTerm {
	var out []pronunciationTerm
	for _, p := range batch {
		first := len(out)
		for _, c := range pronunciationCandidates {
			for _, loc := range c.re.FindAllStringSubmatchIndex(p.Text, -1) {
				groups := make([]string, len(loc)/2)
				for g := range groups {
					gs, ge := loc[2*g], loc[2*g+1]
					if gs < 0 { // group didn't participate in this match
						continue
					}
					groups[g] = p.Text[gs:ge]
				}
				candidates := c.candidates
				if c.dynamicCandidates != nil {
					candidates = c.dynamicCandidates(groups)
				}
				if len(candidates) == 0 {
					continue
				}
				spanStart, spanEnd := loc[2*c.spanGroup], loc[2*c.spanGroup+1]
				out = append(out, pronunciationTerm{
					ParagraphIdx: p.Idx,
					Offset:       spanStart,
					Length:       spanEnd - spanStart,
					Text:         p.Text[spanStart:spanEnd],
					Candidates:   candidates,
					Senses:       c.senses,
					Kind:         c.kind,
				})
			}
		}
		// Numbers (findNumberTerms) go last so a table pattern whose span
		// overlaps one keeps it, and the number term is dropped rather
		// than producing two substitutions for one span.
		tableTerms := out[first:]
		for _, nt := range findNumberTerms(p.Text) {
			overlaps := false
			for _, t := range tableTerms {
				if nt.Offset < t.Offset+t.Length && t.Offset < nt.Offset+nt.Length {
					overlaps = true
					break
				}
			}
			if !overlaps {
				nt.ParagraphIdx = p.Idx
				out = append(out, nt)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ParagraphIdx != out[j].ParagraphIdx {
			return out[i].ParagraphIdx < out[j].ParagraphIdx
		}
		return out[i].Offset < out[j].Offset
	})
	for i := range out {
		out[i].ID = i
	}
	return out
}

// pronunciationBatchParagraphs is how many paragraphs' terms go into one
// LLM call. pronunciationMaxTokens leaves ample room for the reply - one
// short {"id": "letter"} pair per term.
const pronunciationBatchParagraphs = 20
const pronunciationMaxTokens = 2048

// pronunciationSystemPrompt asks a narrower, more mechanical question than
// any other pass in this package: findPronunciationTerms (plain regex, no
// LLM) already found every term and its own small set of candidate
// readings, and resolveHomographByGrammar already settled every term a
// grammatical cue decides; the model's only job is picking one lettered
// meaning per remaining term. It never generates replacement text, which
// is what makes it safe to actually change what's spoken (see the pronounce
// package doc comment for the full safety story).
//
// Each term is shown as its own short passage with the word marked
// ⟦inline⟧ and each option described by *meaning* (pronunciationCandidate.
// senses). Benchmarked on hand-labeled terms from real chapters, an earlier
// shape - whole numbered lines plus a separate "term id -> line number"
// list whose options were bare respellings ("0=winned, 1=wined") - scored
// 45%, worse than always taking the default reading (72%): the model
// couldn't bind a respelling to its sense, and a line containing the same
// word twice made the id-to-occurrence mapping ambiguous. This shape
// scored 89% on the same terms.
const pronunciationSystemPrompt = `You decide how an audiobook narrator should pronounce ambiguous words. Each numbered item is a short passage from a novel with one word marked ⟦like this⟧, followed by its possible meanings, lettered A, B, and so on. For each item, decide which meaning the marked word has in that exact spot. Judge each item on its own grammar and context - items are independent.

Reply with ONLY a JSON object mapping each item number to a letter, e.g. {"0": "A", "1": "B"}. Answer every item.`

// pronunciationSnippetContext bounds how much of a term's paragraph is
// shown on either side of it (in bytes, before trimming to a sentence
// boundary) - enough for the sentence it's in, without a whole paragraph.
const pronunciationSnippetContext = 220

// markedSnippet returns the sentence(s) around text[start:end], whitespace-
// collapsed, with the term wrapped in ⟦⟧ and "…" where the paragraph was
// cut.
func markedSnippet(text string, start, end int) string {
	from := max(0, start-pronunciationSnippetContext)
	for from > 0 && !utf8.RuneStart(text[from]) {
		from++
	}
	pre := text[from:start]
	if from > 0 {
		// Start after the last sentence end before the term, if any.
		if j := strings.LastIndexAny(pre, ".!?"); j >= 0 {
			pre = pre[j+1:]
		}
		pre = "…" + pre
	}
	to := min(len(text), end+pronunciationSnippetContext)
	for to < len(text) && !utf8.RuneStart(text[to]) {
		to--
	}
	post := text[end:to]
	if to < len(text) {
		// End at the first sentence end after the term, if any.
		if j := strings.IndexAny(post, ".!?"); j >= 0 {
			post = post[:j+1]
		}
		post += "…"
	}
	return oneLine(pre + "⟦" + text[start:end] + "⟧" + post)
}

func (c *Client) pronunciationBatch(ctx context.Context, batch []ParagraphInput, terms []pronunciationTerm) (map[int]int, error) {
	textByIdx := make(map[int]string, len(batch))
	for _, p := range batch {
		textByIdx[p.Idx] = p.Text
	}
	var user strings.Builder
	for _, t := range terms {
		fmt.Fprintf(&user, "%d. %s\n", t.ID, markedSnippet(textByIdx[t.ParagraphIdx], t.Offset, t.Offset+t.Length))
		for i, cand := range t.Candidates {
			sense := cand
			if i < len(t.Senses) {
				sense = t.Senses[i]
			}
			fmt.Fprintf(&user, "   %c: %s\n", 'A'+i, sense)
		}
		user.WriteString("\n")
	}
	if c.cfg.NoThink {
		user.WriteString("/no_think")
	}

	content, err := c.generate(ctx, pronunciationSystemPrompt, user.String(), 0, pronunciationMaxTokens)
	if err != nil {
		return nil, err
	}
	return parsePronunciationChoices(content, terms), nil
}

// parsePronunciationChoices extracts the JSON object from content
// (tolerating prose/code-fence wrapping) and returns a map from every
// term's own ID to its resolved candidate index. It never returns an error
// and never drops a term: every id in terms starts defaulted to 0
// (candidates[0], always the more common reading - see
// pronunciationCandidate's own doc comment), then is overridden only by a
// reply entry naming one of terms' own ids with a letter that's a valid
// option for that specific term. That's safe because every term was
// already positively identified by the caller's own regex before the model
// ever saw it, and its index 0 is always a reasonable reading.
func parsePronunciationChoices(content string, terms []pronunciationTerm) map[int]int {
	out := make(map[int]int, len(terms))
	byID := make(map[int]pronunciationTerm, len(terms))
	for _, t := range terms {
		out[t.ID] = 0
		byID[t.ID] = t
	}
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start == -1 || end == -1 || end < start {
		return out
	}
	var choices map[string]string
	if err := json.Unmarshal([]byte(content[start:end+1]), &choices); err != nil {
		return out
	}
	for key, letter := range choices {
		id, err := strconv.Atoi(strings.TrimSpace(key))
		if err != nil {
			continue
		}
		t, ok := byID[id]
		letter = strings.ToUpper(strings.TrimSpace(letter))
		if !ok || letter == "" {
			continue
		}
		if choice := int(letter[0] - 'A'); choice >= 0 && choice < len(t.Candidates) {
			out[id] = choice
		}
	}
	return out
}

// ResolvePronunciation resolves every pronunciation-worthy span
// (pronunciationCandidates - "Dr.", "St.", "Ft.", "Mt.", "No.", "C'mon",
// "etc."/"e.g."/"i.e."/"vs."/"approx."/"Jr."/"Sr.", a Name-plus-Roman-
// numeral pair, and numbers - see findNumberTerms) found by regex in
// chapterTitle's
// paragraphs, batching in groups of pronunciationBatchParagraphs.
// findPronunciationTerms (plain regex, no LLM involved at all) does 100%
// of the actual *detection* and candidate-list computation; within each
// batch, a term with only one candidate (e.g. "C'mon" - see
// cmonCandidates) is resolved immediately, without ever touching the LLM,
// since there's nothing left to disambiguate, and so is a homograph whose
// sense a grammatical cue settles (resolveHomographByGrammar) - only the
// terms left after both are sent to the model, and a batch with none at
// all (the common case) skips the LLM call entirely. bookTitle and
// chapterTitle are unused since the prompt switched to per-term passages.
//
// Returns a map from paragraph Idx to the pronounce.Substitutions
// resolved for that paragraph (absent = no pronunciation-worthy span
// found there), persisted via Store.SetParagraphPronunciation and applied
// only at generation time (store.Paragraph.ResolveGenerationText, via
// pronounce.Apply) - never to the reader's own on-screen text or forced
// alignment's own reference text, exactly like the two delivery-tag
// passes (DirectChapter/TagSfx).
//
// shouldPause/remaining/err follow AttributeChapter's own contract
// exactly (see its doc comment): nil-safe shouldPause, checked between
// batches only (never mid-batch, and never for a batch with nothing to
// send the LLM at all, since there's no LLM call to interrupt); remaining
// holds whatever wasn't reached so the caller can persist real progress
// and requeue just the rest.
func (c *Client) ResolvePronunciation(ctx context.Context, _, _ string, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int][]pronounce.Substitution, remaining []ParagraphInput, err error) {
	out = make(map[int][]pronounce.Substitution, len(paragraphs))

	addSubstitution := func(t pronunciationTerm, choice int) {
		out[t.ParagraphIdx] = append(out[t.ParagraphIdx], pronounce.Substitution{
			Offset:      t.Offset,
			Length:      t.Length,
			Replacement: t.Candidates[choice],
		})
	}

	for start := 0; start < len(paragraphs); start += pronunciationBatchParagraphs {
		if start > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphs[start:]
			break
		}
		end := start + pronunciationBatchParagraphs
		if end > len(paragraphs) {
			end = len(paragraphs)
		}
		batch := paragraphs[start:end]

		byIdx := make(map[int]ParagraphInput, len(batch))
		for _, p := range batch {
			byIdx[p.Idx] = p
		}
		allTerms := findPronunciationTerms(batch)
		var ambiguous []pronunciationTerm
		for _, t := range allTerms {
			if len(t.Candidates) < 2 {
				// Nothing to disambiguate (len == 1; len == 0 is already
				// filtered out by findPronunciationTerms itself) - resolve
				// directly, no LLM call spent on a foregone conclusion.
				addSubstitution(t, 0)
				continue
			}
			if t.Kind != "" {
				p := byIdx[t.ParagraphIdx]
				if choice, ok := resolveHomographByGrammar(t.Kind, p.Text, t.Offset, t.Offset+t.Length, p.IsQuote); ok {
					addSubstitution(t, choice)
					continue
				}
			}
			// Renumbered so the model sees a contiguous 0..n-1 item list.
			t.ID = len(ambiguous)
			ambiguous = append(ambiguous, t)
		}
		if len(ambiguous) == 0 {
			continue
		}
		choices, berr := c.pronunciationBatch(ctx, batch, ambiguous)
		if berr != nil {
			return out, paragraphs[start:], fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, berr)
		}
		for _, t := range ambiguous {
			addSubstitution(t, choices[t.ID])
		}
	}
	return out, remaining, nil
}
