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
}

var pronunciationCandidates = []pronunciationCandidate{
	{re: regexp.MustCompile(`\bDr\.`), candidates: []string{"Doctor", "Drive"}},
	{re: regexp.MustCompile(`\bSt\.`), candidates: []string{"Street", "Saint"}},
	{re: regexp.MustCompile(`\bFt\.`), candidates: []string{"Fort", "Feet"}},
	{re: regexp.MustCompile(`\bMt\.`), candidates: []string{"Mount", "Mountain"}},
	// "No." only when immediately followed by a number ("No. 5", "Room
	// No. 12") - RE2 has no lookahead, so the digit(s) have to be part of
	// the match itself even though they're never part of the
	// substitution (spanGroup: 1 - just "No.", never the number after
	// it). Requiring a following number is what keeps this from matching
	// an ordinary "No." ending a sentence - real prose essentially never
	// follows that with a bare number.
	{re: regexp.MustCompile(`(No\.)\s*\d+`), spanGroup: 1, candidates: []string{"Number", "No"}},
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
	// heading/label/version number ("World War II", "Chapter IV", "Level
	// III") that reads as a plain cardinal or shouldn't be touched at all -
	// romanNumeralCandidates screens out a denylist of common non-name
	// words for exactly that reason before ever offering the respelling.
	{re: romanNumeralRE, dynamicCandidates: romanNumeralCandidates},
	// A bare "M/D" number pair ("3/4") - genuinely ambiguous between a
	// calendar date and a fraction/ratio, unlike a full "M/D/YYYY" (left
	// alone entirely - unambiguously a date already, not worth a
	// substitution). dateCandidates discards anything that isn't a
	// plausible date (month outside 1-12, day outside 1-31) rather than
	// guessing at a fraction reading of its own - see its own doc
	// comment for why "leave it unchanged" is the fallback instead.
	{re: regexp.MustCompile(`\b(\d{1,2})/(\d{1,2})\b`), dynamicCandidates: dateCandidates},

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
	{re: regexp.MustCompile(`\b[Rr]ead\b`), dynamicCandidates: homographCandidates("reed", "red")},
	{re: regexp.MustCompile(`\b[Ll]ead\b`), dynamicCandidates: homographCandidates("leed", "led")},
	{re: regexp.MustCompile(`\b[Ww]ind\b`), dynamicCandidates: homographCandidates("winned", "wined")},
	{re: regexp.MustCompile(`\b[Tt]ear\b`), dynamicCandidates: homographCandidates("tare", "tier")},
	{re: regexp.MustCompile(`\b[Bb]ow\b`), dynamicCandidates: homographCandidates("beau", "bough")},
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

// dateCandidates is the bare-"M/D" pattern's own dynamicCandidates -
// groups[1]/groups[2] are the month/day digit strings (see its own regexp
// literal above). Returns nil (discard the match entirely) for anything
// that isn't a plausible calendar date, rather than guess - the same
// "parse it properly or leave it alone" principle asked for roman
// numerals elsewhere, applied here too. Candidate 0 (the default) is the
// fully spelled-out date reading, since a bare "M/D" is far more often a
// date than a fraction in narrative prose; candidate 1 is the original
// text completely unchanged - a safe "this was actually a fraction/ratio,
// don't touch it" fallback that needs no fraction-reading logic of its
// own to get right.
func dateCandidates(groups []string) []string {
	month, err := strconv.Atoi(groups[1])
	if err != nil || month < 1 || month > 12 {
		return nil
	}
	day, err := strconv.Atoi(groups[2])
	if err != nil || day < 1 || day > 31 {
		return nil
	}
	return []string{
		fmt.Sprintf("%s the %s", monthNames[month], dayOrdinals[day]),
		groups[0],
	}
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
// ordinal - a title/heading/grouping number instead, which either reads as
// a cardinal or shouldn't be touched at all ("World War II", "Chapter IV",
// "Level III", "Super Bowl LVIII" - wrong here would be the jarring "World
// War the Second"). Checked before ever offering the ordinal respelling;
// see romanNumeralCandidates.
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
// numeral (see romanNumeralRE). Candidate 0 (the default) is the unchanged
// original text; candidate 1, offered only past the romanNumeralNonNames
// screen and a successful parse, is the ordinal respelling ("Henry VIII"
// -> "Henry the Eighth") - reusing dayOrdinals rather than a second lookup
// table, since the two passes need the exact same word for the same
// number.
func romanNumeralCandidates(groups []string) []string {
	if romanNumeralNonNames[groups[1]] {
		return nil
	}
	val, ok := romanToInt(groups[2])
	if !ok || val < 1 || val >= len(dayOrdinals) {
		return nil
	}
	return []string{groups[0], fmt.Sprintf("%s the %s", groups[1], dayOrdinals[val])}
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
				})
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

// pronunciationBatchParagraphs/pronunciationMaxTokens mirror
// sfxBatchParagraphs/sfxMaxTokens's own reasoning (a small batch, real
// token headroom) - kept slightly larger than sfx's since this pass's own
// response is much smaller per item (one {id, choice} pair, not an echoed
// full line of text), so token pressure is lighter for the same batch
// size.
const pronunciationBatchParagraphs = 20
const pronunciationMaxTokens = 8192

// pronunciationSystemPrompt asks a narrower, more mechanical question than
// any other pass in this package: it's never shown a fixed universal tag
// vocabulary, and never asked to find anything itself - findPronunciationTerms
// (plain regex, no LLM involved at all) already found every term and its
// own small set of candidate readings; the model's only job is picking
// one, by index, per term. This is deliberate: it bounds the model to a
// closed choice instead of ever generating replacement text, which is what
// makes it safe to actually change what's spoken (see this package's own
// pronounce subpackage doc comment for the full safety story). Only
// genuinely multi-candidate terms are ever listed here at all - a
// single-candidate term (e.g. "C'mon") is resolved before this prompt is
// even built (see ResolvePronunciation), so the model never wastes effort
// confirming a foregone conclusion.
const pronunciationSystemPrompt = `You are a pronunciation disambiguator for an audiobook narrator. You'll be given numbered lines from a novel, and a list of ambiguous words/abbreviations already found in them, each with a small set of possible readings. For each one, decide which reading is correct given how it's actually used in its own line.

Reply with ONLY a JSON array, no other text: [{"id": <term id>, "choice": <candidate index>}, ...] - exactly one entry for every term listed below, no more, no fewer, using the exact id and one of the candidate indices given for that term. If you genuinely can't tell from context, pick index 0 (always the more common reading).

Rules:
- Base your choice on how the abbreviation is actually used in its own line - what kind of word follows it, and the surrounding context. "Dr." immediately before a name ("Dr. Chen") is almost always "Doctor"; before a place-sounding word with no name attached ("the old Dr. curved north") is "Drive". "St." before a name-like word ("St. Andrews", "St. Louis") is usually "Saint"; as part of a street address or ending a location name ("Main St.", "42nd St.") is "Street". "Ft." before a place name ("Ft. Worth") is "Fort"; after a number ("6 Ft. tall") is "Feet". "Mt." before a place name ("Mt. Rainier") is almost always "Mount"; "Mountain" is rare - only pick it if "Mount" would read strangely in context.
- "No." is only ever listed here when a number immediately follows it ("No. 5", "Room No. 12") - almost always "Number". Only pick "No" if the sentence is unmistakably a negation despite the number that happens to follow.
- A bare number pair like "3/4" is listed with a fully spelled-out date reading as one option and the original text completely unchanged as the other. Pick the spelled-out date unless the sentence clearly means a fraction or ratio instead of a calendar date (e.g. "drank 3/4 of the bottle," "won 3/4 of the votes") - in that case pick the unchanged option.
- "read"/"lead"/"wind"/"tear"/"bow" are each listed with two respellings, one per sense, never their own original ambiguous spelling - index 0 is always the more common sense in ordinary narrative prose, the safe default whenever you're not sure. Pick index 0 unless the sentence clearly means the other, less common sense: "read" is index 0 (present/infinitive tense, "will you read this") unless it's clearly past tense ("she read it yesterday"), which is index 1. "lead" is index 0 (the verb/front-position sense, "lead the charge", "in the lead") unless it clearly means the metal or the past tense of "to lead" ("a lead pipe", "she had led them"), which is index 1. "wind" is index 0 (the weather noun) unless it clearly means the verb sense ("wind the clock", "the road will wind"), which is index 1. "tear" is index 0 (the verb "to rip") unless it clearly means a teardrop, which is index 1. "bow" is index 0 (a weapon, ribbon, or knot) unless it clearly means bending at the waist or a ship's own front - both read the same way - which is index 1.
- A capitalized word immediately followed by a Roman numeral ("Henry VIII", "Elizabeth II") is listed with the unchanged text as one option and a spelled-out ordinal ("Henry the Eighth") as the other. Pick the ordinal only when the leading word is genuinely a person's own name and the numeral is their regnal/ordinal number. Pick unchanged for anything else the numeral could be labeling - a war, chapter, act, part, level, section, round, or version number, or any other heading/grouping/count that wouldn't naturally be spoken as "the Nth". If genuinely unsure, pick unchanged.
- Every term listed must get exactly one answer - never omit one, even if you're unsure (pick index 0 instead of skipping it).`

func (c *Client) pronunciationBatch(ctx context.Context, bookTitle, chapterTitle string, batch []ParagraphInput, terms []pronunciationTerm) (map[int]int, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	user.WriteString("\nLines:\n")
	for i, p := range batch {
		if i > 0 && !p.Inline {
			user.WriteString("\n")
		}
		fmt.Fprintf(&user, "%d: %s\n", p.Idx, oneLine(p.Text))
	}
	user.WriteString("\nTerms:\n")
	for _, t := range terms {
		opts := make([]string, len(t.Candidates))
		for i, cand := range t.Candidates {
			opts[i] = fmt.Sprintf("%d=%s", i, cand)
		}
		fmt.Fprintf(&user, "%d: %q in line %d -> %s\n", t.ID, t.Text, t.ParagraphIdx, strings.Join(opts, ", "))
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}

	content, err := c.generate(ctx, pronunciationSystemPrompt, user.String(), 0, pronunciationMaxTokens)
	if err != nil {
		return nil, err
	}
	return parsePronunciationChoices(content, terms), nil
}

// pronunciationChoice is one raw {id, choice} entry from the model's own
// JSON reply - see parsePronunciationChoices.
type pronunciationChoice struct {
	ID     int `json:"id"`
	Choice int `json:"choice"`
}

// parsePronunciationChoices extracts the JSON array from content
// (tolerating prose/code-fence wrapping, the same as parseTaggedLines/
// parseAttributions) and returns a map from every term's own ID to its
// resolved candidate index. Unlike parseTaggedLines, this never returns an
// error and never drops a term: every id in terms starts defaulted to 0
// (candidates[0], always the more common reading - see
// pronunciationCandidate's own doc comment), then overridden only by a
// response entry whose id is one of terms' own and whose choice is a
// genuinely valid index into that specific term's own candidate list.
// This is safe precisely because every term was already positively
// identified as ambiguous by the caller's own regex before the model ever
// saw it - unlike a delivery tag (where "drop it, do nothing" is always a
// valid fallback), a term needs *some* reading resolved, and its own
// index 0 is always a reasonable one, never a broken result the way an
// unparseable delivery-tag response would be.
func parsePronunciationChoices(content string, terms []pronunciationTerm) map[int]int {
	out := make(map[int]int, len(terms))
	byID := make(map[int]pronunciationTerm, len(terms))
	for _, t := range terms {
		out[t.ID] = 0
		byID[t.ID] = t
	}
	start := strings.IndexByte(content, '[')
	end := strings.LastIndexByte(content, ']')
	if start == -1 || end == -1 || end < start {
		return out
	}
	var choices []pronunciationChoice
	if err := json.Unmarshal([]byte(content[start:end+1]), &choices); err != nil {
		return out
	}
	for _, c := range choices {
		t, ok := byID[c.ID]
		if !ok || c.Choice < 0 || c.Choice >= len(t.Candidates) {
			continue
		}
		out[c.ID] = c.Choice
	}
	return out
}

// ResolvePronunciation resolves every pronunciation-worthy span
// (pronunciationCandidates - "Dr.", "St.", "Ft.", "Mt.", "No.", "C'mon",
// "etc."/"e.g."/"i.e."/"vs."/"approx."/"Jr."/"Sr.", a Name-plus-Roman-
// numeral pair, and ambiguous "M/D" number pairs) found by regex in
// chapterTitle's
// paragraphs, batching in groups of pronunciationBatchParagraphs.
// findPronunciationTerms (plain regex, no LLM involved at all) does 100%
// of the actual *detection* and candidate-list computation; within each
// batch, a term with only one candidate (e.g. "C'mon" - see
// cmonCandidates) is resolved immediately, without ever touching the LLM,
// since there's nothing left to disambiguate - only genuinely
// multi-candidate terms are ever sent to the model, and a batch with none
// at all (the overwhelmingly common case) skips the LLM call entirely.
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
func (c *Client) ResolvePronunciation(ctx context.Context, bookTitle, chapterTitle string, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int][]pronounce.Substitution, remaining []ParagraphInput, err error) {
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
			ambiguous = append(ambiguous, t)
		}
		if len(ambiguous) == 0 {
			continue
		}
		choices, berr := c.pronunciationBatch(ctx, bookTitle, chapterTitle, batch, ambiguous)
		if berr != nil {
			return out, paragraphs[start:], fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, berr)
		}
		for _, t := range ambiguous {
			addSubstitution(t, choices[t.ID])
		}
	}
	return out, remaining, nil
}
