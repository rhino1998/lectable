package semantic

import (
	"context"
	"testing"
)

// TestAlike: paragraphs about one thing read alike, against the book as a
// whole, and paragraphs about another don't.
func TestAlike(t *testing.T) {
	paragraphs := []string{
		"Doctor Zheng collected pond samples for the biology lab.",
		"Doctor Zheng knelt in the freezing stream with her sample jars.",
		"Dr. Zheng labelled the pond samples in the biology lab.",
		"Dr. Zheng waded into the freezing stream holding sample jars.",
		"The gunner loaded another railgun magazine and fired at the cruiser.",
		"The railgun fired again and the enemy cruiser broke apart.",
		"Missiles streaked past the frigate as the pilot rolled away.",
		"The admiral ordered every frigate to jump before the missiles hit.",
		"Rain hammered the tin roof of the market all afternoon.",
		"A merchant argued over the price of salted fish and bread.",
		"The old king signed the treaty with a shaking hand.",
		"Nobody at court believed the treaty would last the winter.",
	}
	paragraphs = append(paragraphs, "Too short.")
	svc, st, _, _, bookID, chapterID := newTestService(t, paragraphs...)
	if _, err := svc.EmbedChapter(context.Background(), bookID, chapterID); err != nil {
		t.Fatal(err)
	}
	stored, err := st.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatal(err)
	}
	usage := func(idxs ...int) *Usage {
		u := &Usage{}
		ids := make([]string, len(idxs))
		for i, idx := range idxs {
			ids[i] = stored[idx].ID
		}
		if err := svc.AddParagraphs(u, bookID, ids); err != nil {
			t.Fatal(err)
		}
		return u
	}
	among := &Usage{}
	if err := svc.AddBook(among, bookID); err != nil {
		t.Fatal(err)
	}
	if among.N() != 12 {
		t.Errorf("the book has %d embedded paragraphs, want 12 (one is too short)", among.N())
	}
	doctor, dr, gunner := usage(0, 1), usage(2, 3), usage(4, 5)
	if got := usage(0, 12).N(); got != 1 {
		t.Errorf("a paragraph with no vector was counted: N = %d", got)
	}
	same, other := Alike(doctor, dr, among), Alike(doctor, gunner, among)
	if same <= 0 || other >= 0 {
		t.Errorf("Alike: the two spellings %.2f (want above 0), the doctor and the gunner %.2f (want below 0)", same, other)
	}
	if got := Alike(doctor, &Usage{}, among); got != 0 {
		t.Errorf("Alike with nothing = %.2f, want 0", got)
	}
}
