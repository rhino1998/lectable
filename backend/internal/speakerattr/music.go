package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// MusicRegionResult is one tone region ScoreMusic judged - see
// store.MusicRegionInput, which this maps directly onto (kept as a plain
// value here rather than importing store, matching this package's
// existing rule - see ParagraphInput). Carries no end of its own: a region
// runs to the next one's start (store.MusicRegion.EndIdx), so regions are
// contiguous and never overlap by construction.
type MusicRegionResult struct {
	StartIdx int
	Mood     string
	Prompt   string // Stable Audio music prompt - see buildMusicPrompt
	// Ambience is the Stable Audio prompt for this region's ambient
	// soundscape layer (see buildAmbiencePrompt), mixed under the music -
	// "" when the region has no clear physical setting to hear.
	Ambience   string
	Transition string // "cut" (a new scene: fresh palette) or "continuation"
}

// musicBoundary is a region's start and transition as sceneBoundaries
// places it, before describeMusicRegion has filled in its Mood/Prompt from
// the region's paragraph span. Never exported: only ScoreMusic's final,
// combined MusicRegionResult is a public shape.
type musicBoundary struct {
	StartIdx   int
	Transition string
	// split marks a boundary splitLongRegions inserted into one tone
	// region purely for length - ScoreMusic reuses the region's own
	// description for it rather than asking the model again.
	split bool
}

// minRegionParagraphs is mergeShortRegions' floor on a region's paragraph
// span: a 1-3 paragraph region is almost always a line or two of dialogue,
// not a real scene.
const minRegionParagraphs = 5

// mergeShortRegions drops any region shorter than minRegionParagraphs
// paragraphs from out, folding its span into whichever region precedes
// it (out's own regions carry no explicit end - so simply removing a
// too-short entry is exactly a merge: the region before it implicitly
// grows to cover the gap once EndIdx is later derived from these starts,
// store.AppendMusicRegions's own job). Runs on musicBoundary, before anything
// is described - a region merged away here never costs a
// describeMusicRegion call.
//
// A deterministic floor rather than something asked of a model: region
// starts once came from an LLM pass (see sceneBoundaries), and asking it for
// fewer, longer regions never held - the result swung between a region on
// nearly every paragraph and one region for everything. Summary scenes can
// be as short too (a scene of two paragraphs between longer ones).
//
// Never merges away a "cut" region, no matter how short: a "cut" is a
// deliberate, sharp musical transition its source specifically chose to
// mark distinct; merging it away would silently defeat the entire reason
// it was marked "cut" rather than "continuation" in the first place. The
// first and last regions in regions are also always kept regardless of
// span: the first has nothing before it to merge into, and the last has
// no reliably-known span from inside this package alone (its own true end
// is the chapter's own last paragraph, tracked by the caller - see
// AppendMusicRegions - not regions itself).
func mergeShortRegions(regions []musicBoundary) []musicBoundary {
	if len(regions) <= 1 {
		return regions
	}
	out := make([]musicBoundary, 0, len(regions))
	out = append(out, regions[0])
	for i := 1; i < len(regions)-1; i++ {
		r := regions[i]
		span := regions[i+1].StartIdx - r.StartIdx
		if span < minRegionParagraphs && r.Transition != "cut" {
			continue // merge into whichever region precedes it
		}
		out = append(out, r)
	}
	out = append(out, regions[len(regions)-1])
	return out
}

// maxRegionParagraphs caps how many logical paragraphs (a non-Inline
// paragraph plus every Inline continuation folded onto it - one complete
// narrated beat) a single region may span before splitLongRegions
// deterministically splits it. mergeShortRegions' minRegionParagraphs is
// the floor this is the ceiling for: one unbroken scene can run a whole
// chapter, and a region is one music clip, sized to its narration. The
// splits are length-only - they share their scene's one description.
const maxRegionParagraphs = 10

// splitLongRegions deterministically inserts a "continuation" boundary
// after every maxRegionParagraphs-th logical paragraph within any region
// that would otherwise run longer than that - a hard ceiling layered on
// top of whatever sceneBoundaries/mergeShortRegions already decided,
// counted in logical paragraphs (an Inline paragraph is never a split
// point: it continues the line before it mid-beat). Runs after mergeShortRegions, not
// before: splitting first and merging second would risk mergeShortRegions
// immediately folding a deterministically-split remainder back into its
// own predecessor if that remainder came out shorter than
// minRegionParagraphs, silently undoing the split it was never supposed
// to touch. A split is always "continuation", never "cut" - a length cap
// isn't a real musical event, and forcing one here would be exactly the
// kind of choppy, unwarranted hard cross-fade a continuation exists to
// avoid.
func splitLongRegions(regions []musicBoundary, paragraphs []ParagraphInput) []musicBoundary {
	if len(regions) == 0 || len(paragraphs) == 0 {
		return regions
	}
	logicalStarts := make([]int, 0, len(paragraphs))
	for _, p := range paragraphs {
		if !p.Inline {
			logicalStarts = append(logicalStarts, p.Idx)
		}
	}
	lastIdx := paragraphs[len(paragraphs)-1].Idx

	out := make([]musicBoundary, 0, len(regions))
	for i, r := range regions {
		regionEnd := lastIdx
		if i+1 < len(regions) {
			regionEnd = regions[i+1].StartIdx - 1
		}
		out = append(out, r)
		count := 0
		for _, idx := range logicalStarts {
			if idx < r.StartIdx || idx > regionEnd {
				continue
			}
			count++
			if count > 1 && (count-1)%maxRegionParagraphs == 0 {
				out = append(out, musicBoundary{StartIdx: idx, Transition: "continuation", split: true})
			}
		}
	}
	return out
}

// sceneBoundaries places paragraphs' region starts: the chapter summary's
// scenes (notes, nil for none), then the length splits. The first region
// opens on paragraphs' first line and is the only "cut" decided here; each
// later scene starts a "continuation", which the describe call - reading the
// region's text against the one before - turns into a cut where the scene
// really is a new one (see ScoreMusic). A scene start on an Inline line
// moves back to the line it continues, and a scene shorter than
// minRegionParagraphs joins the one before it (mergeShortRegions). With no
// summary the chapter is one region, split for length.
//
// This replaced an LLM pass that read the whole chapter in numbered batches
// to mark tone shifts. Measured 2026-10-10 on 8 chapters from 8 books, it
// marked nothing past the chapter's opening in 6 of them (so their regions
// were the length splits alone, and every cut came from the describe call,
// at whichever 30-paragraph block the scene change fell in), while the
// summaries, already written, held 36 scenes for the same chapters - at ~9
// calls and ~7K prompt tokens a chapter, a fifth of the pass's time.
func sceneBoundaries(notes *ChapterSummary, paragraphs []ParagraphInput) []musicBoundary {
	if len(paragraphs) == 0 {
		return nil
	}
	out := []musicBoundary{{StartIdx: paragraphs[0].Idx, Transition: "cut"}}
	if notes != nil {
		pos := make(map[int]int, len(paragraphs))
		for i, p := range paragraphs {
			pos[p.Idx] = i
		}
		starts := make([]int, 0, len(notes.Scenes))
		for _, sc := range notes.Scenes {
			i, ok := pos[sc.StartIdx]
			if !ok {
				continue // before a resumed run's start, or not a paragraph at all
			}
			for i > 0 && paragraphs[i].Inline {
				i--
			}
			starts = append(starts, paragraphs[i].Idx)
		}
		sort.Ints(starts)
		for _, idx := range starts {
			if idx > out[len(out)-1].StartIdx {
				out = append(out, musicBoundary{StartIdx: idx, Transition: "continuation"})
			}
		}
	}
	return splitLongRegions(mergeShortRegions(out), paragraphs)
}

// musicDescribeMaxTokens bounds one describe call's generated JSON - a mood
// label, a handful of short music fields and one ambience description.
const musicDescribeMaxTokens = 1024

// musicDescribeSystemPrompt is the describe call: given the real text of one
// region sceneBoundaries already placed (rather than a numbered listing
// spanning several regions at once, the way the old single-pass prompt
// worked), describe that region's own background bed - a music cue and,
// where the scene has a clear physical setting, an ambience layer mixed
// under it (see musicgen.MixAmbience). Splitting this out from the
// boundary judgment is the lesson a folded attribute-and-describe call
// already taught (two judgments per line hurt both) - asking one call to both
// place a boundary and justify a full composer brief for it in the same
// breath left the single-pass version measurably worse on real chapters
// (ad hoc comparison against two real book chapters: the combined pass
// produced 15 regions for a 63-paragraph chapter - its own prompt calls 4+
// "a sign something went wrong" - with all but one marked "cut",
// apparently because justifying a differently-worded brief made every tone
// change look like a hard cut; splitting the judgment out produced 8
// regions with far fewer spurious cuts on the same chapter, and also
// sidestepped a batch-seam parse failure that had permanently lost the
// back half of another chapter under the combined prompt).
//
// The model fills structured fields (genres, instruments, bpm, a short
// description) rather than writing the final Stable Audio prompt itself:
// buildMusicPrompt assembles them into Stable Audio 3's own documented
// AudioSparx tag shape ("TrackType: Music, VocalType: Instrumental,
// Genre: ..., Instruments: ..., <description>, <n> BPM" - see the
// stable-audio-3 prompting guide), so the tag syntax can never drift or be
// half-remembered by a 4B model. The previous region's own description is
// passed back in (musicDescribeContext) so a "continuation" keeps its
// palette and a setting that hasn't changed keeps byte-identical ambience,
// which is what lets the ambience layer be continuation-seeded across
// regions (jobs.Manager.musicRegionSeeds). For an unchanged place the model
// answers "same" (ambienceSameMarker) rather than copying the string out:
// 59% of non-opening describe calls in the library (2026-10-10) kept the
// previous ambience, ~30 of each such call's ~115 generated tokens.
//
// The ambience examples are deliberately era-neutral: one used to read
// "busy medieval tavern interior, ..., crackling hearth fire, wooden room",
// and the model copied it verbatim for bars in present-day books (Arise's
// Greek taverna), which is where the lighting sub-pass's "modern dive bar
// as a medieval tavern" came from. The book's setting notes
// (BookSetting) now supply the era instead.
const musicDescribeSystemPrompt = `You are the sound designer for an audiobook. Behind the narrator's voice plays a quiet background bed made of two layers: an instrumental music underscore, and (when the scene has a clear physical setting) an ambient soundscape of that place - ocean waves, a busy tavern, city streets, rain on a roof, a forest at night. You are designing the bed for one contiguous stretch of a novel chapter, which an earlier pass has already identified as a single tone region. You will be given the full text of every paragraph within that region, in order, possibly notes on the book's setting, and possibly a description of the region that plays just before it.

SCENE - decide this first, when a previous region is given:
- "new" when this region starts a different scene from the previous one: the story jumps forward in time or to a different place, switches to other characters, or something shocking erupts out of calm (a twist, sudden violence).
- "same" when it carries on the same scene - the same people in the same place and moment, even if the mood has shifted.
- With no previous region, or when told an earlier pass already marked a new scene, answer "new".

MUSIC - an underscore that sits under spoken narration, never competing with it:
- Match the region's emotional register, and pick a palette that fits the book's world - its setting notes, when given (historical or fantasy settings suit acoustic, folk or orchestral instruments; futuristic settings suit synths and electronic textures; contemporary settings can go either way).
- Keep it sparse and steady: sustained pads, soft textures, low or mid-register instruments, gentle pulses. Avoid a prominent lead melody, bright high-pitched solo instruments, heavy drums, sudden drops or big builds - anything that would fight the voice for attention. Even tense or action scenes stay restrained: tension comes from rhythm, low drones and dissonance, not loudness.
- Instrumental only - never vocals, choirs singing words, or lyrics.
- For the "same" scene, keep the previous region's genre and core instruments, evolving the mood and energy rather than switching style. For a "new" scene, choose a fresh palette that fits it, rather than reusing the previous one out of habit.

AMBIENCE - the continuous sound of the place where this region physically happens:
- Describe it the way a field recording is labeled: the sound sources, how busy or sparse they are, the space and perspective (e.g. "ocean waves breaking on a rocky shore, distant gulls, steady sea wind, wide open outdoor perspective"; "crowded drinking hall, low indistinct crowd murmur, clinking glasses, wooden room"; "night city street, distant traffic hum, occasional far-off car horn, light rain on pavement").
- Only continuous, loopable background sound - never a one-off event (no single door slam, gunshot, scream, explosion or line of speech), and never music. Crowds are fine as indistinct murmur, never as intelligible speech.
- Use "" (empty) when there is no clear, sustained physical setting to hear: a quiet interior with nothing notable audible, a dream or abstract space, a stretch of reflection or summary, or a setting you would only be guessing at.
- Keep the soundscape true to the book's era and technology from its setting notes: no engines, traffic or electronics in a pre-industrial world, no hearths, horses or tavern crowds in a modern city unless the text puts them there. The notes describe the world, not where this region is - the place still comes from the text.
- Scene notes, when given, say where and when this stretch happens - prefer them to guessing, but the region's own text wins if it has clearly moved on.
- Decide the setting from this region's own text first. If the previous region's ambience is given and this text clearly still takes place in that same place, answer with the single word "same" instead of writing it out again. If the characters have moved somewhere else, or the text no longer fits that place, write a new one (or "") - never carry over a place the text has left.

Never mention plot events, character names or story specifics in any field - describe only sound, as if briefing someone who has never read the book.

The "music" field is read by a text-to-music model, not a person: write it as a short run of comma-separated sound descriptors (mood, energy, dynamics, texture), at most 15 words - e.g. "slow, brooding, low sustained drones, faint uneasy pulse, sparse". Don't repeat the instruments, don't explain what the music reflects or mirrors, and never refer to the narration or the story.

Reply with a compact JSON object on a single line only, no other text, no markdown code fence:

{"scene": "same" | "new", "mood": "<2-4 word emotional label, e.g. tense confrontation, quiet melancholy, warm banter>", "setting": "<2-6 word physical setting, or empty>", "genres": ["<1-2 genres, e.g. Ambient, Cinematic, Folk, Orchestral, Electronic>"], "instruments": ["<2-4 instruments or textures>"], "bpm": <integer tempo, 50-120>, "music": "<at most 15 words of comma-separated sound descriptors>", "ambience": "<field-recording style description, "same", or empty>"}`

// musicDescription is the describe call's finished judgment for one region - the
// assembled Stable Audio prompts (see buildMusicPrompt/
// buildAmbiencePrompt), plus the raw fields the next region's own call is
// shown as context (see musicDescribeContext).
type musicDescription struct {
	Mood     string
	Setting  string
	Prompt   string // assembled music prompt
	Ambience string // assembled ambience prompt, "" for none
	// music/ambience are the model's own raw sentences, before assembly -
	// what the next region's own call sees as its predecessor, so it can
	// copy an unchanged ambience back verbatim.
	music       string
	rawAmbience string
	genres      []string
	instruments []string
	// newScene is the model's "scene" judgment - see ScoreMusic, which
	// turns it into a "cut".
	newScene bool
}

type musicDescriptionRaw struct {
	Scene       string      `json:"scene"`
	Mood        string      `json:"mood"`
	Setting     string      `json:"setting"`
	Genres      []string    `json:"genres"`
	Instruments []string    `json:"instruments"`
	BPM         looseNumber `json:"bpm"`
	Music       string      `json:"music"`
	Ambience    string      `json:"ambience"`
	// Prompt is the old single-string shape - accepted as a fallback for
	// Music so a model that ignores the new schema still yields a usable
	// cue instead of a failed parse.
	Prompt string `json:"prompt"`
}

// musicDescribeContext is the previous region's own description, as shown
// to the next region's describe call - nil for a chapter's (or resumed
// run's) first region.
type musicDescribeContext struct {
	prev       *musicDescription
	transition string
}

func (c *Client) describeMusicRegion(ctx context.Context, bookTitle, chapterTitle string, setting *BookSetting, scene *Scene, regionParagraphs []ParagraphInput, mctx musicDescribeContext) (musicDescription, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	if lines := setting.promptLines(); lines != "" {
		fmt.Fprintf(&user, "\nBook setting notes (the world as a whole):\n%s\n", lines)
	}
	user.WriteString(scene.promptLines())
	switch {
	case mctx.prev == nil:
		user.WriteString("This is the chapter's opening region - its scene is \"new\".\n")
	case mctx.transition == "cut":
		user.WriteString("An earlier pass already marked this region as a new scene - its scene is \"new\".\n")
	}
	if p := mctx.prev; p != nil {
		user.WriteString("\nPrevious region:\n")
		fmt.Fprintf(&user, "- mood: %s\n", p.Mood)
		fmt.Fprintf(&user, "- setting: %s\n", orNone(p.Setting))
		fmt.Fprintf(&user, "- genres: %s\n", strings.Join(p.genres, ", "))
		fmt.Fprintf(&user, "- instruments: %s\n", strings.Join(p.instruments, ", "))
		fmt.Fprintf(&user, "- music: %s\n", p.music)
		fmt.Fprintf(&user, "- ambience: %q\n", p.rawAmbience)
	}
	user.WriteString("\nRegion text:\n")
	for i, p := range regionParagraphs {
		if p.Inline {
			fmt.Fprintf(&user, " %s", oneLine(p.Text))
			continue
		}
		if i > 0 {
			user.WriteString("\n\n")
		}
		user.WriteString(oneLine(p.Text))
	}
	user.WriteString("\n")
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}

	raw, err := generateAndParse(ctx, c, musicDescribeSystemPrompt, user.String(), 0, musicDescribeMaxTokens, parseMusicRegionDescription)
	if err != nil {
		return musicDescription{}, err
	}
	d := musicDescription{
		Mood:        raw.Mood,
		Setting:     raw.Setting,
		Prompt:      buildMusicPrompt(raw),
		music:       raw.Music,
		rawAmbience: raw.Ambience,
		genres:      raw.Genres,
		instruments: raw.Instruments,
		newScene:    strings.EqualFold(strings.TrimSpace(raw.Scene), "new"),
	}
	// An unchanged setting must produce a byte-identical ambience prompt
	// (see musicDescribeSystemPrompt): "same" takes the previous region's
	// string as it stands ("" when there is none to keep), and a copy the
	// model wrote out anyway, which can drift by a character or two, is
	// snapped back - either would otherwise silently lose the seeded
	// continuation.
	switch p := mctx.prev; {
	case isSameAmbienceMarker(raw.Ambience):
		d.rawAmbience = ""
		if p != nil {
			d.rawAmbience = p.rawAmbience
		}
	case p != nil && p.rawAmbience != "" && sameAmbience(raw.Ambience, p.rawAmbience):
		d.rawAmbience = p.rawAmbience
	}
	d.Ambience = buildAmbiencePrompt(d.rawAmbience)
	return d, nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// ambienceSameMarker is what the describe call answers for "ambience" when
// the region is still in the previous region's place - see
// musicDescribeSystemPrompt.
const ambienceSameMarker = "same"

// isSameAmbienceMarker reports whether s is ambienceSameMarker, allowing
// the punctuation and wording a model wraps a one-word answer in.
func isSameAmbienceMarker(s string) bool {
	switch strings.Trim(strings.ToLower(s), ".()[]\"' ") {
	case ambienceSameMarker, "same as previous", "same as before", "same as above", "unchanged":
		return true
	}
	return false
}

// sameAmbience reports whether a is the previous region's ambience b,
// copied back with only case/whitespace/punctuation drift.
func sameAmbience(a, b string) bool {
	norm := func(s string) string {
		var sb strings.Builder
		for _, r := range strings.ToLower(s) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				sb.WriteRune(r)
			}
		}
		return sb.String()
	}
	return norm(a) != "" && norm(a) == norm(b)
}

func parseMusicRegionDescription(content string) (musicDescriptionRaw, error) {
	value, found := firstJSONValue(content, '{')
	if !found {
		return musicDescriptionRaw{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var raw musicDescriptionRaw
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return musicDescriptionRaw{}, fmt.Errorf("parse JSON object: %w", err)
	}
	raw.Mood = strings.TrimSpace(raw.Mood)
	raw.Setting = strings.TrimSpace(raw.Setting)
	raw.Music = strings.TrimSpace(raw.Music)
	if raw.Music == "" {
		raw.Music = strings.TrimSpace(raw.Prompt)
	}
	raw.Ambience = cleanAmbience(raw.Ambience)
	raw.Genres = cleanTagList(raw.Genres, 2)
	raw.Instruments = cleanTagList(raw.Instruments, 4)
	if raw.Music == "" && len(raw.Instruments) == 0 {
		return musicDescriptionRaw{}, fmt.Errorf("empty music description")
	}
	return raw, nil
}

// cleanAmbience normalizes the model's "no ambience" spellings ("none",
// "n/a", "silence") to "" - an ambience layer is an extra Stable Audio
// render, so a placeholder must never get sent as a real prompt.
func cleanAmbience(s string) string {
	s = strings.TrimSpace(s)
	switch strings.Trim(strings.ToLower(s), ".()[] ") {
	case "", "none", "n/a", "na", "null", "empty", "silence", "no ambience", "nothing":
		return ""
	}
	return s
}

// cleanTagList trims, drops empties/duplicates and anything containing the
// ", " / ":" separators buildMusicPrompt's own tag syntax relies on, and
// caps the list at max entries.
func cleanTagList(in []string, max int) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(strings.ReplaceAll(s, ":", ""))
		if s == "" || strings.Contains(s, ",") || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
		if len(out) == max {
			break
		}
	}
	return out
}

// musicMinBPM/musicMaxBPM clamp the model's own tempo - anything outside
// is either a hallucination or far too busy to sit under narration.
const (
	musicMinBPM = 40
	musicMaxBPM = 140
)

// buildMusicPrompt assembles raw into Stable Audio 3's own tag shape - see
// musicDescribeSystemPrompt's own doc comment. "VocalType: Instrumental" is
// the guide's own documented way to keep vocals out (the default
// guidance_scale of 1.0 makes a negative prompt a no-op, so this has to
// live in the positive prompt), and "underscore, background music" steer
// toward the stock/library-music end of its training data.
func buildMusicPrompt(raw musicDescriptionRaw) string {
	parts := []string{"TrackType: Music", "VocalType: Instrumental"}
	for _, g := range raw.Genres {
		parts = append(parts, "Genre: "+g)
	}
	if len(raw.Instruments) > 0 {
		parts = append(parts, "Instruments: "+strings.Join(raw.Instruments, ", "))
	}
	if raw.Music != "" {
		parts = append(parts, strings.TrimRight(raw.Music, ". "))
	}
	parts = append(parts, "subtle background underscore")
	if bpm := int(raw.BPM); bpm > 0 {
		bpm = max(musicMinBPM, min(musicMaxBPM, bpm))
		parts = append(parts, fmt.Sprintf("%d BPM", bpm))
	}
	return strings.Join(parts, ", ")
}

// buildAmbiencePrompt wraps a raw ambience description in the guide's own
// "TrackType: SFX" tag (sound rather than music) plus Freesound-style
// field-recording descriptors - "" stays "" (no ambience layer).
func buildAmbiencePrompt(raw string) string {
	if raw == "" {
		return ""
	}
	return "TrackType: SFX, ambience field recording, " + strings.TrimRight(raw, ". ") + ", continuous seamless background atmosphere, no music"
}

// musicDescribeTextRegions is how many regions' text one
// describeMusicRegion call is written from: a scene is described once,
// however long it runs, from its opening regions - the scene's first region
// plus the splitLongRegions chunks right after it - and every later chunk
// of it takes that description without a call. Describe calls were ~70% of
// the pass's model time. Until 2026-10-12 a scene was described again every
// three regions; measured then on 30 chapters (cmd/ambprobe), those repeat
// calls were 42% of all describe calls and changed the scene, ambience,
// genres or instruments in 16% of cases, against 61% for a call at a scene's
// start. What a long scene gives up is its mood label and tempo moving as it
// goes. The text shown stays capped so one prompt is a bounded ~30
// paragraphs. A chapter with no summary scenes has nothing saying where a
// scene ends, and is still described every three regions.
const musicDescribeTextRegions = 3

// ScoreMusic scores chapterTitle's paragraphs into background-music regions
// (see MusicRegionResult): sceneBoundaries places where each region starts,
// from the chapter summary's scenes (notes, nil for none) and with no model
// call; then describeMusicRegion writes each region's mood label, music
// prompt and ambience prompt from its own paragraph span - one call per
// region (a length-only split shares its region's call -
// musicDescribeTextRegions) - and decides whether it opens a new scene, a
// "cut". Describe calls run strictly in region order, each shown the
// previous region's description (musicDescribeContext) so palettes carry
// through continuations and an unchanged setting keeps an identical
// ambience prompt. setting (nil for none) is the book's setting profile;
// the scene covering each region's start is shown to its describe call.
//
// shouldPause (nil-safe: nil never pauses) is checked between describe
// calls, never before the first. out only ever contains regions that were
// described; remaining is the paragraphs from wherever it stopped (a pause
// or a failed call) on - the same "out reflects real, finished work"
// contract AttributeChapter documents in full. httpapi.ambienceChapter
// passes nil and resumes a failed run from remaining's start.
func (c *Client) ScoreMusic(ctx context.Context, bookTitle, chapterTitle string, setting *BookSetting, notes *ChapterSummary, paragraphs []ParagraphInput, shouldPause func() bool) (out []MusicRegionResult, remaining []ParagraphInput, err error) {
	if len(paragraphs) == 0 {
		return nil, nil, nil
	}
	boundaries := sceneBoundaries(notes, paragraphs)
	scenes := notes != nil && len(notes.Scenes) > 0

	lastIdx := paragraphs[len(paragraphs)-1].Idx
	regionEnd := func(i int) int {
		if i+1 < len(boundaries) {
			return boundaries[i+1].StartIdx - 1
		}
		return lastIdx
	}
	out = make([]MusicRegionResult, 0, len(boundaries))
	var prev *musicDescription
	for i := 0; i < len(boundaries); {
		b := boundaries[i]
		if i > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphsFromIdx(paragraphs, b.StartIdx)
			return out, remaining, nil
		}
		// One description covers this region and every length-only split
		// after it, written from the first musicDescribeTextRegions of them.
		// With no summary scenes the whole chapter is such a run of splits,
		// so it is described again every musicDescribeTextRegions regions.
		n := 1
		for i+n < len(boundaries) && boundaries[i+n].split && (scenes || n < musicDescribeTextRegions) {
			n++
		}
		span := paragraphsInIdxRange(paragraphs, b.StartIdx, regionEnd(i+min(n, musicDescribeTextRegions)-1))
		if len(span) == 0 {
			i += n
			continue
		}
		d, derr := c.describeMusicRegion(ctx, bookTitle, chapterTitle, setting, notes.SceneAt(b.StartIdx), span, musicDescribeContext{prev: prev, transition: b.Transition})
		if derr != nil {
			return out, paragraphsFromIdx(paragraphs, b.StartIdx), fmt.Errorf("describing region starting at %d: %w", b.StartIdx, derr)
		}
		// A summary scene starts wherever the place, time or company
		// changes; the describe call, reading the region's full text against
		// the previous region's setting, says which of those the music
		// should restart for.
		transition := b.Transition
		if prev != nil && d.newScene {
			transition = "cut"
		}
		prev = &d
		for k, g := range boundaries[i : i+n] {
			if k == 0 {
				g.Transition = transition
			}
			out = append(out, MusicRegionResult{StartIdx: g.StartIdx, Mood: d.Mood, Prompt: d.Prompt, Ambience: d.Ambience, Transition: g.Transition})
		}
		i += n
	}
	return out, remaining, nil
}

func paragraphsFromIdx(paragraphs []ParagraphInput, startIdx int) []ParagraphInput {
	for i, p := range paragraphs {
		if p.Idx >= startIdx {
			return paragraphs[i:]
		}
	}
	return nil
}

func paragraphsInIdxRange(paragraphs []ParagraphInput, start, end int) []ParagraphInput {
	var out []ParagraphInput
	for _, p := range paragraphs {
		if p.Idx >= start && p.Idx <= end {
			out = append(out, p)
		}
	}
	return out
}
