package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// DescribeChapter is a separate, single-purpose sibling to AttributeChapter:
// it tags which narration paragraphs describe a character's appearance or
// personality, asking nothing about who's speaking. This is deliberately
// its own call rather than a "describes" field folded into AttributeChapter
// itself - an earlier version tried exactly that (one combined judgment per
// line) and it measurably hurt precision: Qwen3-4B-Instruct-2507 tagged
// 30-52% of paragraphs as "describing" someone, overwhelmingly narration
// that merely mentioned a name rather than actually describing the person,
// even after tightening the combined prompt with explicit negative
// examples. Isolating description-tagging into its own call, and batching
// it much more finely (see describeBatchParagraphs) than AttributeChapter's
// own 40, fixed that: the combination of judging two things per line across
// a large batch was making the model lapse into blanket-tagging every line
// touching a salient character rather than exercising real per-line
// judgment - a "streak" bias that tracked batch size, not prompt wording.
//
// describeSystemPrompt is a narrower version of AttributeChapter's own
// (now-removed) "describes" rules, asking ONLY for description-tagging.
const describeSystemPrompt = `You are a literary analysis assistant. Given numbered lines from a novel (a mix of narration and spoken dialogue), find the rare lines whose NARRATION physically or personality-describes a specific named person.
Reply with ONLY a JSON array, no other text: [{"idx": <line number>, "describes": ["<name>", ...]}, ...] - include an entry ONLY for a line that clears the bar below; omit every other line entirely. This should be a short list - most lines belong in neither.
Rules:
- Only a narration line (not spoken dialogue) can ever belong in this list.
- This is for a PERSON's own appearance or personality only - never a place, ship, vehicle, object, item, card, spell, energy/magic type, organization, or a group of people, even when a line is centered on one of those. Only ever list individual named (or nameable) people.
- The bar is high: a line belongs in the list only if, read on its own with no other context, it would teach someone what a specific person looks like or is generally like as a person - their build, face, age impression, clothing, a distinctive feature, or their characteristic temperament. A line that names a character while narrating the plot around them - what they did, decided, fought, crafted, said, or felt in this moment - does NOT belong, no matter how long it is or how much it's "about" them. This applies just as much to a first-person narrator, who is the grammatical subject of nearly every line without most of those lines describing them at all.
  Examples that DO belong: "Edward was rail thin but tall, with a well-manicured mustache." -> describes Edward. "Hadiza was a darker-skinned woman with hair braided down her back, her face wrinkled from a life at sea." -> describes Hadiza. "Marcus never raised his voice, but everyone in the room straightened up when he spoke." -> describes Marcus.
  Examples that must NOT be included, despite naming someone: "Jake walked to the door and knocked." "I spent hours crafting the new card, then tested it against a dummy." "Yina grabbed her warhammer and led the charge." "The Yellow Squad gathered near the door." "Bend Like Water granted a passive boost to agility." "The ship's cargo hold smelled of salt and rope."
- A person referred to ONLY by an unnamed role/description ("a middle-aged man," "the one in the centre," "a woman in the crowd") never belongs in "describes," no matter how much physical detail follows or how consistently the passage keeps referring back to them - "middle-aged man" is a role, not a name, and heavy detail doesn't turn it into one. Example that must NOT be included even though it's clearly a physical description: "The one in the centre, hefting the lantern, was a middle-aged man. He looked as if he were carved of wood, gnarled, with notches and scars littering his bald head and face." - leave this out entirely unless a real proper name is given somewhere in the line itself.
- A paragraph explaining what an ITEM, CARD, or SPELL does - its abilities, mechanics, or flavor text, or how it affects someone using/wearing it - is NOT a description of that person, even when the effect happens to their own body or the flavor text mentions an unnamed generic figure ("a man dove into the ocean..."). A card/item's own name (often its own short line, sometimes immediately followed by a "Level N" or power-tier line) is never a person - never put an item/card name in "describes" under any circumstance.
- The physical/personality detail has to be the actual point of the line, not a two- or three-word aside tacked onto a dialogue tag or action beat - "she said with a grin" or "he said, rapier in hand" *by themselves* are still real dialogue/action, not a description, and don't qualify on their own. If a line is fundamentally reporting an action or a line of dialogue and only incidentally brushes against how they looked while doing it, leave it out.
- The person's own name (or an unambiguous designation of them, like "the captain" right after they were named) must actually appear in THIS line's own text. Never name someone in "describes" because they're relevant from earlier context, because they're in "Known characters," or because the scene is generally about them - only because this specific line names them.
- A line starting with a character's name is not, by itself, a signal of anything - what matters is the verb and what follows. "Hadiza was/looked/had ..." stating an inherent trait is a description; "Hadiza waited/joined/grabbed/fought/turned/said ..." reporting an event is not, even though both start the same way and both are "about" Hadiza. Test each line by its own verb, not its opening word.
- Name whoever a line is actually about, using their short proper name - reused exactly from "Known characters" when they're already known. A line can describe more than one person at once, in which case list all of them. A brand-new person can be named this way even if they aren't in "Known characters" yet, as long as the text gives their name somewhere nearby; never invent a name or use a pronoun/generic label if none is ever given - leave them out instead.
- Expect this list to be very short - in a typical chapter, only a handful of lines (often just the first time a character is properly introduced) ever qualify. If you're unsure whether a line clears the bar, leave it out.
- Each entry's "idx" must be copied exactly from the number printed right before that line in "Lines:" - never shift it to a neighboring line.`

// describeBatchParagraphs is DescribeChapter's own, much smaller batch size
// than AttributeChapter's maxBatchParagraphs (40) - a first isolated-pass
// attempt at 40 still showed the same "streak" bias described above (once a
// character became salient anywhere in the batch, the model kept
// blanket-tagging nearly every later line mentioning them, including plain
// dialogue the prompt explicitly forbids), just with a different trigger
// than the folded prompt's. Benchmarked down from 40 to 10 to 5 against
// real chapters (see backend/CLAUDE.md's "Speaker attribution" section):
// precision rose from ~6% to ~25-37% to ~80% as the batch shrank, giving
// the model far less room for a streak to run before a fresh batch
// boundary forces independent judgment again.
const describeBatchParagraphs = 5

// DescribeChapter tags chapterTitle's narration paragraphs that describe a
// character, batching in groups of describeBatchParagraphs.
// knownCharacters (already-seen character names) is passed in every batch's
// prompt and grows as each batch's own newly-seen names are folded in, the
// same reuse-a-consistent-name reasoning as AttributeChapter - a character
// can be discovered this way, through how they're described, before they
// ever speak. Returns a map from paragraph Idx to the character name(s), if
// any, that paragraph describes - a paragraph with no entry describes no
// one (the common case).
//
// shouldPause/remaining/err follow AttributeChapter's own contract exactly
// (see its doc comment) - nil-safe shouldPause, checked between batches
// only, remaining holds whatever wasn't reached (by a pause or a genuine
// failure) so the caller can persist real progress and requeue just the
// rest. canonicalizeDescribes runs once, at the end, over whatever this
// call actually completed - see canonicalizeSpeakerNames' own doc comment
// for the same best-effort, reader-correctable trade-off on cross-batch/
// chapter/book name variants.
func (c *Client) DescribeChapter(ctx context.Context, bookTitle, chapterTitle string, knownCharacters []string, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int][]string, remaining []ParagraphInput, err error) {
	out = make(map[int][]string, len(paragraphs))
	known := append([]string(nil), knownCharacters...)

	for start := 0; start < len(paragraphs); start += describeBatchParagraphs {
		if start > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphs[start:]
			break
		}
		end := start + describeBatchParagraphs
		if end > len(paragraphs) {
			end = len(paragraphs)
		}
		batch := paragraphs[start:end]

		raw, err := c.describeBatch(ctx, bookTitle, chapterTitle, known, batch)
		if err != nil {
			return canonicalizeDescribes(knownCharacters, out), paragraphs[start:], fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, err)
		}
		for idx, names := range raw {
			cleaned := cleanDescribes(names)
			if len(cleaned) == 0 {
				continue
			}
			out[idx] = cleaned
			for _, name := range cleaned {
				known = addKnown(known, name)
			}
		}
	}
	return canonicalizeDescribes(knownCharacters, out), remaining, nil
}

// canonicalizeDescribes is canonicalizeSpeakerNames' counterpart for
// DescribeChapter's own map[int][]string shape - same whole-word-merge
// heuristic (see canonMapFromPool), just resolving every name in each
// paragraph's own list instead of one Speaker value, and re-deduping
// afterward (two raw names can canonicalize to the same longer name).
func canonicalizeDescribes(knownCharacters []string, raw map[int][]string) map[int][]string {
	pool := map[string]bool{}
	for _, k := range knownCharacters {
		pool[k] = true
	}
	for _, names := range raw {
		for _, n := range names {
			pool[n] = true
		}
	}
	canon := canonMapFromPool(pool)

	out := make(map[int][]string, len(raw))
	for idx, names := range raw {
		seen := make(map[string]bool, len(names))
		var resolved []string
		for _, n := range names {
			c := n
			if cn, ok := canon[n]; ok {
				c = cn
			}
			if !seen[c] {
				seen[c] = true
				resolved = append(resolved, c)
			}
		}
		out[idx] = resolved
	}
	return out
}

func (c *Client) describeBatch(ctx context.Context, bookTitle, chapterTitle string, knownCharacters []string, batch []ParagraphInput) (map[int][]string, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	if len(knownCharacters) > 0 {
		fmt.Fprintf(&user, "Known characters: %s\n", strings.Join(knownCharacters, ", "))
	}
	user.WriteString("\nLines:\n")
	for i, p := range batch {
		if i > 0 && !p.Inline {
			user.WriteString("\n")
		}
		fmt.Fprintf(&user, "%d: %s\n", p.Idx, oneLine(p.Text))
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}

	return generateAndParse(ctx, c, describeSystemPrompt, user.String(), 0, jsonPassMaxTokens, parseDescribeItems)
}

type describeItem struct {
	Idx       int      `json:"idx"`
	Describes []string `json:"describes"`
}

// parseDescribeItems extracts the JSON array from content, tolerating
// models that wrap it in prose or a markdown code fence despite the system
// prompt asking for JSON only - the same tolerance parseAttributions
// applies for AttributeChapter's own response.
func parseDescribeItems(content string) (map[int][]string, error) {
	start := strings.IndexByte(content, '[')
	end := strings.LastIndexByte(content, ']')
	if start == -1 || end == -1 || end < start {
		return nil, fmt.Errorf("no JSON array found in response: %s", truncate(content, 200))
	}
	var items []describeItem
	if err := json.Unmarshal([]byte(content[start:end+1]), &items); err != nil {
		return nil, fmt.Errorf("parse JSON array: %w", err)
	}
	out := make(map[int][]string, len(items))
	for _, it := range items {
		if len(it.Describes) == 0 {
			continue
		}
		out[it.Idx] = it.Describes
	}
	return out, nil
}
