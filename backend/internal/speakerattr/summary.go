package speakerattr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
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

// summaryMinChars is the chapter text under which an empty summary is an
// answer rather than a failure: a converter's one-line "Converted Ebook"
// page or a bare half-title has no story to tell, the model says so, and
// rejecting that failed the task for good - taking the book's ambience and
// attribution runs with it. Such a chapter gets empty notes.
const summaryMinChars = 200

// errEmptySummary is parseSummary's error for an answer with no summary.
var errEmptySummary = errors.New("empty summary")

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
- "scenes": in order. A new scene starts where the story moves to a different place, jumps in time, or switches to other characters. "start" is the number printed before the scene's first paragraph; the first scene starts at the first number shown. "location": where it happens, at most 8 words (e.g. "a cramped cellar beneath the inn", "the docks at low tide"), "" if the text doesn't say. "time": the time of day if the text shows it ("dawn", "midday", "night"), otherwise "". "present": the cast members physically there, by their cast "name" - including anyone the text only calls "she", "he" or by a description once it has shown who that is, and never someone who has left or died.
- "cast": every character who speaks or acts in this chapter - including in a dream, vision, memory being relived, or a mental or magical space, which counts as being there - never someone only mentioned, remembered or talked about. Anyone the text credits with a line of dialogue belongs here. "name": a known character's exact main name when it is them (never one of their "also called" names); a new character's proper name as the text gives it; one specific person known only by a title, the bare title with no article ("King", "Count"); unnamed minor people, just their kind ("Guard", "Woman", "Thug") with "role": true - several of one kind share one entry. "pronoun": "he", "she", "they", "it", or "" if the text doesn't show it. "refs": short phrases copied exactly from the text that it uses for that one person instead of their name - a title, epithet or description like "the ferrywoman" - only when they clearly mean that person; [] otherwise. Never a bare pronoun, never their own name.
- Only state what the text supports.`

// ChapterSummary is one chapter's summary-pass notes - store.ChapterSummary's
// own shape, kept as a plain value here rather than importing store (see
// ParagraphInput).
type ChapterSummary struct {
	Summary string
	Scenes  []Scene
	Cast    []CastMember
	// Parts are the summaries Summary was joined from, one per window of a
	// chapter too long for one call, each with where its window starts - a
	// recap for a reader partway through uses the ones they have passed.
	// Empty for notes from before parts were kept.
	Parts []SummaryPart
}

// SummaryPart is the summary of one window of a chapter: the paragraphs
// from StartIdx to the next part's StartIdx (or the chapter's end).
type SummaryPart struct {
	StartIdx int
	Summary  string
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
// reference survives only if it appears word for word in the text. A
// chapter under summaryMinChars the model finds nothing to say about gets
// empty notes.
func (c *Client) SummarizeChapter(ctx context.Context, bookTitle, chapterTitle string, setting *BookSetting, roster Roster, paragraphs []ParagraphInput) (ChapterSummary, error) {
	resolver := NewSpeakerNameResolver(roster.Names, roster.Aliases).WithFirstPerson(roster.FirstPerson)
	var out ChapterSummary
	castIndex := map[string]int{}
	var prev []ParagraphInput
	for _, window := range summaryWindows(paragraphs) {
		carry := carryOver(out) + leadIn(prev)
		raw, err := c.summarizeWindow(ctx, bookTitle, chapterTitle, setting, roster, window, carry, "")
		prev = window
		if errors.Is(err, errEmptySummary) && len(strings.TrimSpace(windowText(paragraphs))) < summaryMinChars {
			return ChapterSummary{}, nil
		}
		if err != nil {
			return out, err
		}
		// The model sometimes tells a first-person chapter as some other
		// character's - Reclaimer ch. 14's "Dan", named once in a memory,
		// was cast and placed in every scene in Steve's stead (9 of its
		// chapters). Ask once more, pointing that out; keep the retry only
		// if it now has the narrator.
		if note := narratorMissedNote(raw, roster, window, resolver); note != "" {
			retry, err := c.summarizeWindow(ctx, bookTitle, chapterTitle, setting, roster, window, carry, note)
			kept := err == nil && castHasNarrator(retry, roster, resolver)
			if kept {
				raw = retry
			}
			log.Printf("speakerattr: summary of %q left out narrator %s, re-asked (kept: %v)", chapterTitle, roster.FirstPerson, kept)
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
			out.Parts = append(out.Parts, SummaryPart{StartIdx: window[0].Idx, Summary: s})
		}
	}
	// Whoever an explicit speech tag credits with a line is in the chapter,
	// whatever the model listed - it once left a whole chapter's cast empty
	// because its three speakers talked inside a character's mind-space.
	for _, name := range TaggedSpeakers(paragraphs, resolver) {
		if _, ok := castIndex[strings.ToLower(name)]; ok {
			continue
		}
		castIndex[strings.ToLower(name)] = len(out.Cast)
		out.Cast = append(out.Cast, CastMember{Name: name, Pronouns: roster.Pronouns[name], Role: roster.Roles[name]})
	}
	addTaggedToScenes(&out, paragraphs, resolver)
	return out, nil
}

// addTaggedToScenes adds to each scene's Present whoever an explicit speech
// tag credits with a line inside it ("…,” Ingrid asked) - attribution
// narrows each batch to its scenes' Present, so a speaker the summary
// model forgot there would be left out of the prompt for their own lines.
func addTaggedToScenes(sum *ChapterSummary, paragraphs []ParagraphInput, resolver *SpeakerNameResolver) {
	for i := range sum.Scenes {
		lo := sum.Scenes[i].StartIdx
		hi := -1
		if i+1 < len(sum.Scenes) {
			hi = sum.Scenes[i+1].StartIdx
		}
		var in []ParagraphInput
		for _, p := range paragraphs {
			if (i == 0 || p.Idx >= lo) && (hi < 0 || p.Idx < hi) {
				in = append(in, p)
			}
		}
		for _, name := range TaggedSpeakers(in, resolver) {
			if !slices.ContainsFunc(sum.Scenes[i].Present, func(n string) bool { return strings.EqualFold(n, name) }) {
				sum.Scenes[i].Present = append(sum.Scenes[i].Present, name)
			}
		}
	}
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

// carryOver tells a later summary window what the earlier ones found: the
// summary so far and who was present as the last one ended. Without it a
// window starting mid-scene met its characters as strangers - Arise ch. 31's
// second window began mid-conversation and listed the "she" there (Ingrid,
// named a few paragraphs before the cut) as an unnamed "Woman". "" for the
// first window.
func carryOver(sofar ChapterSummary) string {
	if sofar.Summary == "" && len(sofar.Scenes) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\nThe paragraphs below continue this chapter partway through - notes on the part before them:\n")
	if sofar.Summary != "" {
		fmt.Fprintf(&sb, "- so far: %s\n", sofar.Summary)
	}
	if n := len(sofar.Scenes); n > 0 {
		last := sofar.Scenes[n-1]
		if last.Location != "" {
			fmt.Fprintf(&sb, "- the scene in progress: %s\n", last.Location)
		}
		if len(last.Present) > 0 {
			fmt.Fprintf(&sb, "- present when it broke off: %s - someone the paragraphs below only call \"she\" or \"he\" is most likely one of them\n", strings.Join(last.Present, ", "))
		}
	}
	return sb.String()
}

// summaryLeadParagraphs is how many of the previous window's last
// paragraphs a later summary window is shown as context.
const summaryLeadParagraphs = 8

// leadIn shows a later summary window the end of the one before it,
// unnumbered: the carry-over's list of who was present didn't let the
// model tell which of them an unnamed "she" was (Arise ch. 31's second
// window still cast her as the barmaid killed a page earlier), while the
// paragraphs themselves name her ("seeing the way Ingrid was watching
// me"). "" for the first window.
func leadIn(prev []ParagraphInput) string {
	if len(prev) == 0 {
		return ""
	}
	start := max(0, len(prev)-summaryLeadParagraphs)
	for start > 0 && prev[start].Inline {
		start-- // don't cut a paragraph in half
	}
	var sb strings.Builder
	sb.WriteString("\nThe paragraphs right before these, for context only (already covered - never start a scene or take a \"start\" from them):\n")
	for i, p := range prev[start:] {
		if p.Inline && i > 0 {
			fmt.Fprintf(&sb, " %s", oneLine(p.Text))
			continue
		}
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "> %s", oneLine(p.Text))
	}
	sb.WriteString("\n")
	return sb.String()
}

// narratorMissedNote is the correction for a window whose cast leaves out
// a first-person book's narrator although its narration is theirs ("" when
// nothing is missed). It names the stand-in when there is one - the
// non-role cast member in the most scenes - and how rarely the text itself
// names them, which is what gives the swap away.
func narratorMissedNote(raw summaryRaw, roster Roster, window []ParagraphInput, resolver *SpeakerNameResolver) string {
	narrator := roster.FirstPerson
	if narrator == "" || castHasNarrator(raw, roster, resolver) || !FirstPersonNarration(window) {
		return ""
	}
	standIn, most := "", 0
	for _, m := range raw.Cast {
		if m.Role {
			continue
		}
		n := 0
		for _, sc := range raw.Scenes {
			if slices.ContainsFunc(sc.Present, func(p string) bool { return strings.EqualFold(strings.TrimSpace(p), m.Name) }) {
				n++
			}
		}
		if n > most {
			standIn, most = strings.TrimSpace(m.Name), n
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\nA first draft of these notes left %s out of the cast. This chapter is told by %s as \"I\": %s is in every scene the narration is in, and what \"I\" does, %s does.", narrator, narrator, narrator, narrator)
	if standIn != "" {
		fmt.Fprintf(&sb, " It made %s, whom these paragraphs name in only %d of them, the one acting instead.", standIn, paragraphsNaming(window, standIn, roster.Aliases[resolver.Canonical(standIn)]))
	}
	fmt.Fprintf(&sb, " Write the notes again with %s in the cast and the scenes, and list anyone else only if they are actually there.\n", narrator)
	return sb.String()
}

// castHasNarrator reports whether raw's cast names the first-person
// narrator, by name, alias or a placeholder ("Protagonist" - which
// FoldFirstPersonCast renames).
func castHasNarrator(raw summaryRaw, roster Roster, resolver *SpeakerNameResolver) bool {
	for _, m := range raw.Cast {
		if IsFirstPersonLabel(m.Name) || strings.EqualFold(resolver.Canonical(strings.TrimSpace(m.Name)), roster.FirstPerson) {
			return true
		}
	}
	return false
}

// paragraphsNaming counts the paragraphs of window whose text has name, or
// one of its aliases, as a whole word.
func paragraphsNaming(window []ParagraphInput, name string, aliases []string) int {
	alts := []string{regexp.QuoteMeta(name)}
	for _, a := range aliases {
		alts = append(alts, regexp.QuoteMeta(a))
	}
	re := regexp.MustCompile(`\b(?:` + strings.Join(alts, "|") + `)\b`)
	n := 0
	for _, p := range window {
		if re.MatchString(p.Text) {
			n++
		}
	}
	return n
}

// summarizeWindow asks for one window's notes. note, when set, goes after
// the paragraphs - a correction for a retry (narratorMissedNote).
func (c *Client) summarizeWindow(ctx context.Context, bookTitle, chapterTitle string, setting *BookSetting, roster Roster, window []ParagraphInput, carry, note string) (summaryRaw, error) {
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
	if roster.FirstPerson != "" {
		fmt.Fprintf(&user, "\nThis book is narrated in the first person by %s - the \"I\" is %s, a cast member like anyone else.\n", roster.FirstPerson, roster.FirstPerson)
	}
	user.WriteString(carry)
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
	user.WriteString(note)
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
	value, found := firstJSONValue(content, '{')
	if !found {
		return summaryRaw{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var raw summaryRaw
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return summaryRaw{}, fmt.Errorf("parse JSON object: %w", err)
	}
	if strings.TrimSpace(raw.Summary) == "" {
		return summaryRaw{}, errEmptySummary
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
		// "dude", "man", "mate", "hero": one lowercase word is a term of
		// address or a generic noun anyone gets called, not a reference to
		// one person ("the ferrywoman", "Sinclair the enforcer") - and as a
		// reference it would turn a "Man" role answer into them.
		if !strings.Contains(r, " ") && r == k {
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

// firstPersonWordRe matches a first-person "I" as a word ("I", "I'm",
// "I’d") - case-sensitive, so the Roman numeral in "Henry I" would count
// but "i" in running text never does.
var firstPersonWordRe = regexp.MustCompile(`(?:^|[^\p{L}])I(?:['’](?:m|d|ll|ve))?(?:[^\p{L}'’]|$)`)

// FirstPersonNarration reports whether paragraphs' narration (its non-quote
// paragraphs) is told in the first person: at least 3 of them, and a fifth
// of all, use "I". A first-person book's prologue or interlude told in the
// third person ("Prologue - 1614 AD") reads false.
func FirstPersonNarration(paragraphs []ParagraphInput) bool {
	narration, first := 0, 0
	for _, p := range paragraphs {
		if p.IsQuote {
			continue
		}
		narration++
		if firstPersonWordRe.MatchString(p.Text) {
			first++
		}
	}
	return first >= 3 && first*5 >= narration
}

// FoldFirstPersonCast renames the cast member that is really a
// first-person book's narrator (roster.FirstPerson) to the narrator's own
// name, returning the name it replaced ("" when it changed nothing). The
// summary model knows the narrator only as "I", so when the text calls
// them something the roster doesn't know - Arise's Steve, addressed as
// "Jamey Sultan" - it lists a new character instead, whom attribution
// then treats as someone else in the room. Applies only when paragraphs
// are first-person narration, the cast has no entry for the narrator, and
// exactly one unknown, non-role cast member with compatible pronouns is in
// the most scenes (at least half of them) - the narrator is in nearly
// every scene of their own chapter.
func FoldFirstPersonCast(sum ChapterSummary, roster Roster, paragraphs []ParagraphInput) (ChapterSummary, string) {
	narrator := roster.FirstPerson
	if narrator == "" || len(sum.Cast) == 0 || !FirstPersonNarration(paragraphs) {
		return sum, ""
	}
	// "Protagonist", "Narrator", "I": a placeholder for the narrator, never
	// anyone else (Arise ch. 7's cast listed "Protagonist" beside Steve).
	if slices.ContainsFunc(sum.Cast, func(m CastMember) bool { return IsFirstPersonLabel(m.Name) }) {
		folded := sum
		folded.Cast = nil
		for _, m := range sum.Cast {
			if IsFirstPersonLabel(m.Name) {
				renameInScenes(&folded, m.Name, narrator)
				m.Name, m.Role = narrator, false
			}
			folded.Cast = mergeCastMember(folded.Cast, m)
		}
		sum = folded
	}
	resolver := NewSpeakerNameResolver(roster.Names, roster.Aliases)
	known := func(name string) bool {
		c := resolver.Canonical(name)
		return c != name || slices.ContainsFunc(roster.Names, func(n string) bool { return strings.EqualFold(n, name) })
	}
	for _, m := range sum.Cast {
		if strings.EqualFold(resolver.Canonical(m.Name), narrator) {
			return sum, ""
		}
	}
	scenesWith := func(name string) int {
		n := 0
		for _, sc := range sum.Scenes {
			if slices.ContainsFunc(sc.Present, func(p string) bool { return strings.EqualFold(p, name) }) {
				n++
			}
		}
		return n
	}
	pronouns := roster.Pronouns[narrator]
	best, bestScenes, tied := "", -1, false
	for _, m := range sum.Cast {
		if m.Role || isSentinel(m.Name) || known(m.Name) {
			continue
		}
		if pronouns != "" && m.Pronouns != "" && m.Pronouns != pronouns {
			continue
		}
		switch n := scenesWith(m.Name); {
		case n > bestScenes:
			best, bestScenes, tied = m.Name, n, false
		case n == bestScenes:
			tied = true
		}
	}
	if best == "" || tied || len(sum.Scenes) > 0 && bestScenes*2 < len(sum.Scenes) {
		return sum, ""
	}
	out := sum
	out.Cast = nil
	for _, m := range sum.Cast {
		if m.Name == best {
			m.Name = narrator
			if pronouns != "" {
				m.Pronouns = pronouns
			}
		}
		out.Cast = mergeCastMember(out.Cast, m)
	}
	renameInScenes(&out, best, narrator)
	return out, best
}

// WithCast returns roster extended with a chapter's cast (a
// ChapterSummary's): every cast member is listed in every attribution
// batch (added to Names when new), with their pronouns when the roster has
// none and the chapter's references to them in their description - so a
// batch whose lines only say "the smith" or "he" still sees who is in the
// scene. The cast replaces Prominent rather than adding to it: a book's
// frequent speakers from other chapters would otherwise be offered for
// lines in a chapter they're not in (Arise's prologue-only Petros got a
// first-person narrator's unnamed lines three chapters later). Anyone the
// cast missed is still listed wherever a batch's own text names them
// (relevantNames).
func (r Roster) WithCast(cast []CastMember) Roster {
	out := r
	out.Names = append([]string(nil), r.Names...)
	out.Prominent = nil
	out.Descriptions = cloneMap(r.Descriptions)
	out.Pronouns = cloneMap(r.Pronouns)
	out.Roles = cloneMap(r.Roles)
	out.Cast = append([]CastMember(nil), cast...)
	resolver := NewSpeakerNameResolver(r.Names, r.Aliases)
	for i, m := range cast {
		m.Refs = mergeRefs(nil, m.Refs) // notes stored before a filter was added
		m.Refs = resolver.dropOthersNames(m.Name, m.Refs)
		out.Cast[i] = m
		out.Names = addKnown(out.Names, m.Name)
		out.Prominent = addKnown(out.Prominent, m.Name)
		if m.Role {
			out.Roles[m.Name] = true
		}
		switch {
		case m.Pronouns == "":
		case !m.Role:
			// The chapter's own pronouns win for a named character, under
			// the roster's spelling ("Amanda" is Francine Amanda Deena's
			// alias): the roster keeps one value per character, but a book
			// can change one mid-way - Spire's Spite calls Sid "he" until a
			// reveal, then "she" - and a roster "she/her" had
			// RefinePronounTags move three of five of Sid's early "he said"
			// lines to someone else (and the prompt list her as she/her).
			// Notes and roster agree on 500 of 504 named characters.
			out.Pronouns[resolver.Canonical(m.Name)] = m.Pronouns
		case out.Pronouns[m.Name] == "":
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

// dropOthersNames removes the refs that are a known character's own name
// or alias - someone other than name. The summary pass sometimes files one
// character under another ("Seneschal" as what Brightblade's chapter 26
// calls Bob, a skeleton who never speaks), and every line the model then
// gave Seneschal was moved to Bob.
func (r *SpeakerNameResolver) dropOthersNames(name string, refs []string) []string {
	if r == nil {
		return refs
	}
	self := r.Canonical(name)
	var keep []string
	for _, ref := range refs {
		if c, known := r.exact[strings.ToLower(ref)]; known && !strings.EqualFold(c, self) {
			continue
		}
		keep = append(keep, ref)
	}
	return keep
}

// ResolveCastRefs maps an attribution answer that is one of a cast
// member's references ("the old sailor", with or without its article) to
// that member's name.
func ResolveCastRefs(speakers map[int]string, cast []CastMember) map[int]string {
	byRef := map[string]string{}
	ambiguous := map[string]bool{}
	// A reference that is another cast entry's own name ("the werewolf"
	// when Werewolf is in the cast too) can't single anyone out - it once
	// sent a whole pack's lines to one named werewolf.
	for _, m := range cast {
		ambiguous[refKey(m.Name)] = true
	}
	for _, m := range cast {
		for _, r := range mergeRefs(nil, m.Refs) {
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
	for _, m := range cast { // a member's own name still maps to them
		delete(byRef, refKey(m.Name))
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
