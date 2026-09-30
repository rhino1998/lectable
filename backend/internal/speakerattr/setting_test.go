package speakerattr

import (
	"strings"
	"testing"
)

func TestParseSetting(t *testing.T) {
	s, err := parseSetting(`Sure: {"era": "late Victorian", "technology": "gas lamps, carriages", "place": "London", "accent": "unknown", "genre": "gothic mystery"}`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Era != "late Victorian" || s.Accent != "" || s.Genre != "gothic mystery" {
		t.Fatalf("parseSetting = %+v", s)
	}
	if _, err := parseSetting(`{"era": "n/a", "accent": "unknown"}`); err == nil {
		t.Fatal("an empty setting should fail (and so retry)")
	}
}

func TestSettingPromptLines(t *testing.T) {
	s := &BookSetting{Era: "present day", Technology: "modern", Accent: "Scottish"}
	world := s.promptLines()
	if !strings.Contains(world, "- era: present day") || !strings.Contains(world, "- technology: modern") || strings.Contains(world, "Scottish") {
		t.Fatalf("promptLines =\n%s", world)
	}
	if c := s.castingLines(); !strings.Contains(c, "accent: Scottish") || strings.Contains(c, "modern") {
		t.Fatalf("castingLines =\n%s", c)
	}
	var none *BookSetting
	if none.promptLines() != "" || none.castingLines() != "" {
		t.Fatal("nil setting should render nothing")
	}
}

func TestSettingSampleChapters(t *testing.T) {
	got := SettingSampleChapters(64)
	if len(got) != 64 || got[0] != 0 || got[1] != 4 || got[15] != 60 || got[16] != 1 {
		t.Fatalf("SettingSampleChapters(40) = %v", got)
	}
	if got := SettingSampleChapters(3); len(got) != 3 {
		t.Fatalf("SettingSampleChapters(3) = %v", got)
	}
}

func TestTechCounts(t *testing.T) {
	c := TechCounts{}
	c.Add("She drew her swords by lantern light; the carriage waited. No electricity, no cars.")
	c.Add("A Sword and a cartwheel.")
	if c["sword"] != 2 || c["lantern"] != 1 || c["carriage"] != 1 || c["electric"] != 1 || c["car"] != 1 || c["cart"] != 0 {
		t.Fatalf("counts = %v", c)
	}
	got := c.String()
	for _, want := range []string{"- modern: car 1, electric 1\n", "- industrial: none\n", "- pre-industrial: sword 2, "} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() missing %q:\n%s", want, got)
		}
	}
}

func TestIdiomCounts(t *testing.T) {
	c := IdiomCounts{}
	c.Add("“Bloody hell, mate, get your arse over here.”")
	c.Add("“Oi! Y’all hear that, dude?”")
	c.Add("“Classmate, she said, a bloodying mess.”")
	if c["bloody"] != 1 || c["mate"] != 1 || c["arse"] != 1 || c["oi"] != 1 || c["y'all"] != 1 || c["dude"] != 1 {
		t.Fatalf("counts = %v", c)
	}
	got := c.String()
	for _, want := range []string{"- British, 4 in all: ", "- American, 2 in all: "} {
		if !strings.Contains(got, want) {
			t.Fatalf("String() missing %q:\n%s", want, got)
		}
	}
}
