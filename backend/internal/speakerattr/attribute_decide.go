package speakerattr

import (
	"context"
	"fmt"
	"log"
	"maps"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/namekey"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// AttributeOptions are experimental ways of asking for a chapter's
// speakers, benched against the default through the attribution preview
// (attreval.py --seed / --decide). The zero value is what the app runs.
type AttributeOptions struct {
	// Seed settles the lines the speech-tag rules already decide before
	// the model sees the chapter (seedFromTags): shown as settled, like a
	// reader's correction, so the turns around them follow them, and never
	// asked.
	Seed bool
	// Decide asks each line by reading the model's choice over a short
	// list of candidates (attributeBatchDecide) instead of generating a
	// name.
	Decide bool
	// NoUnknown never offers "Unknown": the prompt asks for the likeliest
	// speaker instead (noUnknownSystemPrompt) and decide's candidates
	// leave it out.
	NoUnknown bool
	// RedecideBelow (Decide only; 0 for off) decides the chapter a second
	// time with every line the first pass decided at or above this
	// confidence settled, so each line left is chosen again with settled
	// answers on both sides of it (attributeRedecided).
	RedecideBelow float64
	// RedecideRounds is how many times RedecideBelow decides the unsure
	// lines again (0 means 1), each round settling what the last decided
	// confidently.
	RedecideRounds int
	// SeedOnly returns just the lines Seed settles, asking the model
	// nothing - for scoring the speech-tag rules on their own.
	SeedOnly bool
	// PrevNext notes under every asked line the nearest known speaker
	// before and after it in the batch.
	PrevNext bool
	// TaggedSeeds prints a seeded line as "12 (tagged: Mira)" rather than
	// as a reader-confirmed "12 (Mira)".
	TaggedSeeds bool
	// SeedInReply (Decide only) settles the lines Seed would, but keeps
	// them in the prompt and the reply as plain decide has them: each is
	// shown as "(who?)" and its answer written into the reply ahead of
	// the next question (ParagraphInput.Forced), costing no decode step.
	// Seed's own way of settling them - as context, absent from the
	// reply - lost turn-taking on the untagged lines around them (8.2% of
	// those wrong under decide, 10.9% under seed + decide, 2026-10-17).
	SeedInReply bool
}

// seedFromTags returns paragraphs with Known set on every dialogue line
// RefineWithSpeechTags gives a speaker when no line has one yet - a named
// tag, an action beat, a continuation of the same paragraph's or an
// unclosed speech. Reader-set lines keep theirs and count as answers for
// the continuation rules.
func seedFromTags(paragraphs []ParagraphInput, roster Roster) []ParagraphInput {
	resolver := NewSpeakerNameResolver(roster.Names, roster.Aliases).WithTitles(roster.World).WithFirstPerson(roster.FirstPerson)
	start := make(map[int]string, len(paragraphs))
	for _, p := range paragraphs {
		if p.IsQuote && !p.ContextOnly {
			start[p.Idx] = p.Known
		}
	}
	seeded, _ := RefineWithSpeechTags(paragraphs, start, resolver)
	out := append([]ParagraphInput(nil), paragraphs...)
	n := 0
	for i, p := range out {
		if name := seeded[p.Idx]; p.IsQuote && !p.ContextOnly && p.Known == "" && !isSentinel(name) {
			out[i].Known, out[i].Seeded = name, true
			n++
		}
	}
	log.Printf("speakerattr: seeded %d of %d dialogue lines from speech tags", n, len(start))
	return out
}

// decideMinMass is how much of the model's next-token distribution a
// line's candidates must hold for attributeBatchDecide to keep its pick:
// below it the model wanted to name someone not on the list, and the line
// is asked again by generation.
const decideMinMass = 0.5

// decideWindowBefore/After is how far around a line attributeBatchDecide
// looks for names mentioned in the text, as candidates beside the scene's
// cast.
const (
	decideWindowBefore = 4
	decideWindowAfter  = 2
)

// decideCandidate is one option a line is asked to choose among.
type decideCandidate struct {
	name     string
	role     bool
	pronouns string
}

// option is c written as a reply line's answer, newline included - the
// newline ends every option the same way, so no name is a prefix of
// another ("Sid" beside "Sidney").
func (c decideCandidate) option() string {
	switch {
	case c.role:
		return " " + c.name + " [role]\n"
	case c.pronouns != "":
		return " " + c.name + " | " + c.pronouns + "\n"
	}
	return " " + c.name + "\n"
}

// lineCandidates are the speakers a line at lines[i] is asked to choose
// among: the first-person narrator, who its scene has present, speakers
// already settled and names mentioned near it, then Unknown.
func lineCandidates(roster Roster, lines []ParagraphInput, i int) []decideCandidate {
	var out []decideCandidate
	seen := map[string]bool{}
	add := func(name string) {
		k := namekey.Fold(name)
		if name == "" || isSentinel(name) || seen[k] {
			return
		}
		seen[k] = true
		c := decideCandidate{name: name, role: roster.Roles[name]}
		if !c.role {
			c.pronouns, _, _ = strings.Cut(roster.Pronouns[name], "/")
		}
		out = append(out, c)
	}
	add(roster.FirstPerson)
	for _, n := range roster.scenePresent(lines[i : i+1]) {
		add(n)
	}
	window := lines[max(0, i-decideWindowBefore):min(len(lines), i+decideWindowAfter+1)]
	for _, p := range window {
		if p.IsQuote {
			add(p.Known)
		}
	}
	for _, n := range relevantNames(Roster{Names: roster.Names, Aliases: roster.Aliases}, window) {
		add(n)
	}
	if !roster.noUnknown {
		out = append(out, decideCandidate{name: "Unknown"})
	}
	return out
}

// attributeBatchDecide is attributeBatch answered by choice instead of
// generation: the same prompt, and a reply built one line at a time from
// that line's candidates (lineCandidates), each read off the model's
// logits and written in before the next is asked. Lines where the
// candidates held too little of the model's distribution (decideMinMass)
// are asked again by attributeBatch, with the chosen lines shown as
// settled. A backend with no decide call, or a call the worker refuses,
// falls back to attributeBatch for the whole batch.
func (c *Client) attributeBatchDecide(ctx context.Context, bookTitle, chapterTitle string, roster Roster, lines []ParagraphInput, answer map[int]bool) (map[int]Attributed, error) {
	if len(answer) == 0 {
		return map[int]Attributed{}, nil
	}
	decider, ok := c.llm.(llmDecider)
	if !ok {
		return c.generateWithForced(ctx, bookTitle, chapterTitle, roster, lines, answer)
	}
	var (
		asked []int
		cands [][]decideCandidate
		steps []ttsproto.LLMDecideStep
	)
	forced := map[int]Attributed{}
	var pending strings.Builder // forced answers written ahead of the next question
	for i, p := range lines {
		if !answer[p.Idx] {
			continue
		}
		if p.Forced != "" {
			fc := decideCandidate{name: p.Forced, role: roster.Roles[p.Forced]}
			if !fc.role {
				fc.pronouns, _, _ = strings.Cut(roster.Pronouns[p.Forced], "/")
			}
			fmt.Fprintf(&pending, "%d:%s", p.Idx, fc.option())
			forced[p.Idx] = Attributed{Speaker: fc.name, Role: fc.role, Pronouns: NormalizePronouns(fc.pronouns)}
			continue
		}
		cs := lineCandidates(roster, lines, i)
		step := ttsproto.LLMDecideStep{Prefix: pending.String() + fmt.Sprintf("%d:", p.Idx)}
		pending.Reset()
		for _, c := range cs {
			step.Options = append(step.Options, c.option())
		}
		asked, cands, steps = append(asked, p.Idx), append(cands, cs), append(steps, step)
	}
	if len(steps) == 0 {
		return forced, nil
	}
	decisions, err := decider.LLMDecide(ctx, roster.attributionSystemPrompt(), c.attributionUserPrompt(bookTitle, chapterTitle, roster, lines, answer), steps)
	if err != nil {
		log.Printf("speakerattr: attribution decide failed (%v), generating instead", err)
		return c.generateWithForced(ctx, bookTitle, chapterTitle, roster, lines, answer)
	}
	out := make(map[int]Attributed, len(asked)+len(forced))
	maps.Copy(out, forced)
	unsure := map[int]bool{}
	for i, d := range decisions {
		if d.Mass < decideMinMass {
			unsure[asked[i]] = true
			continue
		}
		pick := cands[i][d.Choice]
		if d.Choice < len(d.Probs) {
			roster.confidence.record(asked[i], d.Probs[d.Choice])
		}
		out[asked[i]] = Attributed{Speaker: pick.name, Role: pick.role, Pronouns: NormalizePronouns(pick.pronouns)}
	}
	log.Printf("speakerattr: attribution decide: %d lines chosen, %d asked again", len(out), len(unsure))
	if len(unsure) == 0 {
		return out, nil
	}
	settled := append([]ParagraphInput(nil), lines...)
	for i, p := range settled {
		if a, ok := out[p.Idx]; ok && !isSentinel(a.Speaker) {
			settled[i].Known = a.Speaker
		}
	}
	more, err := c.attributeBatch(ctx, bookTitle, chapterTitle, roster, settled, unsure)
	if err != nil {
		return out, nil
	}
	for idx, a := range more {
		out[idx] = a
	}
	return out, nil
}

// forcedFromSeeds turns seedFromTags' settled lines back into asked ones
// whose answer decide writes rather than asks (AttributeOptions.
// SeedInReply).
func forcedFromSeeds(paragraphs []ParagraphInput) []ParagraphInput {
	for i, p := range paragraphs {
		if p.Seeded {
			paragraphs[i].Forced, paragraphs[i].Known, paragraphs[i].Seeded = p.Known, "", false
		}
	}
	return paragraphs
}

// generateWithForced is attributeBatch for a decide batch that can't be
// decided: lines with a Forced answer are shown settled and keep it, the
// rest are generated.
func (c *Client) generateWithForced(ctx context.Context, bookTitle, chapterTitle string, roster Roster, lines []ParagraphInput, answer map[int]bool) (map[int]Attributed, error) {
	settled := append([]ParagraphInput(nil), lines...)
	ask := map[int]bool{}
	out := map[int]Attributed{}
	for i, p := range settled {
		if !answer[p.Idx] {
			continue
		}
		if p.Forced != "" {
			settled[i].Known = p.Forced
			out[p.Idx] = Attributed{Speaker: p.Forced, Role: roster.Roles[p.Forced]}
			continue
		}
		ask[p.Idx] = true
	}
	more, err := c.attributeBatch(ctx, bookTitle, chapterTitle, roster, settled, ask)
	if err != nil {
		return nil, err
	}
	maps.Copy(out, more)
	return out, nil
}
