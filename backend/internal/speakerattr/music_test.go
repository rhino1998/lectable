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
