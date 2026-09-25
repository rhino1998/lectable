// Package emotions is the fixed set of delivery emotions/speaking modes a
// dialogue line can be labeled with (internal/speakerattr.Client.
// EmotionChapter), and the recipe for each one's per-voice reference-clip
// variant (internal/voicerefs.EnsureVariantFile).
//
// An emotion never reaches a TTS model as a control token. Instead, every
// voice preset gets one extra reference clip per emotion its speakers
// actually use, rendered lazily by BreezeTTS's instructed cloning (the base
// reference clip + Instruction, speaking RefLine), and an emotional line
// clones from that variant instead of the base clip - zero-shot cloners
// copy a reference's delivery almost as strongly as its timbre, so this
// works through every clone model, not just one with its own emotion
// vocabulary. Chosen over Higgs's own inline <|emotion:*|> tags after a
// listening test found most of those tags pull the voice away from its
// reference entirely (see task-backlog.md item 13).
//
// A leaf package (no internal imports) so speakerattr (prompting/
// validation), store (Paragraph.EffectiveEmotion), voicerefs (rendering),
// and httpapi (the manual-override endpoint) can all share one list.
package emotions

// Emotion is one entry of the fixed emotion set.
type Emotion struct {
	// ID is the stored/wire value (store.Paragraph.Emotion) and the
	// variant clip's file name - lowercase, stable, never renamed.
	ID string `json:"id"`
	// Label is the human-readable name shown in the UI.
	Label string `json:"label"`
	// Description tells the labeling LLM what this emotion covers - see
	// speakerattr's emotion prompt.
	Description string `json:"-"`
	// Instruction is the BreezeTTS clone-time style instruction the
	// variant is rendered with.
	Instruction string `json:"-"`
	// RefLine is what the variant clip actually says. Chosen to carry the
	// emotion in its own words, since a flat line reads flat whatever the
	// instruction asks for.
	RefLine string `json:"-"`
}

// Neutral is the absence of an emotion - a line with no label clones from
// the voice's own base reference clip, exactly as before this package
// existed. Never stored as a value of its own; "" means neutral.
const Neutral = ""

// All is the full set, in display order. Kept deliberately small: every
// entry is one more reference clip to render per voice, the labeling LLM
// gets less accurate as the set grows, and clone models blur fine
// distinctions anyway.
var All = []Emotion{
	{
		ID:          "warm",
		Label:       "Warm",
		Description: "happy, amused, affectionate, cheerful, fond, relieved",
		Instruction: "Speak warmly and affectionately, with a gentle smile in the voice, relaxed and fond.",
		RefLine:     "Oh, come here, you daft thing. I've missed you, I really have. Sit down, sit down, I'll put the kettle on and you can tell me everything.",
	},
	{
		ID:          "excited",
		Label:       "Excited",
		Description: "excited, surprised, eager, triumphant, delighted, astonished",
		Instruction: "Speak with breathless excitement and delight, fast and bright, voice lifting with joy.",
		RefLine:     "We did it! Do you hear me? We actually did it! I can't believe it worked, it actually worked!",
	},
	{
		ID:          "teasing",
		Label:       "Teasing",
		Description: "teasing, playful, sly, smug, mischievous, joking, flirtatious",
		Instruction: "Speak playfully and teasingly, with a sly, knowing smile in the voice, light and a little smug.",
		RefLine:     "Oh, is that so? The great hero, scared of a little spider. Don't worry, your secret's safe with me. Mostly.",
	},
	{
		ID:          "sad",
		Label:       "Sad",
		Description: "sad, grieving, melancholy, resigned, heartbroken, close to tears",
		Instruction: "Speak sadly and quietly, voice heavy with grief, slow and close to tears.",
		RefLine:     "He's gone. I kept thinking he'd walk back through that door, but he's not coming back, is he. Not this time.",
	},
	{
		ID:          "pleading",
		Label:       "Pleading",
		Description: "pleading, begging, desperate, imploring, beseeching",
		Instruction: "Speak pleadingly and desperately, voice strained and rising, urgently begging.",
		RefLine:     "Please, I'm begging you, don't do this. He's all I have left. Take anything else, anything, just let him go.",
	},
	{
		ID:          "angry",
		Label:       "Angry",
		Description: "angry, furious, snapping, frustrated, heated, bitter",
		Instruction: "Speak furiously, voice raised and tight with anger, biting off each word.",
		RefLine:     "Don't you dare walk away from me! You lied to my face, you lied to all of us, and you've got the nerve to stand there smiling?",
	},
	{
		ID:          "afraid",
		Label:       "Afraid",
		Description: "afraid, terrified, panicked, frightened",
		Instruction: "Speak fearfully, voice trembling and breathless with panic, quick and shaky.",
		RefLine:     "Did you hear that? Something's out there. Please, please, we have to go, we have to go right now, it's coming closer.",
	},
	{
		ID:          "nervous",
		Label:       "Nervous",
		Description: "nervous, hesitant, awkward, unsure, flustered, stammering",
		Instruction: "Speak nervously and hesitantly, unsure of yourself, with small pauses and a slightly shaky voice.",
		RefLine:     "I, um... I wasn't sure if I should say anything. It's probably nothing. I just thought... well, maybe you'd want to know?",
	},
	{
		ID:          "cold",
		Label:       "Cold",
		Description: "cold, contemptuous, cutting sarcasm, menacing, disdainful, threatening",
		Instruction: "Speak coldly and contemptuously, flat and controlled, with icy disdain.",
		RefLine:     "How touching. You really thought that would work? Run along now, and do try not to embarrass yourself any further.",
	},
	{
		ID:          "whisper",
		Label:       "Whisper",
		Description: "whispered, hushed, murmured, secretive, spoken under one's breath",
		Instruction: "Whisper very quietly and secretively, breathy and hushed, barely voiced.",
		RefLine:     "Keep your voice down. The guards change at midnight. When the bell rings, we slip out the back, and not a word to anyone.",
	},
	{
		ID:          "shout",
		Label:       "Shout",
		Description: "shouted, yelled, screamed, called out across a distance, bellowed",
		Instruction: "Shout loudly across a distance, voice at full volume, urgent and projecting.",
		RefLine:     "Over here! Hey! Can anyone hear me? We're down here, by the river! Bring the ropes, hurry!",
	},
	{
		ID:          "weary",
		Label:       "Weary",
		Description: "exhausted, weak, drained, sleepy, worn out",
		Instruction: "Speak wearily, exhausted and weak, slow and breathy, as if barely able to stand.",
		RefLine:     "Just... give me a minute. I haven't slept in three days. My legs won't hold me much longer, I'm sorry.",
	},
	{
		ID:          "pained",
		Label:       "Pained",
		Description: "in pain, hurt, wounded, strained, gasping, speaking through physical effort",
		Instruction: "Speak through physical pain, voice tight and strained, gasping between words, teeth gritted.",
		RefLine:     "Argh... it's my leg. I think it's broken. Just... pull me up. On three. One, two... ahh!",
	},
}

var byID = func() map[string]Emotion {
	m := make(map[string]Emotion, len(All))
	for _, e := range All {
		m[e.ID] = e
	}
	return m
}()

// Get returns id's entry, ok false for Neutral or an unknown id.
func Get(id string) (Emotion, bool) {
	e, ok := byID[id]
	return e, ok
}

// Valid reports whether id names a real (non-neutral) emotion.
func Valid(id string) bool {
	_, ok := byID[id]
	return ok
}
