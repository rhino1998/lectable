package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// A reader's speaker/emotion edits are recorded with the machine's
// original, listed by GET .../paragraph-edits, and survive a re-run of the
// passes that wrote the original.
func TestParagraphEditsRecordedAndListed(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title: "One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Hi,”", IsQuote: true},
			{Kind: store.BlockText, Text: "he said.", Inline: true},
			{Kind: store.BlockText, Text: "“Bye.”", IsQuote: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Petros", 2: "Dave"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphEmotions(ch.ID, map[int]string{0: "angry"}); err != nil {
		t.Fatal(err)
	}
	base := ts.URL + "/api/books/" + bookID
	put := func(path string, body any) {
		t.Helper()
		resp := doJSON(t, http.MethodPut, base+path, body, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("PUT %s: %d", path, resp.StatusCode)
		}
	}
	put("/chapters/0/paragraphs/0/speaker", map[string]string{"speaker": "Steve"})
	put("/chapters/0/paragraphs/0/speaker", map[string]string{"speaker": "Dave"}) // a second edit keeps the first original
	put("/chapters/0/paragraphs/0/emotion", map[string]string{"emotion": "warm"})
	put("/chapters/0/paragraphs/2/speaker", map[string]string{"speaker": "Steve"})
	put("/chapters/0/paragraphs/2/speaker", map[string]string{"speaker": "Dave"}) // back to the original: no record
	put("/chapters/0/paragraphs/2/scare-quote", map[string]bool{"scareQuote": true})

	list := func(query string) []paragraphEditDTO {
		t.Helper()
		var out paragraphEditsResponse
		resp := doJSON(t, http.MethodGet, base+"/paragraph-edits"+query, nil, nil)
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Edits
	}
	if got := list("?field=scare_quote"); len(got) != 1 || got[0].ParagraphIdx != 2 || got[0].Original != "false" || got[0].Value != "true" {
		t.Fatalf("scare-quote edits = %+v", got)
	}
	edits := list("?field=speaker")
	edits = append(list("?field=emotion"), edits...)
	if len(edits) != 2 {
		t.Fatalf("edits = %+v, want the speaker and emotion of paragraph 0", edits)
	}
	if e := edits[1]; e.Field != paragraphEditSpeaker || e.Original != "Petros" || e.Value != "Dave" || e.Text != "“Hi,”" {
		t.Fatalf("speaker edit = %+v", e)
	}
	if e := edits[0]; e.Field != paragraphEditEmotion || e.Original != "angry" || e.Value != "warm" {
		t.Fatalf("emotion edit = %+v", e)
	}
	if got := list("?field=speaker"); len(got) != 1 {
		t.Fatalf("?field=speaker = %+v", got)
	}
	if got, _ := s.EditedParagraphIdx(ch.ID, store.EditSpeaker); !got[0] || got[2] {
		t.Fatalf("EditedParagraphIdx = %v", got)
	}
}

// A re-run of attribution leaves a reader-corrected line alone; the
// preview reports it with the model's proposal and the original answer.
func TestAttributionKeepsEditedLines(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, &summaryLLM{}) // answers "the old sailor" -> Mocar via the cast
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{
		Title: "One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "The old sailor squinted at the dawn."},
			{Kind: store.BlockText, Text: "“Weigh anchor,”", IsQuote: true},
			{Kind: store.BlockText, Text: "“Aye,”", IsQuote: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if _, _, err := srv.summarizeChapter(context.Background(), book, ch); err != nil {
		t.Fatal(err)
	}
	if _, _, err := srv.attributeChapter(context.Background(), book, ch, attributeFull); err != nil {
		t.Fatal(err)
	}
	base := ts.URL + "/api/books/" + bookID
	resp := doJSON(t, http.MethodPut, base+"/chapters/0/paragraphs/2/speaker", map[string]string{"speaker": "Bosun"}, nil)
	resp.Body.Close()
	if _, _, err := srv.attributeChapter(context.Background(), book, ch, attributeFull); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.ListParagraphsRaw(ch.ID)
	if ps[1].Speaker != "Mocar" || ps[2].Speaker != "Bosun" {
		t.Fatalf("speakers = %q, %q; want Mocar re-attributed and the edited Bosun kept", ps[1].Speaker, ps[2].Speaker)
	}

	var preview attributionPreviewResponse
	resp = doJSON(t, http.MethodPost, base+"/chapters/0/attribute-speakers/preview", nil, nil)
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&preview); err != nil {
		t.Fatal(err)
	}
	want := []attributionPreviewLineDTO{
		{ParagraphIdx: 1, Text: "“Weigh anchor,”", Current: "Mocar", Proposed: "Mocar"},
		{ParagraphIdx: 2, Text: "“Aye,”", Current: "Bosun", Proposed: "Mocar", Edited: true, Original: "Mocar"},
	}
	if !reflect.DeepEqual(preview.Lines, want) {
		t.Fatalf("preview = %+v, want %+v", preview.Lines, want)
	}
}

// whoLLM answers every "(who?)" line of an attribution prompt (whoLine)
// with Bert, recording each prompt.
type whoLLM struct{ prompts []string }

func (l *whoLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	l.prompts = append(l.prompts, user)
	var out strings.Builder
	for _, m := range whoLine.FindAllStringSubmatch(user, -1) {
		fmt.Fprintf(&out, "%s: Bert\n", m[1])
	}
	return out.String(), nil
}

// Re-attributing around the reader's corrections (?scope=edits) asks only
// the dialogue near a corrected line, shows the correction, and leaves the
// rest of the chapter as it was.
func TestAttributeAroundEdits(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	llm := &whoLLM{}
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, llm)
	var blocks []store.BlockInput
	for i := 0; i < 60; i++ {
		blocks = append(blocks, store.BlockInput{Kind: store.BlockText, Text: fmt.Sprintf("“Line %d.”", i), IsQuote: true})
	}
	bookID, _, err := s.CreateBook("Test Book", "A", "en", "", "", 0, []store.ChapterInput{{Title: "One", Blocks: blocks}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	ch, _ := s.GetChapterByIdx(bookID, 0)
	old := map[int]string{}
	for i := 0; i < 60; i++ {
		old[i] = "Old"
	}
	if err := s.SetParagraphSpeakers(ch.ID, old); err != nil {
		t.Fatal(err)
	}
	base := ts.URL + "/api/books/" + bookID
	resp := doJSON(t, http.MethodPost, base+"/chapters/0/attribute-speakers?scope=edits", nil, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("scope=edits with no corrections: %d, want 409", resp.StatusCode)
	}
	resp = doJSON(t, http.MethodPut, base+"/chapters/0/paragraphs/30/speaker", map[string]string{"speaker": "Dave"}, nil)
	resp.Body.Close()

	if _, _, err := srv.attributeChapter(context.Background(), book, ch, attributeAroundEdits); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.ListParagraphsRaw(ch.ID)
	for _, p := range ps {
		want := "Old"
		switch {
		case p.Idx == 30:
			want = "Dave"
		case p.Idx >= 30-aroundEditsAsk && p.Idx <= 30+aroundEditsAsk:
			want = "Bert"
		}
		if p.Speaker != want {
			t.Errorf("¶%d speaker = %q, want %q", p.Idx, p.Speaker, want)
		}
	}
	prompt := strings.Join(llm.prompts, "\n")
	for _, want := range []string{"30 (Dave): “Line 30.”", "10: “Line 10.”", "22 (who?)"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q:\n%s", want, prompt)
		}
	}
	// ¶9 is outside the context band; ¶21 is context, shown but not asked.
	if strings.Contains(prompt, "\n9: ") || strings.Contains(prompt, "21 (who?)") {
		t.Errorf("prompt has a line outside the context band or a context line asked:\n%s", prompt)
	}
}
