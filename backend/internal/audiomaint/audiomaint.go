// Package audiomaint keeps DATA_DIR/audio tidy in the background: it
// converts served clips written before the switch to Ogg Opus
// (MigrateLegacyWAV), and deletes files the database no longer refers to
// (SweepOrphans). Run does both from cmd/server.
package audiomaint

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/musicgen"
	"github.com/rhino1998/lectable/backend/internal/oggopus"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// sweepEvery is how often SweepOrphans runs after the startup pass.
const sweepEvery = 24 * time.Hour

// sweepGrace is how old an unreferenced file or directory must be before
// SweepOrphans deletes it, so nothing mid-write (a clip whose DB row
// commits a moment later, a temp file about to be renamed) is touched.
const sweepGrace = time.Hour

// Run sweeps orphans, converts legacy WAV clips, then sweeps again every
// sweepEvery until ctx ends. Sweeping first means orphans aren't
// converted only to be deleted - and conversion rewrites directories,
// which would shield orphans in them from a sweep until the grace period
// passed. Meant to run in its own goroutine.
func Run(ctx context.Context, dataDir string, st store.Store) {
	sweep := func() {
		refs, err := st.AudioRefs()
		if err != nil {
			log.Printf("audiomaint: read audio references: %v", err)
			return
		}
		if s := SweepOrphans(ctx, dataDir, refs, time.Now().Add(-sweepGrace)); s.Removed > 0 {
			log.Printf("audiomaint: removed %d orphaned audio files/dirs (%s)", s.Removed, mb(s.BytesRemoved))
		}
	}
	sweep()
	start := time.Now()
	m := MigrateLegacyWAV(ctx, dataDir, max(1, runtime.NumCPU()/4))
	if m.Converted+m.Failed > 0 {
		log.Printf("audiomaint: converted %d legacy WAV clips to Opus in %s (%s -> %s), %d failed",
			m.Converted, time.Since(start).Round(time.Second), mb(m.BytesBefore), mb(m.BytesAfter), m.Failed)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(sweepEvery):
		}
		sweep()
	}
}

// MigrateStats summarizes a MigrateLegacyWAV run.
type MigrateStats struct {
	Converted, Failed       int
	BytesBefore, BytesAfter int64
}

// clipName matches a paragraph or SFX clip's file name (audiopath's
// "%05d" + extension).
var clipName = regexp.MustCompile(`^\d{5}\.wav$`)

// voiceDirName matches a voice directory (store.VoiceID's 16 hex chars).
var voiceDirName = regexp.MustCompile(`^[0-9a-f]{16}$`)

// MigrateLegacyWAV converts every served clip still stored as WAV -
// paragraph narration, SFX, music regions - to Ogg Opus, and cuts each
// music region's legacy full-length stems down to its seed tail
// (audiopath.MusicRegionSeedFile). Voice reference clips, ambience loops
// and seeds are model inputs and stay WAV. Safe to interrupt and rerun: a
// clip is only ever converted or skipped as a whole.
func MigrateLegacyWAV(ctx context.Context, dataDir string, workers int) MigrateStats {
	var (
		mu    sync.Mutex
		stats MigrateStats
		wg    sync.WaitGroup
	)
	work := make(chan string)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range work {
				before, after, err := convertClip(path)
				mu.Lock()
				switch {
				case err != nil:
					stats.Failed++
					log.Printf("audiomaint: convert %s: %v", path, err)
				case after > 0:
					stats.Converted++
					stats.BytesBefore += before
					stats.BytesAfter += after
					if stats.Converted%2000 == 0 {
						log.Printf("audiomaint: converted %d clips so far (%s -> %s)", stats.Converted, mb(stats.BytesBefore), mb(stats.BytesAfter))
					}
				}
				mu.Unlock()
			}
		}()
	}
	send := func(path string) bool {
		select {
		case work <- path:
			return true
		case <-ctx.Done():
			return false
		}
	}

	audioDir := filepath.Join(dataDir, "audio")
walk:
	for _, book := range readDir(audioDir) {
		if !book.IsDir() {
			continue
		}
		bookDir := filepath.Join(audioDir, book.Name())
		for _, ch := range readDir(bookDir) {
			if !ch.IsDir() || ch.Name() == "ambience" {
				continue
			}
			chDir := filepath.Join(bookDir, ch.Name())
			for _, sub := range readDir(chDir) {
				subDir := filepath.Join(chDir, sub.Name())
				switch {
				case !sub.IsDir():
				case sub.Name() == "music":
					for _, clip := range prepareMusicDir(subDir) {
						if !send(clip) {
							break walk
						}
					}
				case sub.Name() == "sfx" || voiceDirName.MatchString(sub.Name()):
					for _, f := range readDir(subDir) {
						if clipName.MatchString(f.Name()) && !send(filepath.Join(subDir, f.Name())) {
							break walk
						}
					}
				}
			}
		}
	}
	close(work)
	wg.Wait()
	return stats
}

// prepareMusicDir replaces dir's legacy region stems with seed tails and
// returns the region clips still to convert. Seeds are cut before any
// clip is converted, since a region generated before stems existed has
// only its WAV mix to cut one from.
func prepareMusicDir(dir string) []string {
	var clips []string
	entries := readDir(dir)
	has := map[string]bool{}
	for _, e := range entries {
		has[e.Name()] = true
	}
	seedFrom := func(id, src string) {
		seed := filepath.Join(dir, id+".seed.wav")
		if has[id+".seed.wav"] || has[id+".seed"+latents.Ext] {
			return
		}
		data, err := os.ReadFile(src)
		if err == nil {
			data, err = musicgen.SeedTail(data)
		}
		if err == nil {
			err = audiopath.WriteFileAtomic(seed, data)
		}
		if err != nil {
			log.Printf("audiomaint: cut music seed from %s: %v", src, err)
			return
		}
		has[id+".seed.wav"] = true
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".music.wav"):
			path := filepath.Join(dir, name)
			seedFrom(strings.TrimSuffix(name, ".music.wav"), path)
			_ = os.Remove(path)
		case strings.HasSuffix(name, ".ambience.wav"):
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
	for _, e := range entries {
		name := e.Name()
		id, ok := strings.CutSuffix(name, audiopath.LegacyClipExt)
		if !ok || strings.Contains(id, ".") || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		seedFrom(id, path)
		clips = append(clips, path)
	}
	return clips
}

// convertClip re-encodes one legacy WAV clip as its .opus sibling and
// deletes the WAV, returning both sizes (after is 0 when there was
// nothing to convert). The new file keeps the WAV's mtime - it's the
// clip's version in audio URLs (httpapi.audioVersion), which clients key
// caches and offline copies on, and the audio itself hasn't changed.
//
// Generation runs alongside this, so the clip can be regenerated or
// invalidated mid-conversion. The .opus is linked into place rather than
// renamed, so it never overwrites a fresh render (audiopath.WriteClip
// deletes the WAV after writing one); and if the WAV is gone by the time
// the conversion lands, the clip was deleted meanwhile and the converted
// copy is withdrawn rather than resurrected.
func convertClip(wavPath string) (before, after int64, err error) {
	opusPath := strings.TrimSuffix(wavPath, audiopath.LegacyClipExt) + audiopath.ClipExt
	if _, err := os.Stat(opusPath); err == nil {
		return 0, 0, removeIfExists(wavPath)
	}
	info, err := os.Stat(wavPath)
	if err != nil {
		return 0, 0, ignoreNotExist(err)
	}
	data, err := os.ReadFile(wavPath)
	if err != nil {
		return 0, 0, ignoreNotExist(err)
	}
	encoded, err := oggopus.EncodeWAV(data)
	if err != nil {
		return 0, 0, err
	}
	want, err := wav.Duration(data)
	if err != nil {
		return 0, 0, err
	}
	if got, err := oggopus.Duration(encoded); err != nil || (got-want).Abs() > time.Millisecond {
		return 0, 0, errors.Join(errors.New("encoded duration doesn't match the WAV"), err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(wavPath), "."+filepath.Base(opusPath)+".tmp-*")
	if err != nil {
		return 0, 0, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(encoded)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o644)
	}
	if err == nil {
		err = os.Chtimes(tmp.Name(), info.ModTime(), info.ModTime())
	}
	var tmpInfo fs.FileInfo
	if err == nil {
		tmpInfo, err = os.Stat(tmp.Name())
	}
	if err != nil {
		return 0, 0, err
	}
	if err := os.Link(tmp.Name(), opusPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return 0, 0, removeIfExists(wavPath)
		}
		return 0, 0, err
	}
	if err := os.Remove(wavPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if cur, err := os.Stat(opusPath); err == nil && os.SameFile(cur, tmpInfo) {
				_ = os.Remove(opusPath)
			}
			return 0, 0, nil
		}
		return 0, 0, err
	}
	return info.Size(), int64(len(encoded)), nil
}

// SweepStats summarizes a SweepOrphans run.
type SweepStats struct {
	Removed      int
	BytesRemoved int64
}

// SweepOrphans deletes whatever under DATA_DIR/audio refs no longer
// accounts for, touching only entries last modified before cutoff: whole
// books and chapters that are gone, voice directories no paragraph_audio
// row names, music files of deleted regions, ambience loops no region
// uses, and stale temp files. Names it doesn't recognize are left alone.
func SweepOrphans(ctx context.Context, dataDir string, refs store.AudioRefs, cutoff time.Time) SweepStats {
	return sweepOrphans(ctx, dataDir, refs, cutoff, func(path string) error { return os.RemoveAll(path) })
}

// sweepOrphans is SweepOrphans with the deletion itself supplied, so a
// dry run can list what would go.
func sweepOrphans(ctx context.Context, dataDir string, refs store.AudioRefs, cutoff time.Time, removeAll func(string) error) SweepStats {
	var stats SweepStats
	remove := func(path string, e fs.DirEntry) {
		if !olderThan(e, cutoff) {
			return
		}
		size := diskUsage(path)
		if err := removeAll(path); err != nil {
			log.Printf("audiomaint: remove %s: %v", path, err)
			return
		}
		stats.Removed++
		stats.BytesRemoved += size
	}
	removeStaleTemps := func(dir string) {
		for _, f := range readDir(dir) {
			if !f.IsDir() && strings.HasPrefix(f.Name(), ".") && strings.Contains(f.Name(), ".tmp-") {
				remove(filepath.Join(dir, f.Name()), f)
			}
		}
	}

	audioDir := filepath.Join(dataDir, "audio")
	for _, book := range readDir(audioDir) {
		if ctx.Err() != nil {
			return stats
		}
		bookDir := filepath.Join(audioDir, book.Name())
		if !book.IsDir() {
			continue
		}
		if !refs.Books[book.Name()] {
			remove(bookDir, book)
			continue
		}
		for _, ch := range readDir(bookDir) {
			chDir := filepath.Join(bookDir, ch.Name())
			switch {
			case !ch.IsDir():
				continue
			case ch.Name() == "ambience":
				keep := map[string]bool{}
				for prompt := range refs.BookAmbience[book.Name()] {
					loop := audiopath.AmbienceLoopFile("", "", prompt)
					keep[filepath.Base(loop)] = true
					keep[filepath.Base(latents.Sidecar(loop))] = true
				}
				for _, f := range readDir(chDir) {
					if !keep[f.Name()] {
						remove(filepath.Join(chDir, f.Name()), f)
					}
				}
				continue
			case refs.ChapterBooks[ch.Name()] != book.Name():
				remove(chDir, ch)
				continue
			}
			for _, sub := range readDir(chDir) {
				subDir := filepath.Join(chDir, sub.Name())
				switch {
				case !sub.IsDir():
				case sub.Name() == "music":
					regions := refs.ChapterRegions[ch.Name()]
					for _, f := range readDir(subDir) {
						id, _, _ := strings.Cut(f.Name(), ".")
						if !regions[id] {
							remove(filepath.Join(subDir, f.Name()), f)
						}
					}
				case sub.Name() == "sfx":
					removeStaleTemps(subDir)
				case voiceDirName.MatchString(sub.Name()):
					if !refs.ChapterVoices[ch.Name()][sub.Name()] {
						remove(subDir, sub)
					} else {
						removeStaleTemps(subDir)
					}
				}
			}
		}
	}
	return stats
}

func olderThan(e fs.DirEntry, cutoff time.Time) bool {
	info, err := e.Info()
	return err == nil && info.ModTime().Before(cutoff)
}

func diskUsage(path string) int64 {
	var n int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// readDir lists dir, logging (rather than failing on) anything but a
// missing directory.
func readDir(dir string) []fs.DirEntry {
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("audiomaint: read %s: %v", dir, err)
	}
	return entries
}

func removeIfExists(path string) error {
	return ignoreNotExist(os.Remove(path))
}

func ignoreNotExist(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func mb(n int64) string {
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}
