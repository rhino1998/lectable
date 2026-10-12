package narration

import (
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicefx"
)

// scopeEffects is one roster's characters with a voice effect, as of at.
type scopeEffects struct {
	at     time.Time
	byChar map[string]voicefx.VoiceEffect
}

// effectsMaxAge is how long a roster's effects are reused. A chapter is
// resolved a paragraph at a time, each asking for its speaker's effect;
// the answer only changes when a fact or a character's race is edited,
// and a couple of seconds of the old one then does no harm.
const effectsMaxAge = 2 * time.Second

// CharacterEffect is the voice effect of the character's kind: the effect
// named by the world fact in force (store.BookWorldFacts) whose word is
// the character's race (their accepted store.NoteKind note) - "insectoid"
// for an Antinium once the Antinium fact says so. "" for most.
func (r *Resolver) CharacterEffect(book *store.Book, characterID string) (voicefx.VoiceEffect, error) {
	return r.scopeEffect(store.SeriesScope(book), characterID)
}

// PresetEffect is the voice effect of the character presetID is the voice
// of (CharacterEffect) - for playing a voice's reference clip the way its
// lines sound. A derived preset id (a numbered role member's, a stage's:
// "<preset>~...") is its base preset's. "" for a voice no character with
// an effect has.
func (r *Resolver) PresetEffect(presetID string) (voicefx.VoiceEffect, error) {
	base, _, _ := strings.Cut(presetID, "~")
	chars, err := r.store.VoicePresetCharacters(base)
	if err != nil {
		return "", err
	}
	for _, c := range chars {
		if effect, err := r.scopeEffect(c.Scope, c.ID); err != nil || effect != "" {
			return effect, err
		}
	}
	return "", nil
}

func (r *Resolver) scopeEffect(scope, characterID string) (voicefx.VoiceEffect, error) {
	r.effectsMu.Lock()
	defer r.effectsMu.Unlock()
	if cached, ok := r.effects[scope]; ok && time.Since(cached.at) < effectsMaxAge {
		return cached.byChar[characterID], nil
	}
	byChar, err := r.loadEffects(scope)
	if err != nil {
		return "", err
	}
	if r.effects == nil {
		r.effects = map[string]scopeEffects{}
	}
	r.effects[scope] = scopeEffects{at: time.Now(), byChar: byChar}
	return byChar[characterID], nil
}

func (r *Resolver) loadEffects(scope string) (map[string]voicefx.VoiceEffect, error) {
	facts, err := store.ScopeWorldFacts(r.store, scope)
	if err != nil {
		return nil, err
	}
	byWord := map[string]voicefx.VoiceEffect{}
	for _, f := range facts {
		if f.Effect == "" {
			continue
		}
		for _, w := range f.Words() {
			byWord[w] = f.Effect
		}
	}
	if len(byWord) == 0 {
		return nil, nil // the usual case: no notes to read
	}
	notes, err := r.store.ListCharacterNotes(scope)
	if err != nil {
		return nil, err
	}
	byChar := map[string]voicefx.VoiceEffect{}
	for _, n := range notes {
		if n.Kind != store.NoteKind || n.Status != store.ProposalAccepted {
			continue
		}
		race := strings.ToLower(strings.TrimSpace(n.Value))
		if effect, ok := byWord[race]; ok {
			byChar[n.CharacterID] = effect
		} else if effect, ok := byWord[strings.TrimSuffix(race, "s")]; ok {
			byChar[n.CharacterID] = effect
		}
	}
	return byChar, nil
}
