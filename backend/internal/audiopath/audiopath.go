// Package audiopath computes the on-disk location of generated audio and
// extracted images from IDs alone, so neither ever needs to be persisted
// in the database.
//
// Served clips (paragraph narration, SFX, music regions) are Ogg Opus -
// written through WriteClip, never os.WriteFile. They used to be WAV, and
// a data dir from before the switch is converted in the background after
// startup (internal/audiomaint), so until that finishes a clip may still
// exist only as its legacy .wav sibling: read through Resolve and delete
// through RemoveClip, which both cover it. Everything a model reads back
// as input (voice reference clips, ambience loops, music seeds) stays WAV.
package audiopath

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/oggopus"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// ClipExt is the served-clip extension, and LegacyClipExt the one they
// had before.
const (
	ClipExt       = ".opus"
	LegacyClipExt = ".wav"
)

// ClipContentType is what a served clip is sent as - set explicitly since
// Go's mime table doesn't know .opus.
const ClipContentType = "audio/ogg; codecs=opus"

// LegacyWAV is clip path's pre-Opus sibling.
func LegacyWAV(path string) string {
	return strings.TrimSuffix(path, ClipExt) + LegacyClipExt
}

// Resolve returns path, or its legacy WAV sibling when only that exists
// (a clip the background conversion hasn't reached yet). Returns path
// when neither exists, so callers' own not-found handling still applies.
func Resolve(path string) string {
	if _, err := os.Stat(path); err != nil {
		if legacy := LegacyWAV(path); legacy != path {
			if _, err := os.Stat(legacy); err == nil {
				return legacy
			}
		}
	}
	return path
}

// RemoveClip deletes clip path, its legacy WAV sibling and its latents
// sidecar; a missing file is not an error.
func RemoveClip(path string) error {
	var errs []error
	for _, p := range []string{path, LegacyWAV(path), latents.Sidecar(path)} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// WriteClip encodes wavBytes as Ogg Opus and atomically replaces path
// with it (a reader mid-ServeFile keeps the old file), then drops any
// legacy WAV sibling so it can't be converted over the new clip later.
func WriteClip(path string, wavBytes []byte) error {
	data, err := oggopus.EncodeWAV(wavBytes)
	if err != nil {
		return fmt.Errorf("encode opus: %w", err)
	}
	if err := WriteFileAtomic(path, data); err != nil {
		return err
	}
	_ = os.Remove(LegacyWAV(path))
	// Whatever latents sat beside the old clip describe the old audio.
	_ = os.Remove(latents.Sidecar(path))
	return nil
}

// WriteClipLatents is WriteClip plus the clip's latents sidecar
// (latents.Sidecar(path)) when lat is non-nil: the generated audio's exact
// decoder input, stored losslessly beside the lossy served clip. A failed
// sidecar write leaves the clip without one rather than failing the clip.
func WriteClipLatents(path string, wavBytes []byte, lat *ttsproto.Latents) error {
	if err := WriteClip(path, wavBytes); err != nil {
		return err
	}
	if lat == nil {
		return nil
	}
	if err := latents.WriteFile(latents.Sidecar(path), lat, ""); err != nil {
		log.Printf("audiopath: write latents for %s: %v", path, err)
		_ = os.Remove(latents.Sidecar(path))
	}
	return nil
}

// WriteFileAtomic writes data to a temp file beside path and renames it
// into place. Temp files are dot-prefixed, which the orphan sweep treats
// as removable once stale.
func WriteFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// VoiceDir is where one voice's generated audio for one chapter lives.
// Keyed by voiceID (a hash of the full voice config - see store.VoiceID) so
// different voices for the same book never collide or overwrite each other.
func VoiceDir(dataDir, bookID, chapterID, voiceID string) string {
	return filepath.Join(dataDir, "audio", bookID, chapterID, voiceID)
}

func ParagraphFile(dataDir, bookID, chapterID, voiceID string, idx int) string {
	return filepath.Join(VoiceDir(dataDir, bookID, chapterID, voiceID), fmt.Sprintf("%05d"+ClipExt, idx))
}

func EnsureVoiceDir(dataDir, bookID, chapterID, voiceID string) error {
	return os.MkdirAll(VoiceDir(dataDir, bookID, chapterID, voiceID), 0o755)
}

func CoverFile(dataDir, bookID, ext string) string {
	return filepath.Join(dataDir, "covers", bookID+ext)
}

func EnsureCoverDir(dataDir string) error {
	return os.MkdirAll(filepath.Join(dataDir, "covers"), 0o755)
}

// SourceEpubFile is the original uploaded epub a book was imported from,
// kept so a single chapter can later be re-imported from it (httpapi's
// handleReimportChapter) - e.g. after a parser fix - without re-uploading.
func SourceEpubFile(dataDir, bookID string) string {
	return filepath.Join(dataDir, "epubs", bookID+".epub")
}

func EnsureSourceEpubDir(dataDir string) error {
	return os.MkdirAll(filepath.Join(dataDir, "epubs"), 0o755)
}

func ImageDir(dataDir, bookID, chapterID string) string {
	return filepath.Join(dataDir, "images", bookID, chapterID)
}

func ImageFile(dataDir, bookID, chapterID, imageID, ext string) string {
	return filepath.Join(ImageDir(dataDir, bookID, chapterID), imageID+ext)
}

func EnsureImageDir(dataDir, bookID, chapterID string) error {
	return os.MkdirAll(ImageDir(dataDir, bookID, chapterID), 0o755)
}

// SFXDir is where one chapter's generated sound-effect clips (Stable
// Audio SFX today) live - voice-independent (see store.SFXState's own
// doc comment), so unlike VoiceDir there's no voiceID segment.
func SFXDir(dataDir, bookID, chapterID string) string {
	return filepath.Join(dataDir, "audio", bookID, chapterID, "sfx")
}

func SFXFile(dataDir, bookID, chapterID string, idx int) string {
	return filepath.Join(SFXDir(dataDir, bookID, chapterID), fmt.Sprintf("%05d"+ClipExt, idx))
}

func EnsureSFXDir(dataDir, bookID, chapterID string) error {
	return os.MkdirAll(SFXDir(dataDir, bookID, chapterID), 0o755)
}

// MusicDir is where one chapter's generated background-music region clips
// (Stable Audio Medium, via internal/musicgen) live - voice-independent
// (a region's own clip has nothing to do with which narration voice reads
// the chapter), so like SFXDir there's no voiceID segment.
func MusicDir(dataDir, bookID, chapterID string) string {
	return filepath.Join(dataDir, "audio", bookID, chapterID, "music")
}

// MusicRegionFile is one music region's own clip, keyed by its own row id
// rather than an index - unlike paragraph/SFX audio, a chapter's regions
// can be entirely replaced by a rescoring run (store.Store.ClearMusicRegions),
// so a positional filename would risk a stale file from a deleted region
// silently lingering under a new region's own index.
func MusicRegionFile(dataDir, bookID, chapterID, regionID string) string {
	return filepath.Join(MusicDir(dataDir, bookID, chapterID), regionID+ClipExt)
}

// MusicRegionSeedFile is the tail of a region's music layer (no ambience),
// kept as WAV beside the served mix (MusicRegionFile) so a following
// "continuation" region can be seeded from it - only the tail, since
// that's all musicgen.GenerateRegion ever feeds back in.
func MusicRegionSeedFile(dataDir, bookID, chapterID, regionID string) string {
	return filepath.Join(MusicDir(dataDir, bookID, chapterID), regionID+".seed.wav")
}

// MusicRegionSeedLatentsFile is MusicRegionSeedFile's successor: the same
// tail as Stable Audio latents (f16), fed back in without re-encoding. A
// region has one or the other.
func MusicRegionSeedLatentsFile(dataDir, bookID, chapterID, regionID string) string {
	return filepath.Join(MusicDir(dataDir, bookID, chapterID), regionID+".seed"+latents.Ext)
}

// LegacyMusicRegionStemFile is a region's full-length stem as written
// before seeds were cut to their tail - "music" or "ambience" (from before
// ambience became a shared loop). Only internal/audiomaint's conversion
// and RemoveMusicRegionFiles still touch these.
func LegacyMusicRegionStemFile(dataDir, bookID, chapterID, regionID, stem string) string {
	return filepath.Join(MusicDir(dataDir, bookID, chapterID), regionID+"."+stem+".wav")
}

// RemoveMusicRegionFiles best-effort deletes a region's served clip, its
// seed, and any legacy stems - a file that was never written is not an
// error.
func RemoveMusicRegionFiles(dataDir, bookID, chapterID, regionID string) {
	_ = RemoveClip(MusicRegionFile(dataDir, bookID, chapterID, regionID))
	_ = os.Remove(MusicRegionSeedFile(dataDir, bookID, chapterID, regionID))
	_ = os.Remove(MusicRegionSeedLatentsFile(dataDir, bookID, chapterID, regionID))
	for _, stem := range []string{"music", "ambience"} {
		_ = os.Remove(LegacyMusicRegionStemFile(dataDir, bookID, chapterID, regionID, stem))
	}
}

// AmbienceDir holds a book's ambience loops - one per distinct ambience
// prompt, shared by every region (in any chapter) set in that place.
func AmbienceDir(dataDir, bookID string) string {
	return filepath.Join(dataDir, "audio", bookID, "ambience")
}

// AmbienceLoopFile is the ambience loop for one ambience prompt, keyed by
// the prompt's hash (the describe pass keeps an unchanged setting's prompt
// byte-identical, so the same place maps to the same file).
func AmbienceLoopFile(dataDir, bookID, prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	return filepath.Join(AmbienceDir(dataDir, bookID), hex.EncodeToString(sum[:8])+".wav")
}

func EnsureMusicDir(dataDir, bookID, chapterID string) error {
	return os.MkdirAll(MusicDir(dataDir, bookID, chapterID), 0o755)
}

// VoicePresetRefFile is where a custom voice preset's reference clip
// lives - the clip tts-service cloned it from, kept here (not in
// tts-service) so the app can serve it back for preview playback, same as
// any other generated audio.
func VoicePresetRefFile(dataDir, presetID string) string {
	return filepath.Join(dataDir, "voice-refs", presetID+".wav")
}

func EnsureVoiceRefDir(dataDir string) error {
	return os.MkdirAll(filepath.Join(dataDir, "voice-refs"), 0o755)
}

// VoicePresetVariantDir holds every emotion variant of presetID's reference
// clip (see voicerefs.EnsureVariantFile) - one directory per preset so a
// base-clip change can drop them all at once.
func VoicePresetVariantDir(dataDir, presetID string) string {
	return filepath.Join(dataDir, "voice-refs", "variants", presetID)
}

// VoicePresetVariantFile is presetID's emotion variant for emotion (an
// internal/emotions id).
func VoicePresetVariantFile(dataDir, presetID, emotion string) string {
	return filepath.Join(VoicePresetVariantDir(dataDir, presetID), emotion+".wav")
}

// VoicePresetVariantFailedFile marks a variant whose render failed on its
// final attempt, so generation stops waiting on it and falls back to the
// base clip - see voicerefs.MarkVariantFailed.
func VoicePresetVariantFailedFile(dataDir, presetID, emotion string) string {
	return filepath.Join(VoicePresetVariantDir(dataDir, presetID), emotion+".failed")
}

// VoiceDesignCacheFile is a config-addressed cache slot for a raw,
// natural-pace VoiceDesign render, keyed by a hash of the exact
// (instruct, seed, text, language) recipe that produced it - see
// voicerefs.DesignConfigHash - rather than by preset id. A "test this
// voice design" preview (httpapi.handleTestVoiceDesign) has no preset id
// to key by yet, so this is what lets saving the preset right after
// testing it reuse that exact render instead of paying for a second one.
func VoiceDesignCacheFile(dataDir, hash string) string {
	return filepath.Join(dataDir, "voice-refs", "cache", hash+".wav")
}

func EnsureVoiceDesignCacheDir(dataDir string) error {
	return os.MkdirAll(filepath.Join(dataDir, "voice-refs", "cache"), 0o755)
}

// EnsureDir creates path's parent directory.
func EnsureDir(path string) error {
	return os.MkdirAll(filepath.Dir(path), 0o755)
}
