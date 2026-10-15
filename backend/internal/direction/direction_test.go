package direction

import (
	"errors"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
)

func TestParseStyleMarksSpans(t *testing.T) {
	text := "“Oh, you didn't. Fine. Promise me you won't tell her.”"
	d, err := Parse(text, "[dry, amused] Oh, you didn't. [sighs] Fine. Promise me <whisper>you won't tell her</whisper>.")
	if err != nil {
		t.Fatal(err)
	}
	if d.Style != "dry, amused" {
		t.Errorf("style = %q", d.Style)
	}
	if len(d.Marks) != 1 || d.Marks[0].Tag != "sighs" || string([]rune(text)[d.Marks[0].At:d.Marks[0].At+4]) != "Fine" {
		t.Errorf("marks = %+v", d.Marks)
	}
	if len(d.Spans) != 1 || string([]rune(text)[d.Spans[0].Start:d.Spans[0].End]) != "you won't tell her" {
		t.Errorf("spans = %+v", d.Spans)
	}
	if !d.Current(text) || d.Current(text+" ") {
		t.Error("Current should hold for text only")
	}
	want := "[dry, amused] “Oh, you didn't. [sighs] Fine. Promise me <whisper>you won't tell her</whisper>.”"
	if got := d.Annotate(text); got != want {
		t.Errorf("Annotate =\n%s\nwant\n%s", got, want)
	}
	again, err := Parse(text, d.Annotate(text))
	if err != nil || !again.Equal(d) {
		t.Errorf("round trip = %+v, %v", again, err)
	}
}

func TestParseLeadingPointTagIsNotStyle(t *testing.T) {
	d, err := Parse("Ha! No.", "[laughs] Ha! No.")
	if err != nil {
		t.Fatal(err)
	}
	if d.Style != "" || len(d.Marks) != 1 || d.Marks[0].At != 0 {
		t.Errorf("got %+v", d)
	}
}

func TestParseRejectsChangedWords(t *testing.T) {
	if _, err := Parse("Get out.", "[angry] Get out now."); !errors.Is(err, ErrTextChanged) {
		t.Errorf("err = %v", err)
	}
	// An unknown mid-line bracket is dropped, its words not counted.
	if d, err := Parse("Get out.", "[angry] Get [softly] out."); err != nil || d.Style != "angry" || len(d.Marks) != 0 {
		t.Errorf("got %+v, %v", d, err)
	}
}

func TestParseDropsUnknownTagsAndLongStyle(t *testing.T) {
	d, err := Parse("Get out.", "[one two three four five six seven eight nine ten eleven twelve thirteen] Get <shout>out</shout>.")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Empty() {
		t.Errorf("got %+v", d)
	}
}

func TestParseKeepsBookBrackets(t *testing.T) {
	text := "“I got it! [Skill: Fireball] at last.”"
	d, err := Parse(text, "[thrilled] I got it! [Skill: Fireball] at last.")
	if err != nil {
		t.Fatal(err)
	}
	if d.Style != "thrilled" || len(d.Marks) != 0 {
		t.Errorf("got %+v", d)
	}
}

func TestParseSpanErrors(t *testing.T) {
	for _, a := range []string{
		"<whisper>Go now.",
		"Go </whisper>now.",
		"<whisper>Go <slow>now</slow></whisper>.",
	} {
		if _, err := Parse("Go now.", a); err == nil {
			t.Errorf("%q: want an error", a)
		}
	}
}

func TestInsertions(t *testing.T) {
	text := "Fine. Promise me you won't tell Dr. Ng."
	d, err := Parse(text, "[tired] Fine. [sighs] Promise me <whisper>you won't tell Dr. Ng</whisper>. [laughs]")
	if err != nil {
		t.Fatal(err)
	}
	// "Dr." respelled; the close tag after "Ng" stays outside it.
	subs := []pronounce.Substitution{{Offset: 32, Length: 3, Replacement: "Doctor"}}
	got := pronounce.Apply(text, d.Insertions(text, subs), subs)
	want := "[tired] Fine. [sighs] Promise me <whisper>you won't tell Doctor Ng</whisper>. [laughs]"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if s := Strip(got); s != "Fine. Promise me you won't tell Doctor Ng." {
		t.Errorf("Strip = %q", s)
	}
	if s := StripTags(got); s != "[tired] Fine. Promise me you won't tell Doctor Ng." {
		t.Errorf("StripTags = %q", s)
	}
}

func TestInsertionsSnapOutOfSubstitution(t *testing.T) {
	text := "say HP10 now"
	d := Direction{Spans: []Span{{Start: 6, End: 12, Tag: "slow"}}, For: TextHash(text)}
	subs := []pronounce.Substitution{{Offset: 4, Length: 4, Replacement: "H P ten"}}
	got := pronounce.Apply(text, d.Insertions(text, subs), subs)
	if got != "say <slow>H P ten now</slow>" {
		t.Errorf("got %q", got)
	}
}

func TestValueScan(t *testing.T) {
	var d Direction
	if v, _ := d.Value(); v != "{}" {
		t.Errorf("empty Value = %v", v)
	}
	in := Direction{Style: "cold", Marks: []Mark{{At: 3, Tag: "pause"}}, For: "x"}
	v, err := in.Value()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Scan(v); err != nil || !d.Equal(in) || d.For != "x" {
		t.Errorf("scan = %+v, %v", d, err)
	}
}

func TestLiteralBracketSubs(t *testing.T) {
	text := "[Skill: Fireball] <ok>"
	if got := pronounce.Apply(text, nil, LiteralBracketSubs(text)); got != "Skill: Fireball ok" {
		t.Errorf("got %q", got)
	}
}

func TestCarryAcross(t *testing.T) {
	l, r := CarryAcross("[tired] Fine. Promise me <whisper>you won't", "tell her</whisper>. Go.")
	if l != "[tired] Fine. Promise me <whisper>you won't</whisper>" || r != "[tired] <whisper>tell her</whisper>. Go." {
		t.Errorf("got %q | %q", l, r)
	}
	l, r = CarryAcross("[sighs] Fine.", "Go.")
	if l != "[sighs] Fine." || r != "Go." {
		t.Errorf("point tag carried: %q | %q", l, r)
	}
}
