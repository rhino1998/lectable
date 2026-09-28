package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// lightingMaxTokens bounds one lighting call's generated JSON - a short
// description plus a handful of scalar fields, musicDescribeMaxTokens'
// sibling.
const lightingMaxTokens = 768

// lightingSystemPrompt is the ambience pass's lighting sub-pass: given the
// real text of one music region (the same span describeMusicRegion wrote
// the music for) plus that region's mood and ambience, pick a room-lighting
// cue for it. Physical light first (what would you see standing in the
// scene), mood as a tint on top - a small model asked for "mood lighting"
// alone paints every tense scene red and every sad one blue, which reads as
// a disco rather than a place. The model writes a short "light" description
// before the numbers so the color follows from something concrete.
//
// Tuned against a review of the first ~500 cues (2026-09-27): the model took
// the ambience pass's soundscape as gospel (a modern dive bar lit as a
// candlelit medieval cellar because the soundscape said "tavern"), drifted
// the time of day between calls (a morning scene turning to "dusk
// streetlight"), wrote darkness into the color (31% dark, 19% grey - but a
// Hue light takes color as chromaticity only, so every grey played as the
// same harsh daylight white; normalizeCueColor now moves darkness into
// brightness), and held one mood's tint for a whole chapter (an argument's
// red through ~300 paragraphs of later banter). Hence: the soundscape is a
// hint, the chapter's opening anchors the time of day
// (LightingInput.Opening), and continuity is conditioned on the mood too
// (LightingInput.PrevMood). Even flagged as "a guess, may be wrong", the
// soundscape still won ("busy medieval tavern" kept the dive bar in
// candlelight), so the lighting call no longer sees it at all: the place
// comes from the text, which the review found was the reliable signal.
// What it does see is the book's setting profile (LightingInput.Setting):
// the era and technology decide which light sources can exist at all, and
// say nothing about where a stretch is.
//
// Effects are a fixed set the clients animate (store.LightEffect); anything
// else parses as steady, the safe default. The previous region's cue is
// passed back in (LightingInput.Prev) so a scene that hasn't moved keeps its
// light instead of re-rolling it every region.
const lightingSystemPrompt = `You are the lighting designer for an immersive audiobook. The listener's room has color smart lights, and while one stretch of a novel chapter is narrated you set them so the room feels like being inside that scene. You will be given the full text of the stretch, its emotional mood, the opening lines of the chapter, possibly notes on the book's setting, and possibly the lighting (and mood) of the stretch that plays just before it.

How to choose:
- First the scene's physical light: time of day and weather (noon sun, golden dusk, overcast grey, moonlit night, pitch dark), and the light sources actually present (hearth fire, candles, torches, lanterns, fluorescent tubes, neon signs, screens, glowing magic, underwater gloom).
- Only use light sources the book's era and technology allow (from its setting notes, when given): no electric light, neon or screens in a pre-industrial world; a modern interior is lit electrically unless the text names candles or a fire. The notes describe the world, not where this stretch is.
- Scene notes, when given, say where and at what time of day the stretch happens - use them, unless the text has clearly moved on.
- Decide where and when the scene is from the text itself - a place merely mentioned in conversation is not where the characters are. Keep the time of day the chapter's opening establishes (morning stays morning) unless the text says time has passed.
- Then tint it gently by the mood: warmer and softer for comfort or romance, colder and dimmer for dread or grief, deeper and redder for danger or rage. The place still comes first - a tense scene by a campfire is still firelight, just lower and redder.
- Flames - candles, oil lamps, lanterns, torches, a hearth or camp fire - are a saturated amber-orange, never pale yellow or white. Moonlight is a cool pale blue; overcast daylight a soft grey-white; electric lamps a warm amber-white. Only use orange when a flame is actually the light source - other interiors, screens, offices and streets get their own colors.
- If the text gives no clue about light at all, use a soft neutral warm white at moderate brightness tinted slightly by the mood.
- If a previous stretch is given and this one is in the same place at the same time, keep its light source and effect. Keep its exact color only if the mood is similar too; when the mood has changed, shift the tint to the new mood - a scene that calms down should lose an angry red. Change the light fully when the scene moves somewhere or somewhen else.
- "color" is the hue of the light at full strength - never grey, black or a dark muddy shade. Put how dark the scene is in "brightness" instead: a dim stone corridor is a warm or cool hue at low brightness, not #333333.

Brightness is 1-100 and must stay comfortable to sit in: bright daylight 60-85, ordinary interiors 35-60, dusk or lamplight 20-40, night or darkness 5-20. Never 100.

Effect - how the light moves. Use "steady" unless the scene clearly has one of these:
- "flicker": only when the light comes from flames - fire, candles, torches, lanterns. Never for electric light, screens, weather or tension.
- "pulse": a slow rhythmic throb - an alarm light, a pulsing magical glow, a heartbeat of dread. Use sparingly.
- "lightning": a thunderstorm is actually happening right now. The accent is the flash color.
- "shimmer": slow drifting between two colors - light through water, an aurora, dappled moonlight through leaves, shifting magic. The accent is the second color.

Never mention plot events or character names.

Reply with a compact JSON object on a single line only, no other text, no markdown code fence:

{"light": "<at most 12 words describing the light in the scene, e.g. dim flickering candlelight in a stone cellar>", "color": "#rrggbb", "accent": "#rrggbb or empty", "brightness": <integer 1-100>, "effect": "steady" | "flicker" | "pulse" | "lightning" | "shimmer"}`

// Lighting is one region's lighting cue - store.Lighting's own shape, kept
// as a plain value here rather than importing store (see ParagraphInput).
type Lighting struct {
	Description string // the model's short "light" description
	Color       string // "#rrggbb"
	Accent      string // "#rrggbb", "" for none
	Brightness  int    // 1-100
	Effect      string // one of LightingEffects
}

// LightingEffects are the effect names the lighting sub-pass may return -
// store.LightEffects' values; anything else parses as "steady".
var LightingEffects = []string{"steady", "flicker", "pulse", "lightning", "shimmer"}

// LightingInput is one lighting call's context.
type LightingInput struct {
	// Paragraphs is the region's own text - the span one describeMusicRegion
	// call covered (a region plus the length-only splits sharing it).
	Paragraphs []ParagraphInput
	// Mood is the region's music mood label (store.MusicRegion.Mood).
	Mood string
	// NewScene reports that this region starts a new scene (a "cut"), so
	// the previous region's light shouldn't simply carry over.
	NewScene bool
	// Prev is the previous region's cue, nil for a chapter's first region.
	Prev *Lighting
	// PrevMood is the previous region's mood label, so the model can tell
	// whether to keep Prev's tint.
	PrevMood string
	// Opening is the chapter's first few lines (see ChapterOpening) - the
	// time of day and place they establish anchor every call, since each
	// one otherwise sees only its own region.
	Opening string
	// Setting is the book's setting profile, nil for none (see
	// lightingSystemPrompt's doc comment).
	Setting *BookSetting
	// Scene is the chapter summary's scene covering this stretch, nil for
	// none: its location and time of day.
	Scene *Scene
}

// chapterOpeningChars bounds LightingInput.Opening.
const chapterOpeningChars = 600

// ChapterOpening is the start of a chapter's text for LightingInput.Opening:
// its non-inline paragraphs joined up to chapterOpeningChars, skipping a
// bare heading.
func ChapterOpening(paragraphs []ParagraphInput) string {
	var sb strings.Builder
	for _, p := range paragraphs {
		t := oneLine(p.Text)
		if t == "" || len(strings.Fields(t)) < 4 && sb.Len() == 0 {
			continue // "Chapter 12" and the like
		}
		if sb.Len() > 0 {
			sb.WriteString(" ")
		}
		sb.WriteString(t)
		if sb.Len() >= chapterOpeningChars {
			break
		}
	}
	out := sb.String()
	if len(out) > chapterOpeningChars {
		out = out[:chapterOpeningChars] + "..."
	}
	return out
}

// minLitWords is how much text a region needs before it's worth a lighting
// call - below it (a title page, a bare heading) NeedsLighting is false and
// the caller uses NeutralLighting.
const minLitWords = 25

// NeedsLighting reports whether paragraphs hold enough text for
// DescribeLighting to judge a scene from.
func NeedsLighting(paragraphs []ParagraphInput) bool {
	n := 0
	for _, p := range paragraphs {
		n += len(strings.Fields(p.Text))
	}
	return n >= minLitWords
}

// NeutralLighting is the cue for a region with no scene to read: a soft
// warm white at moderate brightness.
var NeutralLighting = Lighting{Description: "soft neutral warm white", Color: "#ffe2c4", Brightness: 40, Effect: "steady"}

type lightingRaw struct {
	Light      string  `json:"light"`
	Color      string  `json:"color"`
	Accent     string  `json:"accent"`
	Brightness float64 `json:"brightness"`
	Effect     string  `json:"effect"`
}

// DescribeLighting picks one region's lighting cue - see
// lightingSystemPrompt. httpapi.lightChapter calls it once per described
// region group, in order, feeding each result back as the next call's Prev.
func (c *Client) DescribeLighting(ctx context.Context, bookTitle, chapterTitle string, in LightingInput) (Lighting, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	fmt.Fprintf(&user, "Mood: %s\n", orNone(in.Mood))
	if lines := in.Setting.promptLines(); lines != "" {
		fmt.Fprintf(&user, "\nBook setting notes (the world as a whole):\n%s", lines)
	}
	user.WriteString(in.Scene.promptLines())
	if in.Opening != "" {
		fmt.Fprintf(&user, "\nChapter opening (for the time of day and place): %s\n", in.Opening)
	}
	if p := in.Prev; p != nil {
		if in.NewScene {
			user.WriteString("\nThis stretch starts a new scene - don't carry the previous light over unless the text clearly stays in the same place and time.\n")
		}
		user.WriteString("\nPrevious stretch's lighting:\n")
		fmt.Fprintf(&user, "- mood: %s\n", orNone(in.PrevMood))
		fmt.Fprintf(&user, "- light: %s\n", p.Description)
		fmt.Fprintf(&user, "- color: %s\n", p.Color)
		fmt.Fprintf(&user, "- accent: %s\n", orNone(p.Accent))
		fmt.Fprintf(&user, "- brightness: %d\n", p.Brightness)
		fmt.Fprintf(&user, "- effect: %s\n", p.Effect)
	}
	user.WriteString("\nText:\n")
	for i, p := range in.Paragraphs {
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
	var text strings.Builder
	for _, p := range in.Paragraphs {
		text.WriteString(p.Text)
		text.WriteString(" ")
	}
	flameInText := flameSourceWords.MatchString(text.String())
	return generateAndParse(ctx, c, lightingSystemPrompt, user.String(), 0, lightingMaxTokens, func(content string) (Lighting, error) {
		return parseLighting(content, flameInText)
	})
}

// parseLighting validates one lighting reply: the base color must be a real
// hex color (a retry otherwise - there's no sensible fallback for a
// missing one), everything else is clamped or defaulted. flameInText
// reports that the region's own text names a flame light source (see
// flameSourceWords), which lets a flicker stand even when the model's
// description forgot to mention the lantern.
func parseLighting(content string, flameInText bool) (Lighting, error) {
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start == -1 || end == -1 || end < start {
		return Lighting{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var raw lightingRaw
	if err := json.Unmarshal([]byte(content[start:end+1]), &raw); err != nil {
		return Lighting{}, fmt.Errorf("parse JSON object: %w", err)
	}
	color, ok := normalizeHexColor(raw.Color)
	if !ok {
		return Lighting{}, fmt.Errorf("invalid color %q", raw.Color)
	}
	accent, _ := normalizeHexColor(raw.Accent)
	l := Lighting{
		Description: strings.TrimSpace(raw.Light),
		Color:       color,
		Accent:      accent,
		Brightness:  max(1, min(95, int(raw.Brightness+0.5))),
		Effect:      "steady",
	}
	effect := strings.ToLower(strings.TrimSpace(raw.Effect))
	for _, e := range LightingEffects {
		if effect == e {
			l.Effect = e
		}
	}
	// Flicker is a flame effect (store.LightEffectFlicker) and flames are
	// orange; the model reaches for "flicker" for fluorescent tubes, rain
	// and tension too. A flame in the description, or a flicker over text
	// that names one (a party carrying a lantern the description forgot),
	// keeps (or gets pulled to) flame orange; any other flicker is steady.
	// Clients animate flicker from the base color alone, so it has no accent.
	flame := flameWords.MatchString(l.Description) || l.Effect == "flicker" && flameInText
	switch {
	case flame:
		l.Color = flameColor(l.Color)
	case l.Effect == "flicker":
		l.Effect = "steady"
	}
	if l.Effect == "flicker" {
		l.Accent = ""
	}
	l.Color, l.Brightness = normalizeCueColor(l.Color, l.Brightness, l.Description)
	if l.Accent != "" {
		l.Accent, _ = normalizeCueColor(l.Accent, 0, l.Description)
	}
	switch l.Effect {
	case "lightning":
		if l.Accent == "" {
			l.Accent = "#dce6ff" // a cold blue-white flash
		}
	case "shimmer":
		// Nothing to drift toward - holding the base is the honest result.
		if l.Accent == "" || l.Accent == l.Color {
			l.Effect = "steady"
			l.Accent = ""
		}
	}
	return l, nil
}

// flameSourceWords spots a flame light source in a region's own text -
// narrower than flameWords, since story text says "fire" and "flames" for
// spells and gunfire far more often than for a light.
var flameSourceWords = regexp.MustCompile(`(?i)\b(candles?|candlelight|candlelit|lanterns?|torch(es)?|torchlight|hearth|campfire|fireplace|braziers?|oil lamps?)\b`)

// flameWords spots a flame light source in a cue's description - candles,
// lanterns and the like read as steady too, but should still be orange.
var flameWords = regexp.MustCompile(`(?i)\b(candle|candles|candlelight|candlelit|lantern|lanterns|lamplight|oil lamp|torch|torches|torchlight|fire|firelight|fireplace|hearth|campfire|flame|flames|ember|embers|brazier|braziers)\b`)

// Cue colors reach a Hue light as chromaticity only - brightness is its own
// field - so a dark or grey hex means nothing to the room: #4a4a4a plays as
// the same harsh daylight white as #ffffff. normalizeCueColor lifts a color
// to full value and folds its darkness into brightness (halfway - the model
// usually lowered brightness for a dark scene too), and swaps near-greys
// for a white that fits the description: cool for moonlight/cold/blue
// light, warm otherwise. brightness 0 means "an accent - leave it".
const (
	greySaturation = 0.15
	warmWhite      = "#ffe2c4"
	coolWhite      = "#dfe8ff"
)

var coolWords = regexp.MustCompile(`(?i)\b(cool|cold|icy|frost|moon|moonlight|moonlit|blue|blueish|bluish|steel|clinical|fluorescent|overcast|grey|gray)\b`)

func normalizeCueColor(hex string, brightness int, description string) (string, int) {
	r, g, b, ok := parseHexRGB(hex)
	if !ok {
		return hex, brightness
	}
	h, sat, v := rgbToHSV(r, g, b)
	if brightness > 0 && v < 1 {
		brightness = max(1, int(float64(brightness)*(0.5+0.5*v)+0.5))
	}
	if sat < greySaturation {
		if coolWords.MatchString(description) {
			return coolWhite, brightness
		}
		return warmWhite, brightness
	}
	r, g, b = hsvToRGB(h, sat, 1)
	return fmt.Sprintf("#%02x%02x%02x", int(r*255+0.5), int(g*255+0.5), int(b*255+0.5)), brightness
}

// Flame hues: a small model's "candlelight" often comes back pale yellow or
// near-white, which on real bulbs reads as a dim office. flameColor keeps a
// color that's already a saturated amber-orange and otherwise pulls it into
// that band (hue flameMinHue-flameMaxHue degrees, saturation >= flameMinSat,
// full value - brightness is a separate field).
const (
	flameMinHue = 16.0
	flameMaxHue = 28.0
	flameMinSat = 0.78
)

func flameColor(hex string) string {
	r, g, b, ok := parseHexRGB(hex)
	if !ok {
		return "#ff8a2a"
	}
	h, sat, _ := rgbToHSV(r, g, b)
	if sat < 0.15 {
		h = 26 // a grey/white carries no hue worth keeping
	}
	if h > 300 { // reds wrapping past 360 are closer to the low end
		h = flameMinHue
	}
	h = max(flameMinHue, min(flameMaxHue, h))
	sat = max(sat, flameMinSat)
	r, g, b = hsvToRGB(h, sat, 1)
	return fmt.Sprintf("#%02x%02x%02x", int(r*255+0.5), int(g*255+0.5), int(b*255+0.5))
}

func parseHexRGB(hex string) (r, g, b float64, ok bool) {
	var n uint32
	if _, err := fmt.Sscanf(strings.TrimPrefix(hex, "#"), "%06x", &n); err != nil || len(strings.TrimPrefix(hex, "#")) != 6 {
		return 0, 0, 0, false
	}
	return float64(n>>16&0xff) / 255, float64(n>>8&0xff) / 255, float64(n&0xff) / 255, true
}

func rgbToHSV(r, g, b float64) (h, s, v float64) {
	hi := max(r, g, b)
	lo := min(r, g, b)
	v = hi
	d := hi - lo
	if hi > 0 {
		s = d / hi
	}
	switch {
	case d == 0:
		h = 0
	case hi == r:
		h = 60 * math.Mod((g-b)/d, 6)
	case hi == g:
		h = 60 * ((b-r)/d + 2)
	default:
		h = 60 * ((r-g)/d + 4)
	}
	if h < 0 {
		h += 360
	}
	return h, s, v
}

func hsvToRGB(h, s, v float64) (r, g, b float64) {
	c := v * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := v - c
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return r + m, g + m, b + m
}

// normalizeHexColor accepts "#rrggbb", "rrggbb", "#rgb" or "rgb" (any case)
// and returns lower-case "#rrggbb".
func normalizeHexColor(s string) (string, bool) {
	s = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "#"))
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", false
		}
	}
	switch len(s) {
	case 3:
		s = string([]byte{s[0], s[0], s[1], s[1], s[2], s[2]})
	case 6:
	default:
		return "", false
	}
	return "#" + s, true
}
