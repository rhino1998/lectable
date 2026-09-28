package store

import (
	"testing"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
)

func TestLexiconRoundTrip(t *testing.T) {
	s := openTestStore(t)
	const scope = "series:Arise"

	a, prev, err := s.UpsertLexiconEntry(scope, pronounce.LexiconEntry{Term: "Nyx", Replacement: "Nicks"})
	if err != nil || prev != nil {
		t.Fatalf("insert: %v, prev %v", err, prev)
	}
	if _, _, err := s.UpsertLexiconEntry(scope, pronounce.LexiconEntry{Term: "Aeris", Replacement: "Air-iss"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.UpsertLexiconEntry("book:other", pronounce.LexiconEntry{Term: "Nyx", Replacement: "Nix"}); err != nil {
		t.Fatal(err)
	}

	// Same term in another case replaces, keeping the id.
	b, prev, err := s.UpsertLexiconEntry(scope, pronounce.LexiconEntry{Term: "NYX", Replacement: "Nyeks", CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || prev.Replacement != "Nicks" || b.ID != a.ID {
		t.Fatalf("replace: prev %+v, id %s want %s", prev, b.ID, a.ID)
	}

	got, err := s.ListLexicon(scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Term != "Aeris" || got[1].Term != "NYX" || got[1].Replacement != "Nyeks" || !got[1].CaseSensitive {
		t.Fatalf("list = %+v", got)
	}

	del, err := s.DeleteLexiconEntry(a.ID)
	if err != nil || del == nil || del.Term != "NYX" {
		t.Fatalf("delete = %+v, %v", del, err)
	}
	if del, err := s.DeleteLexiconEntry(a.ID); err != nil || del != nil {
		t.Fatalf("second delete = %+v, %v; want nil, nil", del, err)
	}
	if got, _ := s.ListLexicon(scope); len(got) != 1 {
		t.Fatalf("after delete: %+v", got)
	}
}

func TestResolveGenerationTextAppliesLexicon(t *testing.T) {
	p := Paragraph{
		Text:          "Dr. Nyx waved.",
		Pronunciation: []pronounce.Substitution{{Offset: 0, Length: 3, Replacement: "Doctor"}, {Offset: 4, Length: 3, Replacement: "Nix"}},
	}
	lex := []pronounce.LexiconEntry{{Term: "nyx", Replacement: "Nicks"}}
	if got, want := p.ResolveGenerationText("audiocpp-pocket-100m", lex), "Doctor Nicks waved."; got != want {
		t.Fatalf("= %q, want %q (lexicon overrides the overlapping LLM fix)", got, want)
	}
	if got, want := p.ResolveGenerationText("audiocpp-pocket-100m", nil), "Doctor Nix waved."; got != want {
		t.Fatalf("no lexicon = %q, want %q", got, want)
	}
}
