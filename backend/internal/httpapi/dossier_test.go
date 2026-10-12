package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// storyLLM answers findStory's calls by the prompt each starts with. It
// passes the gap statements and places the jump before chapter 5, names
// "kobold" a kind, passes only the claim that rests on "was a kobold",
// reads two ages for Jason and passes the first.
type storyLLM struct{ fakeLLMBackend }

func (l storyLLM) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	switch {
	case strings.HasPrefix(systemPrompt, "You read passages from a novel and pick out the ones that show a long stretch"):
		var gaps []string
		for i, line := range strings.Split(userPrompt, "\n") {
			if strings.Contains(line, "Over the last few years, the town had grown.") {
				gaps = append(gaps, `{"n": `+string(rune('0'+i))+`, "how": "lookback", "years": 3, "evidence": "Over the last few years, the town had grown."}`)
			}
			if strings.Contains(line, "It had been five years since the siege.") {
				gaps = append(gaps, `{"n": `+string(rune('0'+i))+`, "how": "lookback", "years": 5, "evidence": "It had been five years since the siege."}`)
			}
		}
		return `{"gaps": [` + strings.Join(gaps, ",") + `]}`, nil
	case strings.HasPrefix(systemPrompt, "You place a time jump"):
		return `{"jump": true, "before_chapter": 5, "years": 5, "why": "test"}`, nil
	case strings.HasPrefix(systemPrompt, "You are given words a novel uses"):
		return `{"kinds": ["kobold", "moment"]}`, nil
	case strings.HasPrefix(systemPrompt, "You check claims about what kind"):
		var who []string
		for i, line := range strings.Split(userPrompt, "\n") {
			if strings.Contains(line, "Loth was a kobold") {
				who = append(who, `{"n": `+string(rune('0'+i))+`, "who": "Loth"}`)
			} else if i > 0 && line != "" {
				who = append(who, `{"n": `+string(rune('0'+i))+`, "who": "nobody"}`)
			}
		}
		return `{"claims": [` + strings.Join(who, ",") + `]}`, nil
	case strings.HasPrefix(systemPrompt, "You read passages from a novel and pick out the ones that state how old"):
		return `{"ages": [
			{"n": 1, "who": "Jason", "age": 12, "evidence": "Jason was a twelve-year-old boy."},
			{"n": 2, "who": "Jason", "age": 8, "evidence": "since he was eight years old"},
			{"n": 2, "who": "Nobody", "age": 30, "evidence": "since he was eight years old"}]}`, nil
	case strings.HasPrefix(systemPrompt, "You check statements about one character's age"):
		return `{"holds": [1]}`, nil
	}
	return l.fakeLLMBackend.LLMGenerate(ctx, systemPrompt, userPrompt, temp, maxTokens)
}

// TestFindStory runs fact finding's second half over a small book with a
// stand-in model: a run of "years have passed" statements becomes one
// proposed time jump the reader can accept at another chapter; a
// character named beside a kind, and one whose age is stated, get notes
// that wait for review, and an accepted kind makes them one of that kind
// for the facts about it.
func TestFindStory(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, storyLLM{})
	texts := [][]string{
		{"Jason was a twelve-year-old boy.", "He had wanted a sword since he was eight years old."},
		{"Loth was a kobold, and nobody could tell her age."},
		{"Will was a moment too late. Loth was a moment behind him."},
		{"The siege ended at dawn."},
		{"An epilogue of sorts."},
		{"Over the last few years, the town had grown."},
		{"It had been five years since the siege."},
	}
	var chapters []store.ChapterInput
	for i, ps := range texts {
		blocks := make([]store.BlockInput, len(ps))
		for j, p := range ps {
			blocks[j] = store.BlockInput{Kind: store.BlockText, Text: p}
		}
		chapters = append(chapters, store.ChapterInput{Title: "Chapter " + string(rune('A'+i)), Blocks: blocks})
	}
	bookID, _, err := s.CreateBook("Tower", "Author", "en", "", "", 0, chapters)
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	scope := store.SeriesScope(book)
	for _, name := range []string{"Jason", "Loth", "Will"} {
		c, _, _ := s.UpsertCharacter(scope, name, false)
		if err := s.SetCharacterSummary(c.ID, "Speak as a young man. He is brisk.", "A line to read."); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UpsertWorldFact(scope, store.WorldFact{Key: "kobold", Fact: "Kobolds live to about fifteen."}); err != nil {
		t.Fatal(err)
	}

	if err := srv.findStory(context.Background(), book, nil); err != nil {
		t.Fatal(err)
	}

	// One proposed jump, before chapter 5, with both sentences.
	base := ts.URL + "/api/books/" + bookID
	var jumps []timeJumpDTO
	doJSON(t, http.MethodGet, base+"/time-jumps", nil, &jumps)
	if len(jumps) != 1 || !jumps[0].Proposed || jumps[0].ChapterIdx != 5 || jumps[0].Years != 5 ||
		!strings.Contains(jumps[0].Evidence, "Chapter F: Over the last few years") || !strings.Contains(jumps[0].Evidence, "five years since the siege") {
		t.Fatalf("proposed jumps = %+v", jumps)
	}
	// Accepted a chapter earlier than proposed: a real jump there, the
	// proposal gone.
	if resp := doJSON(t, http.MethodPost, base+"/time-jumps/proposals/"+jumps[0].ID+"/accept", timeJumpRequest{ChapterIdx: 4, Years: 5}, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("accept jump: %d", resp.StatusCode)
	}
	jumps = nil // decoding into the old slice would keep its omitted fields
	doJSON(t, http.MethodGet, base+"/time-jumps", nil, &jumps)
	if len(jumps) != 1 || jumps[0].Proposed || jumps[0].ChapterIdx != 4 {
		t.Fatalf("jumps after accept = %+v", jumps)
	}
	// Found again, the same statements are explained by the jump there.
	if err := srv.findStory(context.Background(), book, nil); err != nil {
		t.Fatal(err)
	}
	jumps = nil // decoding into the old slice would keep its omitted fields
	doJSON(t, http.MethodGet, base+"/time-jumps", nil, &jumps)
	if len(jumps) != 1 {
		t.Fatalf("jumps after a second run = %+v", jumps)
	}

	// Notes: Loth a kobold (not "a moment"), Jason twelve (not eight).
	var notes []characterNoteDTO
	doJSON(t, http.MethodGet, base+"/character-notes", nil, &notes)
	if len(notes) != 2 || notes[0].CharacterName != "Loth" || notes[0].Kind != store.NoteKind || notes[0].Value != "kobold" || !notes[0].Proposed ||
		!strings.Contains(notes[0].Evidence, "Loth was a kobold") || notes[0].EvidenceChapterIdx == nil || *notes[0].EvidenceChapterIdx != 1 ||
		notes[1].CharacterName != "Jason" || notes[1].Kind != store.NoteAge || notes[1].Value != "12" {
		t.Fatalf("notes = %+v", notes)
	}
	// A proposed note reaches nobody's casting.
	loth, _ := s.GetCharacter(notes[0].CharacterID)
	if got := srv.castingWorldNotes(book, *loth, loth.Summary); len(got) != 0 {
		t.Fatalf("world notes before accepting = %q", got)
	}
	var ruled ruleCharacterNotesResponse
	doJSON(t, http.MethodPost, base+"/character-notes/rule", ruleCharacterNotesRequest{IDs: []string{notes[0].ID, notes[1].ID}, Accept: true}, &ruled)
	if ruled.Recast != 1 {
		t.Fatalf("recast = %d, want 1 (Loth, whom the kobold fact now reaches)", ruled.Recast)
	}
	got := srv.castingWorldNotes(book, *loth, loth.Summary)
	if len(got) != 2 || !strings.HasPrefix(got[0], "kobold: ") || !strings.Contains(got[1], "Loth's kind: kobold") {
		t.Fatalf("Loth's world notes = %q", got)
	}
	jason, _ := s.GetCharacter(notes[1].CharacterID)
	if got := srv.castingWorldNotes(book, *jason, jason.Summary); len(got) != 1 || !strings.Contains(got[0], `Jason's age: 12 years old as of "Chapter A"`) {
		t.Fatalf("Jason's world notes = %q", got)
	}
	// Removed, a note is listed as skipped and gone from casting; put back
	// for review, it is a proposal again.
	removed := notes[1].ID
	doJSON(t, http.MethodPost, base+"/character-notes/rule", ruleCharacterNotesRequest{IDs: []string{removed}}, &ruled)
	notes = nil
	doJSON(t, http.MethodGet, base+"/character-notes", nil, &notes)
	if len(notes) != 2 || notes[0].Proposed || notes[0].Skipped || notes[1].ID != removed || !notes[1].Skipped {
		t.Fatalf("notes after removing one = %+v", notes)
	}
	if got := srv.castingWorldNotes(book, *jason, jason.Summary); len(got) != 0 {
		t.Fatalf("Jason's world notes after removing his age = %q", got)
	}
	doJSON(t, http.MethodPost, base+"/character-notes/rule", ruleCharacterNotesRequest{IDs: []string{removed}, Review: true}, &ruled)
	notes = nil
	doJSON(t, http.MethodGet, base+"/character-notes", nil, &notes)
	if len(notes) != 2 || !notes[1].Proposed || notes[1].Skipped {
		t.Fatalf("notes after putting one back = %+v", notes)
	}
}

func TestNeverYounger(t *testing.T) {
	for _, c := range []struct{ ages, want []int }{
		{[]int{15, 12, 14}, []int{0}},
		{[]int{12, 12, 17}, []int{0, 1, 2}},
		{[]int{15, 12}, []int{0}},
		{nil, nil},
	} {
		got := neverYounger(c.ages)
		if len(got) != len(c.want) {
			t.Fatalf("neverYounger(%v) = %v, want %v", c.ages, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("neverYounger(%v) = %v, want %v", c.ages, got, c.want)
			}
		}
	}
}

func TestTimeGapRuns(t *testing.T) {
	runs := timeGapRuns([]gapStatement{
		{chapter: 1, years: 5},              // the opening: backstory
		{chapter: 40, years: 2},             // one remark looking back
		{chapter: 90, years: 3, skip: true}, // one "three years later"
		{chapter: 202, years: 3}, {chapter: 217, years: 5}, {chapter: 231, years: 5},
		{chapter: 300, years: 3}, {chapter: 311, years: 3},
	})
	if len(runs) != 3 || len(runs[0]) != 1 || runs[0][0].chapter != 90 || len(runs[1]) != 3 || len(runs[2]) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
}
