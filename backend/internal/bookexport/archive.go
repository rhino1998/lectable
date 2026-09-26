package bookexport

import (
	"encoding/json"
	"strings"
	"time"
)

// The lectable-only part of an export lives under META-INF/lectable/ -
// EPUB reserves META-INF for container metadata, and reading systems
// ignore anything there they don't recognize, so it's invisible to every
// other reader and to the package document:
//
//	META-INF/lectable/manifest.json      Manifest
//	META-INF/lectable/db/<table>.jsonl   the book's rows, one JSON object per line
//	META-INF/lectable/data/<data path>   files readers never need (source epub,
//	                                     voice refs and variants, SFX, music, ambience)
//
// Narration clips, images and the cover are EPUB resources and stay where
// the package document puts them (OEBPS/...); Manifest.Files maps every
// file in the archive, wherever it is, back to its place under DATA_DIR,
// so Import never needs to know the EPUB layout.
const (
	lectableDir      = "META-INF/lectable/"
	manifestPath     = lectableDir + "manifest.json"
	dbDir            = lectableDir + "db/"
	lectableDataDir  = lectableDir + "data/"
	manifestFormat   = "lectable-book"
	manifestVersion  = 1
	voiceRefsDataDir = "voice-refs/"
)

// Lectable is what a full export carries for lectable itself.
type Lectable struct {
	// Rows are the book's database rows by table (store.ExportBookRows),
	// trimmed to what the export holds: paragraph_audio only for the
	// exported clips.
	Rows map[string][]json.RawMessage
	// Files are lectable-only files (not EPUB resources).
	Files []File
	// Voices describes each voice the exported narration used.
	Voices []VoiceInfo
}

// VoiceInfo is one narration voice - informational, the rows and files
// are what Import restores.
type VoiceInfo struct {
	VoiceID       string `json:"voiceId"`
	PresetID      string `json:"presetId"`
	Instruct      string `json:"instruct,omitempty"`
	CloneInstruct string `json:"cloneInstruct,omitempty"`
	Language      string `json:"language,omitempty"`
	CloneModel    string `json:"cloneModel,omitempty"`
}

// Manifest is META-INF/lectable/manifest.json.
type Manifest struct {
	Format     string    `json:"format"`
	Version    int       `json:"version"`
	ExportedAt time.Time `json:"exportedAt"`
	BookID     string    `json:"bookId"`
	Title      string    `json:"title"`
	// Full is true for a whole-book export - the only kind Import takes.
	Full bool `json:"full"`
	// Latents marks a latent-only transfer (Options.Latents). A partial one
	// (Full false) imports as its own standalone book - see
	// collector.standalone.
	Latents bool `json:"latents,omitempty"`
	// Chapters is the exported chapter indexes.
	Chapters  []int       `json:"chapters"`
	WordLevel bool        `json:"wordLevel"`
	Voices    []VoiceInfo `json:"voices,omitempty"`
	// Tables lists the db/*.jsonl files present.
	Tables []string `json:"tables,omitempty"`
	// Files maps archive entries to DATA_DIR (full exports only).
	Files []ManifestFile `json:"files,omitempty"`
}

// ManifestFile is one archive entry's place in the library.
type ManifestFile struct {
	Archive string    `json:"archive"`
	Data    string    `json:"data"`
	ModTime time.Time `json:"modTime"`
}

// shared reports whether a data path belongs to the whole library rather
// than one book - Import never overwrites those.
func shared(dataPath string) bool {
	return strings.HasPrefix(dataPath, voiceRefsDataDir)
}
