package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const classifyWordsSystemPrompt = `You are given words, and some phrases of two or three words, taken from the cast list of a novel. Each comes with some of the cast-list names that use it and sentences from the novel that use it. Say what each word is in this story's world. A reader will review your answers.

- "title": a rank, office or honorific that a person holds and is addressed by, written before their name ("Ser Raim", "Chieftain Eska", "Archmage Feor", "the Prognugator", "Wall Lord Ilvriss").
- "role": a job, occupation or class - what someone does, said of anyone who does it ("a baker", "the guards", "two soldiers", "an adventurer", "the cook").
- "race": a species, race or kind of being ("the Drake", "a Gnoll", "half-Elves", "Antinium", "the Drowned Folk").
- "nationality": where someone is from or which people they belong to - a nation, city, tribe, clan, order or faction, as a noun or an adjective ("the Pallassian soldiers", "a Terandrian", "Silverfang Gnolls", "the Molten Stone tribe").
- "place": the name of a place - a city, country, continent, region, building or world ("in Pallass", "the walls of Liscor", "King of Khelt", "the Blighted Lands").
- "name": a personal name, family name or nickname of one person or one family, alone or with their title ("King Itorin"); also the name of a thing, a ship or a group.
- "other": anything else - an ordinary word, an adjective, a number.

Judge by how the sentences use the word, not by what it means in our world: an invented word is as likely to be a race or a title as a name. When the sentences don't make it clear, answer "other".

Reply with a single JSON object and nothing else, one entry per word, in the order given:
{"words": [{"word": "<the word or phrase, as given>", "is": "title" | "role" | "race" | "nationality" | "place" | "name" | "other"}]}`

// classifySampleChars caps one sentence shown to ClassifyWords.
const classifySampleChars = 200

// ClassifiedWord is one of ClassifyWords' answers.
type ClassifiedWord struct {
	Word string
	// Class is WordTitle, WordRole, WordRace, WordNationality or WordPlace.
	Class string
}

// WordSample is one word for ClassifyWords: some names that use it and
// sentences of the text that do.
type WordSample struct {
	Word      string
	Names     []string
	Sentences []string
}

// ClassifyWords says which of words are titles, roles, races, nationalities or
// places of the story's world, judging from the names and sentences given with
// each. Words it calls a name or anything else are left out, as is any
// word it wasn't given.
func (c *Client) ClassifyWords(ctx context.Context, words []WordSample) ([]ClassifiedWord, error) {
	if len(words) == 0 {
		return nil, nil
	}
	given := map[string]string{}
	var user strings.Builder
	for _, w := range words {
		given[strings.ToLower(w.Word)] = w.Word
		names := "(none)"
		if len(w.Names) > 0 {
			names = strings.Join(w.Names[:min(len(w.Names), 5)], "; ")
		}
		fmt.Fprintf(&user, "Word: %s\nNames: %s\n", w.Word, names)
		for _, s := range w.Sentences {
			fmt.Fprintf(&user, "- %s\n", truncate(oneLine(s), classifySampleChars))
		}
		user.WriteString("\n")
	}
	if c.cfg.NoThink {
		user.WriteString("/no_think")
	}
	return retryParse(func(attempt int) (string, error) {
		return c.generate(ctx, classifyWordsSystemPrompt, user.String(), float32(attempt)*retryTempBump, 40*len(words)+100)
	}, func(content string) ([]ClassifiedWord, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return nil, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var result struct {
			Words []struct {
				Word string `json:"word"`
				Is   string `json:"is"`
			} `json:"words"`
		}
		if err := json.Unmarshal([]byte(value), &result); err != nil {
			return nil, fmt.Errorf("parse JSON object: %w", err)
		}
		var out []ClassifiedWord
		for _, w := range result.Words {
			word, ok := given[strings.ToLower(strings.TrimSpace(w.Word))]
			class := strings.ToLower(strings.TrimSpace(w.Is))
			if !ok || class != WordTitle && class != WordRole && class != WordRace && class != WordNationality && class != WordPlace {
				continue
			}
			delete(given, strings.ToLower(word))
			out = append(out, ClassifiedWord{Word: word, Class: class})
		}
		return out, nil
	})
}
