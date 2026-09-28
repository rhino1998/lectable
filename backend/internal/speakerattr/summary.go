package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// summaryMaxTokens bounds one summary reply - a few sentences, a handful of
// scenes and a cast list.
const summaryMaxTokens = 2048

// summaryWindowChars caps one summary call's chapter text (~6k tokens); a
// longer chapter is summarized in consecutive windows, merged (see
// SummarizeChapter).
const summaryWindowChars = 24000

// summarySystemPrompt is the per-chapter summary pass: what happens, where
// each scene is, and who is in the chapter - the shared notes the
// attribution, ambience and lighting passes read instead of each guessing
// from their own short span. It needs a model that doesn't invent (see
// backend/CLAUDE.md's model notes): Qwen3-4B's cast lists made up
// references and pronouns; Qwen3.6-35B-A3B's were clean.
const summarySystemPrompt = `You are preparing notes on one chapter of a novel for an audiobook production: a short summary, its scenes, and its cast. You will be given the book's known characters, possibly notes on the book's setting, and the chapter's numbered paragraphs.

Reply with a compact JSON object only, no other text, no markdown code fence:

{"summary": "...", "scenes": [{"start": <paragraph number>, "location": "...", "time": "...", "present": ["Name", ...]}], "cast": [{"name": "...", "role": false, "pronoun": "he", "refs": ["..."]}]}

- "summary": 2-4 sentences on what happens, naming the characters involved.
- "scenes": in order. A new scene starts where the story moves to a different place, jumps in time, or switches to other characters. "start" is the number printed before the scene's first paragraph; the first scene starts at the first number shown. "location": where it happens, at most 8 words (e.g. "a cramped cellar beneath the inn", "the docks at low tide"), "" if the text doesn't say. "time": the time of day if the text shows it ("dawn", "midday", "night"), otherwise "". "present": the cast members physically there, by their cast "name".
- "cast": every character who speaks or is physically present in this chapter - never someone only mentioned, remembered or talked about. "name": a known character's exact main name when it is them (never one of their "also called" names); a new character's proper name as the text gives it; one specific person known only by a title, the bare title with no article ("King", "Count"); unnamed minor people, just their kind ("Guard", "Woman", "Thug") with "role": true - several of one kind share one entry. "pronoun": "he", "she", "they", "it", or "" if the text doesn't show it. "refs": short phrases copied exactly from the text that it uses for that one person instead of their name - a title, epithet or description like "the ferrywoman" - only when they clearly mean that person; [] otherwise. Never a bare pronoun, never their own name.
- Only state what the text supports.`

// ChapterSummary is one chapter's summary-pass notes - store.ChapterSummary's
// own shape, kept as a plain value here rather than importing store (see
// ParagraphInput).
type ChapterSummary struct {
	Summary string
	Scenes  []Scene
	Cast    []CastMember
}

// Scene is one scene of a chapter: it runs from StartIdx until the next
// scene's StartIdx (or the chapter's end).
type Scene struct {
	StartIdx int
	Location string
	Time     string
	// Present are the cast members physically there, by CastMember.Name.
	Present []string
}

// CastMember is one character present in a chapter.
type CastMember struct {
	Name     string
	Role     bool
	Pronouns string // NormalizePronouns form, "" if the text didn't show
	// Refs are other phrases the chapter uses for them ("the ferrywoman"),
	// each found word for word in its text.
	Refs []string
}

// SceneAt returns the scene covering paragraph idx, or nil.
func (s *ChapterSummary) SceneAt(idx int) *Scene {
	if s == nil {
		return nil
	}
	var at *Scene
	for i := range s.Scenes {
		if s.Scenes[i].StartIdx <= idx {
			at = &s.Scenes[i]
		}
	}
	return at
}

// promptLines renders s for the ambience and lighting prompts - "" for a
// nil scene or one with neither a location nor a time.
func (s *Scene) promptLines() string {
	if s == nil || s.Location == "" && s.Time == "" {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nScene notes:\n")
	if s.Location != "" {
		fmt.Fprintf(&sb, "- location: %s\n", s.Location)
	}
	if s.Time != "" {
		fmt.Fprintf(&sb, "- time of day: %s\n", s.Time)
	}
	return sb.String()
}

// SummarizeChapter writes chapterTitle's summary-pass notes - see
// summarySystemPrompt. roster is the book's known characters (the prompt
// lists the Prominent ones plus any the text mentions, like an attribution
// batch); setting, when non-nil, is the book's setting profile. A chapter
// longer than summaryWindowChars is summarized window by window and the
// results merged: summaries joined, scenes appended, casts united. Cast
// names are sanitized and resolved onto the roster like attribution
// answers (a description becomes a role label - see RoleLabel), and a
// reference survives only if it appears word for word in the text.
func (c *Client) SummarizeChapter(ctx context.Context, bookTitle, chapterTitle string, setting *BookSetting, roster Roster, paragraphs []ParagraphInput) (ChapterSummary, error) {
	resolver := NewSpeakerNameResolver(roster.Names, roster.Aliases)
	var out ChapterSummary
	castIndex := map[string]int{}
	for _, window := range summaryWindows(paragraphs) {
		raw, err := c.summarizeWindow(ctx, bookTitle, chapterTitle, setting, roster, window)
		if err != nil {
			return out, err
		}
		text := strings.ToLower(windowText(window))
		rename := map[string]string{} // the model's cast name -> ours
		for _, m := range raw.Cast {
			name, role := sanitizeSpeaker(m.Name, m.Role)
			if isSentinel(name) {
				continue
			}
			if label, isTitle, ok := RoleLabel(name); ok {
				name, role = label, !isTitle
			}
			name = resolver.Canonical(name)
			rename[strings.ToLower(strings.TrimSpace(m.Name))] = name
			member := CastMember{Name: name, Role: role, Refs: verbatimRefs(mergeRefs(nil, m.Refs), text)}
			if !role {
				member.Pronouns = NormalizePronouns(m.Pronoun)
			}
			key := strings.ToLower(name)
			if i, ok := castIndex[key]; ok {
				out.Cast[i].Refs = mergeRefs(out.Cast[i].Refs, member.Refs)
				if out.Cast[i].Pronouns == "" {
					out.Cast[i].Pronouns = member.Pronouns
				}
				continue
			}
			castIndex[key] = len(out.Cast)
			out.Cast = append(out.Cast, member)
		}
		for _, sc := range sceneStarts(raw.Scenes, window) {
			var present []string
			for _, p := range sc.Present {
				if n, ok := rename[strings.ToLower(strings.TrimSpace(p))]; ok {
					present = append(present, n)
				} else if n, ok := resolver.Resolve(p); ok {
					present = append(present, n)
				}
			}
			out.Scenes = append(out.Scenes, Scene{StartIdx: sc.Start, Location: cleanSettingField(sc.Location), Time: cleanSettingField(sc.Time), Present: dedupe(present)})
		}
		if s := strings.TrimSpace(raw.Summary); s != "" {
			if out.Summary != "" {
				out.Summary += " "
			}
			out.Summary += s
		}
	}
	return out, nil
}

func summaryWindows(paragraphs []ParagraphInput) [][]ParagraphInput {
	var out [][]ParagraphInput
	start, size := 0, 0
	for i, p := range paragraphs {
		if size > 0 && size+len(p.Text) > summaryWindowChars && !p.Inline {
			out = append(out, paragraphs[start:i])
			start, size = i, 0
		}
		size += len(p.Text) + 1
	}
	if start < len(paragraphs) {
		out = append(out, paragraphs[start:])
	}
	return out
}

func (c *Client) summarizeWindow(ctx context.Context, bookTitle, chapterTitle string, setting *BookSetting, roster Roster, window []ParagraphInput) (summaryRaw, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	if lines := setting.promptLines(); lines != "" {
		fmt.Fprintf(&user, "\nBook setting notes:\n%s", lines)
	}
	if names := relevantNames(roster, window); len(names) > 0 {
		entries := make([]string, len(names))
		for i, name := range names {
			entries[i] = name
			var notes []string
			if p := roster.Pronouns[name]; p != "" {
				notes = append(notes, p)
			}
			if aliases := roster.Aliases[name]; len(aliases) > 0 {
				notes = append(notes, "also called "+strings.Join(aliases, ", "))
			}
			if roster.Roles[name] {
				entries[i] += " [role]"
			}
			if len(notes) > 0 {
				entries[i] += " (" + strings.Join(notes, "; ") + ")"
			}
		}
		fmt.Fprintf(&user, "\nKnown characters: %s\n", strings.Join(entries, ", "))
	}
	user.WriteString("\nParagraphs:\n")
	for i, p := range window {
		// An Inline paragraph continues the one before it - folded onto
		// its line, so every number starts a real paragraph (a scene can
		// only start there).
		if p.Inline && i > 0 {
			fmt.Fprintf(&user, " %s", oneLine(p.Text))
			continue
		}
		if i > 0 {
			user.WriteString("\n")
		}
		fmt.Fprintf(&user, "%d: %s", p.Idx, oneLine(p.Text))
	}
	user.WriteString("\n")
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	return generateAndParse(ctx, c, summarySystemPrompt, user.String(), 0, summaryMaxTokens, parseSummary)
}

type summaryRaw struct {
	Summary string `json:"summary"`
	Scenes  []struct {
		Start    int      `json:"start"`
		Location string   `json:"location"`
		Time     string   `json:"time"`
		Present  []string `json:"present"`
	} `json:"scenes"`
	Cast []struct {
		Name    string   `json:"name"`
		Role    bool     `json:"role"`
		Pronoun string   `json:"pronoun"`
		Refs    []string `json:"refs"`
	} `json:"cast"`
}

func parseSummary(content string) (summaryRaw, error) {
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start == -1 || end == -1 || end < start {
		return summaryRaw{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var raw summaryRaw
	if err := json.Unmarshal([]byte(content[start:end+1]), &raw); err != nil {
		return summaryRaw{}, fmt.Errorf("parse JSON object: %w", err)
	}
	if strings.TrimSpace(raw.Summary) == "" {
		return summaryRaw{}, fmt.Errorf("empty summary")
	}
	return raw, nil
}

type rawScene = struct {
	Start    int      `json:"start"`
	Location string   `json:"location"`
	Time     string   `json:"time"`
	Present  []string `json:"present"`
}

// sceneStarts validates the model's scenes against the window: each start
// snapped to a real, non-Inline paragraph in it (a scene can't open
// mid-paragraph), strictly increasing, and the first moved to the window's
// first paragraph. No scenes at all yields one covering the window.
func sceneStarts(scenes []rawScene, window []ParagraphInput) []rawScene {
	if len(window) == 0 {
		return nil
	}
	valid := map[int]bool{}
	for _, p := range window {
		if !p.Inline {
			valid[p.Idx] = true
		}
	}
	sort.SliceStable(scenes, func(i, j int) bool { return scenes[i].Start < scenes[j].Start })
	var out []rawScene
	for _, s := range scenes {
		if !valid[s.Start] || len(out) > 0 && s.Start <= out[len(out)-1].Start {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return []rawScene{{Start: window[0].Idx}}
	}
	out[0].Start = window[0].Idx
	return out
}

func dedupe(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// verbatimRefs keeps the refs that appear word for word in text
// (lowercased) and are at most maxRefWords long. The model otherwise
// writes summaries ("the one who dodges snow") and copies prompt examples,
// and a wrong reference in an attribution prompt sends a tagged line
// ("the captain said") to the wrong person.
func verbatimRefs(refs []string, text string) []string {
	var out []string
	for _, r := range refs {
		if len(strings.Fields(r)) <= maxRefWords && strings.Contains(text, strings.ToLower(r)) {
			out = append(out, r)
		}
	}
	return out
}

// maxRefWords caps one cast reference - a title or short description,
// not a sentence.
const maxRefWords = 6

func windowText(window []ParagraphInput) string {
	var sb strings.Builder
	for _, p := range window {
		sb.WriteString(oneLine(p.Text))
		sb.WriteString(" ")
	}
	return sb.String()
}

// mergeRefs appends refs to have, trimmed, dropping empties, bare
// pronouns and case-insensitive duplicates.
func mergeRefs(have, refs []string) []string {
	seen := map[string]bool{}
	for _, r := range have {
		seen[strings.ToLower(r)] = true
	}
	for _, r := range refs {
		r = strings.Trim(strings.TrimSpace(r), `"'“”.`)
		k := strings.ToLower(r)
		if r == "" || seen[k] || pronounSubjects[k] || k == "him" || k == "her" || k == "them" || k == "none" {
			continue
		}
		// "his boots", "her scarf", "his best friend": a belonging or a
		// relation, never what the text calls the person themselves - and
		// a possessive attribution answer is Unknown anyway (sanitizeSpeaker).
		if first, _, _ := strings.Cut(k, " "); possessiveWords[first] {
			continue
		}
		// "Rosie said": a speech tag, not something the text calls them.
		if slices.ContainsFunc(strings.Fields(k), func(w string) bool { return slices.Contains(speechVerbs, strings.Trim(w, ".,")) }) {
			continue
		}
		seen[k] = true
		have = append(have, r)
	}
	return have
}

// WithCast returns roster extended with a chapter's cast (a
// ChapterSummary's): every cast member is listed in every attribution
// batch (added to Prominent, and to Names when new), with their pronouns
// when the roster has none and the chapter's references to them in their
// description - so a batch whose lines only say "the smith" or "he" still
// sees who is in the scene.
func (r Roster) WithCast(cast []CastMember) Roster {
	out := r
	out.Names = append([]string(nil), r.Names...)
	out.Prominent = append([]string(nil), r.Prominent...)
	out.Descriptions = cloneMap(r.Descriptions)
	out.Pronouns = cloneMap(r.Pronouns)
	out.Roles = cloneMap(r.Roles)
	out.Cast = cast
	for _, m := range cast {
		out.Names = addKnown(out.Names, m.Name)
		out.Prominent = addKnown(out.Prominent, m.Name)
		if m.Role {
			out.Roles[m.Name] = true
		}
		if m.Pronouns != "" && out.Pronouns[m.Name] == "" {
			out.Pronouns[m.Name] = m.Pronouns
		}
		if len(m.Refs) > 0 {
			here := "in this chapter called " + strings.Join(m.Refs, ", ")
			if d := out.Descriptions[m.Name]; d != "" {
				here = d + "; " + here
			}
			out.Descriptions[m.Name] = here
		}
	}
	return out
}

// ResolveCastRefs maps an attribution answer that is one of a cast
// member's references ("the old sailor", with or without its article) to
// that member's name.
func ResolveCastRefs(speakers map[int]string, cast []CastMember) map[int]string {
	byRef := map[string]string{}
	ambiguous := map[string]bool{}
	for _, m := range cast {
		for _, r := range m.Refs {
			k := refKey(r)
			if k == "" {
				continue
			}
			if other, ok := byRef[k]; ok && other != m.Name {
				ambiguous[k] = true // "the nobleman" for two people: neither
			}
			byRef[k] = m.Name
		}
	}
	for k := range ambiguous {
		delete(byRef, k)
	}
	if len(byRef) == 0 {
		return speakers
	}
	out := make(map[int]string, len(speakers))
	for idx, sp := range speakers {
		out[idx] = sp
		if n, ok := byRef[refKey(sp)]; ok {
			out[idx] = n
		}
	}
	return out
}

// refKey normalizes a reference or an answer for matching: lower case,
// leading article dropped.
func refKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	for _, a := range []string{"the ", "a ", "an "} {
		s = strings.TrimPrefix(s, a)
	}
	return strings.Trim(s, ` .,'’"“”`)
}

func cloneMap[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
