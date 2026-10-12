package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// The fact-finding pass proposes world facts (store.WorldFact) from a
// series' own text, for the reader to accept or skip, of two sorts
// (FactSorts): how long a kind of being lives and how it ages, which
// casting and age stages most need ("Kobolds only live to fifteen or so"
// makes a kobold of thirteen old), and how a whole people speaks.
//
// It is a search by meaning followed by two rounds of short calls
// (httpapi.FindFacts), per sort: its Queries find the passages, FindFacts
// reads them in batches and names kinds, and SettleFact writes one fact
// per kind from every passage about it. Lifespan, measured on 13 series
// (2026-10-11, the prototype behind this): 64 facts kept, about 4 in 5
// sound when read against their quotes, 10-25s a series. The second round
// dropped 27 of 91 proposed kinds (one individual's age, a danger rather
// than a lifespan) and merged repeats - four elf facts in one series
// into one.
//
// The standing weakness, worst for speech (see SpeechFacts): the model
// turns one character's voice into a rule for their kind ("the giant ...
// deep words with a heavy accent" became "Giants speak with a heavy
// accent"), and neither a stricter prompt nor search by meaning tells the
// two apart every time. Tried and left out: a people's culture in general
// (mostly true, rarely anything a voice can use). A word search for lifespan wording, the retrieval before the embedding
// index existed, missed statements phrased any other way ("Fae don't
// really age, we just grow bigger").

// FactSort is one sort of fact the pass looks for: its search phrases
// and its two prompts.
type FactSort struct {
	// Name labels the sort in logs: "lifespan".
	Name string
	// Queries are the phrases a series is searched for by meaning.
	Queries []string
	// kindQuery is the phrase that finds what a series says of one kind,
	// with two %s for the kind - far sharper than Queries once the kind is
	// named (the three kobold lifespan statements ranked 1-3).
	kindQuery string
	// find and settle are the system prompts of FindFacts and SettleFact.
	// The JSON keys their replies use differ by sort, as each prompt was
	// tuned with its own: listKey holds find's list, kindKey and factKey
	// an entry's kind and fact; subject labels the kind in settle's user
	// prompt.
	find, settle                       string
	listKey, kindKey, factKey, subject string
}

// KindQuery is the phrase to search a series for by meaning to find what
// it says of kind under this sort.
func (s FactSort) KindQuery(kind string) string {
	return fmt.Sprintf(s.kindQuery, kind, kind)
}

// LifespanFacts finds how long a kind of being lives and how it ages.
// Three Queries, because each ranks some statements low that another
// ranks high: on The Legend of William Oh the six known ones sat within
// the top 35 for the second and the top 25 for the third, but one was
// 85th for the first.
var LifespanFacts = FactSort{
	Name: "lifespan",
	Queries: []string{
		"how long members of a species or race live; their lifespan",
		"Their kind is long-lived, or short-lived, or does not age.",
		"they age faster or slower than humans do",
	},
	kindQuery: "How long do %ss live? The lifespan of a %s and how they age.",
	find:      findLifespanFactsSystemPrompt,
	settle:    settleLifespanFactSystemPrompt,
	listKey:   "facts", kindKey: "kind", factKey: "fact", subject: "Kind",
}

// SpeechFacts finds how a whole people speaks: a shared accent, a sound in
// their speech, a habit of speaking - what makes every drake or every
// Drowned sailor sound of one kind. Much less sure than LifespanFacts:
// measured on 10 series (2026-10-11) it kept 18 facts of which about 7
// were sound ("the Drakes elongated their 's' and 'l' sounds"; "The Dread
// had a stilted way of speaking"; a Drowned Folk voice "a deep, bubbling
// growl ... as did much of the Drowned Folk"). The rest were one member's
// voice given to the kind ("Another Gnoll spoke, voice a growl"), or
// something no voice can perform (a language that is mostly scent). A
// real-world accent never came back: these books don't name one for a
// people. Kept because a series proposes only one or two, each with its
// quote, and the sound ones are what a reader would not think to record.
var SpeechFacts = FactSort{
	Name: "speech",
	Queries: []string{
		"their people all speak with the same accent, dialect or manner of speech",
		"the way their kind sounds when they talk: hissing, growling, clipped, formal, sing-song",
		"their language and the way their kind talks",
	},
	kindQuery: "How do %ss speak? The accent, voice and manner of speech of the %ss.",
	find:      findSpeechFactsSystemPrompt,
	settle:    settleSpeechFactSystemPrompt,
	listKey:   "peoples", kindKey: "people", factKey: "speech", subject: "People",
}

// FactSorts is every sort the pass looks for, in the order their
// sentences are joined when one kind has more than one.
var FactSorts = []FactSort{LifespanFacts, SpeechFacts}

// factPassageChars caps one passage in a fact-finding prompt.
const factPassageChars = 500

// FactPassage is a paragraph as the fact-finding prompts show it: on one
// line, cut to factPassageChars. A quote is checked against this, so it
// is what QuotedIn takes.
func FactPassage(text string) string {
	text = oneLine(text)
	if len(text) <= factPassageChars {
		return text
	}
	text = text[:factPassageChars]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

// ProposedFact is one fact a fact-finding call proposes.
type ProposedFact struct {
	// Kind is who it is about, as FactKind leaves it: "kobold".
	Kind string
	Fact string
	// Evidence is the sentence it rests on, word for word.
	Evidence string
}

const factFindingMaxTokens = 1000

const findLifespanFactsSystemPrompt = `You read passages from a novel and pick out the ones that state how long a kind of being lives or how fast it ages in this story's world. A reader will review what you propose.

A fact here is about a kind as a whole (a species, a people, a class of creature: "kobolds", "the deep dwarves", "imps"), and its evidence sentence must itself name that kind or speak of "our kind"/"my people" where the passage makes plain which kind that is. Leave out anything about one individual only, an ability, item, spell or title, a figure of speech, a threat, and ordinary humans living ordinary human lives. Most passages state nothing: an empty list is a normal answer.

Reply with a single JSON object and nothing else:
{"facts": [{"kind": "<the kind, singular, lower case>", "fact": "<one plain sentence in the present tense saying how long they live or how they age>", "evidence": "<the one sentence it rests on, copied word for word>"}]}
One fact per kind.`

const findSpeechFactsSystemPrompt = `You read passages from a novel and pick out the ones that state how a whole people speaks - a species, nation, tribe or other kind of being - in a way a voice actor could perform. A reader will review what you propose.

It counts when the passage says it of the people as a whole: an accent or dialect they share, a sound in their speech (they hiss their s's, roll their r's, growl, click, whistle), or a habit of speaking (formal, clipped, never use contractions, speak of themselves in the third person, speak slowly). The evidence sentence must itself name the people or speak of "their kind"/"our people" where the passage makes plain who that is. It does not count when it describes one member's voice ("the dwarf spoke with a burr"), says only that they have a language of their own or cannot be understood, or is about culture, appearance or temperament rather than speech. Most passages give nothing: an empty list is a normal answer.

Reply with a single JSON object and nothing else:
{"peoples": [{"people": "<the people, singular, lower case>", "speech": "<one plain sentence in the present tense on how they speak>", "evidence": "<the one sentence it rests on, copied word for word>"}]}
One entry per people.`

// FindFacts reads passages (FactPassage lines) and returns the facts of
// sort they state, each under the kind it is about. The facts are leads,
// not answers: the same kind comes back from several batches and many are
// about one individual - SettleFact decides. Facts whose Kind FactKind
// rejects are dropped; quotes are not checked here.
func (c *Client) FindFacts(ctx context.Context, sort FactSort, passages []string) ([]ProposedFact, error) {
	if len(passages) == 0 {
		return nil, nil
	}
	var sb strings.Builder
	sb.WriteString("Passages:\n")
	for _, p := range passages {
		fmt.Fprintf(&sb, "- %s\n", p)
	}
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, sort.find, sb.String(), float32(attempt)*retryTempBump, factFindingMaxTokens)
	}, func(content string) ([]ProposedFact, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return nil, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return nil, fmt.Errorf("parse JSON object: %w", err)
		}
		var entries []map[string]any
		if raw, ok := result[sort.listKey]; ok {
			if err := json.Unmarshal(raw, &entries); err != nil {
				return nil, fmt.Errorf("parse %q: %w", sort.listKey, err)
			}
		}
		var out []ProposedFact
		for _, e := range entries {
			kind, fact, evidence := FactKind(textField(e, sort.kindKey)), textField(e, sort.factKey), trimQuote(textField(e, "evidence"))
			if kind == "" || fact == "" || evidence == "" {
				continue
			}
			out = append(out, ProposedFact{Kind: kind, Fact: fact, Evidence: evidence})
		}
		return out, nil
	})
}

// textField is m[key] when it is a string, trimmed.
func textField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

const settleLifespanFactSystemPrompt = `You settle one proposed fact about a story's world. You are given a kind of being and passages from the novel that mention how long that kind lives or how it ages. A reader will review your answer; a voice-casting director will use it to judge how old a character of that kind sounds at a given age.

Decide whether the passages establish, for the kind as a whole, how long it lives or how fast it ages. If they do, write it as one plain sentence in the present tense, with the figure the text gives when there is one, and quote the single sentence that states it best, word for word. If the passages only speak of one individual, of ordinary humans, of how dangerous life is ("rarely last more than a few days"), or of something other than lifespan and ageing, there is no fact.

Reply with a single JSON object and nothing else:
{"kind": "<the kind, singular, lower case>", "fact": "<the sentence, or empty string for no fact>", "evidence": "<the quoted sentence, or empty string>"}`

const settleSpeechFactSystemPrompt = `You settle one proposed fact about a story's world, for the voice-casting director of its audiobook. You are given a people - a species, nation or other kind of being - and passages from the novel that mention how they speak. A reader will review your answer.

Decide whether the passages establish how the people as a whole speaks, in a way a voice actor could perform: a shared accent or dialect, a sound in their speech, or a habit of speaking. If they do, write it as one plain sentence in the present tense, using the text's own description, and quote the single sentence that states it best, word for word. There is no fact when the passages describe only individual members' voices, say only that the people has its own language or needs translating, say their accents vary, or describe temperament or culture rather than speech.

Reply with a single JSON object and nothing else:
{"speech": "<the sentence, or empty string for no fact>", "evidence": "<the quoted sentence, or empty string>"}`

// SettleFact decides what passages (FactPassage lines: the ones FindFacts
// named kind from, and the closest to sort.KindQuery) establish about
// kind under sort. A zero ProposedFact means nothing: they speak of one
// individual, or of something else. The quote is not checked here
// (QuotedIn).
func (c *Client) SettleFact(ctx context.Context, sort FactSort, kind string, passages []string) (ProposedFact, error) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s: %s\n\nPassages:\n", sort.subject, kind)
	for _, p := range passages {
		fmt.Fprintf(&sb, "- %s\n", p)
	}
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, sort.settle, sb.String(), float32(attempt)*retryTempBump, factFindingMaxTokens)
	}, func(content string) (ProposedFact, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return ProposedFact{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return ProposedFact{}, fmt.Errorf("parse JSON object: %w", err)
		}
		fact, evidence := textField(result, sort.factKey), trimQuote(textField(result, "evidence"))
		if fact == "" || evidence == "" {
			return ProposedFact{}, nil
		}
		// The kind stays the one asked about: asked to name it again, the
		// model renamed "adept" to "vess's father".
		return ProposedFact{Kind: kind, Fact: fact, Evidence: evidence}, nil
	})
}

// trimQuote strips the space and quote marks a model wraps a quotation in.
func trimQuote(s string) string {
	return strings.Trim(strings.TrimSpace(s), "\"“”‘’ ")
}

// QuotedIn returns the index of the first of passages containing evidence
// word for word, -1 when none does - a fact whose quote isn't in the text
// it was shown is not proposed.
func QuotedIn(evidence string, passages []string) int {
	if evidence == "" {
		return -1
	}
	for i, p := range passages {
		if strings.Contains(p, evidence) {
			return i
		}
	}
	return -1
}

// notFactKinds are answers to "which kind" that name none: ordinary
// people, or the model saying it couldn't tell.
var notFactKinds = map[string]bool{
	"human": true, "humanity": true, "people": true, "person": true, "mortal": true, "man": true,
	"kind": true, "specie": true, "species": true, "race": true, "creature": true, "being": true,
}

var factKindArticle = regexp.MustCompile(`^(?:the|a|an) `)

// FactKind is the key a proposed fact is filed under for the model's
// kind: lower case, without an article, its last word singular ("the Deep
// Dwarves" -> "deep dwarf"). "" for what is no kind - humans, "unknown
// species", a description rather than a name (more than three words).
func FactKind(kind string) string {
	kind = factKindArticle.ReplaceAllString(strings.ToLower(oneLine(kind)), "")
	words := strings.Fields(kind)
	if len(words) == 0 || len(words) > 3 || strings.Contains(kind, "unknown") || strings.ContainsAny(kind, "()") {
		return ""
	}
	last := words[len(words)-1]
	switch {
	case strings.HasSuffix(last, "ves"):
		last = strings.TrimSuffix(last, "ves") + "f"
	case strings.HasSuffix(last, "s") && !strings.HasSuffix(last, "ss") && len(last) > 3:
		last = strings.TrimSuffix(last, "s")
	}
	words[len(words)-1] = last
	if notFactKinds[last] {
		return ""
	}
	return strings.Join(words, " ")
}
