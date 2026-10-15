package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/direction"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// TestSetParagraphDirection covers the reader's direction override: the
// line written back with direction is stored and shown on the chapter, a
// reply changing its words or a narration line is refused, and "" clears.
func TestSetParagraphDirection(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "Test Author", "en", "", "", 0, []store.ChapterInput{{
		Title: "Chapter One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Fine. Go, then.”", IsQuote: true},
			{Kind: store.BlockText, Text: "she sighed.", Inline: true},
		},
	}})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	url := func(pidx int) string {
		return ts.URL + "/api/books/" + bookID + "/chapters/0/paragraphs/" + strconv.Itoa(pidx) + "/direction"
	}
	directionOf := func() string {
		var ch chapterDetailDTO
		doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/chapters/0", nil, &ch)
		return ch.Paragraphs[0].Direction
	}
	set := func(pidx int, d string) int {
		return doJSON(t, http.MethodPut, url(pidx), map[string]string{"direction": d}, nil).StatusCode
	}

	if code := set(0, "[worn out, giving in] Fine. [sighs] Go, then."); code != http.StatusNoContent {
		t.Fatalf("set: status %d", code)
	}
	if got, want := directionOf(), "[worn out, giving in] “Fine. [sighs] Go, then.”"; got != want {
		t.Fatalf("direction = %q, want %q", got, want)
	}
	if code := set(0, "[angry] Fine. Go away, then."); code != http.StatusBadRequest {
		t.Fatalf("changed words: status %d, want 400", code)
	}
	if code := set(1, "[sad] she sighed."); code != http.StatusBadRequest {
		t.Fatalf("narration: status %d, want 400", code)
	}
	if code := set(0, ""); code != http.StatusNoContent {
		t.Fatalf("clear: status %d", code)
	}
	if got := directionOf(); got != "" {
		t.Fatalf("direction after clear = %q", got)
	}
}

func TestDirectionMarksForDirectedModel(t *testing.T) {
	text := "“Fine. Go, then.”"
	d, err := direction.Parse(text, "[tired] Fine. [sighs] Go, then.")
	if err != nil {
		t.Fatal(err)
	}
	p := store.Paragraph{Text: text, IsQuote: true, Direction: d}
	marks := directionMarksFor(p, voices.Drama3CloneModel)
	if len(marks) != 2 || marks[0] != (directionMarkDTO{Offset: 0, Tag: "[tired] "}) || marks[1] != (directionMarkDTO{Offset: 7, Tag: "[sighs] "}) {
		t.Fatalf("marks = %+v", marks)
	}
	if marks := directionMarksFor(p, voices.PocketCloneModel); marks != nil {
		t.Fatalf("an undirected model got marks %+v", marks)
	}
}

func TestWantsDirection(t *testing.T) {
	all := []store.Paragraph{
		{Text: "“Hello.”", IsQuote: true},
		{Text: "“Ha. No.”", IsQuote: true},
		{Text: "he laughed.", Inline: true},
		{Text: "“Get out!”", IsQuote: true, Emotion: "shout"},
		{Text: "“Fine.”", IsQuote: true, ScareQuote: true, Emotion: "shout"},
	}
	for i, want := range []bool{false, true, false, true, false} {
		if got := wantsDirection(all, i); got != want {
			t.Errorf("wantsDirection(%d) = %v, want %v", i, got, want)
		}
	}
}

// deliveryLLM answers every direction call with reply, the line given.
type deliveryLLM struct{ reply func(user string) string }

func (l deliveryLLM) LLMGenerate(_ context.Context, _, user string, _ float32, _ int) (string, error) {
	return l.reply(user), nil
}

// TestDeliverChapter: the second stage directs the lines the labels (or a
// delivery verb beside them) pick, clears an old direction from a line no
// longer picked, leaves a hand-directed line alone, and marks the pass.
func TestDeliverChapter(t *testing.T) {
	srv, s, _, _ := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "Test Author", "en", "", "", 0, []store.ChapterInput{{
		Title: "Chapter One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Get out!”", IsQuote: true},
			{Kind: store.BlockText, Text: "“Hello.”", IsQuote: true},
			{Kind: store.BlockText, Text: "“Ha. No.”", IsQuote: true},
			{Kind: store.BlockText, Text: "he laughed.", Inline: true},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := s.GetChapterByIdx(bookID, 0)
	if err := s.SetParagraphEmotions(ch.ID, map[int]string{0: "shout"}); err != nil {
		t.Fatal(err)
	}
	stale, _ := direction.Parse("“Hello.”", "[cold] Hello.")
	if err := s.SetParagraphDirections(ch.ID, map[int]direction.Direction{1: stale}); err != nil {
		t.Fatal(err)
	}
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, deliveryLLM{reply: func(user string) string {
		switch {
		case strings.Contains(user, ">> “Get out!”"):
			return "[roaring, at the end of her rope] Get out!"
		case strings.Contains(user, ">> “Ha. No.”"):
			return "[laughs] Ha. No."
		}
		t.Errorf("unexpected direction call: %s", user)
		return ""
	}})
	book, _ := s.GetBook(bookID)
	all, _ := s.ListParagraphsRaw(ch.ID)
	if _, err := srv.deliverChapter(context.Background(), book, ch, all); err != nil {
		t.Fatal(err)
	}
	all, _ = s.ListParagraphsRaw(ch.ID)
	if got := all[0].Direction.Style; got != "roaring, at the end of her rope" {
		t.Errorf("line 0 style = %q", got)
	}
	if !all[1].Direction.Empty() {
		t.Errorf("line 1 kept %+v", all[1].Direction)
	}
	if len(all[2].Direction.Marks) != 1 {
		t.Errorf("line 2 = %+v", all[2].Direction)
	}
	ch, _ = s.GetChapterByIdx(bookID, 0)
	if !ch.Passes.Delivery {
		t.Error("Passes.Delivery not set")
	}
}

func TestDeliveryFor(t *testing.T) {
	text := "“Fine. Go, then.”"
	d, _ := direction.Parse(text, "[tired] Fine. [sighs] <whisper>Go, then</whisper>.")
	p := store.Paragraph{Text: text, IsQuote: true, Direction: d}
	got := deliveryFor(p, voices.Drama3CloneModel)
	if got == nil || got.Style != "tired" || got.StyleFromLabel || len(got.Marks) != 1 || len(got.Spans) != 1 || got.Spans[0] != (lineDirectionSpanDTO{Start: 7, End: 15, Tag: "whisper"}) {
		t.Fatalf("delivery = %+v", got)
	}
	if deliveryFor(p, voices.HiggsCloneModel) != nil {
		t.Fatal("an undirected model got a delivery")
	}
	labeled := store.Paragraph{Text: text, IsQuote: true, Emotion: "weary"}
	if got := deliveryFor(labeled, voices.Drama3CloneModel); got == nil || !got.StyleFromLabel || got.Style == "" {
		t.Fatalf("label delivery = %+v", got)
	}
}

func TestSpeakerEmotionsUnderDirectedModel(t *testing.T) {
	srv, _, _, _ := newTestServer(t)
	got := srv.speakerEmotions("preset-a", voices.Drama3CloneModel, map[string]int{"angry": 3})
	if len(got) != 1 || got[0].Status != "directed" || got[0].AudioURL != "" {
		t.Fatalf("emotions = %+v", got)
	}
	if got := srv.speakerEmotions("preset-a", voices.HiggsCloneModel, map[string]int{"angry": 3}); got[0].Status != "pending" {
		t.Fatalf("Higgs status = %q", got[0].Status)
	}
}
