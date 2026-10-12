package namekey

import "testing"

func TestEqual(t *testing.T) {
	same := [][2]string{
		{"Ve'delina", "Ve’delina"},
		{"ve‘delina", "VE'DELINA"},
		{"Kal’tsit", "Kalʼtsit"},
		{"O`Brien", "O´Brien"},
		{"Woman", "woman"},
	}
	for _, p := range same {
		if !Equal(p[0], p[1]) {
			t.Errorf("Equal(%q, %q) = false", p[0], p[1])
		}
	}
	different := [][2]string{
		{"Vedelina", "Ve'delina"},
		{"O'Brien", "O Brien"},
		{"Ve'delina", "Ve\"delina"},
	}
	for _, p := range different {
		if Equal(p[0], p[1]) {
			t.Errorf("Equal(%q, %q) = true", p[0], p[1])
		}
	}
}
