package httpapi

import (
	"testing"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// TestResumeMusicScoringFirstAttempt covers a chapter with no persisted
// regions at all yet - the ordinary first-ever run, which starts at
// paragraph 0 and has nothing to wipe.
func TestResumeMusicScoringFirstAttempt(t *testing.T) {
	resumeFrom, wipe := resumeMusicScoring(nil, false)
	if resumeFrom != 0 || wipe {
		t.Fatalf("resumeMusicScoring(nil, false) = (%d, %v), want (0, false)", resumeFrom, wipe)
	}
}

// TestResumeMusicScoringResumesPartialRun is the case the resume fix exists
// for: some regions already persisted, Passes.Music still false (a paused,
// not finished, run) - must resume just past the last region's own EndIdx,
// and must not wipe what's already there.
func TestResumeMusicScoringResumesPartialRun(t *testing.T) {
	existing := []store.MusicRegion{
		{Idx: 0, StartIdx: 0, EndIdx: 9},
		{Idx: 1, StartIdx: 10, EndIdx: 24},
	}
	resumeFrom, wipe := resumeMusicScoring(existing, false)
	if resumeFrom != 25 || wipe {
		t.Fatalf("resumeMusicScoring(partial, false) = (%d, %v), want (25, false)", resumeFrom, wipe)
	}
}

// TestResumeMusicScoringRescoresFullyScoredChapter covers the deliberate
// "re-score a chapter that's already fully scored" button
// (handleScoreChapterMusic's own doc comment) - existing regions are
// present, but Passes.Music is true, so this must wipe and restart from
// scratch rather than resume from what's already a stale/prior complete
// scoring.
func TestResumeMusicScoringRescoresFullyScoredChapter(t *testing.T) {
	existing := []store.MusicRegion{
		{Idx: 0, StartIdx: 0, EndIdx: 49},
	}
	resumeFrom, wipe := resumeMusicScoring(existing, true)
	if resumeFrom != 0 || !wipe {
		t.Fatalf("resumeMusicScoring(existing, true) = (%d, %v), want (0, true)", resumeFrom, wipe)
	}
}
