package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// speakerStageDTO is one of a speaker's stages (store.CharacterStage): from
// this chapter on they are voiced as about Age years old. A speaker's
// stages cover its whole series, so one may start in another book.
type speakerStageDTO struct {
	ID           string `json:"id"`
	BookID       string `json:"bookId"`
	BookTitle    string `json:"bookTitle"`
	ChapterID    string `json:"chapterId"`
	ChapterIdx   int    `json:"chapterIdx"`
	ChapterTitle string `json:"chapterTitle"`
	Age          int    `json:"age"`
	// Instruct is the casting this stage's voice is designed from - the
	// speaker's own Summary rewritten for Age, or the reader's edit of it.
	Instruct string `json:"instruct"`
}

// maxStageAge bounds a stage's age: long-lived kinds count centuries.
const maxStageAge = 9999

// stageVoices lists the voice char has from each of their stages on
// (speakerDTO.Voices), in story order, from stages (every stage of the
// roster). resolved is char's own voice in book; lines and emotionCounts
// are this book's lines per stage id, and those of them carrying each
// emotion. A character with no voice of their own has nothing to age, so
// their stages list no clip.
func (s *Server) stageVoices(book *store.Book, books []store.Book, bookVoice, resolved narration.ResolvedVoice, char store.Character, stages []store.CharacterStage, lines map[string]int, emotionCounts map[string]map[string]int) []speakerVoiceDTO {
	order := map[string]int{}
	titles := map[string]string{}
	for i, b := range books {
		order[b.ID] = i
		titles[b.ID] = b.Title
	}
	var out []speakerVoiceDTO
	for _, st := range stages {
		if st.CharacterID != char.ID {
			continue
		}
		stage := speakerStageDTO{
			ID: st.ID, BookID: st.BookID, BookTitle: titles[st.BookID],
			ChapterID: st.ChapterID, ChapterIdx: st.ChapterIdx,
			Age: st.Age, Instruct: st.Instruct,
		}
		if ch, err := s.chapterByID(st.ChapterID); err == nil && ch != nil {
			stage.ChapterTitle = ch.Title
		}
		dto := speakerVoiceDTO{Stage: &stage}
		v := narration.StageVoice(resolved, bookVoice, char, &st)
		s.voiceSamples(&dto, v.PresetID, lines[st.ID], emotionCounts[st.ID])
		if v.PresetID != resolved.PresetID {
			dto.CanRegenerate = true
			if path := audiopath.VoicePresetRefFile(s.DataDir, v.PresetID); fileExists(path) {
				dto.RefAudioURL = withClipVersion("/api/voices/role-variants/"+v.PresetID+"/audio", path)
			}
		}
		out = append(out, dto)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if oi, oj := order[out[i].Stage.BookID], order[out[j].Stage.BookID]; oi != oj {
			return oi < oj
		}
		return out[i].Stage.ChapterIdx < out[j].Stage.ChapterIdx
	})
	return out
}

type characterStageRequest struct {
	// ChapterIdx is the chapter of this book the stage starts at. Ignored
	// when updating a stage, which stays where it is.
	ChapterIdx int `json:"chapterIdx"`
	// Age is about how old the character is from there on, in years.
	Age int `json:"age"`
	// Instruct is the stage's casting. Empty asks for the character's own
	// casting rewritten for Age (speakerattr.Client.AgeCasting).
	Instruct string `json:"instruct,omitempty"`
}

// handleSetCharacterStage starts a stage of a character's voice at one of
// this book's chapters: from there on - and through the later books of
// the series - their lines are voiced as about that age. Setting one
// where the character already has one replaces it. With no instruct, the
// character's own casting is rewritten for the age, which needs the
// speaker LLM and a characterized character.
//
// Nothing is deleted: the lines from that chapter on resolve to another
// voice, so their audio under the old one stops counting as ready and is
// generated again like any other missing audio.
func (s *Server) handleSetCharacterStage(w http.ResponseWriter, r *http.Request) {
	var req characterStageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	book, char, ok := s.stageSubject(w, r)
	if !ok {
		return
	}
	ch, err := s.Store.GetChapterByIdx(book.ID, req.ChapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	instruct, ok := s.stageInstruct(w, r, book, char, req)
	if !ok {
		return
	}
	st, err := s.Store.SetCharacterStage(char.ID, book.ID, ch.ID, req.Age, instruct, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.renderStageRef(book, *char, *st)
	writeJSON(w, http.StatusCreated, speakerStageDTO{
		ID: st.ID, BookID: st.BookID, BookTitle: book.Title, ChapterID: st.ChapterID, ChapterIdx: st.ChapterIdx,
		ChapterTitle: ch.Title, Age: st.Age, Instruct: st.Instruct,
	})
}

// handleUpdateCharacterStage changes a stage's age and casting; an empty
// instruct has the casting rewritten for the age again.
func (s *Server) handleUpdateCharacterStage(w http.ResponseWriter, r *http.Request) {
	var req characterStageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	book, char, ok := s.stageSubject(w, r)
	if !ok {
		return
	}
	st, err := s.Store.GetCharacterStage(r.PathValue("stageId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if st == nil || st.CharacterID != char.ID {
		writeError(w, http.StatusNotFound, "stage not found")
		return
	}
	instruct, ok := s.stageInstruct(w, r, book, char, req)
	if !ok {
		return
	}
	updated, err := s.Store.SetCharacterStage(char.ID, st.BookID, st.ChapterID, req.Age, instruct, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.renderStageRef(book, *char, *updated)
	writeNoContent(w)
}

// handleDeleteCharacterStage removes a stage: its lines go back to the
// stage before it, or to the character's own voice.
func (s *Server) handleDeleteCharacterStage(w http.ResponseWriter, r *http.Request) {
	_, char, ok := s.stageSubject(w, r)
	if !ok {
		return
	}
	st, err := s.Store.GetCharacterStage(r.PathValue("stageId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if st == nil || st.CharacterID != char.ID {
		writeError(w, http.StatusNotFound, "stage not found")
		return
	}
	if err := s.Store.DeleteCharacterStage(st.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// stageSubject reads the book and character a stage request names,
// answering the request itself when either is missing or the character
// can't have stages.
func (s *Server) stageSubject(w http.ResponseWriter, r *http.Request) (*store.Book, *store.Character, bool) {
	book, err := s.Store.GetBook(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return nil, nil, false
	}
	char, err := s.Store.GetCharacter(r.PathValue("characterId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if char == nil || char.Scope != store.SeriesScope(book) {
		writeError(w, http.StatusNotFound, "character not found")
		return nil, nil, false
	}
	if char.IsRole {
		writeError(w, http.StatusBadRequest, "a role is several people, with no one age")
		return nil, nil, false
	}
	return book, char, true
}

// stageInstruct is the casting a stage request asks for: the one it
// gives, or the character's own rewritten for the age. It answers the
// request itself on failure.
func (s *Server) stageInstruct(w http.ResponseWriter, r *http.Request, book *store.Book, char *store.Character, req characterStageRequest) (string, bool) {
	if req.Age < 1 || req.Age > maxStageAge {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("age must be between 1 and %d", maxStageAge))
		return "", false
	}
	if instruct := strings.TrimSpace(req.Instruct); instruct != "" {
		return instruct, true
	}
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "writing a casting for an age needs the speaker LLM (set LECTABLE_SPEAKER_LLM_MODEL_PATH); give the instruct instead")
		return "", false
	}
	if char.Summary == "" {
		writeError(w, http.StatusBadRequest, "this character has no casting to age yet - characterize them first, or give the instruct")
		return "", false
	}
	var instruct string
	// Keyed apart from the character's own characterization: a task with
	// the same key joins the one already queued, and that one's result
	// isn't this one's.
	err := s.Jobs.RunCharacterization(r.Context(), book.ID, char.ID+"~age", fmt.Sprintf("%s at %d", char.Name, req.Age), func(ctx context.Context) error {
		var err error
		instruct, err = s.Speaker.AgeCasting(ctx, char.Summary, req.Age, s.castingWorldNotes(book, *char, char.Summary))
		return err
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "casting for that age failed: "+err.Error())
		return "", false
	}
	return instruct, true
}

// renderStageRef renders, in the background, the reference clip stage's
// lines will clone from in book, so it can be heard on the Speakers tab
// before any of those lines generate. Nothing to do when the character has
// no voice of their own in this book.
func (s *Server) renderStageRef(book *store.Book, char store.Character, stage store.CharacterStage) {
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		return
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel)
	if err != nil {
		return
	}
	resolved, err := s.Narration.ResolveCharacterVoice(book, bookVoice, char, cloneModel, presetID)
	if err != nil {
		return
	}
	v := narration.StageVoice(resolved, bookVoice, char, &stage)
	if v.PresetID == resolved.PresetID {
		return
	}
	go func() {
		name := fmt.Sprintf("%s at %d", char.Name, stage.Age)
		if _, err := s.ensureVoiceRef(context.Background(), v.PresetID, name, v.Instruct, v.Seed, v.RefText, v.SpeedMultiplier, v.DesignModel); err != nil {
			log.Printf("stage voice %s: render reference clip: %v", name, err)
			return
		}
		// The clip is a file: tell the speakers topic it has appeared.
		if s.Live != nil {
			s.Live.Invalidate(dbDeps("character_stages")...)
		}
	}()
}
