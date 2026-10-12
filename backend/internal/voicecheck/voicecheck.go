// Package voicecheck compares how a voice's reference clip sounds with how
// its casting says it should: the questions asked of an audio-language
// model about the clip (ttsworker's POST /listen, Audio Flamingo), what
// its answers mean, what the casting asks for, and where the two clearly
// disagree. No model calls of its own.
//
// The listener is a coarse judge, and the rules here are sized to what it
// was measured to hear (2026-10-11, 47 reference clips against their
// castings): a man from a woman among adults, a child from an adult, a
// high voice from a deep one. It called most elderly castings
// "middle-aged", heard boys and teenage boys as women, and could not tell
// a Scottish or Cockney accent from "English" - so an age one band off is
// not a mismatch, gender is not judged for the young, and the accent it
// heard is recorded but never held against the casting.
package voicecheck

import (
	"regexp"
	"strings"
)

// Traits is what a voice sounds like, or is meant to: each field one of
// its fixed values, or "" for unknown.
type Traits struct {
	// Gender is GenderMan or GenderWoman.
	Gender string `json:"gender,omitempty"`
	// Age is one of the Ages bands.
	Age string `json:"age,omitempty"`
	// Pitch is PitchLow, PitchMedium or PitchHigh.
	Pitch string `json:"pitch,omitempty"`
	// Accent is free text: the casting's accent name, or the listener's
	// own words.
	Accent string `json:"accent,omitempty"`
}

const (
	GenderMan   = "man"
	GenderWoman = "woman"

	AgeChild      = "child"
	AgeTeenager   = "teenager"
	AgeYoungAdult = "young adult"
	AgeMiddleAged = "middle-aged"
	AgeElderly    = "elderly"

	PitchLow    = "low"
	PitchMedium = "medium"
	PitchHigh   = "high"
)

// Ages is the age bands, youngest first.
var Ages = []string{AgeChild, AgeTeenager, AgeYoungAdult, AgeMiddleAged, AgeElderly}

// Trait names a field of Traits, as Mismatches reports it.
type Trait string

const (
	TraitGender Trait = "gender"
	TraitAge    Trait = "age"
	TraitPitch  Trait = "pitch"
)

// Question is one thing asked about a clip.
type Question struct {
	// Key names the trait the answer is read into: "gender", "age",
	// "pitch" or "accent".
	Key string
	// Text is the question as asked. Closed questions: asked to describe a
	// speaker freely, the listener says "unspecified age and accent".
	Text string
}

// Questions is everything asked of each clip, in order.
var Questions = []Question{
	{"gender", "Is the speaker a man or a woman? Answer with one word."},
	{"age", "Is the speaker a child, a teenager, a young adult, middle-aged, or elderly? Answer with one of those."},
	{"pitch", "Is the speaker's voice high-pitched, medium, or low and deep? Answer with one of those."},
	{"accent", "What accent does the speaker have? Answer with one or two words, for example American, English, Scottish, Irish, Australian, French, German."},
}

var (
	womanWord = regexp.MustCompile(`(?i)\b(woman|female|girl|lady)\b`)
	manWord   = regexp.MustCompile(`(?i)\b(man|male|boy|gentleman)\b`)
)

// Heard reads the listener's answers (Question.Key -> answer) as traits.
// An answer that names none of a question's options leaves its trait
// unknown.
func Heard(answers map[string]string) Traits {
	var t Traits
	switch g := answers["gender"]; {
	case womanWord.MatchString(g):
		t.Gender = GenderWoman
	case manWord.MatchString(g):
		t.Gender = GenderMan
	}
	t.Age = ageIn(answers["age"])
	switch p := strings.ToLower(answers["pitch"]); {
	case strings.Contains(p, "high"):
		t.Pitch = PitchHigh
	case strings.Contains(p, "low"), strings.Contains(p, "deep"):
		t.Pitch = PitchLow
	case strings.Contains(p, "medium"):
		t.Pitch = PitchMedium
	}
	t.Accent = strings.TrimRight(strings.TrimSpace(answers["accent"]), ".")
	return t
}

// ageWords is each band's wording, in castings and in answers. Matched in
// this order, so "young adult" is found before "adult" and "young boy"
// before "young".
var ageWords = []struct {
	age string
	re  *regexp.Regexp
}{
	{AgeChild, regexp.MustCompile(`(?i)\b(child|young (boy|girl)|little (boy|girl)|small (boy|girl))\b`)},
	{AgeTeenager, regexp.MustCompile(`(?i)\b(teen|teenager|teenage|teenaged|adolescent)\b`)},
	{AgeYoungAdult, regexp.MustCompile(`(?i)\b(young (adult|man|woman|male|female)|(early|mid|late)[- ](20s|twenties)|in (his|her|their) twenties)\b`)},
	{AgeElderly, regexp.MustCompile(`(?i)\b(elderly|old (man|woman)|ancient|in (his|her|their) (sixties|seventies|eighties|nineties))\b`)},
	{AgeMiddleAged, regexp.MustCompile(`(?i)\b(middle[- ]aged|mature|in (his|her|their) (thirties|forties|fifties))\b`)},
}

func ageIn(s string) string {
	for _, w := range ageWords {
		if w.re.MatchString(s) {
			return w.age
		}
	}
	return ""
}

// AgeOfYears is the band of a human of that many years, "" for none
// given: the bands a stage's age is cast in (speakerattr's AgeCasting).
func AgeOfYears(years int) string {
	switch {
	case years <= 0:
		return ""
	case years < 12:
		return AgeChild
	case years < 18:
		return AgeTeenager
	case years < 30:
		return AgeYoungAdult
	case years < 60:
		return AgeMiddleAged
	default:
		return AgeElderly
	}
}

var (
	// pitchPhrase is the wording right around "pitch" in a casting: "Use a
	// very low pitch with", "His pitch is low but", "a higher, lighter
	// pitch".
	pitchPhrase = regexp.MustCompile(`(?i)((?:[\w-]+[, ]+){0,4})pitch(?:ed)?((?:\s+is)?(?:\s+[\w-]+){0,2})`)
	pitchLow    = regexp.MustCompile(`(?i)\b(low|lower|deep|deeper|bass|baritone)\b`)
	pitchHigh   = regexp.MustCompile(`(?i)\b(high|higher)\b`)
	pitchMid    = regexp.MustCompile(`(?i)\b(medium|mid|middle|moderate)\b`)
)

func pitchIn(instruct string) string {
	m := pitchPhrase.FindStringSubmatch(instruct)
	if m == nil {
		return ""
	}
	// "medium-high", "mid-range": the hyphenated halves count separately.
	phrase := strings.ReplaceAll(m[1]+" "+m[2], "-", " ")
	low, high, mid := pitchLow.MatchString(phrase), pitchHigh.MatchString(phrase), pitchMid.MatchString(phrase)
	switch {
	case mid || (low && high):
		return PitchMedium
	case low:
		return PitchLow
	case high:
		return PitchHigh
	}
	return ""
}

// opening is a casting's first sentence, where it says who is speaking
// ("Speak as a middle-aged dwarf man.").
func opening(instruct string) string {
	if i := strings.IndexAny(instruct, ".!?"); i >= 0 {
		return instruct[:i]
	}
	return instruct
}

// Cast is what a casting asks a voice to be. instruct is the casting
// instruction the clip was designed from; pronouns (store.Character's)
// settle the gender when given, the opening's own wording otherwise;
// years, when positive, is the age the voice is cast at (an age stage's)
// and stands in for the opening's wording; accent is the casting's accent
// by name (speakerattr.CastingAccent).
func Cast(instruct, pronouns string, years int, accent string) Traits {
	open := opening(instruct)
	t := Traits{Pitch: pitchIn(instruct), Accent: accent}
	switch {
	case strings.HasPrefix(pronouns, "he"):
		t.Gender = GenderMan
	case strings.HasPrefix(pronouns, "she"):
		t.Gender = GenderWoman
	case womanWord.MatchString(open):
		t.Gender = GenderWoman
	case manWord.MatchString(open):
		t.Gender = GenderMan
	}
	if t.Age = AgeOfYears(years); t.Age == "" {
		t.Age = ageIn(open)
	}
	return t
}

func ageIndex(age string) int {
	for i, a := range Ages {
		if a == age {
			return i
		}
	}
	return -1
}

// Mismatches lists the traits on which heard clearly disagrees with cast
// - see the package comment for why each rule is as loose as it is. A
// trait either side leaves unknown is never one.
func Mismatches(cast, heard Traits) []Trait {
	out := []Trait{}
	// The listener hears children and teenagers of either sex as women.
	young := cast.Age == AgeChild || cast.Age == AgeTeenager
	if cast.Gender != "" && heard.Gender != "" && cast.Gender != heard.Gender && !young {
		out = append(out, TraitGender)
	}
	if c, h := ageIndex(cast.Age), ageIndex(heard.Age); c >= 0 && h >= 0 && (c-h >= 2 || h-c >= 2) {
		out = append(out, TraitAge)
	}
	if (cast.Pitch == PitchLow && heard.Pitch == PitchHigh) || (cast.Pitch == PitchHigh && heard.Pitch == PitchLow) {
		out = append(out, TraitPitch)
	}
	return out
}
