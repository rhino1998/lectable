package speakerattr

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// voiceWord matches a word about how someone sounds - what a voice clue
// must contain.
var voiceWord = regexp.MustCompile(`(?i)\b(?:voices?|accents?|accented|brogue|drawl|lilt|baritone|soprano|falsetto|lisp|stammer|stutter)\b`)

// ageWord matches a stated age. Only narration naming the character is
// searched for one: beside their own line it is nearly always someone
// they are looking at ("I muttered at the sight of an elderly man").
var ageWord = regexp.MustCompile(`(?i)\b(?:\w+ years old|\w+-year-old|in (?:his|her|their) (?:teens|twenties|thirties|forties|fifties|sixties|seventies|eighties))\b`)

// voiceAdjective is a word for a lasting quality of a voice. Most also
// describe other things ("the shrill clamour"), so one only counts beside
// the voice itself (voiceQuality).
const voiceAdjective = `(?:gruff|gravelly|husky|rasp(?:y|ing)|reedy|nasal|squeaky|booming|rumbling|sonorous|melodi(?:c|ous)|musical|sing-?song|silky|shrill|piping|deep|smooth|thin|rich|high|light|high-pitched|low-pitched|manly|girlish|boyish|childish)`

// The three things a voice clue is scored on (voiceClueScore).
var (
	// voiceResting marks a statement of how they usually sound ("his
	// normally steady voice", "her usual tone").
	voiceResting = regexp.MustCompile(`(?i)\b(?:normal(?:ly)?|usual(?:ly)?|always|typical(?:ly)?|natural(?:ly)?|customary|characteristic(?:ally)?)[,\s]+(?:[\w-]+[,\s]+){0,3}(?:voice|tone|accent|drawl|lilt|brogue|pitch)\b`)
	// voiceQuality is a lasting quality: an adjective on the voice, an
	// accent, a register, a speech habit they are said to have.
	voiceQuality = regexp.MustCompile(`(?i)\b` + voiceAdjective + `[,\s]+(?:[\w-]+[,\s]+){0,2}(?:voice|tone)\b|\b(?:voice|tone)\s+(?:[\w-]+\s+){0,3}` + voiceAdjective + `\b|\b(?:accents?|accented|brogue|drawl|lilt|baritone|soprano|falsetto)\b|\b(?:a|his|her|my|their)\s+(?:[\w-]+\s+)?(?:lisp|stammer|stutter)\b`)
	// voiceMomentary marks one moment's delivery: a voice lowered or
	// raised, a feeling heard in it, a voice that cracks or shakes.
	voiceMomentary = regexp.MustCompile(`(?i)\b(?:lower|dropp?|rais|pitch|keep|kept|hush|level|smooth|soften|forc)\w*\s+(?:his|her|my|their|the)\s+voice\b|\bin\s+(?:his|her|my|their)\s+voice\b|\bvoice\s+(?:crack|trembl|broke|shook|rose|dropp|waver|caught|echo)\w*|\b(?:hoarse|whisper\w*|shout\w*|trembl\w+)\b`)
	// otherVoice finds whose voice a sentence names ("Vee's calming voice").
	// voiceOwner finds the pronoun a voice is given to ("her voice", "in
	// its rough, high voice").
	voiceOwner = regexp.MustCompile(`(?i)\b(his|her|its)\s+(?:[\w-]+,?\s+){0,3}?(?:voice|accent|drawl|brogue|lilt|tone)\b`)
	otherVoice = regexp.MustCompile(`\b(\p{Lu}[\p{L}-]+)['’]s\s+(?:[\p{L}-]+\s+){0,3}?(?:voice|accent|drawl|brogue|lilt)\b`)
)

// minVoiceClueScore is the lowest voiceClueScore worth showing: one
// lasting quality, or a statement of their usual voice even inside a
// momentary one ("a tremble creeping into his normally steady voice").
const minVoiceClueScore = 2

// voiceClueScore ranks a sentence by how much it says about a resting
// voice rather than one moment's delivery.
func voiceClueScore(sentence string) int {
	score := 0
	if voiceResting.MatchString(sentence) {
		score += 4
	}
	if voiceQuality.MatchString(sentence) || ageWord.MatchString(sentence) {
		score += 2
	}
	if voiceMomentary.MatchString(sentence) {
		score -= 2
	}
	return score
}

// VoiceParagraphs keeps the paragraphs (a whole book's narration) that
// contain a voice word or a stated age - the small set VoiceClues searches, cached per
// book by its caller.
func VoiceParagraphs(texts []string) []string {
	var out []string
	for _, t := range texts {
		if voiceWord.MatchString(t) || ageWord.MatchString(t) {
			out = append(out, t)
		}
	}
	return out
}

// VoiceClues returns up to limit sentences of narration saying how a
// character sounds or how old they are, best first (voiceClueScore, then
// the order given): from tags, the narration following their own lines
// (store.SpeechTagsForBooks), and from paragraphs (see VoiceParagraphs),
// where one of names - as in OriginClues - sits within originClueDistance
// characters before the voice word or stated age (after it, the sentence
// is usually about whoever they are facing). A sentence giving the voice
// to someone else - by name, or by a pronoun that isn't one of pronouns'
// (the character's, "" when unknown) - is left out. CharacterizeVoice
// shows them as "Voice clues": the quote sample is 20 lines of thousands,
// so "Sid's gruff voice" and "his normally steady voice" never reached
// characterization.
func VoiceClues(tags, paragraphs, names, shared []string, pronouns string, limit int) []string {
	nameRes := clueNames(names, shared)
	own := func(sentence string) bool {
		for _, m := range otherVoice.FindAllStringSubmatch(sentence, -1) {
			theirs := false
			for _, re := range nameRes {
				if re.MatchString(m[1]) {
					theirs = true
					break
				}
			}
			if !theirs {
				return false
			}
		}
		for _, m := range voiceOwner.FindAllStringSubmatch(sentence, -1) {
			switch owner := strings.ToLower(m[1]); {
			case owner == "its" && pronouns != "it/its",
				owner == "her" && pronouns == "he/him",
				owner == "his" && pronouns == "she/her":
				return false
			}
		}
		return true
	}
	type clue struct {
		text  string
		score int
	}
	var clues []clue
	add := func(sentence string) {
		if score := voiceClueScore(sentence); score >= minVoiceClueScore && own(sentence) {
			clues = append(clues, clue{sentence, score})
		}
	}
	for _, t := range tags {
		if m := voiceWord.FindStringIndex(t); m != nil {
			add(sentencesAround(t, m[0], m[1]))
		}
	}
	for _, p := range paragraphs {
		hits := append(voiceWord.FindAllStringIndex(p, -1), ageWord.FindAllStringIndex(p, -1)...)
		if dist, span := nearestToName(p, nameRes, hits, true); dist <= originClueDistance {
			add(sentencesAround(p, span[0], span[1]))
		}
	}
	sort.SliceStable(clues, func(i, j int) bool { return clues[i].score > clues[j].score })
	var out []string
	dup := map[string]bool{}
	for _, c := range clues {
		if len(out) == limit {
			break
		}
		if !dup[c.text] {
			dup[c.text] = true
			out = append(out, c.text)
		}
	}
	return out
}

// SpokenLine is one of a character's own lines and the dialogue line it
// answers ("" when it answers none) - store.SpokenLine as plain values.
type SpokenLine struct {
	Text  string
	Asked string
}

// ageNumber is a number as an age is said or printed.
const ageNumber = `(?:\d{1,4}|(?:(?:an?|one|two|three|four|five|six|seven|eight|nine)\s+(?:hundred|thousand)(?:\s+and)?[\s,]+)*(?:(?:twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety)(?:[-\s](?:one|two|three|four|five|six|seven|eight|nine))?|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|two|three|four|five|six|seven|eight|nine)|(?:an?|one|two|three|four|five|six|seven|eight|nine)\s+(?:hundred|thousand))`

var (
	// ageQuestion is someone asking a person their age.
	ageQuestion = regexp.MustCompile(`(?i)\bhow old (?:are|were) (?:you|ya)\b|\bhow old you are\b|\bwhat(?:['’]s| is) your age\b|\byour age\?`)
	// selfAge is a speaker giving their own age: "I'm seventy-six, this
	// past spring", "I am 19 years old". The number must end the clause
	// or be followed by "years" - "I'm twenty feet from the door" isn't
	// one - and "I was twelve" is an age they no longer are.
	selfAge = regexp.MustCompile(`(?i)\bI(?:['’]m| am| turn(?:ed)?|['’]ll be| will be)\s+(?:(?:only|just|barely|nearly|almost|over|past)\s+)?` + ageNumber + `(?:\s+years?\b|\s+(?:this|next|last)\b|\s*(?:[,.;!?…”"]|$))`)
)

// maxAskedChars caps the question shown beside a stated age.
const maxAskedChars = 160

// StatedAges returns up to limit of lines in which the character gives
// their own age, each with the line it answered: replies to someone
// asking how old they are first, then lines stating it unasked
// (selfAge). CharacterizeVoice shows them as "Their age, in their own
// words". Yen's only stated age in four books is her answer "I'm
// seventy-six, this past spring" - one of 393 lines, so rarely in the
// quote sample, naming nobody, so found by no search for her name, and
// dialogue, which VoiceClues doesn't read for ages. She was cast as a
// high-pitched young woman.
func StatedAges(lines []SpokenLine, limit int) []string {
	var asked, unasked []string
	dup := map[string]bool{}
	for _, l := range lines {
		isAnswer := ageQuestion.MatchString(l.Asked)
		if !isAnswer && !selfAge.MatchString(l.Text) {
			continue
		}
		clue := oneLine(l.Text)
		if l.Asked != "" {
			clue = fmt.Sprintf("%s [answering: %s]", clue, oneLine(truncate(l.Asked, maxAskedChars)))
		}
		if dup[clue] {
			continue
		}
		dup[clue] = true
		if isAnswer {
			asked = append(asked, clue)
		} else {
			unasked = append(unasked, clue)
		}
	}
	out := append(asked, unasked...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// genderWords are the words that commit a casting to a gender, by the
// pronouns they belong to.
var genderWords = map[string]map[string]bool{
	"he/him":  {"man": true, "boy": true, "male": true, "gentleman": true, "he": true, "him": true, "his": true, "himself": true},
	"she/her": {"woman": true, "girl": true, "female": true, "lady": true, "she": true, "her": true, "hers": true, "herself": true},
}

// genderContradicts reports whether instruct fails to cast pronouns'
// gender (he/him or she/her; anything else never contradicts): it uses
// the other gender's words, or none of its own. Jax, a he/him narrator
// whose material was all of a fairy he was looking at, was
// cast "Speak as a tiny woman", and then, from a line saying he wasn't
// human, "a young adult non-human entity".
func genderContradicts(instruct, pronouns string) bool {
	own := genderWords[pronouns]
	if own == nil {
		return false
	}
	other := genderWords["he/him"]
	if pronouns == "he/him" {
		other = genderWords["she/her"]
	}
	cast := false
	for _, w := range strings.Fields(strings.ToLower(instruct)) {
		w = strings.Trim(w, `.,;:!?"'’()`)
		if other[w] {
			return true
		}
		cast = cast || own[w]
	}
	return !cast
}
