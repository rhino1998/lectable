package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// A character's dossier: single things the text states about them - what
// kind of being they are, how old they are at some chapter - each kept
// with the sentence it rests on (store.CharacterNote) and proposed for the
// reader to accept. Casting is shown the accepted ones beside its
// material.
//
// This is the second dossier. The first (2026-10-04, see "Dossier -
// tried, removed" in docs/speakerattr.md) had one call distil a profile
// from a character's material and casting read only that; it broke more
// castings than it fixed, because a misread quote ("Bert said, copying
// Fritz's slightly aristocratic accent") became a fact the second call
// trusted. What differs here:
//   - A note is found by searching the whole series for one sort of
//     statement, not by summarizing what casting happened to be shown.
//   - Each is one narrow claim with its sentence, checked again by a call
//     that sees only the claim (VerifyKinds, SettleAges) - the shape the
//     accent step and fact finding's settle step have since shown works.
//   - Nothing is in force until the reader accepts it, and casting still
//     reads the material itself.
//
// Kinds are found from how the text names a character beside their kind
// ("Kikri, the Elf", "Anguin is a Goblin", "the Hobgoblin, Heva" -
// KindMentions), which needs to know which words are kinds: the series'
// world facts and castings give some, and NameKinds picks the rest out of
// the words the pattern turned up. Asking the model per character instead
// (11 passages each, the 25 busiest of Unbound) named a kind for 9 with 5
// right, and missed 5 that casting already had: a main character's kind
// is stated somewhere in thousands of mentions, and a search by meaning
// for it mostly returns scenes. The pattern, over the same series, gave
// 47 characters a kind - about two in three right before VerifyKinds, the
// misses being figures of speech ("Cal was a ghost, flitting between
// enemies"), lists ("the Geist, Theron, and Yttin") and adjectives ("the
// giant Minotaur") - and most were minor characters casting never
// labelled.
//
// Ages are rare: an exact search for age phrases (AgePhrase) finds 42
// paragraphs in The Legend of William Oh, and FindAges reads 17 ages out
// of them - about half sound before SettleAges, the rest someone's age at
// an earlier time ("since he was twelve"), another person's ("No, she's
// twenty"), or a birthday missed by years.

// KindMention is one place the text names a character beside what may be
// their kind.
type KindMention struct {
	// Name is the capitalised words taken for the character, as written;
	// the caller matches them to its roster.
	Name string
	// Word is the kind, lower case ("frost giant").
	Word string
	// Sentence is the sentence it is in.
	Sentence string
}

const (
	kindName = `[A-Z][\pL'’-]+(?: [A-Z][\pL'’-]+){0,2}`
	kindWord = `[A-Z][a-z]+(?:[- ][A-Z][a-z]+)?|[a-z]+`
	kindAdj  = `(?:(?:young|old|tall|short|small|large|female|male|little|big|fellow|[a-z]+ly) ){0,2}`
)

// kindPatterns each capture a name and a kind word; nameFirst says which
// group is the name, and said marks the "Name was a K" form, the one a
// figure of speech takes.
var kindPatterns = []struct {
	re              *regexp.Regexp
	nameFirst, said bool
}{
	{regexp.MustCompile(`\b(` + kindName + `),? (?:was|is|were|being|as) (?:an?|the) ` + kindAdj + `(` + kindWord + `)\b`), true, true},
	{regexp.MustCompile(`\b(` + kindName + `), (?:an?|the) ` + kindAdj + `(` + kindWord + `)\b`), true, false},
	{regexp.MustCompile(`\b[Tt]he ` + kindAdj + `(` + kindWord + `)(?: named| called|,)? (` + kindName + `)\b`), false, false},
}

// figurativeKinds are kinds people are called without being one: "Will
// was a ghost, and always got the upper hand", "Bakton was a demon with
// the blade". Written in lower case after "was a", they are not taken as
// a mention - VerifyKinds passed all three of William Oh's, with that very
// sentence among its examples of what not to pass.
var figurativeKinds = map[string]bool{
	"demon": true, "ghost": true, "monster": true, "beast": true, "giant": true, "devil": true, "angel": true,
	"god": true, "goddess": true, "machine": true, "shadow": true, "animal": true, "wolf": true, "dog": true,
	"snake": true, "rat": true, "pig": true, "fox": true, "lion": true, "bear": true, "titan": true, "zombie": true,
	"robot": true, "witch": true, "wraith": true, "phantom": true, "fiend": true, "saint": true, "vampire": true,
	"ogre": true, "troll": true, "goblin": true, "dragon": true, "spirit": true, "golem": true,
}

// KindMentions returns every place text names someone beside a word that
// may be their kind: "Name was a K", "Name, the K", "the K, Name". Most of
// what it returns is not a kind at all ("Will was a moment too late") -
// the caller keeps the mentions whose Word it knows to be one.
func KindMentions(text string) []KindMention {
	var out []KindMention
	for _, p := range kindPatterns {
		for _, m := range p.re.FindAllStringSubmatchIndex(text, -1) {
			name, word := text[m[2]:m[3]], text[m[4]:m[5]]
			if !p.nameFirst {
				name, word = word, name
			}
			if p.said && figurativeKinds[word] {
				continue
			}
			out = append(out, KindMention{Name: name, Word: strings.ToLower(word), Sentence: sentencesAround(text, m[0], m[1])})
		}
	}
	return out
}

const nameKindsSystemPrompt = `You are given words a novel uses of its characters, in phrases like "Anguin was a ___" and "the ___, Heva". Pick out the ones that name a kind of being - a species, race or sort of creature: goblin, elf, drake, dwarf, kobold, vampire, golem, dragon, spirit, android, and any invented kind the story has (they are often capitalised: Naiad, Korvaa, Geist).

Leave out everything else: jobs, ranks, titles and classes, magical ones included (guard, mage, necromancer, wizard, lord, princess, captain, healer, climber, adventurer, boss), family and age words (man, woman, girl, child, father), nationalities and factions, descriptions (giant as in very large, monster as an insult, bastard, fool), and ordinary nouns (moment, way, bit). Leave out "human" too. When unsure, leave it out.

Reply with a single JSON object and nothing else:
{"kinds": ["<word>", ...]}`

// NameKinds picks, out of words KindMentions turned up, the ones that
// name a kind of being. Only words it was given come back.
func (c *Client) NameKinds(ctx context.Context, words []string) ([]string, error) {
	if len(words) == 0 {
		return nil, nil
	}
	given := map[string]bool{}
	for _, w := range words {
		given[w] = true
	}
	user := "Words:\n" + strings.Join(words, ", ")
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, nameKindsSystemPrompt, user, float32(attempt)*retryTempBump, 600)
	}, func(content string) ([]string, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return nil, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var result struct {
			Kinds []string `json:"kinds"`
		}
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return nil, fmt.Errorf("parse JSON object: %w", err)
		}
		var out []string
		for _, k := range result.Kinds {
			if k = strings.ToLower(strings.TrimSpace(k)); given[k] && !notFactKinds[k] {
				given[k] = false
				out = append(out, k)
			}
		}
		return out, nil
	})
}

// KindClaim is one "this character is of this kind" for VerifyKinds, with
// the sentence it was read from.
type KindClaim struct {
	// Names are what the character goes by: their name, then aliases.
	Names          []string
	Kind, Sentence string
}

const verifyKindsSystemPrompt = `You check claims about what kind of being a character in a novel is. Each claim gives a kind (a species, race or sort of creature) and one sentence from the novel. A reader will review your answers.

For each claim, say who the sentence states is of that kind: copy the name of that one character as the sentence writes it. Answer "nobody" when the sentence states it of no named character, which is common:
- the word is a figure of speech or a description ("Cal was a ghost, flitting between enemies", "Bakton was a demon with the blade", "the giant Minotaur" where giant only means very large);
- the kind is said of people in general, of a group, or of someone unnamed ("like Will, a Climber could easily get worn down", "the kobold-jibleya alliance");
- a name and the kind merely sit side by side in a list ("the Geist, Theron, and Yttin");
- the sentence asks, doubts or denies it.
"Anguin is a Goblin" -> Anguin. "Kikri, the Elf, whispered" -> Kikri. "The Hobgoblin, Heva, was older than he first noticed" -> Heva. "glancing at Jason as the kobold line formed" -> nobody.

Reply with a single JSON object and nothing else:
{"claims": [{"n": <the claim's number>, "who": "<the name, or nobody>"}]}`

// VerifyKinds checks claims against their own sentences and reports which
// hold, in claims' order. The model is not shown whom a claim is about: it
// names whom the sentence says is of the kind, and the claim holds when
// that is the claim's character. Asked "is Jason a kobold?" of "glancing
// at Jason as the kobold-jibleya alliance formed a line" it said yes;
// asked who the sentence says is a kobold, nobody (2026-10-11: 17 of 18
// claims judged right this way, 6 of 10 the other).
func (c *Client) VerifyKinds(ctx context.Context, claims []KindClaim) ([]bool, error) {
	if len(claims) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString("Claims:\n")
	for i, cl := range claims {
		fmt.Fprintf(&sb, "%d. kind: %s - sentence: %s\n", i+1, cl.Kind, cl.Sentence)
	}
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, verifyKindsSystemPrompt, sb.String(), float32(attempt)*retryTempBump, 900)
	}, func(content string) ([]bool, error) {
		entries, err := jsonList(content, "claims")
		if err != nil {
			return nil, err
		}
		out := make([]bool, len(claims))
		for _, e := range entries {
			n, who := intField(e, "n")-1, strings.ToLower(textField(e, "who"))
			if n < 0 || n >= len(claims) || who == "" || who == "nobody" {
				continue
			}
			for _, name := range claims[n].Names {
				// "Loth" for Loth the Luminary, "Lord Bakton" for Bakton.
				if name = strings.ToLower(name); name != "" && (strings.Contains(name, who) || strings.Contains(who, name)) {
					out[n] = true
				}
			}
		}
		return out, nil
	})
}

// holds runs a call whose reply is {"holds": [numbers]} over n numbered
// items and returns which of them were named.
func (c *Client) holds(ctx context.Context, system, user string, n int) ([]bool, error) {
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, system, user, float32(attempt)*retryTempBump, 400)
	}, func(content string) ([]bool, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return nil, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var result struct {
			Holds []float64 `json:"holds"`
		}
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return nil, fmt.Errorf("parse JSON object: %w", err)
		}
		out := make([]bool, n)
		for _, h := range result.Holds {
			if i := int(h) - 1; i >= 0 && i < n {
				out[i] = true
			}
		}
		return out, nil
	})
}

const (
	statedAgeWord   = `(?:one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety)`
	statedAgeNumber = `(?:\d{1,4}|` + statedAgeWord + `(?:[- ]` + statedAgeWord + `)?|(?:a|one|two|three|four|five|six|seven|eight|nine) hundred(?: and ` + statedAgeWord + `)?)`
	statedAgeHedge  = `(?:just |only |barely |nearly |almost |about |already )?`
)

// agePhrase finds a stated age. agePlain ("she was sixteen") needs the
// word after it checked (ageNotAfter): Go's regexp has no lookahead.
var (
	agePhrase = regexp.MustCompile(`(?i)\b(?:` + statedAgeNumber + `[- ]years?[- ]old\b|` + statedAgeNumber + ` years of age|aged? ` + statedAgeNumber + `\b|at the age of ` + statedAgeNumber +
		`|` + statedAgeHedge + `turn(?:ed|ing|s) ` + statedAgeNumber + `\b|` + statedAgeWord + `(?:teen)?th birthday` +
		`|in (?:his|her|their) (?:early |mid[- ]|late )?(?:teens|twenties|thirties|forties|fifties|sixties|seventies|eighties|nineties))`)
	agePlain    = regexp.MustCompile(`(?i)\b(?:I(?:'|’)m|I am|(?:he|she|they|you) (?:is|was|were|are)|(?:he|she)(?:'|’)s) ` + statedAgeHedge + statedAgeWord + `(?:[- ]` + statedAgeWord + `)?\b`)
	ageNotAfter = regexp.MustCompile(`(?i)^ (?:of|feet|foot|inches|miles|years? (?:ago|into|in|from)|times|steps|days|hours|minutes|percent|levels?)\b`)
)

// AgePhrase reports whether text has a phrase that may state someone's
// age ("twelve-year-old", "turned sixteen", "I'm seventy-six", "in her
// forties") - what makes a paragraph a candidate for FindAges.
func AgePhrase(text string) bool {
	if agePhrase.MatchString(text) {
		return true
	}
	for _, m := range agePlain.FindAllStringIndex(text, -1) {
		if !ageNotAfter.MatchString(text[m[1]:]) {
			return true
		}
	}
	return false
}

// AgePassage is one candidate paragraph as FindAges is shown it.
type AgePassage struct {
	// Text is the paragraph (a FactPassage line) - what a quote is checked
	// against.
	Text string
	// Before is the paragraph before it, for who "she" is.
	Before string
	// Speaker is who says it, for dialogue attributed to someone.
	Speaker string
}

// StatedAge is one age FindAges read out of a passage.
type StatedAge struct {
	Passage  int
	Who      string
	Age      int
	Evidence string
}

const findAgesSystemPrompt = `You read passages from a novel and pick out the ones that state how old a named character is, in years. A reader will review what you propose; it is used to make a character's voice the right age.

Each passage is given with the paragraph before it for context, and with who says it when it is dialogue. It counts when the passage gives a character's own age as a number or a decade: "She was sixteen", "I'm seventy-six", "at 34, Halrac", "a man in his fifties" (55), "his twelfth birthday". Name the character as the text names them - from the passage, its context, or the speaker when they give their own age; if you cannot tell who it is, leave it out. It does not count when the number is not an age (a level, a count, a time of day, years of service), is how old they were at some earlier time ("when I was ten"), is how old someone looks or a guess ("looked about forty"), is the age of a thing or of an unnamed person, or is a wish or a joke. Most passages give nothing: an empty list is a normal answer.

Reply with a single JSON object and nothing else:
{"ages": [{"n": <the passage's number>, "who": "<the character's name>", "age": <years>, "evidence": "<the one sentence it rests on, copied word for word>"}]}`

// maxStatedAge bounds an age FindAges may return; beyond it the number
// was something else.
const maxStatedAge = 100000

// FindAges reads passages and returns the ages they state, each with whom
// it is of as the text names them. An age whose quote is not in its
// passage is dropped.
func (c *Client) FindAges(ctx context.Context, passages []AgePassage) ([]StatedAge, error) {
	if len(passages) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString("Passages:\n")
	for i, p := range passages {
		fmt.Fprintf(&sb, "%d. ", i+1)
		if p.Speaker != "" {
			fmt.Fprintf(&sb, "[said by %s] ", p.Speaker)
		}
		sb.WriteString(p.Text + "\n")
		if p.Before != "" {
			fmt.Fprintf(&sb, "   (before it: %s)\n", truncate(p.Before, 250))
		}
	}
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, findAgesSystemPrompt, sb.String(), float32(attempt)*retryTempBump, timeGapMaxTokens)
	}, func(content string) ([]StatedAge, error) {
		entries, err := jsonList(content, "ages")
		if err != nil {
			return nil, err
		}
		var out []StatedAge
		for _, e := range entries {
			n, age := intField(e, "n")-1, intField(e, "age")
			who, evidence := textField(e, "who"), trimQuote(textField(e, "evidence"))
			if n < 0 || n >= len(passages) || age < 1 || age > maxStatedAge || who == "" || evidence == "" || !strings.Contains(passages[n].Text, evidence) {
				continue
			}
			out = append(out, StatedAge{Passage: n, Who: who, Age: age, Evidence: evidence})
		}
		return out, nil
	})
}

// AgeStatement is one age FindAges read for a character, as SettleAges is
// shown it.
type AgeStatement struct {
	// Chapter is where it is said, counted along the series.
	Chapter int
	Age     int
	// Text is the passage and Before the paragraph before it.
	Text, Before string
}

const settleAgesSystemPrompt = `You check statements about one character's age in a novel. Each numbered statement is a passage, with the paragraph before it and the chapter it is in, that was read as giving this character's age in years. A reader will review what you pass; it is used to make the character's voice the right age at that point of the story.

A statement holds only when the passage gives this character's own age at the time of that chapter. It does not hold when the age is someone else's (a speaker talking about another person: "No, she's twenty"), is how old they were at an earlier time ("since he was twelve", "until you were eight"), is a birthday or age being looked back on or missed, is a guess at how old they look, or is not an age at all. Statements from different chapters may give different ages as the character grows older; that is expected.

Reply with a single JSON object and nothing else:
{"holds": [<the number of each statement that holds>]}`

// SettleAges checks the ages read for one character against their own
// passages and reports which hold, in statements' order.
func (c *Client) SettleAges(ctx context.Context, name string, statements []AgeStatement) ([]bool, error) {
	if len(statements) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Character: %s\n\nStatements:\n", name)
	for i, s := range statements {
		fmt.Fprintf(&sb, "%d. [chapter %d, read as age %d] %s\n", i+1, s.Chapter, s.Age, s.Text)
		if s.Before != "" {
			fmt.Fprintf(&sb, "   (before it: %s)\n", truncate(s.Before, 250))
		}
	}
	return c.holds(ctx, settleAgesSystemPrompt, sb.String(), len(statements))
}
