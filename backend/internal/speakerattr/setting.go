package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// settingMaxTokens bounds the setting call's JSON - a few short quotes and
// a handful of short fields.
const settingMaxTokens = 768

// settingSystemPrompt is the book setting pass: one call per book, over
// excerpts from across it (SettingSamples), writing down the world the
// other passes otherwise each guess at on their own from one short span -
// the era and technology above all, which a region's text rarely states
// (the ambience pass heard a modern dive bar as a "busy medieval tavern",
// and lighting copied it into candlelight). Per book, not per series: a
// series can move decades or worlds between books.
//
// The evidence-first shape is from runs over this library. Asked for the
// era directly, Arise: Alpha (present day, phones and cars throughout, plus
// dungeons and catacombs) came back "medieval-style secondary world,
// pre-industrial" while listing earpieces in the same answer; told instead
// that any modern technology means modern, Spire's Spite (swords, lanterns,
// carriages, nothing modern) came back "present day: cars, phones", and
// Jake's Magical Market's similes ("like mini machine guns") counted.
// Quoting what characters use first didn't fix it on its own - every book
// then came back "present day", Spire's Spite with "lantern, wax" as its
// evidence. What did was TechCounts: technology words counted across the
// whole book in Go (no LLM cost), shown ahead of the excerpts - the model
// reads "sword 115, lantern 40, carriage 22, no modern words" far more
// reliably than it infers an era from 24k characters of prose. The
// excerpts still matter for what the counts can't see: Jake's Magical
// Market is a modern world whose technology an apocalypse killed, so
// "technology" is what works in the story's present, not what exists. The
// quotes aren't stored. Recurring locations were
// asked for too and dropped: eight short excerpts can't show what recurs,
// and it listed one-off scenes ("a green glass wall with a hidden treasure
// chest"). Knowing where each scene is needs a per-scene pass, not this.
//
// Deliberately not in SystemPrompts: one call per book isn't worth a
// permanently resident primed sequence.
const settingSystemPrompt = `You are preparing notes on a novel's setting for the sound, lighting and casting designers of its audiobook. You will be given counts of technology words across the whole book, counts of British and American idiom in its dialogue, then excerpts from across it. Describe the world the story takes place in.

First collect evidence, then decide from it:
- "evidence": up to 5 short exact quotes (at most 10 words each) from the excerpts showing what light sources, transport, weapons or devices characters actually use - or can no longer use - in the story's present. Never a comparison ("like a machine gun") or a memory of another life or world.
- "technology": what actually works in the story's present, for everyday light, transport and communication, e.g. "pre-industrial: lanterns, candles, carriages" or "modern: electric light, cars, phones". The counts show which technologies the book talks about at all: a few modern words against many pre-industrial ones are usually comparisons or memories. A modern world whose technology has stopped working (after a collapse, say) is described by what still works.
- "era": when the story takes place, consistent with "technology", e.g. "medieval-style secondary world", "late Victorian", "present day", "far future". Call it present day or modern only if the excerpts show the modern world - its cities, devices or people - not from a few stray modern words. Magic, monsters, dungeons, ruins and game-like systems say nothing about the era either way.
- "place": the region or world, e.g. "a fictional desert empire", "Crete and the Mediterranean", "London", "a generation starship".
- "accent": the accent an audiobook should give most of the main characters - always name one. A stated nationality or hometown, or the real-world place the story is set, decides it ("a London accent", "Scottish", "General American"). Otherwise, invented worlds included, follow the dialogue idiom counts: clearly more British idiom is "standard British", clearly more American is "General American"; with neither clearly ahead, "General American" - unless the excerpts show a mixed cast (characters from both, a story that moves between countries), then name both, the protagonist's first ("mixed: British, General American").
- "genre": 1-3 words, e.g. "epic fantasy", "cozy mystery", "LitRPG".
- "narrator": if the book is told in the first person ("I"), that narrator's name as other characters call them in the excerpts; "" for a third-person book, or when the narrator is never named.
- Only state what the text supports; use "" for anything it doesn't show. Each field at most 12 words. Never mention plot events or character names.

Reply with a compact JSON object on a single line only, no other text, no markdown code fence:

{"evidence": ["...", "..."], "technology": "...", "era": "...", "place": "...", "accent": "...", "genre": "...", "narrator": "..."}`

// BookSetting is one book's setting profile - store.BookSetting's own
// shape, kept as a plain value here rather than importing store (see
// ParagraphInput).
type BookSetting struct {
	Era        string
	Technology string
	Place      string
	Accent     string
	Genre      string
	// Narrator is a first-person book's narrator (the "I"), "" for a
	// third-person book or an unnamed narrator. Attribution credits "I
	// said" lines to them (Roster.FirstPerson).
	Narrator string
}

// IsZero reports whether s says nothing at all.
func (s *BookSetting) IsZero() bool {
	return s == nil || s.Era == "" && s.Technology == "" && s.Place == "" && s.Accent == "" && s.Genre == "" && s.Narrator == ""
}

// promptLines renders the world for the ambience and lighting prompts -
// era, technology, place, genre as "- field: value" lines, empty fields
// left out.
func (s *BookSetting) promptLines() string {
	if s.IsZero() {
		return ""
	}
	var sb strings.Builder
	line := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&sb, "- %s: %s\n", k, v)
		}
	}
	line("era", s.Era)
	line("technology", s.Technology)
	line("place", s.Place)
	line("genre", s.Genre)
	return sb.String()
}

// castingLines renders the fields characterization uses: where and when,
// and the main characters' accent - never the technology.
func (s *BookSetting) castingLines() string {
	if s.IsZero() {
		return ""
	}
	// A mixed cast's accents ("mixed: British, General American") are
	// shown as the first with the rest aside: shown as written, the
	// casting came back "a mixed British-General American accent".
	accent := s.Accent
	if rest, ok := strings.CutPrefix(accent, "mixed:"); ok {
		first, others, _ := strings.Cut(rest, ",")
		accent = strings.TrimSpace(first)
		if others = strings.TrimSpace(others); others != "" {
			accent += " (some of the cast: " + others + ")"
		}
	}
	var sb strings.Builder
	for _, kv := range [][2]string{{"era", s.Era}, {"place", s.Place}, {"main characters' accent", accent}, {"genre", s.Genre}} {
		if kv[1] != "" {
			fmt.Fprintf(&sb, "- %s: %s\n", kv[0], kv[1])
		}
	}
	return sb.String()
}

// settingSampleChapters is how many chapters SettingSample draws from,
// spread across the book, and settingSampleChars how much of each - ~24k
// characters (~6k tokens) in all: still one call, well inside a slot's
// context (SlotNCtx never goes below 12288), and prefill is cheap next to
// decoding. 8 x 1000 was too little to tell a modern-world fantasy from a
// medieval one (see settingSystemPrompt's doc comment).
const (
	settingSampleChapters = 16
	settingSampleChars    = 1500
)

// settingMinChapterWords skips front/back matter (a title page, a
// dedication) as a sample.
const settingMinChapterWords = 200

// SettingSampleChapters picks which of n chapters (by position) to sample
// for the setting call: settingSampleChapters of them, evenly spread and
// always including the first. Candidates past the wanted count are
// fallbacks the caller may use when a pick is too short (front matter).
func SettingSampleChapters(n int) []int {
	if n <= 0 {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	add := func(i int) {
		if i >= 0 && i < n && !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	for k := 0; k < settingSampleChapters; k++ {
		add(k * n / settingSampleChapters)
	}
	for i := 0; i < n; i++ { // fallbacks, in order
		add(i)
	}
	return out
}

// SettingSample is the start of one chapter's text for DescribeSetting,
// or "" when the chapter is too short to be story (front/back matter).
func SettingSample(paragraphs []ParagraphInput) string {
	words := 0
	for _, p := range paragraphs {
		words += len(strings.Fields(p.Text))
	}
	if words < settingMinChapterWords {
		return ""
	}
	var sb strings.Builder
	for _, p := range paragraphs {
		t := oneLine(p.Text)
		if t == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(t)
		if sb.Len() >= settingSampleChars {
			break
		}
	}
	out := sb.String()
	if len(out) > settingSampleChars {
		out = out[:settingSampleChars] + "..."
	}
	return out
}

// SettingSampleCount is how many non-empty samples DescribeSetting wants.
const SettingSampleCount = settingSampleChapters

// techWords are the words TechCounts counts, by technology level - each a
// lowercase stem matched at a word start, so "phone" covers "phones".
// Ambiguous words are left out: "plane" (geometry), "torch" (a flashlight
// in British English), "bow" (the gesture), "light".
var techWords = []struct {
	level string
	words []string
}{
	{"modern", []string{"phone", "smartphone", "car", "truck", "electric", "computer", "laptop", "internet", "wi-fi", "email", "television", "tv", "radio", "flashlight", "lightbulb", "neon", "fluorescent", "gun", "rifle", "pistol", "airplane", "aircraft", "helicopter", "highway", "subway", "elevator", "motorcycle"}},
	{"industrial", []string{"steam", "locomotive", "railway", "telegraph", "gaslight", "gas lamp", "revolver", "musket", "factory"}},
	{"pre-industrial", []string{"sword", "candle", "lantern", "carriage", "horse", "cart", "spear", "quill", "hearth", "oil lamp", "castle", "armour", "armor", "crossbow"}},
}

// TechCounts is how often each techWords word appears across a book -
// DescribeSetting's cheap, whole-book signal for the era (see
// settingSystemPrompt's doc comment).
type TechCounts map[string]int

// techWordRes is techWords compiled: each word at a word start, allowing a
// plural/derived ending ("swords", "electricity").
var techWordRes = func() map[string]*regexp.Regexp {
	out := map[string]*regexp.Regexp{}
	for _, g := range techWords {
		for _, w := range g.words {
			out[w] = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(w) + `(?:s|es|ity|al|ic)?\b`)
		}
	}
	return out
}()

// Add counts text's technology words into c.
func (c TechCounts) Add(text string) {
	for w, re := range techWordRes {
		if n := len(re.FindAllStringIndex(text, -1)); n > 0 {
			c[w] += n
		}
	}
}

// String renders c for the setting prompt: one line per level, words by
// count, "none" for a level with no hits.
func (c TechCounts) String() string {
	var sb strings.Builder
	for _, g := range techWords {
		type wc struct {
			w string
			n int
		}
		var hits []wc
		for _, w := range g.words {
			if c[w] > 0 {
				hits = append(hits, wc{w, c[w]})
			}
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].n > hits[j].n })
		parts := make([]string, len(hits))
		for i, h := range hits {
			parts[i] = fmt.Sprintf("%s %d", h.w, h.n)
		}
		if len(parts) == 0 {
			parts = []string{"none"}
		}
		fmt.Fprintf(&sb, "- %s: %s\n", g.level, strings.Join(parts, ", "))
	}
	return sb.String()
}

// idiomWords are the words IdiomCounts counts in dialogue, by the English
// they mark - specific ones only: "yeah", "gonna", "guys", "reckon",
// "proper" and "brilliant" are everywhere in both.
var idiomWords = []struct {
	dialect string
	words   []string
}{
	{"British", []string{"mate", "bloody", "arse", "bloke", "mum", "innit", "cheers", "sodding", "bugger", "quid", "knackered", "rubbish", "whilst", "fortnight", "telly", "wanker", "oi", "cuppa", "bollocks", "lad", "lads"}},
	{"American", []string{"y'all", "mom", "dude", "buddy", "gotten", "awesome", "ass", "asshole", "folks", "candy", "apartment", "sidewalk", "vacation"}},
	// Counted so one character's dialect shows against the cast's: Captain
	// West says "aye" 7 times and "bairn" in 112 lines, and was cast
	// General British.
	{"Scottish or Northern English", []string{"aye", "bairn", "bairns", "wee", "lass", "lassie", "laddie", "och", "nae", "dinnae", "cannae", "didnae", "ken", "canny"}},
}

// IdiomCounts is how often each idiomWords word appears in a book's
// dialogue - DescribeSetting's signal for the cast's default accent when
// the text never states where they're from (every book in this library:
// Spire's Spite's dialogue has "bloody" 89 times, "arse", "oi"; Jake's
// Magical Market's "dude", "buddy"). Spelling was tried and dropped: Arise
// is set in London with British characters but American spelling.
type IdiomCounts map[string]int

var idiomWordRes = func() map[string]*regexp.Regexp {
	out := map[string]*regexp.Regexp{}
	for _, g := range idiomWords {
		for _, w := range g.words {
			out[w] = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(w) + `\b`)
		}
	}
	return out
}()

// Add counts one dialogue line's idiom words into c.
func (c IdiomCounts) Add(text string) {
	text = strings.ReplaceAll(text, "\u2019", "'")
	for w, re := range idiomWordRes {
		if n := len(re.FindAllStringIndex(text, -1)); n > 0 {
			c[w] += n
		}
	}
}

// String renders c for the setting prompt: one line per dialect with its
// total and words by count.
func (c IdiomCounts) String() string {
	var sb strings.Builder
	for _, g := range idiomWords {
		total := 0
		var parts []string
		words := slices.Clone(g.words)
		sort.SliceStable(words, func(i, j int) bool { return c[words[i]] > c[words[j]] })
		for _, w := range words {
			if c[w] > 0 {
				total += c[w]
				parts = append(parts, fmt.Sprintf("%s %d", w, c[w]))
			}
		}
		if len(parts) == 0 {
			parts = []string{"none"}
		}
		fmt.Fprintf(&sb, "- %s, %d in all: %s\n", g.dialect, total, strings.Join(parts, ", "))
	}
	return sb.String()
}

type settingRaw struct {
	Era        string `json:"era"`
	Technology string `json:"technology"`
	Place      string `json:"place"`
	Accent     string `json:"accent"`
	Genre      string `json:"genre"`
	Narrator   string `json:"narrator"`
}

// DescribeSetting writes a book's setting profile from counts (the whole
// book's TechCounts), idioms (its dialogue's IdiomCounts) and samples (see
// SettingSample) - one call per book, the whole setting pass.
func (c *Client) DescribeSetting(ctx context.Context, bookTitle, author string, counts TechCounts, idioms IdiomCounts, samples []string) (BookSetting, error) {
	return generateAndParse(ctx, c, settingSystemPrompt, c.settingUserPrompt(bookTitle, author, counts, idioms, samples), 0, settingMaxTokens, parseSetting)
}

func (c *Client) settingUserPrompt(bookTitle, author string, counts TechCounts, idioms IdiomCounts, samples []string) string {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\n", bookTitle)
	if author != "" {
		fmt.Fprintf(&user, "Author: %s\n", author)
	}
	user.WriteString("\nTechnology words across the whole book (every use, including comparisons and memories):\n")
	user.WriteString(counts.String())
	user.WriteString("\nBritish and American idiom in the dialogue (quoted speech only):\n")
	user.WriteString(idioms.String())
	user.WriteString("\nExcerpts, in book order:\n")
	for i, s := range samples {
		fmt.Fprintf(&user, "\n[%d] %s\n", i+1, s)
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	return user.String()
}

func parseSetting(content string) (BookSetting, error) {
	value, found := firstJSONValue(content, '{')
	if !found {
		return BookSetting{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var raw settingRaw
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return BookSetting{}, fmt.Errorf("parse JSON object: %w", err)
	}
	s := BookSetting{
		Era:        cleanSettingField(raw.Era),
		Technology: cleanSettingField(raw.Technology),
		Place:      cleanSettingField(raw.Place),
		Accent:     cleanSettingField(raw.Accent),
		Genre:      cleanSettingField(raw.Genre),
		Narrator:   cleanSettingField(raw.Narrator),
	}
	if s.IsZero() {
		return BookSetting{}, fmt.Errorf("empty setting")
	}
	return s, nil
}

// cleanSettingField trims s and folds the model's "don't know" spellings
// to "", which the consuming prompts leave out.
func cleanSettingField(s string) string {
	s = strings.TrimSpace(s)
	switch strings.Trim(strings.ToLower(s), ".()[] ") {
	case "", "none", "n/a", "na", "null", "unknown", "unclear", "not specified", "unspecified":
		return ""
	}
	return s
}
