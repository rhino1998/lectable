package speakerattr

import "testing"

func TestRoleLabel(t *testing.T) {
	for in, want := range map[string]string{
		"One of the thugs":                                    "Thug",
		"the older man":                                       "Man",
		"Guard who grabbed Fritz":                             "Guard",
		"Woman with pink hair net":                            "Woman",
		"another of the Browncoats":                           "Browncoat",
		"the third and last of the women sitting there":       "Woman",
		"a weedy man":                                         "Man",
		"The King":                                            "King",
		"the old Count":                                       "Count",
		"The man inquired loudly":                             "Man",
		"the woman leading him":                               "Woman",
		"The woman he had saved":                              "Woman",
		"Man in tough labourer's clothes":                     "Man",
		"one of the thinner, seemingly thievery-aspected men": "Man",
		"the guard with the lantern":                          "Guard",
		"the old king":                                        "King",
		"The burly man":                                       "Man",
		"the elderly woman":                                   "Woman",
	} {
		if got, _, ok := RoleLabel(in); !ok || got != want {
			t.Errorf("RoleLabel(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"Jeb", "Lady Joyous Greysail", "The Nightshark", "the other", "one argued", "the first", "Quin Cold", "The Duchess Blackbrine", "The Rat Cleaver", "The Duke's son", "the older of the pair", "The voice beyond the door", "One asked", "Neighbor 1", "Thug 2", "Lady of the Lake", "Bearded Man", "Lars Machmüller", "Ingrid's friend"} {
		if got, _, ok := RoleLabel(in); ok {
			t.Errorf("RoleLabel(%q) = %q, want not a role", in, got)
		}
	}
}

func TestUnusableDescription(t *testing.T) {
	for in, want := range map[string]bool{"the other": true, "One asked": true, "the older of the pair": true, "the king's man": false, "the older man": false, "Jeb": false, "The Nightshark": false} {
		if got := UnusableDescription(in); got != want {
			t.Errorf("UnusableDescription(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolveIdentities(t *testing.T) {
	resolver := NewSpeakerNameResolver([]string{"Fritz", "Count"}, nil)
	roles := map[string]bool{"Guard": true}
	got := resolveIdentities(map[int]string{1: "Fritz", 2: "the old Count", 3: "One of the guards", 4: "the older man", 5: "the other", 6: "Unknown", 7: "Quin Cold", 8: "The King"}, resolver, roles)
	want := map[int]string{1: "Fritz", 2: "Count", 3: "Guard", 4: "Man", 5: "Unknown", 6: "Unknown", 7: "Quin Cold", 8: "King"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("idx %d = %q, want %q", k, got[k], v)
		}
	}
	if !roles["Man"] || roles["King"] || roles["Count"] {
		t.Errorf("roles = %v", roles)
	}
}
