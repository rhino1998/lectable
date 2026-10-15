package speakerattr

import (
	"regexp"
	"slices"
	"strings"
)

// Action beats: RefineWithSpeechTags' rule for a quote with no speech
// tag of its own whose printed paragraph has a beat with a named subject
// right next to it ("“Let’s get it done.” Elijah nodded.", "Staavo
// nodded, then offered up a loop of cable. “Tie it…”") - the convention
// that one paragraph is one speaker's. tagFor alone deliberately trusts
// only verbs of speaking, since a beat can be a listener's reaction; on
// readers' corrections (cmd/tageval, 2026-10-15: 96k dialogue lines of 20
// fully corrected books, the machine wrong on 9.3%) a beat right after
// the quote named the speaker 96.9% of the time and one right before it
// 98.3%, against the model's answers on those lines: +242 and +376 lines
// net. Rejected on the same data: a lowercase role-noun subject ("the
// operator said" -> Operator) was right 45% of the time - "the man" and
// "the attendant" are usually a named character the roster also has.

var (
	// “…” Elijah nodded. / “…” Then Velika smiled, …
	beatTrailing = regexp.MustCompile(`^(?:[A-Z][a-z]+ly |Then |then )?(` + nameRe + `)(?: [a-z]+ly)? ([a-z]+)\b`)
	// Staavo nodded, then offered … “…” - the last sentence of a lead-in.
	beatLead = regexp.MustCompile(`^(?:[A-Z][a-z]+ly |Then )?(` + nameRe + `)(?: [a-z]+ly)? ([a-z]+)\b`)
)

// notBeatVerbs are words after a sentence-initial name that make it
// something other than the subject of an action ("Bert and Sid …",
// "Bert's eyes …", "Bert was …" is kept: a state still marks the beat's
// character).
var notBeatVerbs = map[string]bool{
	"and": true, "or": true, "nor": true, "s": true, "who": true,
	// Perceiving someone else's speech: "Jake heard Minaga yell".
	"heard": true, "hears": true, "saw": true, "sees": true, "watched": true, "watches": true,
	"listened": true, "noticed": true, "felt": true, "realized": true, "realised": true,
	// A state, a thought or a beat that didn't happen, not someone acting:
	// "Rain was nowhere to be seen", "Jinn knew that", "Mora didn't respond".
	"was": true, "were": true, "is": true, "had": true, "has": true, "would": true, "could": true,
	"might": true, "must": true, "did": true, "didn": true, "couldn": true, "wasn": true,
	"wouldn": true, "hadn": true, "never": true, "knew": true, "thought": true, "remembered": true,
	"wanted": true, "needed": true, "seemed": true, "looked": true,
}

// notBeatSubjects are capitalized words that start a sentence without
// being anyone's name, however a roster entry happens to contain them
// ("One thing was certain" is not "Old One"'s line).
var notBeatSubjects = map[string]bool{
	"One": true, "Even": true, "Voice": true, "Still": true, "Then": true, "Just": true, "Only": true,
	"Both": true, "Each": true, "Every": true, "All": true, "Some": true, "Most": true,
	"Another": true, "Neither": true, "Either": true, "No": true, "Not": true, "Now": true,
	"Here": true, "There": true, "When": true, "While": true, "After": true, "Before": true,
}

// beatOthers matches what in a beat sentence says someone other than its
// subject is involved: the first person ("I added", "my shoulder"), or a
// second capitalized name run.
var (
	beatFirstPerson = regexp.MustCompile(`\b(?:I|me|my|myself|mine)\b`)
	beatNameRun     = regexp.MustCompile(nameRe)
)

// ExplicitTag is tagFor for callers outside the package (cmd/tageval):
// who an explicit named speech tag next to a quote credits, if anyone.
func ExplicitTag(prev, next *ParagraphInput, resolver *SpeakerNameResolver) (name string, pronoun bool) {
	return tagFor(prev, next, resolver)
}

// BeatTag reports who an action beat in the same printed paragraph
// credits with a quote: the narration right after it, else the last
// sentence of the narration right before it, starting with a known
// character's name followed by a verb. "" when neither does.
func BeatTag(prev, next *ParagraphInput, resolver *SpeakerNameResolver) string {
	// In a first-person book the narrator's lines carry no tag of their
	// own far more often than anyone else's: a beat naming someone there
	// is as often the listener (cmd/tageval's audit, 2026-10-15).
	if resolver.narrator() != "" {
		return ""
	}
	try := func(re *regexp.Regexp, text string, lead bool) string {
		m := re.FindStringSubmatch(text)
		if m != nil && (notBeatSubjects[m[1]] || strings.HasPrefix(m[1], "The ")) {
			return "" // a common word, or an epithet ("The Feather") matching a generic entry
		}
		if m != nil {
			// The whole sentence must be about the subject alone: no "I",
			// no second character ("Kells made to bluster… but Gerhardt cut
			// him off").
			// After the quote, only the beat's own first sentence counts;
			// a break inside the matched name ("Mr. Dale") is no break.
			sentence := text
			if !lead {
				for _, loc := range lastSentence.FindAllStringIndex(text, -1) {
					if loc[0] >= len(m[0]) && !afterHonorific(text[:loc[0]]) {
						sentence = text[:loc[0]]
						break
					}
				}
			}
			if beatFirstPerson.MatchString(sentence) {
				return ""
			}
			subject, _ := resolver.Resolve(m[1])
			for _, n := range beatNameRun.FindAllString(sentence[len(m[0]):], -1) {
				if c, ok := resolver.Resolve(n); ok && c != subject {
					return ""
				}
			}
		}
		// "Jason’s companion said" is someone else's line.
		if m == nil || notBeatVerbs[m[2]] || strings.HasSuffix(m[1], "’s") || strings.HasSuffix(m[1], "'s") ||
			strings.HasSuffix(m[1], "’") || strings.HasSuffix(m[1], "'") {
			return ""
		}
		// Before a quote, "Fritz whispered." is the previous quote's own
		// speech tag, not a beat for this one.
		if lead && slices.Contains(speechVerbs, m[2]) {
			return ""
		}
		c, ok := resolver.Resolve(m[1])
		if !ok {
			return ""
		}
		return c
	}
	if next != nil {
		if c := try(beatTrailing, strings.TrimLeft(next.Text, " ,.;:-—–"), false); c != "" {
			return c
		}
	}
	if prev != nil {
		text := strings.TrimSpace(prev.Text)
		sentence := text
		if locs := lastSentence.FindAllStringIndex(text, -1); len(locs) > 0 {
			sentence = text[locs[len(locs)-1][1]:]
		}
		if c := try(beatLead, sentence, true); c != "" {
			return c
		}
	}
	return ""
}

// afterHonorific reports whether text ends in an abbreviation whose period
// isn't a sentence's end ("…, but Mr" of "but Mr. Dale frowned").
func afterHonorific(text string) bool {
	i := strings.LastIndexAny(text, " \t\n(\"“")
	return honorifics[text[i+1:]]
}

var honorifics = map[string]bool{"Mr": true, "Mrs": true, "Ms": true, "Dr": true, "St": true, "Jr": true, "Sr": true, "Prof": true, "Gen": true, "Capt": true, "Lt": true, "Sgt": true, "Col": true}
