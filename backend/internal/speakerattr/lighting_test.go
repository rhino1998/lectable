package speakerattr

import (
	"strings"
	"testing"
)

func TestParseLighting(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		prev        *Lighting
		flameInText bool
		alarmInText bool
		want        Lighting
		wantErr     bool
	}{
		{
			name: "flame orange kept, flicker loses its accent",
			in:   `{"light": "candlelight", "color": "#FF8A2A", "accent": "#ff5010", "brightness": 30, "effect": "flicker"}`,
			want: Lighting{Description: "candlelight", Color: "#ff8a2a", Brightness: 30, Effect: "flicker"},
		},
		{
			name: "pale yellow flicker pulled orange",
			in:   `{"light": "flickering torches", "color": "#ffe9a0", "brightness": 30, "effect": "flicker"}`,
			want: Lighting{Description: "flickering torches", Color: "#ff9538", Brightness: 30, Effect: "flicker"},
		},
		{
			name: "steady lantern light pulled orange too",
			in:   `{"light": "a lantern on the table", "color": "#fff2d0", "brightness": 25, "effect": "steady"}`,
			want: Lighting{Description: "a lantern on the table", Color: "#ff9538", Brightness: 25, Effect: "steady"},
		},
		{
			name: "flicker without a flame is steady; grey becomes a cool white",
			in:   `{"light": "harsh fluorescent glow in a tunnel", "color": "#d8e0dc", "accent": "#ffffff", "brightness": 40, "effect": "flicker"}`,
			want: Lighting{Description: "harsh fluorescent glow in a tunnel", Color: coolWhite, Accent: coolWhite, Brightness: 38, Effect: "steady"},
		},
		{
			name:        "flicker kept when the text names a flame the description forgot",
			in:          `{"light": "dim flickering stone corridor", "color": "#4a4a4a", "brightness": 30, "effect": "flicker"}`,
			flameInText: true,
			want:        Lighting{Description: "dim flickering stone corridor", Color: "#ff8e38", Brightness: 30, Effect: "flicker"},
		},
		{
			name: "dark grey: warm white, darkness folded into brightness",
			in:   `{"light": "shadowed stairway", "color": "#4a4a4a", "brightness": 30, "effect": "steady"}`,
			want: Lighting{Description: "shadowed stairway", Color: warmWhite, Brightness: 19, Effect: "steady"},
		},
		{
			name: "dark blue lifted to full value, brightness scaled",
			in:   `{"light": "deep water", "color": "#0a3a6a", "brightness": 40, "effect": "steady"}`,
			want: Lighting{Description: "deep water", Color: "#188cff", Brightness: 28, Effect: "steady"},
		},
		{
			name: "short hex, clamped brightness, unknown effect",
			in:   `<think>x</think>{"light": "noon", "color": "fe8", "brightness": 140, "effect": "strobe"}`,
			want: Lighting{Description: "noon", Color: "#ffee88", Brightness: 95, Effect: "steady"},
		},
		{
			name: "lightning gets a default flash",
			in:   `{"light": "storm", "color": "#223344", "brightness": 10, "effect": "lightning"}`,
			want: Lighting{Description: "storm", Color: "#80bfff", Accent: "#dce6ff", Brightness: 6, Effect: "lightning"},
		},
		{
			name: "shimmer without accent holds steady",
			in:   `{"light": "water", "color": "#2266aa", "brightness": 0, "effect": "shimmer"}`,
			want: Lighting{Description: "water", Color: "#3399ff", Brightness: 1, Effect: "steady"},
		},
		{
			name: "flash for a siren keeps both colors",
			in:   `{"light": "police lights flashing through the blinds", "color": "#ff2020", "accent": "#2040ff", "brightness": 45, "effect": "flash"}`,
			want: Lighting{Description: "police lights flashing through the blinds", Color: "#ff2020", Accent: "#2040ff", Brightness: 45, Effect: "flash"},
		},
		{
			name:        "flash kept when the text names an alarm; a same-color accent is dropped",
			in:          `{"light": "red corridor lights", "color": "#ff2020", "accent": "#ff2020", "brightness": 40, "effect": "flash"}`,
			alarmInText: true,
			want:        Lighting{Description: "red corridor lights", Color: "#ff2020", Brightness: 40, Effect: "flash"},
		},
		{
			name: "flash without a siren or alarm is steady",
			in:   `{"light": "tense red glow in the vault", "color": "#ff2020", "brightness": 40, "effect": "flash"}`,
			want: Lighting{Description: "tense red glow in the vault", Color: "#ff2020", Brightness: 40, Effect: "steady"},
		},
		{
			name: "brightness as a string",
			in:   `{"light": "office tubes", "color": "#ffe2c4", "brightness": "45%", "effect": "steady"}`,
			want: Lighting{Description: "office tubes", Color: "#ffe2c4", Brightness: 45, Effect: "steady"},
		},
		{name: "brightness in words", in: `{"light": "office tubes", "color": "#ffe2c4", "brightness": "low", "effect": "steady"}`, wantErr: true},
		{
			name: "same keeps the previous cue",
			in:   `{"light": "Same."}`,
			prev: &Lighting{Description: "candlelit cellar", Color: "#ff8a1f", Brightness: 25, Effect: "flicker"},
			want: Lighting{Description: "candlelit cellar", Color: "#ff8a1f", Brightness: 25, Effect: "flicker"},
		},
		{
			name: "same wins over a color written after it",
			in:   `{"light": "same", "color": "#ffe2c4", "brightness": 42, "effect": "steady"}`,
			prev: &Lighting{Description: "overcast daylight", Color: "#dfe8ff", Brightness: 42, Effect: "steady"},
			want: Lighting{Description: "overcast daylight", Color: "#dfe8ff", Brightness: 42, Effect: "steady"},
		},
		{name: "same with nothing to keep", in: `{"light": "same"}`, wantErr: true},
		{name: "bad color", in: `{"color": "orange", "brightness": 20, "effect": "steady"}`, wantErr: true},
		{name: "no json", in: `sorry`, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseLighting(tc.in, tc.prev, tc.flameInText, tc.alarmInText)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestChapterOpeningAndNeedsLighting(t *testing.T) {
	ps := []ParagraphInput{
		{Idx: 0, Text: "Chapter Nine"},
		{Idx: 1, Text: "The sun was barely up when we reached the bar."},
		{Idx: 2, Text: strings.Repeat("word ", 200)},
	}
	op := ChapterOpening(ps)
	if !strings.HasPrefix(op, "The sun was barely up") || len(op) > chapterOpeningChars+3 {
		t.Fatalf("ChapterOpening = %q", op)
	}
	if NeedsLighting(ps[:1]) {
		t.Fatalf("a bare heading shouldn't need lighting")
	}
	if !NeedsLighting(ps) {
		t.Fatalf("a real passage should need lighting")
	}
}
