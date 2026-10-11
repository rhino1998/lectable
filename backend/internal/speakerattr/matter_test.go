package speakerattr

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func matterSections(titles ...string) []MatterSection {
	out := make([]MatterSection, len(titles))
	for i, t := range titles {
		out[i] = MatterSection{Title: t, Chars: 20000}
	}
	return out
}

func matterIdxs(b []bool) []int {
	var out []int
	for i, v := range b {
		if v {
			out = append(out, i)
		}
	}
	return out
}

func TestParseMatter(t *testing.T) {
	sections := matterSections("Title Page", "Contents", "Chapter One", "Chapter Two", "About the Author")
	got, err := parseMatter(`{"skip": [
		{"n": 0, "title": "Title Page"},
		{"n": 1, "title": "Contents [350 chars] - Chapter One / Chapter Two"},
		{"n": 3, "title": "About the Author"},
		{"n": 9, "title": "Nothing"}
	]}`, sections)
	if err != nil {
		t.Fatal(err)
	}
	// 3 is named with section 4's title: the number is off, so it is dropped.
	if idxs := matterIdxs(got); !slices.Equal(idxs, []int{0, 1}) {
		t.Fatalf("parseMatter = %v", idxs)
	}
	if got, err := parseMatter(`{"skip": []}`, sections); err != nil || len(matterIdxs(got)) != 0 {
		t.Fatalf("an empty list is an answer: %v, %v", got, err)
	}
	for _, bad := range []string{`no json`, `{"keep": [1]}`} {
		if _, err := parseMatter(bad, sections); err == nil {
			t.Errorf("parseMatter(%q) should fail (and so retry)", bad)
		}
	}
}

func TestMatterGuard(t *testing.T) {
	sections := matterSections("Citadel", "Contents", "Prologue", "Chapter One", "Character Sheet", "Chapter Two", "Epilogue", "The End of Book Five", "Post-Credits Scene", "Patreon!")
	sections[7].Chars = 20
	named := []bool{true, true, false, true, true, false, true, false, false, true}
	// Chapter One, the sheet between chapters and the epilogue stay; the
	// end page is added.
	if idxs := matterIdxs(matterGuard(sections, named)); !slices.Equal(idxs, []int{0, 1, 7, 9}) {
		t.Fatalf("matterGuard = %v", idxs)
	}
	// A long section titled "The End" is not the page.
	sections[7].Chars = 3739
	if idxs := matterIdxs(matterGuard(sections, named)); !slices.Equal(idxs, []int{0, 1, 9}) {
		t.Fatalf("matterGuard with a long end section = %v", idxs)
	}
}

func TestMatterGuardWithoutNumberedChapters(t *testing.T) {
	sections := matterSections("A Midsummer Night's Dream", "Dramatis Personæ", "ACT I", "SCENE I. Athens", "SCENE II. The Same", "The Full Project Gutenberg License")
	named := []bool{true, true, false, false, true, true}
	if idxs := matterIdxs(matterGuard(sections, named)); !slices.Equal(idxs, []int{0, 1, 4, 5}) {
		t.Fatalf("matterGuard = %v", idxs)
	}
	named = []bool{true, false, false, true, false, true}
	if idxs := matterIdxs(matterGuard(sections, named)); !slices.Equal(idxs, []int{0, 5}) {
		t.Fatalf("a section with story on both sides is story: %v", idxs)
	}
}

func TestMatterOpening(t *testing.T) {
	got := MatterOpening("Copyright", []string{"COPYRIGHT", "", "Copyright © 2023  J.R. Mathews", "All rights reserved.", strings.Repeat("x", 200), "never reached"})
	if want := "Copyright © 2023 J.R. Mathews / All rights reserved. / xxx"; !strings.HasPrefix(got, want) || len([]rune(got)) != matterOpeningChars || strings.Contains(got, "never") {
		t.Fatalf("MatterOpening = %q", got)
	}
}

func TestMatterUserPromptLongBook(t *testing.T) {
	var titles []string
	for i := range 300 {
		titles = append(titles, fmt.Sprintf("Chapter %d", i))
	}
	c := &Client{}
	prompt := c.matterUserPrompt(matterSections(titles...), matterShown(300))
	if !strings.Contains(prompt, "59. Chapter 59 [") || strings.Contains(prompt, "60. Chapter 60 [") || !strings.Contains(prompt, "(sections 60 to 239, left out here, are chapters)") || !strings.Contains(prompt, "240. Chapter 240 [") {
		t.Fatalf("long book prompt:\n%s", prompt)
	}
	if got := strings.Count(prompt, "\n"); got != 121 {
		t.Fatalf("lines = %d", got)
	}
}
