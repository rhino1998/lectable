package bookexport

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
)

var (
	// ErrNotExport means the file isn't a lectable export at all - an
	// ordinary epub.
	ErrNotExport = errors.New("not a lectable export")
	// ErrPartialExport is a chapter-subset export, which can't be imported.
	ErrPartialExport = errors.New("only a whole-book export can be imported")
	// ErrBookExists means the library already has the exported book.
	ErrBookExists = errors.New("this book is already in the library")
)

// ReadManifest returns r's lectable manifest, or ErrNotExport.
func ReadManifest(r io.ReaderAt, size int64) (*Manifest, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, ErrNotExport
	}
	return readManifest(zr)
}

func readManifest(zr *zip.Reader) (*Manifest, error) {
	f, err := zr.Open(manifestPath)
	if err != nil {
		return nil, ErrNotExport
	}
	defer f.Close()
	var m Manifest
	if err := json.NewDecoder(f).Decode(&m); err != nil || m.Format != manifestFormat {
		return nil, ErrNotExport
	}
	if m.Version > manifestVersion {
		return nil, fmt.Errorf("export format version %d is newer than this server reads (%d)", m.Version, manifestVersion)
	}
	return &m, nil
}

// Import restores the whole-book export in r into the library under its
// original id - ids are random, so they only collide with the same book.
// The rows go in first, in one transaction; then every file, each back at
// its place under DATA_DIR with its original mtime (audio URLs and
// Android's offline sync version clips by it). If a file fails the book
// is deleted again. Library-wide files - voice reference clips and their
// emotion variants - are only added, never overwritten: a preset this
// library already has keeps its own clip, since its other books were
// narrated from it.
//
// A series character this library already knows (same series, same name)
// keeps its own row and id; the export's voice assignments for it only
// fill clone models it has none for.
func (src Source) Import(r io.ReaderAt, size int64) (*Manifest, error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return nil, ErrNotExport
	}
	m, err := readManifest(zr)
	if err != nil {
		return nil, err
	}
	if !m.Full && !m.Latents {
		return nil, ErrPartialExport
	}
	if existing, err := src.Store.GetBook(m.BookID); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, ErrBookExists
	}

	rows := map[string][]json.RawMessage{}
	for _, table := range m.Tables {
		list, err := readJSONLines(zr, dbDir+table+".jsonl")
		if err != nil {
			return nil, fmt.Errorf("%s: %w", table, err)
		}
		rows[table] = list
	}
	if err := src.remapCharacters(rows); err != nil {
		return nil, err
	}

	// Check every file's destination before writing anything.
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		entries[f.Name] = f
	}
	for _, mf := range m.Files {
		if entries[mf.Archive] == nil {
			return nil, fmt.Errorf("export is missing %s", mf.Archive)
		}
		if !allowedDataPath(m.BookID, mf.Data) {
			return nil, fmt.Errorf("export names a file outside its book: %s", mf.Data)
		}
	}

	if err := src.Store.ImportBookRows(rows); err != nil {
		return nil, err
	}
	for _, mf := range m.Files {
		if err := src.extract(entries[mf.Archive], mf); err != nil {
			src.removeBook(m.BookID)
			return nil, fmt.Errorf("restore %s: %w", mf.Data, err)
		}
	}
	return m, nil
}

// remapCharacters points the export's character rows at characters this
// library already has under the same series scope and name, dropping the
// duplicates - (scope, name) is unique, and paragraphs name speakers, not
// ids, so only character_voices refers to a character id.
func (src Source) remapCharacters(rows map[string][]json.RawMessage) error {
	remap := map[string]string{}
	var keep []json.RawMessage
	for _, raw := range rows["characters"] {
		var r map[string]json.RawMessage
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		existing, err := src.Store.GetCharacterByName(jsonString(r["scope"]), jsonString(r["name"]))
		if err != nil {
			return err
		}
		if existing != nil {
			if id := jsonString(r["id"]); existing.ID != id {
				remap[id] = existing.ID
			}
			continue
		}
		keep = append(keep, raw)
	}
	rows["characters"] = keep
	if len(remap) > 0 {
		rows["character_voices"] = patchRows(rows["character_voices"], func(r map[string]json.RawMessage) {
			if to, ok := remap[jsonString(r["character_id"])]; ok {
				r["character_id"], _ = json.Marshal(to)
			}
		})
	}
	return nil
}

// allowedDataPath limits what an export can write to its own book's
// files and to voice refs - never elsewhere under DATA_DIR, or outside it.
func allowedDataPath(bookID, p string) bool {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.HasPrefix(p, "../") || strings.Contains(p, "\\") {
		return false
	}
	for _, prefix := range []string{"audio/" + bookID + "/", "images/" + bookID + "/", voiceRefsDataDir} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return strings.HasPrefix(p, "covers/"+bookID+".") || p == "epubs/"+bookID+".epub"
}

func (src Source) extract(f *zip.File, mf ManifestFile) error {
	dest := filepath.Join(src.DataDir, filepath.FromSlash(mf.Data))
	if shared(mf.Data) {
		if _, err := os.Stat(dest); err == nil {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := out.Name()
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if !mf.ModTime.IsZero() {
		_ = os.Chtimes(tmp, mf.ModTime, mf.ModTime)
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// removeBook undoes a failed import: the book's rows and its own files
// (shared rows and voice refs stay - other books may use them by now).
func (src Source) removeBook(bookID string) {
	_ = src.Store.DeleteBook(bookID)
	for _, dir := range []string{"audio", "images"} {
		_ = os.RemoveAll(filepath.Join(src.DataDir, dir, bookID))
	}
	_ = os.Remove(audiopath.SourceEpubFile(src.DataDir, bookID))
	matches, _ := filepath.Glob(filepath.Join(src.DataDir, "covers", bookID+".*"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

func readJSONLines(zr *zip.Reader, name string) ([]json.RawMessage, error) {
	f, err := zr.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []json.RawMessage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			return nil, fmt.Errorf("invalid JSON row")
		}
		out = append(out, append(json.RawMessage(nil), line...))
	}
	return out, sc.Err()
}
