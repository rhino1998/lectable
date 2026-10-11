package narration

import (
	"crypto/sha256"
	"fmt"
	"sort"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// NoChapter is the chapter index to resolve a voice with when it isn't
// being resolved for a line of any chapter: the character's own voice,
// whatever stages they have.
const NoChapter = -1

// StageSpan is one of a character's stages as it falls in one book: in
// force from chapter FromIdx until the character's next span. A stage
// that started in an earlier book of the series has FromIdx 0.
type StageSpan struct {
	FromIdx int
	Stage   store.CharacterStage
}

// Stages are one book's character stages (store.CharacterStage), by
// character id, each character's in story order. nil for a book whose
// roster has none - nearly every book.
type Stages map[string][]StageSpan

// At returns the stage characterID has reached by chapter chapterIdx, or
// nil before their first one (or for NoChapter).
func (s Stages) At(characterID string, chapterIdx int) *store.CharacterStage {
	var at *store.CharacterStage
	for i, span := range s[characterID] {
		if span.FromIdx > chapterIdx {
			break
		}
		at = &s[characterID][i].Stage
	}
	return at
}

// BookStages lays the stages of book's roster over its chapters. A stage
// set in an earlier book of the series is in force from this one's first
// chapter; one set in a later book doesn't apply here.
func (r *Resolver) BookStages(book *store.Book) (Stages, error) {
	all, err := r.store.ListCharacterStages(store.SeriesScope(book))
	if err != nil || len(all) == 0 {
		return nil, err
	}
	order := map[string]int{book.ID: 0}
	if book.SeriesName != "" {
		series, err := r.store.ListSeriesBooks(book.SeriesName)
		if err != nil {
			return nil, err
		}
		for i, b := range series {
			order[b.ID] = i
		}
	}
	here, ok := order[book.ID]
	if !ok {
		return nil, nil
	}
	sort.SliceStable(all, func(i, j int) bool {
		if oi, oj := order[all[i].BookID], order[all[j].BookID]; oi != oj {
			return oi < oj
		}
		return all[i].ChapterIdx < all[j].ChapterIdx
	})
	out := Stages{}
	for _, st := range all {
		at, ok := order[st.BookID]
		if !ok || at > here {
			continue
		}
		from := st.ChapterIdx
		if at < here {
			from = 0
		}
		spans := out[st.CharacterID]
		// Two stages of earlier books both start at this book's first
		// chapter: the later one is the one still in force.
		if n := len(spans); n > 0 && spans[n-1].FromIdx == from {
			spans = spans[:n-1]
		}
		out[st.CharacterID] = append(spans, StageSpan{FromIdx: from, Stage: st})
	}
	return out, nil
}

// StageVoice is v - char's own resolved voice - as it sounds at stage: the
// same VoiceDesign casting (seed - the stage's first take keeps it -
// reference line, design model) rendered
// from the stage's instruction instead of the character's own, which a
// listening test (2026-10-10, Will Oh at 10/20/40/70) heard as the
// character at that age; cloning the base clip under an "older"
// instruction and pitch/formant-shifting it both kept the voice and lost
// the age. Like a role's numbered sibling (MemberVoice) the stage voice is
// not stored: its preset id is derived (stagePresetID), so its reference
// clip, emotion variants and audio key on it like any preset's.
//
// v is returned as is with no stage, for a role (a label shared by many
// people has no age), and for a character with no voice of their own -
// one resolving to bookVoice, the narrator's. A voice styled per line
// (CloneInstruct - the instructed-narrator modes) takes the stage's
// instruction as its style instead.
func StageVoice(v, bookVoice ResolvedVoice, char store.Character, stage *store.CharacterStage) ResolvedVoice {
	if stage == nil || stage.Instruct == "" || char.IsRole {
		return v
	}
	if v.CloneInstruct != "" {
		v.CloneInstruct = stage.Instruct
		return v
	}
	if v.PresetID == "" || v.VoiceID() == bookVoice.VoiceID() {
		return v
	}
	m := v
	m.Instruct = stage.Instruct
	// Each take (store.CharacterStage.Take) is another seed: a large odd
	// step keeps takes of nearby seeds apart.
	m.Seed = (v.Seed + stage.Take*2_000_003) & 0x7fffffff
	m.PresetID = stagePresetID(m, stage)
	return m
}

// stagePresetID names a voice's stage: the character's preset id, the
// stage's age, and a hash of the casting it's rendered from - so editing
// the stage's instruction, taking another take of it, or recasting the
// character, gives it a new id and new clips. The "~" is what marks a derived preset id (see
// memberPresetID); the "a" keeps a stage apart from a numbered sibling.
func stagePresetID(v ResolvedVoice, stage *store.CharacterStage) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%g\x00%s", stage.Instruct, v.Seed, v.RefText, v.SpeedMultiplier, v.DesignModel)))
	return fmt.Sprintf("%s~a%d-%x", v.PresetID, stage.Age, h[:4])
}
