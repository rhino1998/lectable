// Package narration resolves which narration voice a paragraph should
// actually generate with. Originally every paragraph in a book used the
// same, single book-level voice (store.Book's Voice* fields); this package
// adds a per-paragraph override on top, purely additively: a paragraph
// attributed to a character (store.Paragraph.Speaker, set by
// internal/speakerattr) narrates in that character's own assigned voice
// once one has been assigned (store.Store.SetCharacterVoice), and falls
// back to the book's own voice otherwise - unattributed paragraphs,
// paragraphs attributed to "Narrator", and paragraphs attributed to a
// character nobody has assigned a voice to yet all resolve exactly the way
// every paragraph always did.
package narration

import (
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// ResolvedVoice is a paragraph's fully-resolved narration voice - what
// jobs.Manager needs to actually generate audio, plus PresetID/Instruct/
// Language, which together hash into the same voice_id
// (store.VoiceID/VoiceID below) used everywhere else as the paragraph_audio
// cache key - so "what voice generated this audio" and "what voice should
// this paragraph use" are always expressed the same way.
type ResolvedVoice struct {
	PresetID        string
	Instruct        string
	Language        string
	Seed            int
	RefText         string
	SpeedMultiplier float64
	CloneModel      string
	// DesignModel selects which VoiceDesign engine renders this voice's
	// reference clip (see voices.Preset.DesignModel/store.VoicePreset's own
	// doc comments) - "" defers to the worker's own process-wide default.
	DesignModel string
	// CloneInstruct is a clone-time style instruction sent alongside this
	// voice's reference clip on the actual generation call - distinct from
	// Instruct above, which is only ever a *design*-time instruction (how
	// to synthesize a brand-new voice from scratch, never sent to a clone
	// call). "" (every voice but the one instructedNarratorVoice below
	// produces) means nothing extra is sent, today's unchanged behavior.
	// Only ever honored by a clone model whose family accepts an
	// instruction alongside a reference clip in the same call
	// (voices.InstructedCloneModel today) - silently ignored by any other,
	// same as an ignored RefSamples/RefText resend already is for a
	// noReference family.
	CloneInstruct string
}

// VoiceID is this voice's cache key, matching store.VoiceID exactly (same
// inputs, same hash) so audio generated via a ResolvedVoice lands in the
// same paragraph_audio/on-disk slot as anything else using that voice.
// Folds CloneInstruct into the same hash input as Instruct (NUL-delimited,
// like store.VoiceID's own instruct/language join) - instructedNarratorVoice
// below reuses the book's own PresetID/Instruct verbatim for every
// character, so CloneInstruct is what actually keeps each character's own
// cache entry distinct from the narrator's and from each other's.
func (v ResolvedVoice) VoiceID() string {
	return store.VoiceID(v.PresetID, v.Instruct+"\x00"+v.CloneInstruct, v.Language)
}

// Resolver looks up preset/character configuration in the store to resolve
// effective narration voices.
type Resolver struct {
	store *store.Store
}

func NewResolver(s *store.Store) *Resolver { return &Resolver{store: s} }

// BookVoice resolves book's own narrator voice - the fallback for any
// paragraph not attributed to a character with its own assigned voice.
// Mirrors httpapi.resolveVoiceSeed/jobs.Manager's old resolveVoiceContext,
// now centralized here since both httpapi and jobs need it.
func (r *Resolver) BookVoice(book *store.Book) (ResolvedVoice, error) {
	v := ResolvedVoice{
		PresetID:        book.VoicePresetID,
		Instruct:        book.VoiceInstruct,
		Language:        book.VoiceLanguage,
		Seed:            book.VoiceSeed,
		SpeedMultiplier: 1.0,
	}
	if book.VoicePresetID == "" {
		// Fully custom instruct with no preset backing it - VoiceDesign
		// fallback (see jobs.Manager.generate), no ref clip to resolve.
		return v, nil
	}
	if preset, err := r.store.GetVoicePreset(book.VoicePresetID); err != nil {
		return ResolvedVoice{}, err
	} else if preset != nil {
		v.RefText = preset.RefText
		v.SpeedMultiplier = preset.SpeedMultiplier
		v.CloneModel = preset.CloneModel
		v.DesignModel = preset.DesignModel
		return v, nil
	}
	if p, ok := voices.PresetsByID[book.VoicePresetID]; ok {
		v.RefText = p.RefText
		v.SpeedMultiplier = p.SpeedMultiplier
		v.CloneModel = builtinCloneModel(p)
		v.DesignModel = p.DesignModel
	}
	return v, nil
}

// builtinCloneModel is p's own CloneModel override if it has one (currently
// only voices.FastPresetID does - see its own doc comment), defaulting to
// voices.DefaultCloneModel like every other built-in preset.
func builtinCloneModel(p voices.Preset) string {
	if p.CloneModel != "" {
		return p.CloneModel
	}
	return voices.DefaultCloneModel
}

// CharacterVoice resolves presetID (a character's assigned voice, built-in
// or custom) into a full ResolvedVoice narrating in language (a character's
// assigned voice doesn't carry its own language - see store.VoicePreset -
// so it narrates in whatever language the book itself is set to). ok is
// false if presetID is "" or names a preset that no longer exists (e.g. a
// deleted custom preset); callers should fall back to BookVoice then.
func (r *Resolver) CharacterVoice(presetID, language string) (v ResolvedVoice, ok bool, err error) {
	if presetID == "" {
		return ResolvedVoice{}, false, nil
	}
	if p, found := voices.PresetsByID[presetID]; found {
		return ResolvedVoice{
			PresetID: p.ID, Instruct: p.Instruct, Language: language, Seed: p.Seed,
			RefText: p.RefText, SpeedMultiplier: p.SpeedMultiplier, CloneModel: builtinCloneModel(p),
			DesignModel: p.DesignModel,
		}, true, nil
	}
	custom, err := r.store.GetVoicePreset(presetID)
	if err != nil {
		return ResolvedVoice{}, false, err
	}
	if custom == nil {
		return ResolvedVoice{}, false, nil
	}
	return ResolvedVoice{
		PresetID: custom.ID, Instruct: custom.Instruct, Language: language, Seed: custom.Seed,
		RefText: custom.RefText, SpeedMultiplier: custom.SpeedMultiplier, CloneModel: custom.CloneModel,
		DesignModel: custom.DesignModel,
	}, true, nil
}

// ForParagraph is the convenience one-shot resolution of BookVoice +
// character lookup + CharacterVoice for a single paragraph - one or two
// store round trips, fine for a single paragraph (regenerate, a single
// lookahead paragraph) but wasteful for a whole chapter at once. Callers
// resolving many paragraphs together should call BookVoice and
// CharacterVoice directly against a pre-fetched store.Store.ListCharacters
// map instead (see httpapi.handleGetChapter).
//
// Character overrides only apply when book.MultiVoice() is true (any
// CharacterVoiceMode but CharacterVoiceModeNarrator) - Narrator mode always
// returns BookVoice regardless of speaker or any character's assigned
// voice, so switching a book's mode is a single field change, not
// something that depends on whether attribution happens to have been run
// or characters happen to have voices assigned.
func (r *Resolver) ForParagraph(book *store.Book, speaker string) (ResolvedVoice, error) {
	bookVoice, err := r.BookVoice(book)
	if err != nil {
		return ResolvedVoice{}, err
	}
	if !book.MultiVoice() || speaker == "" || speaker == "Narrator" {
		return bookVoice, nil
	}
	char, err := r.store.GetCharacterByName(store.SeriesScope(book), speaker)
	if err != nil {
		return ResolvedVoice{}, err
	}
	if char == nil {
		return bookVoice, nil
	}
	// A character's assigned voice is per clone model (a preset clones
	// through one specific model - see store.Character's doc comment), so
	// the lookup is keyed by the book's own resolved clone model, not just
	// the character alone.
	cloneModel := EffectiveCloneModel(bookVoice)
	presetID, err := r.store.CharacterVoiceForModel(char.ID, cloneModel)
	if err != nil {
		return ResolvedVoice{}, err
	}
	return r.ResolveCharacterVoice(book, bookVoice, *char, cloneModel, presetID)
}

// ResolveCharacterVoice is ForParagraph's own per-character resolution,
// factored out so a bulk, whole-chapter caller that's already
// batch-prefetched presetID via Store.CharacterVoicesForModel
// (httpapi.handleGetChapter/handleListSpeakers,
// jobs.Manager.paragraphsNeedingGeneration - see ForParagraph's own doc
// comment on why those don't call ForParagraph itself, one paragraph at a
// time) applies the exact same presetID-or-instructed-narrator-or-book-voice
// fallback chain ForParagraph does, rather than each hand-rolling its own
// slightly different copy of it.
func (r *Resolver) ResolveCharacterVoice(book *store.Book, bookVoice ResolvedVoice, char store.Character, cloneModel, presetID string) (ResolvedVoice, error) {
	// CharacterVoiceModeInstructAll overrides everything below, including
	// an explicit assignment - checked first, before presetID is even
	// looked at, since "all" means all. A no-op (falls through to the
	// normal assigned-or-fallback chain) unless the book's resolved clone
	// model actually honors a clone-time instruction and this character
	// has been characterized yet.
	if book.InstructAllCharacterVoices() && char.Summary != "" && cloneModel == voices.InstructedCloneModel {
		return instructedNarratorVoice(bookVoice, char.Summary), nil
	}
	if presetID == "" {
		// No voice explicitly assigned to this character (for this book's
		// resolved clone model) yet - CharacterVoiceModeInstructUnassigned,
		// if set, resolves them straight to the book's own narrator voice
		// instead, styled by their own characterization - see
		// instructedNarratorVoice's own doc comment. A no-op (falls through
		// to plain bookVoice, today's unchanged behavior) unless the book's
		// resolved clone model actually honors a clone-time instruction and
		// this character has been characterized yet.
		if book.InstructCharacterVoices() && char.Summary != "" && cloneModel == voices.InstructedCloneModel {
			return instructedNarratorVoice(bookVoice, char.Summary), nil
		}
		return bookVoice, nil
	}
	v, ok, err := r.CharacterVoice(presetID, book.VoiceLanguage)
	if err != nil {
		return ResolvedVoice{}, err
	}
	if !ok {
		return bookVoice, nil
	}
	return v, nil
}

// instructedNarratorVoice is CharacterVoiceModeInstructUnassigned/
// CharacterVoiceModeInstructAll's actual effect: bookVoice unchanged (same
// PresetID, same already-rendered
// reference clip, same RefText/SpeedMultiplier/CloneModel/DesignModel -
// nothing new to provision or render), except CloneInstruct is set to the
// character's own casting instruction (store.Character.Summary - the same
// text httpapi.provisionCharacterVoiceAttempt would otherwise spend a
// whole separate VoiceDesign render on to synthesize this character a
// wholly independent voice). The result is a character who clones through
// the narrator's own voice, styled per line by their own instruction,
// rather than a different voice entirely.
func instructedNarratorVoice(bookVoice ResolvedVoice, summary string) ResolvedVoice {
	v := bookVoice
	v.CloneInstruct = summary
	return v
}

// EffectiveCloneModel is bookVoice's CloneModel, defaulting to
// voices.DefaultCloneModel when bookVoice is a fully custom instruct with
// no preset backing it (see BookVoice) and so has no clone model of its
// own resolved - used to key character-voice auto-assignment/lookup
// (Store.CharacterVoiceForModel) so "no clone model resolved" doesn't
// become its own distinct, empty-string scope.
func EffectiveCloneModel(bookVoice ResolvedVoice) string {
	if bookVoice.CloneModel == "" {
		return voices.DefaultCloneModel
	}
	return bookVoice.CloneModel
}
