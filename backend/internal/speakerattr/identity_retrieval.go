package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Identity retrieval: deciding whether a phrase a chapter uses for someone
// ("the storm guard sergeant", or a name the roster doesn't know yet) is
// one of the book's known characters, from what the rest of the book says
// - not from the chapter alone, which usually can't tell. The search is
// plain code (FindIdentityEvidence); the model is only asked to read the
// passages it found and point at one that says who it is (MatchIdentity).
//
// Raw co-occurrence doesn't work on its own: in Spire's Spite the
// protagonist appears near "the Nightshark" 180 times because he's in
// every scene, while the one passage that matters for "the storm guard
// sergeant" is "The storm guard sergeant, Louisa, asked". So nearby names
// are scored by proximity (the same sentence counts far more) and
// discounted by how often they appear in the book at all.

// IdentityPassage is one passage of a book for identity retrieval: a
// paragraph's text, with its neighbours as context.
type IdentityPassage struct {
	Text string
}

// IdentityEvidence is what FindIdentityEvidence found for one phrase: the
// known characters the passages mentioning it point to, best first, and
// the passages themselves.
type IdentityEvidence struct {
	Candidates []string
	Snippets   []string
}

const (
	// maxIdentityCandidates and maxIdentitySnippets bound one identity
	// question's prompt.
	maxIdentityCandidates = 4
	maxIdentitySnippets   = 6
	// identitySnippetChars caps one snippet, centred on the phrase.
	identitySnippetChars = 500
)

var sentenceSplit = regexp.MustCompile(`[.!?…]["”’]?\s+`)

// IdentityIndex is a book's text and known characters, prepared once for
// any number of FindIdentityEvidence searches: lower-cased paragraphs, and
// how many paragraphs mention each character (computed on first use).
type IdentityIndex struct {
	paragraphs []string
	lower      []string
	names      map[string][]string // name -> lower-cased forms (name + aliases)
	freq       map[string]int
}

// NewIdentityIndex prepares paragraphs (a whole book, in order) and names
// (each known character with its aliases) for FindIdentityEvidence.
func NewIdentityIndex(paragraphs []string, names map[string][]string) *IdentityIndex {
	ix := &IdentityIndex{paragraphs: paragraphs, lower: make([]string, len(paragraphs)), names: map[string][]string{}}
	for i, p := range paragraphs {
		ix.lower[i] = strings.ToLower(p)
	}
	for name, aliases := range names {
		var forms []string
		for _, f := range append([]string{name}, aliases...) {
			if f = strings.ToLower(strings.TrimSpace(f)); len(f) >= 3 {
				forms = append(forms, f)
			}
		}
		if len(forms) > 0 {
			ix.names[name] = forms
		}
	}
	return ix
}

// MentionsWord reports whether text writes name as a whole word, ignoring
// case.
func MentionsWord(text, name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name != "" && mentions([]string{name}, strings.ToLower(text))
}

// mentions reports whether lower-cased text mentions any of forms as a
// whole word.
func mentions(forms []string, text string) bool {
	for _, f := range forms {
		for off := 0; ; {
			i := strings.Index(text[off:], f)
			if i < 0 {
				break
			}
			i += off
			if wordBoundary(text, i-1) && wordBoundary(text, i+len(f)) {
				return true
			}
			off = i + 1
		}
	}
	return false
}

// wordBoundary reports whether the character touching byte i - the one
// ending there when i is a match's start-1, starting there when it's its
// end - isn't a letter or digit. Decodes whole runes: a curly quote right
// before a name (“Tobias!”) is a boundary, an accented letter isn't.
func wordBoundary(text string, i int) bool {
	if i < 0 || i >= len(text) {
		return true
	}
	var r rune
	if text[i] < utf8.RuneSelf {
		r = rune(text[i])
	} else if utf8.RuneStart(text[i]) {
		r, _ = utf8.DecodeRuneInString(text[i:])
	} else {
		// A continuation byte: i is the last byte of the rune before a
		// match - decode that rune from its start.
		r, _ = utf8.DecodeLastRuneInString(text[:i+1])
	}
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

func (ix *IdentityIndex) frequency(name string) int {
	if ix.freq == nil {
		ix.freq = map[string]int{}
	}
	if n, ok := ix.freq[name]; ok {
		return n
	}
	n := 0
	for _, t := range ix.lower {
		if mentions(ix.names[name], t) {
			n++
		}
	}
	ix.freq[name] = n
	return n
}

// FindIdentityEvidence searches the book for phrase and scores the known
// characters mentioned around each hit: 3 for the same sentence, 1 for the
// same or a neighbouring paragraph, divided by the square root of how many
// paragraphs mention that character at all - so the main cast, near
// everything, doesn't drown out the one appositive that names someone.
// exclude names characters never to propose (the phrase's own entry).
func (ix *IdentityIndex) FindIdentityEvidence(phrase string, exclude map[string]bool) IdentityEvidence {
	phraseRe := wordRegexp(phrase)
	lowerPhrase := strings.ToLower(strings.TrimSpace(phrase))
	if phraseRe == nil || lowerPhrase == "" {
		return IdentityEvidence{}
	}
	var hits []int
	for i, t := range ix.lower {
		if mentions([]string{lowerPhrase}, t) {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		return IdentityEvidence{}
	}
	type snippet struct {
		text  string
		score float64
	}
	scores := map[string]float64{}
	var snippets []snippet
	for _, i := range hits {
		window := strings.Join(ix.lower[max(0, i-1):min(len(ix.lower), i+2)], " ")
		var sentence string
		for _, s := range sentenceSplit.Split(ix.lower[i], -1) {
			if mentions([]string{lowerPhrase}, s) {
				sentence += " " + s
			}
		}
		best := 0.0
		for name, forms := range ix.names {
			if exclude[name] || strings.EqualFold(name, phrase) {
				continue
			}
			w := 0.0
			switch {
			case mentions(forms, sentence):
				w = 3
			case mentions(forms, window):
				w = 1
			default:
				continue
			}
			w /= math.Sqrt(float64(max(1, ix.frequency(name))))
			scores[name] += w
			best = max(best, w)
		}
		if best > 0 {
			orig := strings.Join(ix.paragraphs[max(0, i-1):min(len(ix.paragraphs), i+2)], " ")
			snippets = append(snippets, snippet{centred(orig, phraseRe, identitySnippetChars), best})
		}
	}
	var cands []string
	for n := range scores {
		cands = append(cands, n)
	}
	sort.Slice(cands, func(i, j int) bool {
		if scores[cands[i]] != scores[cands[j]] {
			return scores[cands[i]] > scores[cands[j]]
		}
		return cands[i] < cands[j]
	})
	if len(cands) > maxIdentityCandidates {
		cands = cands[:maxIdentityCandidates]
	}
	sort.SliceStable(snippets, func(i, j int) bool { return snippets[i].score > snippets[j].score })
	out := IdentityEvidence{Candidates: cands}
	for i := 0; i < len(snippets) && i < maxIdentitySnippets; i++ {
		out.Snippets = append(out.Snippets, snippets[i].text)
	}
	return out
}

// FindIdentityEvidence is IdentityIndex.FindIdentityEvidence over a
// one-off index.
func FindIdentityEvidence(paragraphs []string, phrase string, names map[string][]string, exclude map[string]bool) IdentityEvidence {
	return NewIdentityIndex(paragraphs, names).FindIdentityEvidence(phrase, exclude)
}

// wordRegexp matches s as whole words, case-insensitively; nil for a
// blank s.
func wordRegexp(s string) *regexp.Regexp {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return regexp.MustCompile(`(?i)(?:^|[^\pL\pN])` + regexp.QuoteMeta(s) + `(?:$|[^\pL\pN])`)
}

// centred cuts text to about n characters around re's first match.
func centred(text string, re *regexp.Regexp, n int) string {
	if len(text) <= n {
		return text
	}
	loc := re.FindStringIndex(text)
	start := 0
	if loc != nil {
		start = max(0, loc[0]-n/2)
	}
	end := min(len(text), start+n)
	// Keep to whole words and valid UTF-8 at the cut points.
	for start > 0 && start < len(text) && text[start]&0xC0 == 0x80 {
		start++
	}
	for end < len(text) && text[end]&0xC0 == 0x80 {
		end--
	}
	out := text[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(text) {
		out += "…"
	}
	return out
}

// identitySystemPrompt asks whether a phrase is one of a few known
// characters, from passages the retrieval found - see FindIdentityEvidence.
const identitySystemPrompt = `You are checking a novel's cast list. You will be given a phrase the book uses for someone, a few known characters, and passages from the book that mention the phrase. Decide whether the phrase refers to one of those known characters.

Answer with a name only when a passage makes it clear - it names them in the same breath ("The storm guard sergeant, Louisa, asked"), introduces them ("the man in grey, who turned out to be Tobias"), or otherwise leaves no doubt. Being in the same scene, talking to them, or being mentioned near them is not enough. A title or description can also refer to several different people over a book; answer only if these passages show it is that one person.

Reply with a compact JSON object only, no other text:

{"same_as": "<one known character's exact name, or empty>", "evidence": "<the words from one passage that show it, copied exactly, or empty>"}`

type identityRaw struct {
	SameAs   string `json:"same_as"`
	Evidence string `json:"evidence"`
}

// MatchIdentity asks whether phrase is one of ev's candidates, returning
// that candidate's name or "" - and "" too unless the model backs its
// answer with evidence found word for word in one of the snippets.
// candidateNotes (nil-safe) adds a short note per candidate (pronouns).
func (c *Client) MatchIdentity(ctx context.Context, bookTitle, phrase string, ev IdentityEvidence, candidateNotes map[string]string) (string, error) {
	if len(ev.Candidates) == 0 || len(ev.Snippets) == 0 {
		return "", nil
	}
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nPhrase: %q\n\nKnown characters:\n", bookTitle, phrase)
	for _, n := range ev.Candidates {
		if note := candidateNotes[n]; note != "" {
			fmt.Fprintf(&user, "- %s (%s)\n", n, note)
		} else {
			fmt.Fprintf(&user, "- %s\n", n)
		}
	}
	user.WriteString("\nPassages:\n")
	for i, s := range ev.Snippets {
		fmt.Fprintf(&user, "[%d] %s\n", i+1, oneLine(s))
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	raw, err := generateAndParse(ctx, c, identitySystemPrompt, user.String(), 0, 512, func(content string) (identityRaw, error) {
		value, found := firstJSONValue(content, '{')
		if !found {
			return identityRaw{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
		}
		var r identityRaw
		if err := json.Unmarshal([]byte(value), &r); err != nil {
			return identityRaw{}, fmt.Errorf("parse JSON object: %w", err)
		}
		return r, nil
	})
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(raw.SameAs)
	if name == "" {
		return "", nil
	}
	var match string
	for _, cand := range ev.Candidates {
		if strings.EqualFold(cand, name) {
			match = cand
		}
	}
	if match == "" || !evidenceFound(raw.Evidence, ev.Snippets) {
		return "", nil
	}
	return match, nil
}

// evidenceFound reports whether evidence (at least a few words) appears in
// one of snippets, ignoring case, quote style and spacing.
func evidenceFound(evidence string, snippets []string) bool {
	norm := func(s string) string {
		s = strings.ToLower(s)
		s = strings.NewReplacer("“", `"`, "”", `"`, "‘", "'", "’", "'", "…", "...").Replace(s)
		return strings.Join(strings.Fields(s), " ")
	}
	e := strings.Trim(norm(evidence), ` ."'`)
	if len(strings.Fields(e)) < 2 {
		return false
	}
	for _, s := range snippets {
		if strings.Contains(norm(s), e) {
			return true
		}
	}
	return false
}

// maxIdentityChecks bounds how many MatchIdentity calls one chapter's
// ResolveCastIdentities makes.
const maxIdentityChecks = 4

// CastAlias is a name ResolveCastIdentities found to be another name for
// a known character - for the caller to record as an alias.
type CastAlias struct {
	Character string
	Alias     string
}

// ResolveCastIdentities checks a chapter summary's cast against the rest of
// the book (ix - its paragraphs and known characters) and folds members
// onto known characters where the book says they're the same person:
//   - a new name (not in roster) that the book shows is a known character
//     becomes that character, and the name is returned as an alias;
//   - a reference in a role entry ("the storm guard sergeant" in Guard)
//     that the book ties to one known character moves to that character's
//     own entry.
//
// At most maxIdentityChecks questions per chapter; a member the search
// finds no candidates for costs no call. Everything else is left as is.
func (c *Client) ResolveCastIdentities(ctx context.Context, bookTitle string, sum ChapterSummary, ix *IdentityIndex, roster Roster) (ChapterSummary, []CastAlias, error) {
	if ix == nil || len(sum.Cast) == 0 {
		return sum, nil, nil
	}
	resolver := NewSpeakerNameResolver(roster.Names, roster.Aliases).WithTitles(roster.World)
	checks := 0
	ask := func(phrase string, exclude map[string]bool) (string, error) {
		if checks >= maxIdentityChecks {
			return "", nil
		}
		ev := ix.FindIdentityEvidence(phrase, exclude)
		if len(ev.Candidates) == 0 {
			return "", nil
		}
		checks++
		return c.MatchIdentity(ctx, bookTitle, phrase, ev, roster.Pronouns)
	}
	var aliases []CastAlias
	out := sum
	out.Cast = nil
	var moved []CastMember // refs reassigned to a known character
	for _, m := range sum.Cast {
		switch {
		case !m.Role && !isSentinel(m.Name):
			if _, known := resolver.Resolve(m.Name); known {
				break
			}
			match, err := ask(m.Name, map[string]bool{m.Name: true})
			if err != nil {
				return sum, nil, err
			}
			if match != "" {
				aliases = append(aliases, CastAlias{Character: match, Alias: m.Name})
				renameInScenes(&out, m.Name, match)
				m.Name = match
			}
		case m.Role:
			var keep []string
			for i, ref := range m.Refs {
				var match string
				// "the werewolf" under Werewolf, "the man" under Man: the
				// role's own kind, not a description of one person - it
				// once moved a pack's "the werewolf" onto one named
				// werewolf, and every Werewolf line with it.
				if generic := refKey(ref); generic == refKey(m.Name) || isRoleName(roster.Roles, generic) {
					keep = append(keep, ref)
					continue
				}
				if i < 2 {
					var err error
					if match, err = ask(ref, nil); err != nil {
						return sum, nil, err
					}
				}
				if match == "" {
					keep = append(keep, ref)
					continue
				}
				moved = append(moved, CastMember{Name: match, Pronouns: roster.Pronouns[match], Refs: []string{ref}})
			}
			m.Refs = keep
		}
		out.Cast = mergeCastMember(out.Cast, m)
	}
	for _, m := range moved {
		out.Cast = mergeCastMember(out.Cast, m)
	}
	return out, aliases, nil
}

// mergeCastMember adds m to cast, folding it into an existing entry of the
// same name (references united, pronouns filled).
func mergeCastMember(cast []CastMember, m CastMember) []CastMember {
	for i := range cast {
		if strings.EqualFold(cast[i].Name, m.Name) {
			cast[i].Refs = mergeRefs(cast[i].Refs, m.Refs)
			if cast[i].Pronouns == "" {
				cast[i].Pronouns = m.Pronouns
			}
			return cast
		}
	}
	return append(cast, m)
}

func renameInScenes(s *ChapterSummary, from, to string) {
	scenes := make([]Scene, len(s.Scenes))
	for i, sc := range s.Scenes {
		sc.Present = append([]string(nil), sc.Present...)
		for j, p := range sc.Present {
			if p == from {
				sc.Present[j] = to
			}
		}
		sc.Present = dedupe(sc.Present)
		scenes[i] = sc
	}
	s.Scenes = scenes
}

// isRoleName reports whether name is one of roles, ignoring case.
func isRoleName(roles map[string]bool, name string) bool {
	for r, ok := range roles {
		if ok && strings.EqualFold(r, name) {
			return true
		}
	}
	return false
}
