package voicecheck

import (
	"reflect"
	"testing"
)

func TestCast(t *testing.T) {
	cases := []struct {
		instruct, pronouns string
		years              int
		want               Traits
	}{
		{"Speak as a middle-aged dwarf man. Use a low, deep pitch with a thick, gravelly baritone timbre.", "", 0,
			Traits{Gender: GenderMan, Age: AgeMiddleAged, Pitch: PitchLow}},
		{"Speak as a young girl. Use a high, light pitch with a thin, delicate timbre.", "", 0,
			Traits{Gender: GenderWoman, Age: AgeChild, Pitch: PitchHigh}},
		{"Speak as a teenage boy. His pitch is low but noticeably cracking.", "", 0,
			Traits{Gender: GenderMan, Age: AgeTeenager, Pitch: PitchLow}},
		{"Speak as a young man. Use a medium-high pitch with a bright, energetic timbre.", "", 0,
			Traits{Gender: GenderMan, Age: AgeYoungAdult, Pitch: PitchMedium}},
		{"Speak as an elderly elf. Use a low, resonant pitch.", "she/her", 0,
			Traits{Gender: GenderWoman, Age: AgeElderly, Pitch: PitchLow}},
		// A stage's age stands in for the opening's.
		{"Speak as a teenage boy. Use a higher, lighter pitch.", "he/him", 45,
			Traits{Gender: GenderMan, Age: AgeMiddleAged, Pitch: PitchHigh}},
		{"Speak as a mature woman. Her pitch is low and grounded.", "", 0,
			Traits{Gender: GenderWoman, Age: AgeMiddleAged, Pitch: PitchLow}},
		{"Speak warmly and slowly.", "", 0, Traits{}},
	}
	for _, c := range cases {
		if got := Cast(c.instruct, c.pronouns, c.years, ""); got != c.want {
			t.Errorf("Cast(%q, %q, %d) = %+v, want %+v", c.instruct, c.pronouns, c.years, got, c.want)
		}
	}
}

func TestHeard(t *testing.T) {
	got := Heard(map[string]string{
		"gender": "woman",
		"age":    "A young adult.",
		"pitch":  "The speaker's voice is low and deep.",
		"accent": "English.",
	})
	want := Traits{Gender: GenderWoman, Age: AgeYoungAdult, Pitch: PitchLow, Accent: "English"}
	if got != want {
		t.Errorf("Heard = %+v, want %+v", got, want)
	}
	if got := Heard(map[string]string{"gender": "unclear", "age": "unknown", "pitch": "medium"}); got != (Traits{Pitch: PitchMedium}) {
		t.Errorf("Heard of non-answers = %+v", got)
	}
}

func TestMismatches(t *testing.T) {
	man := Traits{Gender: GenderMan, Age: AgeMiddleAged, Pitch: PitchLow}
	cases := []struct {
		name        string
		cast, heard Traits
		want        []Trait
	}{
		{"same", man, man, []Trait{}},
		{"adult heard as the other sex", man, Traits{Gender: GenderWoman, Age: AgeMiddleAged, Pitch: PitchHigh}, []Trait{TraitGender, TraitPitch}},
		{"a boy heard as a woman is not held against him", Traits{Gender: GenderMan, Age: AgeTeenager}, Traits{Gender: GenderWoman, Age: AgeTeenager}, []Trait{}},
		{"one band off", Traits{Age: AgeElderly}, Traits{Age: AgeMiddleAged}, []Trait{}},
		{"two bands off", Traits{Age: AgeElderly}, Traits{Age: AgeYoungAdult}, []Trait{TraitAge}},
		{"child heard as an adult", Traits{Age: AgeChild}, Traits{Age: AgeMiddleAged}, []Trait{TraitAge}},
		{"medium is no disagreement", Traits{Pitch: PitchLow}, Traits{Pitch: PitchMedium}, []Trait{}},
		{"unknown is no disagreement", man, Traits{}, []Trait{}},
	}
	for _, c := range cases {
		if got := Mismatches(c.cast, c.heard); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: Mismatches = %v, want %v", c.name, got, c.want)
		}
	}
}
