package speakerattr

import "testing"

func regionAt(startIdx int, transition string) musicBoundary {
	return musicBoundary{StartIdx: startIdx, Transition: transition}
}

func startIdxes(regions []musicBoundary) []int {
	out := make([]int, len(regions))
	for i, r := range regions {
		out[i] = r.StartIdx
	}
	return out
}

func TestMergeShortRegionsDropsShortContinuations(t *testing.T) {
	// Matches the real, observed worst case: near-1-paragraph-per-region
	// fragmentation in a reflective passage, none of it a "cut" - every
	// short continuation region should merge away, leaving only the
	// (also short, but "cut") region at 150, the one at 152 (whose own
	// span to the next region, 200, is comfortably long), and the two
	// unconditionally-kept ends.
	in := []musicBoundary{
		regionAt(140, "cut"),
		regionAt(141, "continuation"),
		regionAt(143, "continuation"),
		regionAt(146, "continuation"),
		regionAt(149, "continuation"), // span 1 (150-149) - merges
		regionAt(150, "cut"),          // span 1 - kept, it's a cut
		regionAt(151, "continuation"), // span 1 - merges
		regionAt(152, "continuation"), // span 48 (200-152) - kept, genuinely long
		regionAt(200, "continuation"), // last - always kept
	}
	got := mergeShortRegions(in)
	want := []int{140, 150, 152, 200}
	gotIdx := startIdxes(got)
	if len(gotIdx) != len(want) {
		t.Fatalf("mergeShortRegions() = %v, want %v", gotIdx, want)
	}
	for i := range want {
		if gotIdx[i] != want[i] {
			t.Fatalf("mergeShortRegions() = %v, want %v", gotIdx, want)
		}
	}
}

func TestMergeShortRegionsNeverDropsCut(t *testing.T) {
	// Every region here is span-1 (would merge under the length rule
	// alone), but every one is also "cut" - none should ever be dropped,
	// regardless of how short.
	in := []musicBoundary{
		regionAt(0, "cut"),
		regionAt(1, "cut"),
		regionAt(2, "cut"),
		regionAt(3, "cut"),
	}
	got := mergeShortRegions(in)
	if len(got) != len(in) {
		t.Fatalf("mergeShortRegions() dropped a cut region: got %v, want all of %v kept", startIdxes(got), startIdxes(in))
	}
}

func TestMergeShortRegionsKeepsFirstAndLastRegardlessOfSpan(t *testing.T) {
	in := []musicBoundary{
		regionAt(0, "cut"),          // first - always kept
		regionAt(1, "continuation"), // span 1 - would merge, but only the middle ever merges
		regionAt(50, "continuation"),
	}
	got := mergeShortRegions(in)
	gotIdx := startIdxes(got)
	// Region 1's own span (50-1=49) is far above minRegionParagraphs, so
	// it's kept on its own merits here - construct a case where the
	// *first* region itself would look "short" against its neighbor, to
	// confirm it's kept anyway since nothing merges into a first region.
	if len(gotIdx) != 3 {
		t.Fatalf("mergeShortRegions() = %v, want all 3 kept (none short enough to merge)", gotIdx)
	}

	inShortFirst := []musicBoundary{
		regionAt(0, "continuation"), // span 1 against the next - still always kept, it's first
		regionAt(1, "continuation"),
	}
	gotShortFirst := mergeShortRegions(inShortFirst)
	if len(gotShortFirst) != 2 || gotShortFirst[0].StartIdx != 0 {
		t.Fatalf("mergeShortRegions() = %v, want the first region kept regardless of its own span", startIdxes(gotShortFirst))
	}
}

func TestMergeShortRegionsNoOpBelowTwoRegions(t *testing.T) {
	if got := mergeShortRegions(nil); len(got) != 0 {
		t.Fatalf("mergeShortRegions(nil) = %v, want empty", got)
	}
	one := []musicBoundary{regionAt(0, "cut")}
	if got := mergeShortRegions(one); len(got) != 1 {
		t.Fatalf("mergeShortRegions(one region) = %v, want unchanged", got)
	}
}

// plainParagraphs builds n consecutive, all-non-Inline ParagraphInputs
// (idx 0..n-1) - splitLongRegions' own simplest fixture shape, where every
// paragraph is its own logical unit.
func plainParagraphs(n int) []ParagraphInput {
	out := make([]ParagraphInput, n)
	for i := range out {
		out[i] = ParagraphInput{Idx: i}
	}
	return out
}

func TestSplitLongRegionsSplitsAtMax(t *testing.T) {
	// One region spanning 25 logical paragraphs (idx 0-24) must split into
	// three: 0-9, 10-19, 20-24 - a boundary inserted at logical position 11
	// (idx 10) and again at 21 (idx 20), both "continuation".
	regions := []musicBoundary{regionAt(0, "cut")}
	got := splitLongRegions(regions, plainParagraphs(25))
	wantStarts := []int{0, 10, 20}
	gotStarts := startIdxes(got)
	if len(gotStarts) != len(wantStarts) {
		t.Fatalf("splitLongRegions() = %v, want %v", gotStarts, wantStarts)
	}
	for i, want := range wantStarts {
		if gotStarts[i] != want {
			t.Fatalf("splitLongRegions() = %v, want %v", gotStarts, wantStarts)
		}
	}
	if got[0].Transition != "cut" {
		t.Fatalf("splitLongRegions() = %+v, want the original region's own transition preserved", got[0])
	}
	if got[1].Transition != "continuation" || got[2].Transition != "continuation" {
		t.Fatalf("splitLongRegions() = %+v, want every inserted split to be \"continuation\", never \"cut\"", got)
	}
}

func TestSplitLongRegionsLeavesShortRegionsAlone(t *testing.T) {
	regions := []musicBoundary{regionAt(0, "cut")}
	got := splitLongRegions(regions, plainParagraphs(maxRegionParagraphs))
	if len(got) != 1 {
		t.Fatalf("splitLongRegions() = %v, want the region left untouched at exactly maxRegionParagraphs", startIdxes(got))
	}
}

func TestSplitLongRegionsCountsInlineContinuationsAsOneUnit(t *testing.T) {
	// 11 logical units, but 5 of them carry an extra Inline continuation
	// each (16 ParagraphInputs total) - the split must still land after the
	// 10th *logical* unit, never on an Inline paragraph's own idx.
	var paras []ParagraphInput
	idx := 0
	for unit := 0; unit < 11; unit++ {
		paras = append(paras, ParagraphInput{Idx: idx})
		idx++
		if unit%2 == 0 {
			paras = append(paras, ParagraphInput{Idx: idx, Inline: true})
			idx++
		}
	}
	regions := []musicBoundary{regionAt(0, "cut")}
	got := splitLongRegions(regions, paras)
	if len(got) != 2 {
		t.Fatalf("splitLongRegions() = %v, want exactly one split for 11 logical units", startIdxes(got))
	}
	// The 11th logical unit (0-indexed unit 10) is the first paragraph
	// after 10 non-Inline ones - find its own real idx and confirm the
	// split landed exactly there, on a non-Inline paragraph.
	logical := 0
	var wantSplit int
	for _, p := range paras {
		if p.Inline {
			continue
		}
		if logical == 10 {
			wantSplit = p.Idx
			break
		}
		logical++
	}
	if got[1].StartIdx != wantSplit {
		t.Fatalf("splitLongRegions() split at idx %d, want %d (the 11th logical unit, never an Inline idx)", got[1].StartIdx, wantSplit)
	}
}

func TestSplitLongRegionsHandlesMultipleRegionsIndependently(t *testing.T) {
	// Two regions back to back: the first spans 15 logical paragraphs
	// (idx 0-14, needs one split at idx 10), the second spans only 5
	// (idx 15-19, left alone).
	regions := []musicBoundary{regionAt(0, "cut"), regionAt(15, "continuation")}
	got := splitLongRegions(regions, plainParagraphs(20))
	want := []int{0, 10, 15}
	gotStarts := startIdxes(got)
	if len(gotStarts) != len(want) {
		t.Fatalf("splitLongRegions() = %v, want %v", gotStarts, want)
	}
	for i := range want {
		if gotStarts[i] != want[i] {
			t.Fatalf("splitLongRegions() = %v, want %v", gotStarts, want)
		}
	}
}

func TestParseMusicRegionDescriptionBuildsTaggedPrompts(t *testing.T) {
	raw, err := parseMusicRegionDescription(`{"mood": "quiet dread", "setting": "harbor at night", "genres": ["Ambient", "Cinematic", "Drone"], "instruments": ["low strings", "", "Genre: synth pad", "low strings", "a, b"], "bpm": 300, "music": "Slow, uneasy and sparse.", "ambience": "  None "}`)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Ambience != "" {
		t.Fatalf("ambience = %q, want \"none\" normalized to empty", raw.Ambience)
	}
	got := buildMusicPrompt(raw)
	want := "TrackType: Music, VocalType: Instrumental, Genre: Ambient, Genre: Cinematic, Instruments: low strings, Genre synth pad, Slow, uneasy and sparse, subtle background underscore, 140 BPM"
	if got != want {
		t.Fatalf("buildMusicPrompt() =\n%q\nwant\n%q", got, want)
	}
	if buildAmbiencePrompt("") != "" {
		t.Fatal("buildAmbiencePrompt(\"\") should stay empty")
	}
}

func TestParseMusicRegionDescriptionAcceptsLegacyPrompt(t *testing.T) {
	raw, err := parseMusicRegionDescription(`{"mood": "calm", "prompt": "soft piano"}`)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Music != "soft piano" {
		t.Fatalf("music = %q, want the legacy prompt field used", raw.Music)
	}
	if _, err := parseMusicRegionDescription(`{"mood": "calm"}`); err == nil {
		t.Fatal("want an error for a description with no music at all")
	}
}

func TestSameAmbienceIgnoresCopyDrift(t *testing.T) {
	prev := "ocean waves on a rocky shore, distant gulls"
	if !sameAmbience("Ocean waves on a rocky shore - distant gulls.", prev) {
		t.Fatal("want punctuation/case drift treated as the same ambience")
	}
	if sameAmbience("busy tavern interior, crowd murmur", prev) {
		t.Fatal("want a different setting treated as different")
	}
	if sameAmbience("", "") {
		t.Fatal("want empty never treated as a match")
	}
}

func TestSplitLongRegionsMarksSplits(t *testing.T) {
	got := splitLongRegions([]musicBoundary{regionAt(0, "cut")}, plainParagraphs(25))
	if got[0].split || !got[1].split || !got[2].split {
		t.Fatalf("splitLongRegions() = %+v, want only inserted boundaries marked split", got)
	}
}

func TestParseMusicBoundariesSeamCarriesOver(t *testing.T) {
	// Seam context 16-19, batch 20-29: a lone first region means the tone
	// already playing carries on - no boundary at the seam at all.
	shown := plainParagraphs(30)[16:]
	got, err := parseMusicBoundaries(`[{"startIdx": 16, "transition": "cut"}]`, shown, 20)
	if err != nil || len(got) != 0 {
		t.Fatalf("parseMusicBoundaries() = %+v, %v; want no boundaries", got, err)
	}
}

func TestParseMusicBoundariesSeamKeepsModelTransition(t *testing.T) {
	shown := plainParagraphs(30)[16:]
	got, err := parseMusicBoundaries(`[{"startIdx": 16, "transition": "cut"}, {"startIdx": 20, "transition": "continuation"}, {"startIdx": 25, "transition": "cut"}]`, shown, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []musicBoundary{regionAt(20, "continuation"), regionAt(25, "cut")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseMusicBoundaries() = %+v, want %+v", got, want)
	}
}

func TestParseMusicBoundariesChangeInsideSeamLandsOnBatchStart(t *testing.T) {
	// A change the model placed inside the context is what's playing when
	// the batch starts.
	shown := plainParagraphs(30)[16:]
	got, err := parseMusicBoundaries(`[{"startIdx": 16, "transition": "cut"}, {"startIdx": 18, "transition": "cut"}, {"startIdx": 24, "transition": "continuation"}]`, shown, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []musicBoundary{regionAt(20, "cut"), regionAt(24, "continuation")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseMusicBoundaries() = %+v, want %+v", got, want)
	}
}

func TestMusicSeamContextCountsNumberedLines(t *testing.T) {
	ps := plainParagraphs(10)
	ps[8].Inline = true
	got := musicSeamContext(ps)
	// Lines 5, 6, 7(+8 inline), 9 - four numbered lines.
	if len(got) != 5 || got[0].Idx != 5 {
		t.Fatalf("musicSeamContext() starts at %d with %d paragraphs, want 5 with 5", got[0].Idx, len(got))
	}
	if short := musicSeamContext(ps[:2]); len(short) != 2 {
		t.Fatalf("musicSeamContext() of 2 paragraphs = %d, want all of them", len(short))
	}
}
