package speakerattr

import "strings"

// noUnknownSystemPrompt is systemPrompt with "Unknown" taken away as an
// answer (AttributeOptions.NoUnknown): every rule that fell back to it
// asks for the likeliest speaker instead.
var noUnknownSystemPrompt = mustReplaceAll(systemPrompt, []string{
	"16: Unknown\n", "",
	`, or use "Unknown" if that can't be determined from context - never invent`, `, or else whichever of them is likeliest - never invent`,
	`It always gets either a real character name, a role label (see below), or "Unknown".`, `It always gets either a real character name or a role label (see below).`,
	`, or use exactly "Unknown" if the name genuinely can't be determined from context.`, `, or the likeliest speaker when the text never names them.`,
	`, and if that's genuinely unclear, use "Unknown" rather than defaulting to whoever's name appears in the text.`, `, and if that's unclear, name the likeliest speaker from the turn-taking and who is present - never just whoever's name appears in the text.`,
	` Only fall back to "Unknown" when even a generic type can't be determined.`, ` When even a generic type can't be determined, name the likeliest speaker.`,
	`- If a dialogue line's speaker is genuinely unclear from context, use exactly "Unknown" rather than guessing.`, `- Never answer "Unknown": when a line's speaker is unclear, name the likeliest one - from the turn-taking, who is present in the scene, and who the line answers or is addressed to.`,
	` Never add pronouns after "Unknown" or a " [role]" label.`, ` Never add pronouns after a " [role]" label.`,
	` Still use "Unknown" if the match is genuinely ambiguous (e.g. the role could plausibly fit more than one known character, or nothing in "Known characters" matches it at all).`, ` If the match is genuinely ambiguous (the role could fit more than one known character, or nothing in "Known characters" matches it), use the role itself as a " [role]" label.`,
	`pick the one person the narration credits, or use "Unknown".`, `pick the one person the narration credits, or the likeliest of them.`,
})

// attributionSystemPrompt is the attribution system prompt for r's
// options.
func (r Roster) attributionSystemPrompt() string {
	p := systemPrompt
	if r.noUnknown {
		p = noUnknownSystemPrompt
	}
	if r.prevNext {
		p += "\n- Under a \"(who?)\" line, \"prev: Mira (10)\" and \"next: Tomas (15)\" name the nearest dialogue line before and after it whose speaker is already known - anchors for counting turns in the conversation around it. They are notes, not lines: never answer them."
	}
	if r.taggedSeeds {
		p += "\n- A dialogue line printed as \"12 (tagged: Mira): …\" has its speaker named by a speech tag beside it in the text - almost always right. Like a confirmed line, it is context and never answered."
	}
	return p
}

// mustReplaceAll applies each old/new pair of pairs to s once, panicking
// when an old string isn't in s - so a reworded systemPrompt can't
// silently leave an "Unknown" rule behind.
func mustReplaceAll(s string, pairs []string) string {
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(s, pairs[i]) {
			panic("speakerattr: noUnknownSystemPrompt: not in systemPrompt: " + pairs[i])
		}
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return s
}
