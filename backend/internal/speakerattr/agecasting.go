package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ageCastingSystemPrompt rewrites a character's casting for another age.
// It changes as little as it can: in the listening test behind
// narration.StageVoice (2026-10-10, Will Oh's casting at 10, 20, 40 and
// 70) the castings that came out as the same character at another age
// differed from his own only in the age clause and the sentence on pitch,
// timbre and pace, with the accent and manner sentences left word for
// word.
const ageCastingSystemPrompt = `You adjust a voice-casting instruction for a character in an audiobook so it describes the same person at a different age. A text-to-speech voice-design model will read your instruction and nothing else.

You are given the character's current instruction and the age to cast them at, sometimes with notes on the story's world. Reply with a single JSON object and nothing else:

{"instruct": "..."}

Rules:
- Keep the gender, accent, and every sentence about manner, personality and speaking style exactly as written, word for word.
- Change the opening clause to the age group for the new age: "a young boy" or "a young girl" (under 12), "a teenage boy" or "a teenage girl" (12-17), "a young man" or "a young woman" (18-29), "a man in his thirties" or "a woman in her forties" and so on (30-59), "an elderly man" or "an elderly woman" (60 and over).
- Rewrite only the wording about pitch, timbre and pace so it fits that age: a child's voice is high, thin and unbroken; a teenager's is light and has not settled; a young adult's is clearly lower and fuller than a teenager's, a settled adult voice; a middle-aged voice is fuller and more grounded, with a measured pace; an elderly voice is drier, thinner and weathered, with a slower pace. Keep any lasting quality the instruction already gives the voice (raspy, nasal, booming, soft) unless it only makes sense at the old age.
- Never mention the character's name, the story, or that the voice has changed. Describe the voice at this age as if it were the only one.
- If the instruction names a kind of being (a kobold, an elf), keep that word in the opening clause: "a middle-aged kobold woman", "a young elf".
- The age groups above are a human's. "World notes", when given, are facts about this story's world, and one that says how long the character's kind lives or how it ages decides the age group instead: it is where the new age falls in their own kind's life, so thirteen is "a middle-aged ..." for a kind that is middle-aged in its teens, and ninety is "a young ..." for one that lives a thousand years. Cast the pitch, timbre and pace of that age group, not of a human of that many years.
- With no such note, if the instruction describes someone who is not human or does not age as a human does, change only what the new age would plausibly change.`

// AgeCasting rewrites instruct - a character's voice casting
// (CharacterizeVoice's instruct) - for the same character at about age
// years old: the instruction a store.CharacterStage is voiced from.
//
// notes are the world facts the casting brings up (MatchWorldNotes over
// it), shown as "World notes": without the one on how kobolds age, a
// kobold of thirteen - middle-aged - was cast "a teenage girl".
func (c *Client) AgeCasting(ctx context.Context, instruct string, age int, notes []string) (string, error) {
	user := fmt.Sprintf("Current instruction:\n%s\n%s\nCast them at about %d years old.", strings.TrimSpace(instruct), worldNotesSection(notes), age)
	return retryParse(func(attempt int) (string, error) {
		// Greedy like casting itself, and like it sampled on a retry, which
		// would otherwise repeat the reply that failed.
		temp := characterizeTemp
		if attempt > 0 {
			temp = float32(attempt) * retryTempBump
		}
		return c.generate(ctx, ageCastingSystemPrompt, user, temp, characterizeMaxTokens)
	}, func(content string) (string, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return "", fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var result struct {
			Instruct string `json:"instruct"`
		}
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return "", fmt.Errorf("parse JSON object: %w", err)
		}
		out := strings.TrimSpace(result.Instruct)
		if words := strings.Fields(out); len(words) < minCharacterizeWords {
			return "", fmt.Errorf("llm returned an implausibly short casting (%q) - likely truncated mid-answer", out)
		}
		return out, nil
	})
}

// ageByCastingSystemPrompt is ageCastingSystemPrompt for a time jump: the
// age isn't given, only how many years have passed, and most voices don't
// change - an adult a few years older sounds the same - so the model says
// first whether this one does.
const ageByCastingSystemPrompt = `You adjust a voice-casting instruction for a character in an audiobook after time has passed in the story. A text-to-speech voice-design model will read your instruction and nothing else.

You are given the character's current instruction and how many years have passed, sometimes with notes on the story's world. Reply with a single JSON object and nothing else:

{"was": <their age before, in years>, "age": <their age now, in years>, "changed": true or false, "instruct": "..."}

"was": estimate their age from the instruction's opening clause - a young boy or girl about 8, a teenage boy or girl about 15, a young man or woman about 24, "in his thirties" 35 and so on, middle-aged about 48, elderly about 70; use a stated age when there is one. "age" is "was" plus the years passed.

"changed": true only when a listener would hear the difference - when "age" falls in a different age group than "was": a young child (under 12), a teenager (12-17), a young adult (18-29), an adult (30-59), elderly (60 and over). An adult who is a few years older sounds the same: false. Someone who is not human, or whom the instruction calls ageless, ancient or long-lived, does not change: false - unless a World note says how their kind ages.

"World notes", when given, are facts about this story's world. One that says how long the character's kind lives or how it ages replaces the human ages above for them: read "was" from the opening clause in their own kind's years (a young adult of a kind that is adult within a few years and middle-aged in its teens is about 5), add the years, and take the age group from where "age" falls in their kind's life - "changed" is true when that group differs. A note that their kind does not age: false.

"instruct": when "changed" is false, an empty string. Otherwise the instruction rewritten for the new age:
- Keep the gender, accent, and every sentence about manner, personality and speaking style exactly as written, word for word.
- Change the opening clause to the new age group: "a teenage boy" or "a teenage girl", "a young man" or "a young woman", "a man in his thirties" or "a woman in her forties" and so on, "an elderly man" or "an elderly woman". If the instruction names a kind of being (a kobold, an elf), keep that word: "a middle-aged kobold woman".
- Rewrite only the wording about pitch, timbre and pace so it fits that age group: a teenager's voice is light and has not settled; a young adult's is clearly lower and fuller than a teenager's, a settled adult voice; a middle-aged voice is fuller and more grounded, with a measured pace; an elderly voice is drier, thinner and weathered, with a slower pace. Keep any lasting quality the instruction already gives the voice (raspy, nasal, booming, soft).
- Never mention the character's name, the story, or that the voice has changed. Describe the voice at this age as if it were the only one.`

// AgedCasting is AgeCastingBy's answer.
type AgedCasting struct {
	// Age is about how old the character is after the years have passed.
	Age int
	// Changed says the character now sounds different - their age group
	// changed. Instruct is their casting rewritten for Age when it did,
	// and empty when it didn't.
	Changed  bool
	Instruct string
}

// AgeCastingBy rewrites instruct - a character's voice casting - for the
// same character years later, when that changes how they sound: what a
// store.TimeJump asks of every character it passes over. Most come back
// unchanged.
// notes are the world facts the casting brings up, as for AgeCasting.
func (c *Client) AgeCastingBy(ctx context.Context, instruct string, years int, notes []string) (AgedCasting, error) {
	user := fmt.Sprintf("Current instruction:\n%s\n%s\nYears passed: %d", strings.TrimSpace(instruct), worldNotesSection(notes), years)
	return retryParse(func(attempt int) (string, error) {
		temp := characterizeTemp
		if attempt > 0 {
			temp = float32(attempt) * retryTempBump
		}
		return c.generate(ctx, ageByCastingSystemPrompt, user, temp, characterizeMaxTokens)
	}, func(content string) (AgedCasting, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return AgedCasting{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var result struct {
			Age      float64 `json:"age"`
			Changed  bool    `json:"changed"`
			Instruct string  `json:"instruct"`
		}
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return AgedCasting{}, fmt.Errorf("parse JSON object: %w", err)
		}
		out := AgedCasting{Age: int(result.Age), Changed: result.Changed, Instruct: strings.TrimSpace(result.Instruct)}
		if !out.Changed {
			out.Instruct = ""
			return out, nil
		}
		if out.Age < 1 {
			return AgedCasting{}, fmt.Errorf("llm gave no age for a changed casting: %s", truncate(content, 200))
		}
		if words := strings.Fields(out.Instruct); len(words) < minCharacterizeWords {
			return AgedCasting{}, fmt.Errorf("llm returned an implausibly short casting (%q) - likely truncated mid-answer", out.Instruct)
		}
		return out, nil
	})
}

// worldNotesSection renders notes ("key: fact" lines, see MatchWorldNotes)
// for the age-casting prompts: "" with none.
func worldNotesSection(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nWorld notes:\n")
	for _, n := range notes {
		fmt.Fprintf(&sb, "- %s\n", oneLine(n))
	}
	return sb.String()
}
