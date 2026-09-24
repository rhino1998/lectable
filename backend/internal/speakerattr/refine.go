package speakerattr

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file holds the deterministic, no-LLM half of attribution: cleaning
// up the speaker names the model emits (sanitizeSpeaker), and correcting
// its per-line answers against explicit speech tags in the text itself
// (RefineWithSpeechTags). Both exist because of measured failure modes
// on real books (Spire's Spite chapters 40-58, 1300 dialogue lines), not
// hypotheticals:
//
//   - The model ignores a named tag right next to a quote. Of 624 quotes
//     followed by an explicit "…,” Bert said" style tag, it credited 42
//     to someone else - every one checked was a model error.
//   - It alternates speakers inside one paragraph. 25% of paragraphs
//     holding more than one quote got mixed speakers, almost all of the
//     shape “A,” X said. “B” with B handed to someone other than X -
//     exactly what systemPrompt tells it not to do.
//   - It emits non-names as speakers: joint names ("Fritz and Bert"),
//     non-answers ("None", "No one", "Yes"), possessive descriptions
//     ("his foe"), and its own prompt's "[role]" marker glued onto a name.

// speechVerbs are verbs that, placed right next to a quote with a named
// subject, make that subject the quote's speaker. Deliberately limited to
// verbs of speaking (plus a few near-universal tag verbs like "smirked" /
// "grinned" that real books use as tags) - never plain actions like
// "nodded"/"shrugged"/"smiled", which as often describe a listener's
// reaction as the speaker's own beat.
var speechVerbs = []string{
	"said", "says", "asked", "asks", "replied", "answered", "responded", "shouted", "yelled",
	"called", "cried", "screamed", "shrieked", "whispered", "murmured", "muttered", "mumbled",
	"grumbled", "groused", "growled", "snarled", "snapped", "hissed", "barked", "roared",
	"bellowed", "boomed", "added", "continued", "began", "started", "finished", "interrupted",
	"interjected", "countered", "retorted", "argued", "insisted", "protested", "explained",
	"stated", "declared", "announced", "proclaimed", "admitted", "agreed", "conceded",
	"confessed", "suggested", "offered", "noted", "observed", "remarked", "commented", "mused",
	"wondered", "joked", "teased", "quipped", "ribbed", "taunted", "mocked", "jeered", "chided",
	"scolded", "laughed", "chuckled", "giggled", "snickered", "scoffed", "snorted", "sneered",
	"sighed", "groaned", "moaned", "grunted", "huffed", "gasped", "breathed", "spat",
	"demanded", "pleaded", "begged", "urged", "warned", "promised", "assured", "reassured",
	"corrected", "clarified", "prompted", "pressed", "repeated", "echoed", "exclaimed",
	"drawled", "rasped", "croaked", "intoned", "entreated", "hedged", "complained", "accused",
	"bemoaned", "lamented", "espoused", "boasted", "inquired", "enquired", "piped", "chimed",
	"stammered", "stuttered", "blurted", "ventured", "supplied", "guessed", "reasoned",
	"concluded", "ordered", "commanded", "instructed", "greeted", "smirked", "grinned",
	"sobbed", "wailed", "yelped", "deadpanned", "informed", "told", "prodded", "rejoined",
	"probed", "queried", "questioned", "cajoled", "coaxed", "soothed", "interposed",
	"burst", "lied", "amended", "relented", "acknowledged", "fumed", "seethed", "sputtered",
	"spluttered", "wheezed", "whined", "pouted", "scowled",
	// British spellings (and their American twins) - real books use both.
	"apologised", "apologized", "eulogised", "eulogized", "sympathised", "sympathized",
	"emphasised", "emphasized", "criticised", "criticized", "theorised", "theorized",
	"summarised", "summarized", "surmised", "advised", "rationalised", "rationalized",
}

// speechParticiples are the "-ing" forms that end a lead-in tag right
// before its quote ("Bert grinned even harder saying, “…”").
var speechParticiples = []string{
	"saying", "asking", "adding", "replying", "answering", "calling", "shouting", "yelling",
	"whispering", "muttering", "murmuring", "continuing", "explaining", "declaring",
	"announcing", "demanding",
}

// nameRe matches a capitalized name run as it appears in narration
// ("Bert", "Jagged Nic", "Mr. Dale", "Runs-With-Purpose").
const nameRe = `[A-Z][\p{L}'’.-]*(?: [A-Z][\p{L}'’.-]*)*`

var (
	verbAlt = strings.Join(speechVerbs, "|")
	partAlt = strings.Join(speechParticiples, "|")

	// Trailing tags, matched at the start of the narration right after a
	// quote: “…,” Bert said / “…,” said Bert / “…,” she said.
	trailingNameVerb = regexp.MustCompile(`^(?:[a-z]+ly |then )?(` + nameRe + `)(?: [a-z]+ly| then| finally)? (?:` + verbAlt + `)\b`)
	trailingVerbName = regexp.MustCompile(`^(?:` + verbAlt + `)(?: [a-z]+ly)? (` + nameRe + `)\b`)
	// "Toby and Jane said together" - a tag, but naming no single speaker.
	trailingJoint   = regexp.MustCompile(`^(?:` + nameRe + `)(?:,? (?:and|&) |, )(?:` + nameRe + `)(?: [a-z]+ly)? (?:` + verbAlt + `)\b`)
	trailingPronoun = regexp.MustCompile(`^(?:[a-z]+ly |then )?(?i:he|she|they|I|we)(?: [a-z]+ly| then)? (?:` + verbAlt + `)\b`)

	// Lead-in tags, matched at the end of the narration right before a
	// quote: Bert said, “…” / said Bert: “…” / Bert stood, saying, “…”.
	// leadInEnd only says the narration ends in a speech verb/participle;
	// leadInSubject then picks out who that verb belongs to.
	leadInEnd = regexp.MustCompile(`\b(?:` + verbAlt + `|` + partAlt + `)(?: [a-z]+ly)?\s*[,:]?\s*$`)
	// Up to three lowercase words may trail the verb before a comma or
	// colon ("Fritz sighed outwardly this time,").
	leadingNameVerb = regexp.MustCompile(`(?:^|[\s,;])(` + nameRe + `)(?: [a-z]+ly)? (?:` + verbAlt + `)(?:(?: [a-z]+ly)?\s*[,:]?|(?: [a-z]+){1,3}\s*[,:])\s*$`)
	leadingVerbName = regexp.MustCompile(`(?:^|[\s,;])(?:` + verbAlt + `) (` + nameRe + `)\s*[,:]\s*$`)
	// Every name or pronoun in a lead-in sentence, with an optional
	// possessive - see leadInTag for how each is judged subject vs. object.
	leadInToken  = regexp.MustCompile(`(` + nameRe + `|\b(?:he|she|they|we|He|She|They|We|I)\b)(’s|'s)?`)
	lastSentence = regexp.MustCompile(`[.!?]["”’]?\s+`)
)

// notSubjectFollowers are words that, right after a name, mean the name
// isn't the subject of what follows ("Steve and yelling", "the Duskmoth
// saying").
var notSubjectFollowers = func() map[string]bool {
	m := map[string]bool{"and": true, "or": true, "nor": true}
	for _, p := range speechParticiples {
		m[p] = true
	}
	return m
}()

// subjectPreceders are the only words a subject name may follow in a
// lead-in sentence ("Sid nodded and Bert shrugged then added,", "when Sid
// said,"). Any other lowercase word before a name makes it an object
// ("slapped Fritz", "offered it to the Duskmoth", "before Fritz did").
var subjectPreceders = map[string]bool{
	"and": true, "then": true, "but": true, "so": true, "when": true, "while": true, "as": true,
	"until": true, "because": true, "though": true, "although": true, "yet": true, "once": true,
	"finally": true, "suddenly": true, "now": true, "meanwhile": true,
}

var pronounSubjects = map[string]bool{"he": true, "she": true, "they": true, "we": true, "i": true}

// leadInTag reads a lead-in before a quote. A name directly before the
// closing speech verb ("Sid interjected,") is the speaker. Otherwise the
// text must end in a speech verb or participle, and its speaker is the
// last subject in that final sentence. A name counts as a subject only when it starts the sentence
// or clause (after a comma or a subjectPreceders word) and a lowercase
// word follows it - "Sid smirked and Bert grinned saying," is Bert's,
// "then Bert was on top of Steve and yelling," is Bert's, "Bert slapped
// Fritz on the back and asked," is Bert's. Two exceptions: a name followed
// by "who" is a subject whatever precedes it ("looked to Bert who just
// intoned,"), and a name continuing a list after an object is an object
// too ("met the gazes of Veronica, Lynn and Naomi then asked,"). A
// pronoun subject after the last named one ("Not thinking he offered it
// to the Duskmoth saying") names no one.
func leadInTag(text string, resolver *SpeakerNameResolver) (name string, pronoun bool) {
	// A name right against the closing verb wins outright, whatever comes
	// before it ("…before Greg growled,", "…before he could speak Sid
	// interjected,", "said Bert:").
	for _, re := range []*regexp.Regexp{leadingNameVerb, leadingVerbName} {
		if m := re.FindStringSubmatch(text); m != nil {
			if c, ok := resolver.Resolve(m[1]); ok {
				return c, false
			}
		}
	}
	if !leadInEnd.MatchString(text) {
		return "", false
	}
	sentence := text
	if locs := lastSentence.FindAllStringIndex(text, -1); len(locs) > 0 {
		sentence = text[locs[len(locs)-1][1]:]
	}
	prevEnd, prevObject := -1, false
	for _, m := range leadInToken.FindAllStringSubmatchIndex(sentence, -1) {
		word := sentence[m[2]:m[3]]
		possessive := m[4] >= 0
		before := strings.Fields(sentence[:m[0]])
		prevWord := ""
		if len(before) > 0 {
			prevWord = before[len(before)-1]
		}
		after := strings.Fields(strings.TrimLeft(sentence[m[1]:], " ,;"))
		follower := ""
		if len(after) > 0 {
			follower = after[0]
		}

		object := prevWord != "" && !strings.HasSuffix(prevWord, ",") && !subjectPreceders[strings.ToLower(prevWord)]
		// A list continuing from an object ("of Veronica, Lynn and Naomi").
		if prevEnd >= 0 && prevObject {
			if gap := strings.TrimSpace(sentence[prevEnd:m[0]]); gap == "," || gap == "and" || gap == ", and" {
				object = true
			}
		}
		prevEnd, prevObject = m[1], object
		if strings.TrimRight(follower, ",") == "who" {
			object = false
		} else if possessive || follower == "" || !startsLower(follower) || notSubjectFollowers[strings.Trim(follower, ",")] {
			continue
		}
		if object {
			continue
		}
		if pronounSubjects[strings.ToLower(word)] {
			name, pronoun = "", true
			continue
		}
		if c, ok := resolver.Resolve(word); ok {
			name, pronoun = c, false
		}
	}
	return name, pronoun
}

func startsLower(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsLower(r)
}

// SpeakerNameResolver maps a name as written in narration ("Nic",
// "Albert") to a known character's canonical name ("Jagged Nic", "Bert").
type SpeakerNameResolver struct {
	exact map[string]string // lower-cased full name or alias -> canonical
	token map[string]string // lower-cased single word -> canonical, "" if ambiguous
}

// titleWords never resolve a character on their own - "Captain said" or
// "Lord said" names no one in particular.
var titleWords = map[string]bool{
	"the": true, "a": true, "an": true, "lord": true, "lady": true, "sir": true, "mr": true, "mr.": true,
	"mrs": true, "mrs.": true, "ms": true, "ms.": true, "miss": true, "master": true, "mistress": true,
	"captain": true, "king": true, "queen": true, "prince": true, "princess": true, "doctor": true,
	"dr.": true, "old": true, "young": true, "elder": true, "guard": true, "general": true,
	"sergeant": true, "commander": true, "chief": true, "duke": true, "duchess": true, "count": true,
	"countess": true, "father": true, "mother": true, "brother": true, "sister": true,
}

// NewSpeakerNameResolver builds a resolver over the known characters'
// names plus their aliases (canonical name -> aliases). A single word
// resolves on its own only if it belongs to exactly one character and
// isn't a title word - "Nic" resolves to "Jagged Nic" unless another
// known character is also a Nic.
func NewSpeakerNameResolver(names []string, aliases map[string][]string) *SpeakerNameResolver {
	r := &SpeakerNameResolver{exact: map[string]string{}, token: map[string]string{}}
	add := func(form, canonical string) {
		form = strings.TrimSpace(form)
		if form == "" {
			return
		}
		lower := strings.ToLower(form)
		if prev, ok := r.exact[lower]; ok && prev != canonical {
			r.exact[lower] = "" // two characters answer to this exact form
		} else if !ok {
			r.exact[lower] = canonical
		}
		for _, w := range strings.Fields(lower) {
			if len(w) < 3 || titleWords[w] {
				continue
			}
			if prev, ok := r.token[w]; ok && prev != canonical {
				r.token[w] = ""
			} else if !ok {
				r.token[w] = canonical
			}
		}
	}
	for _, n := range names {
		// A junk roster entry ("Someone", "Fritz and Bert") must never be
		// what "Someone answered" resolves to.
		if clean, _ := sanitizeSpeaker(n, false); isSentinel(n) || clean == "Unknown" {
			continue
		}
		add(n, n)
		for _, a := range aliases[n] {
			add(a, n)
		}
	}
	return r
}

// Resolve returns the canonical character name written, or false.
func (r *SpeakerNameResolver) Resolve(written string) (string, bool) {
	if r == nil {
		return "", false
	}
	lower := strings.ToLower(strings.TrimSpace(written))
	lower = strings.TrimSuffix(strings.TrimSuffix(lower, "’s"), "'s")
	if c, ok := r.exact[lower]; ok {
		return c, c != ""
	}
	words := strings.Fields(lower)
	// A multi-word run like "Then Bert" or "Captain Mocar" - try each word
	// on its own, but only accept one unambiguous hit.
	found := ""
	for _, w := range words {
		c, ok := r.token[w]
		if !ok || c == "" {
			continue
		}
		if found != "" && found != c {
			return "", false
		}
		found = c
	}
	return found, found != ""
}

// Canonical maps a name the model emitted to the known character it
// names, when it's a case variant or alias of one ("albert" -> "Bert").
// Only exact full-form matches count - a single matching word isn't
// enough to rename a model answer.
func (r *SpeakerNameResolver) Canonical(name string) string {
	if r == nil || isSentinel(name) {
		return name
	}
	if c, ok := r.exact[strings.ToLower(strings.TrimSpace(name))]; ok && c != "" {
		return c
	}
	return name
}

func isSentinel(name string) bool {
	return name == "" || name == "Narrator" || name == "Unknown"
}

// tagFor reports who an explicit tag next to a quote names: the narration
// segment right after it (a trailing tag) wins over the narration right
// before it in the same paragraph (a lead-in). pronoun is true when a tag
// is there but only says "he"/"she" - no name to bind, but enough to know
// the line isn't simply continuing the previous quote's speaker.
func tagFor(prev, next *ParagraphInput, resolver *SpeakerNameResolver) (name string, pronoun bool) {
	if next != nil {
		text := strings.TrimLeft(next.Text, " ,.;:-—–")
		for _, re := range []*regexp.Regexp{trailingNameVerb, trailingVerbName} {
			if m := re.FindStringSubmatch(text); m != nil {
				if c, ok := resolver.Resolve(m[1]); ok {
					return c, false
				}
			}
		}
		if trailingPronoun.MatchString(text) || trailingJoint.MatchString(text) {
			pronoun = true
		}
	}
	if prev != nil {
		lead, leadPronoun := leadInTag(strings.TrimSpace(prev.Text), resolver)
		if lead != "" {
			return lead, false
		}
		pronoun = pronoun || leadPronoun
	}
	return "", pronoun
}

// sentenceStartName finds a capitalized name run at the start of a
// sentence ("Bert rounded on Fritz.", "…then smiled. Naomi nodded").
var sentenceStartName = regexp.MustCompile(`(?:^|[.!?]["”’]?\s+)(` + nameRe + `)`)

// otherActorBetween reports whether the narration between two quotes of
// one paragraph starts a sentence with a character other than speaker -
// "Bert rounded on Fritz. “You skulg-sucking…”" is Bert's line, not a
// continuation of whoever spoke before the beat. The previous quote's own
// tag names speaker and so never counts.
func otherActorBetween(between []ParagraphInput, speaker string, resolver *SpeakerNameResolver) bool {
	for _, seg := range between {
		if seg.IsQuote {
			continue
		}
		for _, m := range sentenceStartName.FindAllStringSubmatch(strings.TrimSpace(seg.Text), -1) {
			if c, ok := resolver.Resolve(m[1]); ok && c != speaker {
				return true
			}
		}
	}
	return false
}

// TagChange is one line RefineWithSpeechTags changed, and why.
type TagChange struct {
	Idx      int
	From, To string
	Reason   string // "tag" or "continuation"
}

// RefineWithSpeechTags corrects speakers (paragraph Idx -> speaker, as
// AttributeChapter returns) against the chapter's own text. paras must be
// the whole chapter in order (context for lines not in speakers is still
// read); only idx values present in speakers are ever changed, so a
// caller can pass just the lines it's about to persist and never touch
// anything else (e.g. a reader's manual fix elsewhere in the chapter).
//
// Two rules, applied per source paragraph (a run of Inline segments):
//
//  1. A quote with an explicit named tag - the narration right after it
//     ("…,” Bert said / said Bert), else a lead-in right before it in the
//     same paragraph (Bert said, “…” / Bert stood, saying, “…”) - is that
//     character's, when the name resolves to exactly one known character.
//  2. A quote with no tag of its own continues the speaker of the
//     previous quote in the same paragraph ("“Maybe,” Sid hedged. “But if
//     I…”" stays Sid) - the convention systemPrompt already states. A
//     pronoun tag ("she replied") blocks this, since it can mark a new
//     speaker in a paragraph that merged several turns.
//
// Anything else keeps the model's answer.
func RefineWithSpeechTags(paras []ParagraphInput, speakers map[int]string, resolver *SpeakerNameResolver) (map[int]string, []TagChange) {
	out := make(map[int]string, len(speakers))
	for k, v := range speakers {
		out[k] = v
	}
	var changes []TagChange
	set := func(idx int, name, reason string) {
		cur, ok := out[idx]
		if !ok || cur == name {
			return
		}
		out[idx] = name
		changes = append(changes, TagChange{Idx: idx, From: cur, To: name, Reason: reason})
	}

	for start := 0; start < len(paras); {
		end := start + 1
		for end < len(paras) && paras[end].Inline {
			end++
		}
		group := paras[start:end]
		prevSpeaker := ""
		lastQuote := -1
		for i := range group {
			p := &group[i]
			if !p.IsQuote {
				continue
			}
			var prev, next *ParagraphInput
			if i > 0 && !group[i-1].IsQuote {
				prev = &group[i-1]
			}
			if i+1 < len(group) && !group[i+1].IsQuote {
				next = &group[i+1]
			}
			name, pronoun := tagFor(prev, next, resolver)
			switch {
			case name != "":
				set(p.Idx, name, "tag")
			case !pronoun && prevSpeaker != "" && !otherActorBetween(group[lastQuote+1:i], prevSpeaker, resolver):
				set(p.Idx, prevSpeaker, "continuation")
			}
			lastQuote = i
			if s := out[p.Idx]; !isSentinel(s) {
				prevSpeaker = s
			} else if name == "" {
				prevSpeaker = ""
			}
		}
		start = end
	}
	return out, changes
}

// nonAnswers are speaker values that name no one - a model filling the
// field with a refusal, a yes/no, or a placeholder instead of "Unknown".
var nonAnswers = map[string]bool{
	"none": true, "no one": true, "noone": true, "nobody": true, "no-one": true,
	"someone": true, "somebody": true, "anyone": true, "anybody": true, "everyone": true,
	"everybody": true, "all": true, "both": true, "yes": true, "no": true, "n/a": true,
	"na": true, "null": true, "nil": true, "unknown speaker": true, "speaker": true,
	"unnamed": true, "unidentified": true, "various": true, "multiple": true, "crowd": true,
}

var possessiveWords = map[string]bool{
	"his": true, "her": true, "their": true, "my": true, "our": true, "its": true, "your": true,
}

var jointSeparators = regexp.MustCompile(`\s+(?:and|&|or)\s+|\s*,\s*|\s*/\s*`)

// sanitizeSpeaker cleans one raw speaker value from the model (and its
// "role" flag), returning "Unknown" for anything that names no single
// person. See this file's own doc comment for the observed cases.
func sanitizeSpeaker(raw string, role bool) (string, bool) {
	s := strings.TrimSpace(raw)
	// The prompt's own "Known characters" markup leaking back out:
	// "Thug [role]", "Captain Mocar (a weathered sea captain)".
	if i := strings.Index(strings.ToLower(s), "[role]"); i >= 0 {
		s = strings.TrimSpace(s[:i] + s[i+len("[role]"):])
		role = true
	}
	if i := strings.Index(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		s = strings.TrimSpace(s[:i])
	}
	s = normalizeBarePronoun(s)
	lower := strings.ToLower(s)
	switch {
	case lower == "unknown":
		return "Unknown", false
	case lower == "narrator":
		return "Narrator", false
	case s == "" || nonAnswers[lower]:
		return "Unknown", false
	}
	words := strings.Fields(s)
	if possessiveWords[strings.ToLower(words[0])] || isGroupLabel(words) {
		return "Unknown", false
	}
	if parts := jointSeparators.Split(s, -1); len(parts) >= 2 {
		joint := true
		for _, p := range parts {
			if !startsUpper(strings.TrimSpace(p)) {
				joint = false
				break
			}
		}
		if joint {
			return "Unknown", false
		}
	}
	// A role label reads "Guard", not "the guard" (systemPrompt's own
	// rule). Only for roles - "The Duskmoth" can be a real name.
	if role && len(words) > 1 {
		switch strings.ToLower(words[0]) {
		case "the", "a", "an":
			s = capitalizeFirst(strings.Join(words[1:], " "))
		}
	}
	return s, role
}

// groupWords open a label that names several people at once - "the two
// clerks", "both of them", "several guards" - never one speaker.
var groupWords = map[string]bool{
	"two": true, "three": true, "four": true, "five": true, "several": true, "some": true,
	"many": true, "both": true, "all": true, "everyone": true, "twins": true, "pair": true,
	"group": true, "crowd": true,
}

func isGroupLabel(words []string) bool {
	for i, w := range words {
		w = strings.ToLower(strings.Trim(w, ",."))
		if i == 0 && (w == "the" || w == "a") {
			continue
		}
		return groupWords[w]
	}
	return false
}

// SanitizeSpeaker is sanitizeSpeaker for callers outside this package
// (no role flag): the cleaned name, "Unknown" for anything naming no
// single person.
func SanitizeSpeaker(name string) (string, bool) {
	return sanitizeSpeaker(name, false)
}

// IsGroupSpeaker reports whether name names more than one person ("Bert
// and Sid", "Bert & Sid", "Bert, Sid", "the two clerks") - never a valid
// character, whoever proposes it.
func IsGroupSpeaker(name string) bool {
	clean, _ := sanitizeSpeaker(name, false)
	return clean == "Unknown" && !strings.EqualFold(strings.TrimSpace(name), "unknown") && isGroupShaped(name)
}

func isGroupShaped(name string) bool {
	words := strings.Fields(name)
	if len(words) == 0 {
		return false
	}
	if isGroupLabel(words) {
		return true
	}
	parts := jointSeparators.Split(strings.TrimSpace(name), -1)
	if len(parts) < 2 {
		return false
	}
	for _, p := range parts {
		if !startsUpper(strings.TrimSpace(p)) {
			return false
		}
	}
	return true
}

func capitalizeFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
