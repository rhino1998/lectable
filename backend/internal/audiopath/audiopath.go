// Package audiopath computes the on-disk location of generated audio and
// extracted images from IDs alone, so neither ever needs to be persisted
// in the database.
package audiopath

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// VoiceDir is where one voice's generated audio for one chapter lives.
// Keyed by voiceID (a hash of the full voice config - see store.VoiceID) so
// different voices for the same book never collide or overwrite each other.
func VoiceDir(dataDir, bookID, chapterID, voiceID string) string {
	return filepath.Join(dataDir, "audio", bookID, chapterID, voiceID)
}

func ParagraphFile(dataDir, bookID, chapterID, voiceID string, idx int) string {
	return filepath.Join(VoiceDir(dataDir, bookID, chapterID, voiceID), fmt.Sprintf("%05d.wav", idx))
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
	return filepath.Join(SFXDir(dataDir, bookID, chapterID), fmt.Sprintf("%05d.wav", idx))
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
	return filepath.Join(MusicDir(dataDir, bookID, chapterID), regionID+".wav")
}

// MusicRegionStemFile is one layer of a music region's clip, kept beside
// the served mix (MusicRegionFile) so the next region can be continuation-
// seeded from that layer alone: stem is "music" or "ambience".
func MusicRegionStemFile(dataDir, bookID, chapterID, regionID, stem string) string {
	return filepath.Join(MusicDir(dataDir, bookID, chapterID), regionID+"."+stem+".wav")
}

// RemoveMusicRegionFiles best-effort deletes a region's served clip and
// both of its stems - a file that was never written is not an error.
func RemoveMusicRegionFiles(dataDir, bookID, chapterID, regionID string) {
	_ = os.Remove(MusicRegionFile(dataDir, bookID, chapterID, regionID))
	for _, stem := range []string{"music", "ambience"} {
		_ = os.Remove(MusicRegionStemFile(dataDir, bookID, chapterID, regionID, stem))
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
