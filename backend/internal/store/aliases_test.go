package store

import (
	"reflect"
	"testing"
)

func TestCharacterAliases(t *testing.T) {
	s := openTestStore(t)
	bert, _, err := s.UpsertCharacter("series:x", "Bert", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCharacterAliases(bert.ID, []string{"Albert", " al ", "", "albert", "Bert"}); err != nil {
		t.Fatalf("SetCharacterAliases: %v", err)
	}
	if err := s.AddCharacterAliases(bert.ID, "Al", "Bertie"); err != nil {
		t.Fatalf("AddCharacterAliases: %v", err)
	}
	roster, err := s.ListCharacters("series:x")
	if err != nil {
		t.Fatal(err)
	}
	if want := (AliasList{"Albert", "Bertie", "al"}); len(roster) != 1 || !reflect.DeepEqual(roster[0].Aliases, want) {
		t.Fatalf("aliases = %+v, want %v", roster, want)
	}

	// A character created without any starts with an empty list, not null.
	sid, _, err := s.UpsertCharacter("series:x", "Sid", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetCharacter(sid.ID); got == nil || got.Aliases == nil || len(got.Aliases) != 0 {
		t.Fatalf("new character aliases = %#v", got)
	}
}
