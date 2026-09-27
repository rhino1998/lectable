package speakerattr

import "strings"

// The pronoun sets a character can carry (store.Character.Pronouns). ""
// means the text never established one.
const (
	PronounsHe   = "he/him"
	PronounsShe  = "she/her"
	PronounsThey = "they/them"
	PronounsIt   = "it/its"
)

// pronounForms maps every spelling the model (or a reader) might give to
// its canonical set - a bare subject pronoun is what the attribution
// prompt asks for, the rest are tolerated variants.
var pronounForms = map[string]string{
	"he": PronounsHe, "him": PronounsHe, "his": PronounsHe, "he/him": PronounsHe, "he/his": PronounsHe,
	"male": PronounsHe, "m": PronounsHe, "man": PronounsHe, "boy": PronounsHe,
	"she": PronounsShe, "her": PronounsShe, "hers": PronounsShe, "she/her": PronounsShe, "she/hers": PronounsShe,
	"female": PronounsShe, "f": PronounsShe, "woman": PronounsShe, "girl": PronounsShe,
	"they": PronounsThey, "them": PronounsThey, "their": PronounsThey, "they/them": PronounsThey, "they/their": PronounsThey,
	"it": PronounsIt, "its": PronounsIt, "it/its": PronounsIt, "it/it": PronounsIt,
}

// NormalizePronouns maps raw to one of the Pronouns* constants, or ""
// for anything it doesn't recognize (including "unknown"/"?").
func NormalizePronouns(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.Trim(s, ` "'*.()[]“”`)
	s = strings.ReplaceAll(s, " ", "")
	return pronounForms[s]
}

// splitPronouns splits a reply value "Name | she" into its speaker and
// normalized pronouns. A value with no " | " part has no pronouns.
func splitPronouns(value string) (speaker, pronouns string) {
	i := strings.LastIndex(value, "|")
	if i < 0 {
		return value, ""
	}
	return strings.TrimSpace(value[:i]), NormalizePronouns(value[i+1:])
}

// pronounVotes tallies each speaker's per-line pronoun answers, so one
// stray line can't set a character's pronouns on its own.
type pronounVotes map[string]map[string]int

func (v pronounVotes) add(speaker, pronouns string) {
	if pronouns == "" || isSentinel(speaker) {
		return
	}
	if v[speaker] == nil {
		v[speaker] = map[string]int{}
	}
	v[speaker][pronouns]++
}

// winners returns each speaker's strict-majority pronouns (more than half
// of the lines that gave any). Speakers without one are left out.
func (v pronounVotes) winners() map[string]string {
	out := make(map[string]string, len(v))
	for speaker, counts := range v {
		total, best, bestN := 0, "", 0
		for p, n := range counts {
			total += n
			if n > bestN {
				best, bestN = p, n
			}
		}
		if bestN*2 > total {
			out[speaker] = best
		}
	}
	return out
}
