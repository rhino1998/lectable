package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// samePersonLLM answers every same-person question with reply, counting
// the questions.
type samePersonLLM struct {
	reply string
	calls int
}

func (l *samePersonLLM) LLMGenerate(_ context.Context, sys, _ string, _ float32, _ int) (string, error) {
	if strings.Contains(sys, "cast list for duplicates") {
		l.calls++
		return l.reply, nil
	}
	return "", nil
}

// rosterTestBook makes a book whose paragraphs are attributed to the given
// speakers ("" for narration), each a character of its roster.
func rosterTestBook(t *testing.T, s *store.SQLiteStore, lines ...[2]string) *store.Book {
	t.Helper()
	blocks := make([]store.BlockInput, len(lines))
	speakers := map[int]string{}
	for i, l := range lines {
		blocks[i] = store.BlockInput{Kind: store.BlockText, Text: l[1], IsQuote: l[0] != ""}
		if l[0] != "" {
			speakers[i] = l[0]
		}
	}
	bookID, _, err := s.CreateBook("Underverse", "A", "en", "", "", 0, []store.ChapterInput{{Title: "One", Blocks: blocks}})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	for _, name := range speakers {
		if _, _, err := s.UpsertCharacter(store.SeriesScope(book), name, false); err != nil {
			t.Fatal(err)
		}
	}
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphSpeakers(ch.ID, speakers); err != nil {
		t.Fatal(err)
	}
	return book
}

func pendingProposals(t *testing.T, url string) map[string]rosterProposalDTO {
	t.Helper()
	var got rosterReviewResponse
	if resp := doJSON(t, http.MethodGet, url, nil, &got); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET roster-review: %d", resp.StatusCode)
	}
	out := map[string]rosterProposalDTO{}
	for _, p := range got.Proposals {
		key := string(p.Kind) + " " + p.Subject
		if p.Target != "" {
			key += " -> " + p.Target
		}
		out[key] = p
	}
	return out
}

func TestRosterReview(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	llm := &samePersonLLM{reply: `{"evidence": "Baron Sanguis—no, just Sanguis now", "verdict": "same"}`}
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, llm)
	book := rosterTestBook(t, s,
		[2]string{"Baron Sanguis", "“Kneel.”"},
		[2]string{"Baron Sanguis", "“Now.”"},
		[2]string{"Sanguis", "“Never again.”"},
		[2]string{"", "Baron Sanguis—no, just Sanguis now—hung his head."},
		[2]string{"", "Prince Sanguis watched his father from the door."},
		[2]string{"Prince Sanguis", "“Father.”"},
		[2]string{"Prince Sanguis", "“Please.”"},
		[2]string{"Lars", "“Hello.”"},
		[2]string{"Lars Machmüller", "“Hello again.”"},
		[2]string{"", "Lars Machmüller waved; Lars was like that."},
		[2]string{"helmsman", "“Aye.”"},
	)
	url := ts.URL + "/api/books/" + book.ID + "/roster-review"
	if err := srv.ReviewRoster(context.Background(), book); err != nil {
		t.Fatal(err)
	}
	got := pendingProposals(t, url)
	for _, want := range []string{"merge Sanguis -> Baron Sanguis", "merge Lars -> Lars Machmüller", "mark_role helmsman"} {
		if _, ok := got[want]; !ok {
			t.Errorf("no proposal %q in %v", want, got)
		}
	}
	// The evidence names neither "Prince Sanguis" pair's two names in one
	// passage, so those are ruled out, not proposed.
	if len(got) != 3 {
		t.Errorf("%d pending proposals, want 3: %v", len(got), got)
	}
	if p := got["merge Sanguis -> Baron Sanguis"]; p.Source != store.RosterFromLLM || len(p.Evidence) != 1 || p.SubjectLines != 1 || p.TargetLines != 2 {
		t.Errorf("Sanguis proposal = %+v", p)
	}

	// A second run asks about nothing it already settled.
	calls := llm.calls
	if err := srv.ReviewRoster(context.Background(), book); err != nil {
		t.Fatal(err)
	}
	if llm.calls != calls {
		t.Errorf("second run made %d more calls", llm.calls-calls)
	}

	// Accept the merge: the lines move, the name becomes an alias.
	merge := got["merge Sanguis -> Baron Sanguis"]
	if resp := doJSON(t, http.MethodPost, url+"/proposals/"+merge.ID+"/accept", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("accept: %d", resp.StatusCode)
	}
	scope := store.SeriesScope(book)
	if c, _ := s.GetCharacterByName(scope, "Sanguis"); c != nil {
		t.Error("Sanguis still a character after the merge")
	}
	baron, _ := s.GetCharacterByName(scope, "Baron Sanguis")
	if counts, _ := s.SpeakerLineCounts(book.ID); baron == nil || counts["Baron Sanguis"] != 3 || len(baron.Aliases) != 1 {
		t.Errorf("after merge: %+v, lines %v", baron, counts)
	}
	if resp := doJSON(t, http.MethodPost, url+"/proposals/"+merge.ID+"/accept", nil, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("accepting twice: %d, want 409", resp.StatusCode)
	}

	// Skip one; it is gone and never proposed again.
	role := got["mark_role helmsman"]
	if resp := doJSON(t, http.MethodPost, url+"/proposals/"+role.ID+"/skip", nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("skip: %d", resp.StatusCode)
	}
	if err := srv.ReviewRoster(context.Background(), book); err != nil {
		t.Fatal(err)
	}
	if got := pendingProposals(t, url); len(got) != 1 {
		t.Errorf("pending after accept and skip: %v", got)
	}

	// A proposal filed from outside replaces a ruled-out pair.
	req := createRosterProposalRequest{Kind: speakerattr.RosterMerge, Subject: "Prince Sanguis", Target: "Baron Sanguis", Reason: "read chapter 3", Evidence: []string{"watched his father"}}
	var created idResponse
	if resp := doJSON(t, http.MethodPost, url+"/proposals", req, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPost, url+"/proposals", req, nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("creating twice: %d, want 409", resp.StatusCode)
	}
	if p := pendingProposals(t, url)["merge Prince Sanguis -> Baron Sanguis"]; p.ID != created.ID || p.Source != store.RosterFromAgent {
		t.Errorf("filed proposal = %+v", p)
	}
	if resp := doJSON(t, http.MethodPost, url+"/proposals", createRosterProposalRequest{Kind: speakerattr.RosterMerge, Subject: "Nobody", Target: "Baron Sanguis"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown subject: %d, want 400", resp.StatusCode)
	}
}
