package store

import "testing"

func TestAudioRefs(t *testing.T) {
	s := openTestStore(t)
	bookID, chapterID := oneChapterBook(t, s, "", 0, "One.", "Two.")
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphGenerating(paragraphs[0].ID, "voice-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphReady(paragraphs[1].ID, "voice-b", 1.5); err != nil {
		t.Fatal(err)
	}
	regions, err := s.AppendMusicRegions(chapterID, []MusicRegionInput{
		{StartIdx: 0, Prompt: "strings", Ambience: "ocean"},
		{StartIdx: 1, Prompt: "drums"},
	}, 1)
	if err != nil {
		t.Fatal(err)
	}

	refs, err := s.AudioRefs()
	if err != nil {
		t.Fatal(err)
	}
	if !refs.Books[bookID] || refs.ChapterBooks[chapterID] != bookID {
		t.Errorf("book/chapter missing: %+v", refs)
	}
	// A generating row counts: its file may be written any moment.
	if v := refs.ChapterVoices[chapterID]; len(v) != 2 || !v["voice-a"] || !v["voice-b"] {
		t.Errorf("ChapterVoices = %v, want voice-a and voice-b", v)
	}
	if r := refs.ChapterRegions[chapterID]; len(r) != len(regions) || !r[regions[0].ID] || !r[regions[1].ID] {
		t.Errorf("ChapterRegions = %v, want both regions", r)
	}
	if a := refs.BookAmbience[bookID]; len(a) != 1 || !a["ocean"] {
		t.Errorf("BookAmbience = %v, want just ocean", a)
	}
}
