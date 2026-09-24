package pronounce

import "testing"

func TestComposeNestsPronunciationInsideEmphasis(t *testing.T) {
	sub := func(off, n int, r string) Substitution { return Substitution{Offset: off, Length: n, Replacement: r} }
	tests := []struct {
		name     string
		plain    string
		pron     []Substitution
		emphasis []Substitution
		want     string
	}{
		{"bold line", "Trait 2/3", []Substitution{sub(6, 3, "two of three")}, []Substitution{sub(0, 9, "TRAIT 2/3")}, "TRAIT TWO OF THREE"},
		{"italic word", "he read it", []Substitution{sub(3, 4, "red")}, []Substitution{sub(3, 4, `"read"`)}, `he "red" it`},
		{"bold italic", "x Dr. Y", []Substitution{sub(2, 3, "Doctor")}, []Substitution{sub(0, 7, `"X DR. Y"`)}, `"X DOCTOR Y"`},
		{"disjoint", "Level 7 now", []Substitution{sub(6, 1, "seven")}, []Substitution{sub(0, 5, "LEVEL")}, "LEVEL seven now"},
		{"straddle drops emphasis", "No. 5 left", []Substitution{sub(0, 5, "Number five")}, []Substitution{sub(4, 6, "5 LEFT")}, "Number five left"},
		{"no emphasis", "Level 7", []Substitution{sub(6, 1, "seven")}, nil, "Level seven"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Apply(tt.plain, nil, Compose(tt.plain, tt.pron, tt.emphasis)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
