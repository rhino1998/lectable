package voices

import "testing"

func TestPresetsUniqueAndComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Presets {
		if p.ID == "" || p.Name == "" || p.Instruct == "" || p.RefText == "" {
			t.Errorf("preset %q: id/name/instruct/refText must all be set", p.ID)
		}
		if seen[p.ID] {
			t.Errorf("duplicate preset id %q", p.ID)
		}
		seen[p.ID] = true
		if p.SpeedMultiplier <= 0 {
			t.Errorf("preset %q: speed multiplier %v must be positive", p.ID, p.SpeedMultiplier)
		}
		if p.DesignModel != "" && !hasDesignModel(p.DesignModel) {
			t.Errorf("preset %q: unknown design model %q", p.ID, p.DesignModel)
		}
	}
	if len(PresetsByID) != len(Presets) {
		t.Errorf("PresetsByID has %d entries, Presets %d", len(PresetsByID), len(Presets))
	}
	for _, id := range []string{DefaultPresetID, FastPresetID} {
		if _, ok := PresetsByID[id]; !ok {
			t.Errorf("%q is not a built-in preset", id)
		}
	}
}

func TestCatalogReferencesExist(t *testing.T) {
	for _, id := range []string{DefaultCloneModel, InstructedCloneModel, FastCloneModel, PocketCloneModel, HiggsCloneModel} {
		if !hasCloneModel(id) {
			t.Errorf("clone model %q missing from CloneModels", id)
		}
	}
	for _, id := range []string{DefaultDesignModel, DesignModelQwen3, DesignModelBreeze} {
		if !hasDesignModel(id) {
			t.Errorf("design model %q missing from DesignModels", id)
		}
	}
	seen := map[string]bool{}
	for _, m := range CloneModels {
		if seen[m.ID] || m.Label == "" {
			t.Errorf("clone model %q: duplicate or unlabeled", m.ID)
		}
		seen[m.ID] = true
	}
	seen = map[string]bool{}
	for _, m := range DesignModels {
		if seen[m.ID] || m.Label == "" {
			t.Errorf("design model %q: duplicate or unlabeled", m.ID)
		}
		seen[m.ID] = true
	}
}

func hasCloneModel(id string) bool {
	for _, m := range CloneModels {
		if m.ID == id {
			return true
		}
	}
	return false
}

func hasDesignModel(id string) bool {
	for _, m := range DesignModels {
		if m.ID == id {
			return true
		}
	}
	return false
}
