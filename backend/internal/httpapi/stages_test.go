package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// A stage gives a character another voice from its chapter on: the speakers
// table lists it, the chapters before it stay ready under the old voice,
// and the ones from it on need generating again.
func TestCharacterStage(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	chapters := make([]store.ChapterInput, 3)
	for i := range chapters {
		chapters[i] = store.ChapterInput{Title: "Ch", Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true}}}
	}
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, chapters)
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, store.CharacterVoiceModeAssigned, false, false); err != nil {
		t.Fatal(err)
	}
	book, _ = s.GetBook(bookID)
	will, _, _ := s.UpsertCharacter(store.SeriesScope(book), "Will", false)
	guard, _, _ := s.UpsertCharacter(store.SeriesScope(book), "Guard", true)
	preset, err := s.CreateVoicePreset("Will", "Speak as a teenage boy.", "ref", 7, 1.0, voices.DefaultDesignModel)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCharacterVoice(will.ID, narration.BookCloneModel(book), preset.ID); err != nil {
		t.Fatal(err)
	}
	own, err := srv.Narration.ForParagraph(book, "Will", 0, narration.NoChapter)
	if err != nil {
		t.Fatal(err)
	}
	for idx := range chapters {
		ch, _ := s.GetChapterByIdx(bookID, idx)
		if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Will"}); err != nil {
			t.Fatal(err)
		}
		ps, _ := s.ListParagraphsRaw(ch.ID)
		if err := s.SetParagraphReady(ps[0].ID, own.VoiceID(), 1); err != nil {
			t.Fatal(err)
		}
	}
	stagesURL := ts.URL + "/api/books/" + bookID + "/characters/" + will.ID + "/stages"

	for name, tc := range map[string]struct {
		url  string
		body characterStageRequest
		want int
	}{
		"no age":          {stagesURL, characterStageRequest{ChapterIdx: 1, Instruct: "Speak as a young man."}, http.StatusBadRequest},
		"no such chapter": {stagesURL, characterStageRequest{ChapterIdx: 9, Age: 20, Instruct: "Speak as a young man."}, http.StatusNotFound},
		"a role":          {ts.URL + "/api/books/" + bookID + "/characters/" + guard.ID + "/stages", characterStageRequest{ChapterIdx: 1, Age: 20, Instruct: "Speak as a young man."}, http.StatusBadRequest},
		// No instruct means an LLM rewrite, which this server has no model for.
		"no instruct": {stagesURL, characterStageRequest{ChapterIdx: 1, Age: 20}, http.StatusServiceUnavailable},
	} {
		resp := doJSON(t, http.MethodPost, tc.url, tc.body, nil)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s: status %d, want %d", name, resp.StatusCode, tc.want)
		}
	}

	var created speakerStageDTO
	resp := doJSON(t, http.MethodPost, stagesURL, characterStageRequest{ChapterIdx: 1, Age: 20, Instruct: "Speak as a young man."}, &created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || created.ID == "" || created.ChapterIdx != 1 || created.Age != 20 {
		t.Fatalf("create: status %d, stage %+v", resp.StatusCode, created)
	}

	ready := func() []int {
		t.Helper()
		var detail bookDetailDTO
		resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID, nil, &detail)
		resp.Body.Close()
		out := make([]int, len(detail.Chapters))
		for i, ch := range detail.Chapters {
			out[i] = ch.ReadyCount
		}
		return out
	}
	if got := ready(); len(got) != 3 || got[0] != 1 || got[1] != 0 || got[2] != 0 {
		t.Errorf("ready per chapter after the stage = %v, want [1 0 0]", got)
	}

	var speakers []speakerDTO
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/speakers", nil, &speakers)
	resp.Body.Close()
	var row *speakerDTO
	for i := range speakers {
		if speakers[i].Name == "Will" {
			row = &speakers[i]
		}
	}
	if row == nil || len(row.Voices) != 2 || row.Voices[0].Stage != nil || row.Voices[1].Stage == nil ||
		row.Voices[1].Stage.ID != created.ID || row.Voices[1].Stage.Instruct != "Speak as a young man." {
		t.Fatalf("speakers row = %+v, want Will with his own voice and the stage's", row)
	}
	if own, staged := row.Voices[0], row.Voices[1]; own.ParagraphCount != 1 || own.NeutralCount != 1 || staged.ParagraphCount != 2 || !staged.CanRegenerate {
		t.Errorf("voices = %+v, want 1 line in his own and 2 in the stage's, which can be rendered again", row.Voices)
	}

	// A fresh take is another voice for the stage's lines.
	before, err := srv.Narration.ForParagraph(book, "Will", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/speakers/variants/regenerate", regenerateVariantRequest{Speaker: "Will", StageID: created.ID}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("regenerate: status %d", resp.StatusCode)
	}
	after, err := srv.Narration.ForParagraph(book, "Will", 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if after.PresetID == before.PresetID || after.Seed == before.Seed || after.Instruct != before.Instruct {
		t.Errorf("after a fresh take: %+v, want the same casting under another seed and preset id than %+v", after, before)
	}

	resp = doJSON(t, http.MethodPut, stagesURL+"/"+created.ID, characterStageRequest{Age: 21, Instruct: "Speak as a man of twenty-one."}, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("update: status %d", resp.StatusCode)
	}
	if st, _ := s.GetCharacterStage(created.ID); st == nil || st.Age != 21 || st.ChapterIdx != 1 {
		t.Errorf("after update: %+v, want age 21 still at chapter 1", st)
	}

	resp = doJSON(t, http.MethodDelete, stagesURL+"/"+created.ID, nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	if got := ready(); len(got) != 3 || got[0] != 1 || got[1] != 1 || got[2] != 1 {
		t.Errorf("ready per chapter after removing the stage = %v, want [1 1 1]", got)
	}
}

// TestRecastRewritesStages checks that recasting a character rewrites
// their age stages' castings from the new one, rather than leaving the
// stage voiced from the casting they had before.
func TestRecastRewritesStages(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, fakeLLMBackend{})
	bookID := createTestBook(t, s, "", 0, "“Hi,” Will said.")
	book, _ := s.GetBook(bookID)
	ch, _ := s.GetChapterByIdx(bookID, 0)
	will, _, _ := s.UpsertCharacter(store.SeriesScope(book), "Will", false)
	if _, err := s.SetCharacterStage(will.ID, bookID, ch.ID, 20, "Speak as a young man, as he was cast before.", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.recharacterizeAndInvalidate(context.Background(), book, will.ID, func(ctx context.Context) error {
		_, err := srv.characterizeVoice(ctx, book, will)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stages, err := s.ListCharacterStages(store.SeriesScope(book))
	if err != nil || len(stages) != 1 {
		t.Fatalf("stages = %+v, %v", stages, err)
	}
	// fakeLLMBackend answers every call with the same casting.
	if got := stages[0].Instruct; !strings.HasPrefix(got, "Speak as a calm, warm middle-aged narrator") || stages[0].Age != 20 {
		t.Fatalf("stage after recast = %d %q, want it rewritten from the new casting", stages[0].Age, got)
	}
}
