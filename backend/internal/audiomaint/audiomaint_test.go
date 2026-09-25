package audiomaint

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/oggopus"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

func sineWAV(t *testing.T, rate, channels int, seconds float64) []byte {
	t.Helper()
	frames := int(seconds * float64(rate))
	s := make([]float32, frames*channels)
	for i := range frames {
		for c := range channels {
			s[i*channels+c] = float32(0.4 * math.Sin(2*math.Pi*330*float64(i)/float64(rate)))
		}
	}
	data, err := wav.Encode(s, rate, channels)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// age sets every file and directory under root to mtime.
func age(t *testing.T, root string, mtime time.Time) {
	t.Helper()
	var paths []string
	_ = filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err == nil {
			paths = append(paths, p)
		}
		return nil
	})
	// Deepest first, so touching a file doesn't refresh its directory after.
	for i := len(paths) - 1; i >= 0; i-- {
		if err := os.Chtimes(paths[i], mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateLegacyWAV(t *testing.T) {
	data := t.TempDir()
	voice := audiopath.VoiceDir(data, "b", "c", "0123456789abcdef")
	para := filepath.Join(voice, "00000.wav")
	write(t, para, sineWAV(t, 24000, 1, 1.5))
	// Already regenerated as Opus: the stale WAV just goes.
	write(t, filepath.Join(voice, "00001.opus"), []byte("fresh render"))
	write(t, filepath.Join(voice, "00001.wav"), sineWAV(t, 24000, 1, 0.5))
	sfx := audiopath.SFXFile(data, "b", "c", 3)
	write(t, audiopath.LegacyWAV(sfx), sineWAV(t, 44100, 2, 1))

	music := audiopath.MusicDir(data, "b", "c")
	stem := sineWAV(t, 44100, 2, 25)
	write(t, filepath.Join(music, "r1.wav"), stem)
	write(t, filepath.Join(music, "r1.music.wav"), stem)
	write(t, filepath.Join(music, "r1.ambience.wav"), stem)
	write(t, filepath.Join(music, "r2.wav"), sineWAV(t, 44100, 2, 4)) // from before stems existed
	loop := audiopath.AmbienceLoopFile(data, "b", "ocean")
	write(t, loop, stem)

	mtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	age(t, filepath.Join(data, "audio"), mtime)

	stats := MigrateLegacyWAV(context.Background(), data, 2)
	if stats.Converted != 4 || stats.Failed != 0 {
		t.Fatalf("converted %d, failed %d; want 4 and 0", stats.Converted, stats.Failed)
	}

	for _, clip := range []string{
		audiopath.ParagraphFile(data, "b", "c", "0123456789abcdef", 0),
		sfx,
		audiopath.MusicRegionFile(data, "b", "c", "r1"),
		audiopath.MusicRegionFile(data, "b", "c", "r2"),
	} {
		if exists(audiopath.LegacyWAV(clip)) {
			t.Errorf("%s: legacy WAV left behind", clip)
		}
		info, err := os.Stat(clip)
		if err != nil {
			t.Errorf("%s: not converted: %v", clip, err)
			continue
		}
		if !info.ModTime().Equal(mtime) {
			t.Errorf("%s: mtime %v, want the WAV's %v (it versions audio URLs)", clip, info.ModTime(), mtime)
		}
		encoded, _ := os.ReadFile(clip)
		if _, err := oggopus.Duration(encoded); err != nil {
			t.Errorf("%s: not Ogg Opus: %v", clip, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(voice, "00001.opus")); string(got) != "fresh render" {
		t.Error("an existing Opus clip was overwritten")
	}
	if exists(filepath.Join(voice, "00001.wav")) {
		t.Error("stale WAV beside an existing Opus clip wasn't removed")
	}

	for _, id := range []string{"r1", "r2"} {
		seed, err := os.ReadFile(audiopath.MusicRegionSeedFile(data, "b", "c", id))
		if err != nil {
			t.Fatalf("%s: no seed: %v", id, err)
		}
		if d, err := wav.Duration(seed); err != nil || d > 10*time.Second+time.Millisecond {
			t.Errorf("%s: seed is %v (%v), want at most the 10s tail", id, d, err)
		}
	}
	for _, stem := range []string{"r1.music.wav", "r1.ambience.wav"} {
		if exists(filepath.Join(music, stem)) {
			t.Errorf("legacy stem %s left behind", stem)
		}
	}
	if !exists(loop) {
		t.Error("ambience loop (a model input) must stay WAV")
	}

	// A second run finds nothing left to do.
	if again := MigrateLegacyWAV(context.Background(), data, 2); again.Converted+again.Failed != 0 {
		t.Errorf("rerun converted %d, failed %d; want nothing", again.Converted, again.Failed)
	}
}

// TestConvertClipNeverOverwritesOpus: converting reports both sizes, and
// a WAV that reappears beside an existing Opus clip (a stale file, or a
// conversion racing a fresh render) is dropped, never converted over it.
func TestConvertClipNeverOverwritesOpus(t *testing.T) {
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "00000.wav")
	opusPath := filepath.Join(dir, "00000.opus")
	data := sineWAV(t, 24000, 1, 0.3)
	write(t, wavPath, data)
	before, after, err := convertClip(wavPath)
	if err != nil || after == 0 || before != int64(len(data)) {
		t.Fatalf("convert: %d -> %d, %v", before, after, err)
	}
	converted, _ := os.ReadFile(opusPath)

	write(t, wavPath, sineWAV(t, 24000, 1, 0.7))
	if _, after, err := convertClip(wavPath); err != nil || after != 0 {
		t.Fatalf("reconvert: %d, %v", after, err)
	}
	if exists(wavPath) {
		t.Error("WAV beside an existing Opus clip should be removed")
	}
	if got, _ := os.ReadFile(opusPath); string(got) != string(converted) {
		t.Error("existing Opus clip was replaced")
	}
}

func TestSweepOrphans(t *testing.T) {
	data := t.TempDir()
	const liveVoice, oldVoice, newVoice = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"
	keep := []string{
		audiopath.ParagraphFile(data, "b", "c", liveVoice, 0),
		audiopath.SFXFile(data, "b", "c", 0),
		audiopath.MusicRegionFile(data, "b", "c", "r1"),
		audiopath.MusicRegionSeedFile(data, "b", "c", "r1"),
		audiopath.AmbienceLoopFile(data, "b", "ocean"),
		filepath.Join(data, "audio", "b", "c", "foley", "x.wav"), // unknown: left alone
	}
	gone := []string{
		filepath.Join(data, "audio", "deleted-book", "c", liveVoice, "00000.opus"),
		filepath.Join(data, "audio", "b", "deleted-chapter", liveVoice, "00000.opus"),
		audiopath.ParagraphFile(data, "b", "c", oldVoice, 0),
		audiopath.MusicRegionFile(data, "b", "c", "rescored-away"),
		audiopath.AmbienceLoopFile(data, "b", "a place no longer used"),
		filepath.Join(filepath.Dir(keep[0]), ".00007.opus.tmp-123"),
	}
	for _, p := range append(keep, gone...) {
		write(t, p, []byte("x"))
	}
	age(t, filepath.Join(data, "audio"), time.Now().Add(-48*time.Hour))
	// Written after the cutoff: a voice whose row may not have committed yet.
	fresh := audiopath.ParagraphFile(data, "b", "c", newVoice, 0)
	write(t, fresh, []byte("x"))
	keep = append(keep, fresh)

	refs := store.AudioRefs{
		Books:          map[string]bool{"b": true},
		ChapterBooks:   map[string]string{"c": "b"},
		ChapterVoices:  map[string]map[string]bool{"c": {liveVoice: true}},
		ChapterRegions: map[string]map[string]bool{"c": {"r1": true}},
		BookAmbience:   map[string]map[string]bool{"b": {"ocean": true}},
	}
	stats := SweepOrphans(context.Background(), data, refs, time.Now().Add(-time.Hour))
	for _, p := range keep {
		if !exists(p) {
			t.Errorf("removed %s, which is still referenced (or too new, or unknown)", p)
		}
	}
	for _, p := range gone {
		if exists(p) {
			t.Errorf("kept orphan %s", p)
		}
	}
	if stats.Removed != len(gone) {
		t.Errorf("Removed = %d, want %d", stats.Removed, len(gone))
	}
}
