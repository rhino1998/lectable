package jobs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

func TestRetryAttemptsDropDirectionLast(t *testing.T) {
	text := "[tired] Fine. [sighs] Go, then, and don't come back here until you have learned some manners."
	got := retryAttempts(text, true)
	n := len(retryChunkSizes(text))
	if len(got) != n+2 {
		t.Fatalf("got %d attempts, want %d", len(got), n+2)
	}
	if got[n].text != "[tired] Fine. Go, then, and don't come back here until you have learned some manners." {
		t.Errorf("tags dropped = %q", got[n].text)
	}
	if got[n+1].text != "Fine. Go, then, and don't come back here until you have learned some manners." {
		t.Errorf("style dropped = %q", got[n+1].text)
	}
	if len(retryAttempts(text, false)) != n {
		t.Error("an undirected model gets only the chunk-size retries")
	}
}

func TestExtraWordsIgnoreAskedForLaughter(t *testing.T) {
	if run := extraWordRun("Oh, you didn't. Fine.", dropVocalizations("Ha ha ha ha! Oh, you didn't. Fine.")); run != 0 {
		t.Errorf("laughter counted as %d extra words", run)
	}
	// A tag read aloud is still caught.
	if run := extraWordRun("Fine. Go.", dropVocalizations("Fine. Sighs heavily. Go.")); run < 2 {
		t.Errorf("spoken tag run = %d", run)
	}
}

// TestDirectedModelRendersNoVariant: under a clone model that takes
// direction as text, an emotional line dispatches straight away - no
// variant render is queued for it - and clones from the base clip even
// when a variant from an earlier model is already on disk.
func TestDirectedModelRendersNoVariant(t *testing.T) {
	mgr := newTestManager()
	mgr.dataDir = t.TempDir()
	if err := audiopath.EnsureVoiceRefDir(mgr.dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audiopath.VoicePresetRefFile(mgr.dataDir, "preset-a"), []byte("wav"), 0o644); err != nil {
		t.Fatal(err)
	}
	clone := &task{
		kind: KindVoiceClone, tier: TierBackground, bookID: "book-1", chapterID: "ch-1", presetID: "preset-a",
		cloneModel: voices.Drama3CloneModel,
		paragraph:  store.Paragraph{ID: "p-1", Speaker: "Alice", IsQuote: true, Emotion: "angry"},
	}
	mgr.pushTask(clone)
	got, ok := mgr.sched.Pop()
	if !ok || got.(*task) != clone {
		t.Fatalf("expected the clone itself to dispatch, got %v ok=%v", got, ok)
	}
	if clone.refKey() != "preset-a:" {
		t.Errorf("refKey = %q, want the base clip's", clone.refKey())
	}

	// A variant left from another model is never used.
	if err := os.MkdirAll(filepath.Dir(audiopath.VoicePresetVariantFile(mgr.dataDir, "preset-a", "angry")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audiopath.VoicePresetVariantFile(mgr.dataDir, "preset-a", "angry"), []byte("wav"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, ok := mgr.emotionVariant(clone); ok {
		t.Error("emotionVariant picked a variant under a directed model")
	}
}
