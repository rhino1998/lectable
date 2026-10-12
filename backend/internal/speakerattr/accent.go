package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"
)

// accentMaxTokens bounds the accent decision's reply: a short JSON object,
// plus a thinking model's trace.
const accentMaxTokens = 1536

// accentSystemPrompt asks one thing of a character's casting material:
// what accent they speak with, and the sentence that says so. It is its
// own call because the casting call, asked for everything at once, did not
// hold to its own accent rules: shown "Ingrid, a Danish archeologist" it
// cast her British in 4 of 6 runs and Scylla, "the insane Greek woman",
// British in 6 of 6, drawn by the setting's accent; and a sentence that
// only held the word ("there was Miryam's Italian accent" - a stranger's,
// which Miryam had described; "northern Russia") became the character's
// accent. Here the reply has to name whose the evidence is before it names
// an accent (2026-10-11).
const accentSystemPrompt = `You decide the accent an audiobook character speaks English with. You are given the material gathered about one character: the setting, their dialogue, narration about them, and sentences found by searching the book. Decide their accent and nothing else.

Reply with ONLY a JSON object, no other text:
{"evidence": "...", "whose": "...", "basis": "...", "accent": "..."}

"evidence": the one sentence or fact the accent rests on, quoted exactly from the material ("Unfortunately, outside he bumped into the second woman, Ingrid, a Danish archeologist."), or the idiom counts, or "none" when it rests on the setting alone.
"whose": who that sentence is about - "this character", or the other person it describes. Read it closely: sentences found by search contain the character's name and the word, but are often about someone else.
"basis": one of "origin", "described accent", "dialect", "idiom", "kind", "world note", "setting".
"accent": one named accent, in a word or two: "Danish", "Greek", "Scottish", "General American", "Received Pronunciation", "Southern American", "Italian".

Decide in this order, stopping at the first that applies to THIS character:
1. origin - the material gives them a nationality or a homeland ("a Danish archeologist", "the Greek woman", "born in Naples"), or they speak a language among their own people or under stress (muttering, swearing, praying, singing to themselves in Danish). Their accent is that country's. It stays theirs wherever they live now and whoever they travel with, however long ago they left and however old or powerful they are - unless the narration says they speak without an accent or have lost it. A language they learned, study, translate or talk about is not an origin.
2. described accent - narration describes how they themselves sound ("her thick Scottish burr", "his drawl").
3. dialect - dialect or foreign words written into their own lines (dinnae, y'all, ja, nein).
4. world note - a "World notes" line gives the accent of a kind of being the material establishes them as.
5. idiom - "Idiom words across all their lines" lean clearly to one national English: British (mate, bloody, arse, bloke), Scottish (aye, wee, lass, bairn), American (dude, gotten, mom). One stray word is not an accent.
6. kind - a non-human kind the material establishes them as, with no signal above: dwarf - Scottish; elf - Received Pronunciation; orc or goblin - Cockney; giant or troll - West Country; dragon or demon - Received Pronunciation; fairy - Irish; robot, AI or construct - the setting's accent.
7. setting - otherwise the "main characters' accent" of the setting notes (the first, when it lists several), or General American when there are none.

A sentence is evidence only when it is about this character's own origin or own way of speaking. It is NOT theirs when:
- it is an accent they mentioned, described, recognized, imitated or put on ("the German accent Mara had mentioned", "there was Mara's Italian accent" said of a stranger Mara had described, "Mara said, adopting the man's accent");
- the nationality or accent belongs to someone else in the sentence, or to a thing or place ("northern Russia", "the Greek ages", "an Italian restaurant");
- someone wrongly believes or pretends it ("Mara, who thinks Tor is Italian").
If the only sentences are like these, they are not evidence: go on down the order.

Slang, swearing, rough grammar and being poor are not accents. Never answer "Southern American" unless the material places them in the American South or describes a drawl of their own.`

// AccentDecision is what the accent step decided for a casting.
type AccentDecision struct {
	// Accent is the accent's name ("Danish", "General American").
	Accent string `json:"accent"`
	// Evidence is the sentence it rests on, "none" for the setting's.
	Evidence string `json:"evidence"`
	// Whose says who Evidence is about, as the model read it.
	Whose string `json:"whose"`
	// Basis is which rule decided it ("origin", "setting", ...).
	Basis string `json:"basis"`
}

// decideAccent asks for the accent of the character whose casting material
// is material (CharacterizeVoice's user turn, lookup results included). A
// reply that names no accent is an error; the caller casts without one.
func (c *Client) decideAccent(ctx context.Context, material string) (AccentDecision, error) {
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, accentSystemPrompt, material, float32(attempt)*retryTempBump, accentMaxTokens)
	}, func(content string) (AccentDecision, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return AccentDecision{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var d AccentDecision
		if err := json.Unmarshal([]byte(value), &d); err != nil {
			return AccentDecision{}, fmt.Errorf("parse JSON object: %w", err)
		}
		d.Accent = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(d.Accent), "accent"))
		if d.Accent == "" || len(strings.Fields(d.Accent)) > 4 {
			return AccentDecision{}, fmt.Errorf("no accent named: %s", truncate(content, 200))
		}
		// Evidence about someone else decides nothing, whatever the reply
		// went on to name.
		if d.Basis != "setting" && d.Basis != "kind" && d.Basis != "world note" && d.Basis != "idiom" && !aboutThisCharacter(d.Whose) {
			return AccentDecision{}, fmt.Errorf("accent %q rests on a sentence about %q", d.Accent, d.Whose)
		}
		return d, nil
	})
}

// aboutThisCharacter reports whether an AccentDecision.Whose says the
// evidence is the character's own.
func aboutThisCharacter(whose string) bool {
	whose = strings.ToLower(strings.TrimSpace(whose))
	return whose == "" || strings.Contains(whose, "this character")
}

// accentSection is the casting turn's note of a decided accent.
func accentSection(d AccentDecision) string {
	return fmt.Sprintf("\nAccent (decided from the evidence, do not choose another): %s\n", d.Accent)
}

// castsAccent reports whether instruct names accent: every word of it, in
// order ("General American accent", "a Danish accent speaking English").
func castsAccent(instruct, accent string) bool {
	words := strings.Fields(accent)
	for i, w := range words {
		words[i] = regexp.QuoteMeta(w)
	}
	re, err := regexp.Compile(`(?i)\b` + strings.Join(words, `\W+`) + `\b`)
	return err == nil && re.MatchString(instruct)
}

// accentFor runs the accent step for a casting, answering nil when it
// fails: a casting without a decided accent is still a casting.
func (c *Client) accentFor(ctx context.Context, name, material string) *AccentDecision {
	d, err := c.decideAccent(ctx, material)
	if err != nil {
		log.Printf("speakerattr: accent for %s not decided, casting without: %v", name, err)
		return nil
	}
	return &d
}
