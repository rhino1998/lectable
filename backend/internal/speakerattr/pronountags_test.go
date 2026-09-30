package speakerattr

import (
	"reflect"
	"testing"
)

func TestRefinePronounTags(t *testing.T) {
	// Arise: Alpha ch. 10: the man holding Marie says "Steve," - the model
	// gave it (and the line continuing it) to Marie.
	paras := []ParagraphInput{
		{Idx: 0, Text: "Mikhail grunted, grinning down at her as she tried to kick him."},
		{Idx: 1, Text: "“Oi, dickhead!”", IsQuote: true},
		{Idx: 2, Text: "I snarled, stepping in.", Inline: true},
		{Idx: 3, Text: "“Steve,”", IsQuote: true},
		{Idx: 4, Text: "he said, dropping her to the floor.", Inline: true},
		{Idx: 5, Text: "“Out for a stroll?”", IsQuote: true, Inline: true},
		{Idx: 6, Text: "“Leave her,”", IsQuote: true},
		{Idx: 7, Text: "she said.", Inline: true},
		{Idx: 8, Text: "“Who?”", IsQuote: true},
		{Idx: 9, Text: "she asked.", Inline: true},
	}
	resolver := NewSpeakerNameResolver([]string{"Steve", "Marie", "Mikhail Fedorov", "Dave"}, map[string][]string{"Mikhail Fedorov": {"Mikhail"}}).WithFirstPerson("Steve")
	pronouns := map[string]string{"Steve": "he/him", "Marie": "she/her", "Mikhail Fedorov": "he/him", "Dave": "he/him"}
	speakers := map[int]string{1: "Steve", 3: "Marie", 5: "Marie", 6: "Marie", 8: "Dave"}
	got, _ := RefinePronounTags(paras, speakers, resolver, pronouns)
	want := map[int]string{
		1: "Steve",           // the narrator's own line: never touched
		3: "Mikhail Fedorov", // "he said" can't be Marie; Mikhail is the last man named, Steve is "I"
		5: "Mikhail Fedorov", // continues ¶3 in the same paragraph
		6: "Marie",           // "she said" agrees
		8: "Marie",           // "she asked" can't be Dave; the last woman speaking is Marie
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RefinePronounTags = %v, want %v", got, want)
	}
}

func TestRefinePronounTagsFallsBackToUnknown(t *testing.T) {
	paras := []ParagraphInput{
		{Idx: 0, Text: "The room was quiet."},
		{Idx: 1, Text: "“Hello,”", IsQuote: true},
		{Idx: 2, Text: "she said.", Inline: true},
	}
	resolver := NewSpeakerNameResolver([]string{"Dave", "Ann"}, nil)
	// Ann's pronouns aren't known, so she can't be picked as the "she".
	got, _ := RefinePronounTags(paras, map[int]string{1: "Dave"}, resolver, map[string]string{"Dave": "he/him"})
	if got[1] != "Unknown" {
		t.Fatalf("speaker = %q, want Unknown", got[1])
	}
}
