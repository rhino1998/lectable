package store

import (
	"testing"

	"github.com/rhino1998/lectable/backend/internal/direction"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

func TestParagraphDirectionRoundTrips(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := oneChapterBook(t, s, "", 0, `“Fine. Go, then.”`)
	ps, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := direction.Parse(ps[0].Text, "[tired] Fine. [sighs] Go, then.")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphDirections(chapterID, map[int]direction.Direction{0: d}); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetParagraph(ps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Direction.Equal(d) || !p.Direction.Current(p.Text) {
		t.Fatalf("direction = %+v, want %+v", p.Direction, d)
	}
	if err := s.SetParagraphDirections(chapterID, map[int]direction.Direction{0: {}}); err != nil {
		t.Fatal(err)
	}
	if p, _ = s.GetParagraph(ps[0].ID); !p.Direction.Empty() {
		t.Fatalf("direction not cleared: %+v", p.Direction)
	}
}

func TestResolveGenerationTextDirected(t *testing.T) {
	text := "“Fine. [Skill: Rest] Go, then.”"
	d, err := direction.Parse(text, "[tired] Fine. [Skill: Rest] [sighs] Go, then.")
	if err != nil {
		t.Fatal(err)
	}
	p := Paragraph{Text: text, IsQuote: true, Direction: d, Emotion: "angry"}
	want := "[tired] “Fine. Skill: Rest [sighs] Go, then.”"
	if got := p.ResolveGenerationText(voices.Drama3CloneModel, nil); got != want {
		t.Errorf("directed = %q, want %q", got, want)
	}
	// Other models never see it.
	if got := p.ResolveGenerationText(voices.HiggsCloneModel, nil); got != text {
		t.Errorf("Higgs = %q", got)
	}
	// Narration is never directed.
	p.IsQuote = false
	if got := p.EffectiveDirection(); !got.Empty() {
		t.Errorf("narration direction = %+v", got)
	}
	// A direction for other text falls back to the label's style.
	p = Paragraph{Text: "“Get out.”", IsQuote: true, Direction: d, Emotion: "angry"}
	if got, want := p.ResolveGenerationText(voices.Drama3CloneModel, nil), "[angry, voice raised and tight] “Get out.”"; got != want {
		t.Errorf("label fallback = %q, want %q", got, want)
	}
}
