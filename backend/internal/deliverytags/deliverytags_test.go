package deliverytags

import "testing"

func TestExtractInsertionsNoTags(t *testing.T) {
	ins, err := ExtractInsertions("Hello there.", "Hello there.")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ins) != 0 {
		t.Fatalf("expected no insertions, got %v", ins)
	}
}

func TestExtractInsertionsLeadingTag(t *testing.T) {
	plain := "Damn! We're too late!"
	annotated := "<|emotion:anger|>Damn! We're too late!"
	ins, err := ExtractInsertions(plain, annotated)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []Insertion{{Offset: 0, Tag: "<|emotion:anger|>"}}
	if len(ins) != 1 || ins[0] != want[0] {
		t.Fatalf("got %v, want %v", ins, want)
	}
}

func TestExtractInsertionsMidSentence(t *testing.T) {
	plain := `"I love you," she said softly, then screamed, "Get out!"`
	annotated := `<|emotion:affection|>"I love you," she said softly, then <|emotion:fear|>screamed, "Get out!"`
	ins, err := ExtractInsertions(plain, annotated)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ins) != 2 {
		t.Fatalf("expected 2 insertions, got %v", ins)
	}
	if ins[0].Offset != 0 || ins[0].Tag != "<|emotion:affection|>" {
		t.Fatalf("bad first insertion: %v", ins[0])
	}
	wantOffset := len(`"I love you," she said softly, then `)
	if ins[1].Offset != wantOffset || ins[1].Tag != "<|emotion:fear|>" {
		t.Fatalf("bad second insertion: %v (want offset %d)", ins[1], wantOffset)
	}
}

func TestExtractInsertionsSfxNoSpace(t *testing.T) {
	plain := "Haha, so glad you could make it!"
	annotated := "<|sfx:laughter|>Haha, so glad you could make it!"
	ins, err := ExtractInsertions(plain, annotated)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ins) != 1 || ins[0].Offset != 0 || ins[0].Tag != "<|sfx:laughter|>" {
		t.Fatalf("got %v", ins)
	}
}

func TestExtractInsertionsRejectsWordChange(t *testing.T) {
	plain := "Hello there."
	annotated := "<|emotion:anger|>Hello world."
	if _, err := ExtractInsertions(plain, annotated); err == nil {
		t.Fatal("expected an error for a changed word, got nil")
	}
}

func TestExtractInsertionsRejectsDroppedWord(t *testing.T) {
	plain := "Hello there, friend."
	annotated := "<|emotion:anger|>Hello, friend."
	if _, err := ExtractInsertions(plain, annotated); err == nil {
		t.Fatal("expected an error for a dropped word, got nil")
	}
}

func TestExtractInsertionsRejectsTruncated(t *testing.T) {
	plain := "Hello there, friend."
	annotated := "<|emotion:anger|>Hello there"
	if _, err := ExtractInsertions(plain, annotated); err == nil {
		t.Fatal("expected an error for truncated text, got nil")
	}
}

func TestExtractInsertionsMultiByteRunes(t *testing.T) {
	plain := `“I can’t believe it,” she whispered — barely audible.`
	annotated := `<|style:whispering|>“I can’t believe it,” she whispered — barely audible.`
	ins, err := ExtractInsertions(plain, annotated)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ins) != 1 || ins[0].Offset != 0 {
		t.Fatalf("got %v", ins)
	}
}

func TestMergeStacksAtSameOffset(t *testing.T) {
	plain := "Haha, welcome!"
	sentence := []Insertion{{Offset: 0, Tag: "<|emotion:elation|>"}}
	inline := []Insertion{{Offset: 0, Tag: "<|sfx:laughter|>"}}
	got := Merge(plain, sentence, inline)
	want := "<|emotion:elation|><|sfx:laughter|>Haha, welcome!"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestMergeDifferentOffsets(t *testing.T) {
	plain := `"Hello," she said, then screamed, "Get out!"`
	sentence := []Insertion{{Offset: 0, Tag: "<|emotion:affection|>"}}
	inline := []Insertion{{Offset: len(`"Hello," she said, then `), Tag: "<|prosody:pause|>"}}
	got := Merge(plain, sentence, inline)
	want := `<|emotion:affection|>"Hello," she said, then <|prosody:pause|>screamed, "Get out!"`
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestExtractThenMergeRoundTrip(t *testing.T) {
	plain := `Damn! We're too late!" Yina demanded, visibly swelling.`
	sentenceAnnotated := `<|emotion:anger|>Damn! We're too late!" <|emotion:bitterness|>Yina demanded, visibly swelling.`
	ins, err := ExtractInsertions(plain, sentenceAnnotated)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := Merge(plain, ins)
	if got != sentenceAnnotated {
		t.Fatalf("round trip mismatch: got %q, want %q", got, sentenceAnnotated)
	}
}
