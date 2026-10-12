package speakerattr

import (
	"regexp"
	"strings"
)

// castingAccents are the accents CastingAccent knows by name, each with
// the words (lower case) that mean it in a casting, most specific first:
// "plummy aristocratic Received Pronunciation" and "aristocratic RP" are
// one accent, and "a refined British accent" is not Cockney. Castings
// phrased the accent 345 ways across 7,274 characters (2026-10-11); these
// cover all but a handful, which fall to castingAccentTail.
var castingAccents = []struct {
	name  string
	words []string
}{
	{"Received Pronunciation", []string{"received pronunciation", " rp ", "queen's english"}},
	{"Cockney", []string{"cockney"}},
	{"West Country", []string{"west country", "broad rural", "rural english"}},
	{"Scottish", []string{"scottish", "scots"}},
	{"Irish", []string{"irish"}},
	{"Welsh", []string{"welsh"}},
	{"Northern English", []string{"northern english", "yorkshire", "geordie", "mancunian", "scouse"}},
	{"Australian", []string{"australian", "aussie"}},
	{"New Zealand", []string{"new zealand", "kiwi"}},
	{"Southern American", []string{"southern american", "american southern", "southern drawl", "texan", "cajun"}},
	{"New York", []string{"new york", "brooklyn", "bronx"}},
	{"General American", []string{"general american", "american"}},
	{"Canadian", []string{"canadian"}},
	{"British", []string{"british", "english"}},
	{"French", []string{"french"}},
	{"German", []string{"german"}},
	{"Russian", []string{"russian"}},
	{"Eastern European", []string{"eastern european", "slavic", "polish"}},
	{"Scandinavian", []string{"scandinavian", "nordic", "norwegian", "swedish"}},
	{"Italian", []string{"italian"}},
	{"Spanish", []string{"spanish", "latin american", "mexican", "hispanic"}},
	{"Middle Eastern", []string{"middle eastern", "arabic", "persian"}},
	{"Indian", []string{"indian", "south asian"}},
	{"East Asian", []string{"east asian", "japanese", "chinese", "korean"}},
	{"African", []string{"african", "nigerian", "kenyan"}},
	{"Caribbean", []string{"caribbean", "jamaican"}},
}

// castingAccentPhrase is the few words before "accent" in a casting.
var castingAccentPhrase = regexp.MustCompile(`((?:[\pL][\pL'’-]*[ -]){1,5})accent\b`)

// castingAccentTail is the run of capitalized words an accent phrase ends
// in - the name of one castingAccents doesn't list ("a soft Drathian
// accent").
var castingAccentTail = regexp.MustCompile(`((?:\p{Lu}[\pL'’-]*[ -])+)$`)

var notAccentNames = map[string]bool{
	"His": true, "Her": true, "Their": true, "Its": true, "The": true, "This": true, "That": true, "An": true, "A": true, "No": true, "Any": true,
}

// CastingAccent names the accent a casting instruction (CharacterizeVoice's
// instruct) asks for, so castings can be compared: "Adopt a guttural
// Cockney accent" and "with a Cockney accent" are both "Cockney". "" when
// the casting states none.
func CastingAccent(instruct string) string {
	known := func(phrase string) string {
		phrase = " " + strings.ToLower(phrase) + " "
		for _, a := range castingAccents {
			for _, w := range a.words {
				if strings.Contains(phrase, w) {
					return a.name
				}
			}
		}
		return ""
	}
	for _, m := range castingAccentPhrase.FindAllStringSubmatch(instruct, -1) {
		if name := known(m[1]); name != "" {
			return name
		}
		// A sentence's first word is capitalized too: "His accent is ...".
		if tail := strings.TrimSpace(castingAccentTail.FindString(m[1])); tail != "" && !notAccentNames[tail] {
			return tail
		}
	}
	// "in refined Received Pronunciation", with no "accent" after it.
	if strings.Contains(instruct, "Received Pronunciation") {
		return "Received Pronunciation"
	}
	return ""
}

// IsAccentWord reports whether w is a word castingAccents knows an accent
// by ("british", "cockney").
func IsAccentWord(w string) bool {
	w = strings.ToLower(w)
	for _, a := range castingAccents {
		for _, have := range a.words {
			if strings.TrimSpace(have) == w {
				return true
			}
		}
	}
	return false
}
