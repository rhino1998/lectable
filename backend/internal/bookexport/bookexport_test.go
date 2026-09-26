package bookexport

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

type fixture struct {
	src      Source
	bookID   string
	chapters []*store.Chapter
	voiceID  string
	mtime    time.Time
}

func openStore(t *testing.T, dir string) store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(dir, "library.duckdb"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newFixture builds a two-chapter book on disk. Chapter 0: a narration
// paragraph, then a sentence split into narration + inline quote that
// share one clip (a scare-quote merge group: the quote points 1.5s into
// the anchor's clip), then an image and a break. Chapter 1: one paragraph
// whose clip is still a legacy WAV, one with no audio.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	dataDir := t.TempDir()
	st := openStore(t, dataDir)
	bookID, images, err := st.CreateBook("A Book", "An Author", "en", ".jpg", "Saga", 2, []store.ChapterInput{
		{Title: "One", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "It began."},
			{Kind: store.BlockText, Text: "She said,"},
			{Kind: store.BlockText, Text: "\"Hello there, friend.\"", Inline: true, IsQuote: true},
			{Kind: store.BlockImage, Ext: ".png"},
			{Kind: store.BlockBreak},
		}},
		{Title: "", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "Old clip & <markup>."},
			{Kind: store.BlockText, Text: "Not generated."},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{src: Source{Store: st, Voices: narration.NewResolver(st), DataDir: dataDir}, bookID: bookID,
		mtime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	book, _ := st.GetBook(bookID)
	bv, err := f.src.Voices.BookVoice(book)
	if err != nil {
		t.Fatal(err)
	}
	f.voiceID = bv.VoiceID()
	for i := range 2 {
		ch, err := st.GetChapterByIdx(bookID, i)
		if err != nil {
			t.Fatal(err)
		}
		f.chapters = append(f.chapters, ch)
	}

	write := func(path string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, f.mtime, f.mtime); err != nil {
			t.Fatal(err)
		}
	}
	ready := func(ch *store.Chapter, idx int, dur float64, words string) string {
		t.Helper()
		pid, err := st.GetParagraphIDByIdx(ch.ID, idx)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetParagraphReady(pid, f.voiceID, dur); err != nil {
			t.Fatal(err)
		}
		if words != "" {
			if err := st.SetParagraphWordTimings(pid, f.voiceID, words); err != nil {
				t.Fatal(err)
			}
		}
		return pid
	}

	c0, c1 := f.chapters[0], f.chapters[1]
	ready(c0, 0, 1.0, `[{"text":"It","start":0.1,"end":0.3},{"text":"began.","start":0.4,"end":0.9}]`)
	write(audiopath.ParagraphFile(dataDir, bookID, c0.ID, f.voiceID, 0), []byte("opus-0"))
	// Merge group: anchor 1 owns [0, 1.5), member 2 is [1.5, 3.0) of the same file.
	ready(c0, 1, 1.5, "")
	write(audiopath.ParagraphFile(dataDir, bookID, c0.ID, f.voiceID, 1), []byte("opus-1"))
	pid2, _ := st.GetParagraphIDByIdx(c0.ID, 2)
	if err := st.SetParagraphReadyPointer(pid2, f.voiceID, 1, 1.5, 1.5); err != nil {
		t.Fatal(err)
	}
	if err := st.SetParagraphWordTimings(pid2, f.voiceID, `[{"text":"\"Hello","start":1.6,"end":1.9},{"text":"there,","start":2.0,"end":2.3},{"text":"friend.\"","start":2.4,"end":2.9}]`); err != nil {
		t.Fatal(err)
	}
	write(audiopath.ImageFile(dataDir, bookID, images[0].ChapterID, images[0].ImageID, ".png"), []byte("png"))
	write(audiopath.CoverFile(dataDir, bookID, ".jpg"), []byte("jpg"))
	write(audiopath.SourceEpubFile(dataDir, bookID), []byte("source epub"))
	write(audiopath.VoicePresetRefFile(dataDir, book.VoicePresetID), []byte("RIFF ref"))
	write(audiopath.VoicePresetVariantFile(dataDir, book.VoicePresetID, "angry"), []byte("RIFF angry"))

	legacy, err := wav.Encode(make([]float32, 24000), 24000, 1)
	if err != nil {
		t.Fatal(err)
	}
	ready(c1, 0, 1.0, "")
	write(audiopath.LegacyWAV(audiopath.ParagraphFile(dataDir, bookID, c1.ID, f.voiceID, 0)), legacy)
	return f
}

func (f *fixture) export(t *testing.T, opts Options) (*zip.Reader, []byte) {
	t.Helper()
	opts, err := opts.Normalize(len(f.chapters))
	if err != nil {
		t.Fatal(err)
	}
	book, err := f.src.Collect(f.bookID, opts)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteEPUB(t.Context(), &buf, book, opts, time.Unix(0, 0), nil); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr, buf.Bytes()
}

func readEntry(t *testing.T, zr *zip.Reader, name string) string {
	t.Helper()
	rc, err := zr.Open(name)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return string(b)
}

func TestWriteEPUB(t *testing.T) {
	f := newFixture(t)
	zr, raw := f.export(t, Options{Format: FormatEPUB, WordLevel: true})

	// OCF: mimetype first, stored, readable at its fixed offset.
	if zr.File[0].Name != "mimetype" || zr.File[0].Method != zip.Store {
		t.Fatalf("first entry %s method %d", zr.File[0].Name, zr.File[0].Method)
	}
	if got := string(raw[30:58]); got != "mimetypeapplication/epub+zip" {
		t.Errorf("bytes 30-58 = %q", got)
	}

	// Every XML document parses.
	for _, file := range zr.File {
		if strings.HasSuffix(file.Name, ".xhtml") || strings.HasSuffix(file.Name, ".smil") || strings.HasSuffix(file.Name, ".opf") || strings.HasSuffix(file.Name, ".xml") {
			dec := xml.NewDecoder(strings.NewReader(readEntry(t, zr, file.Name)))
			for {
				if _, err := dec.Token(); err == io.EOF {
					break
				} else if err != nil {
					t.Errorf("%s: %v", file.Name, err)
					break
				}
			}
		}
	}

	c0 := f.chapters[0]
	text := readEntry(t, zr, "OEBPS/text/c00000.xhtml")
	// The inline quote shares its sentence's <p>; image and break follow.
	if !strings.Contains(text, "<p><span id=\"p-") || strings.Count(text, "<p>") != 2 {
		t.Errorf("paragraph grouping wrong:\n%s", text)
	}
	if !strings.Contains(text, "<img src=\"../images/"+c0.ID+"/") || !strings.Contains(text, "<hr class=\"break\"/>") {
		t.Errorf("image/break missing:\n%s", text)
	}
	if strings.Count(readEntry(t, zr, "OEBPS/text/c00001.xhtml"), "&amp; &lt;markup&gt;") != 1 {
		t.Error("chapter 1 text not escaped")
	}

	smil := readEntry(t, zr, "OEBPS/smil/c00000.smil")
	anchorClip := "../audio/" + c0.ID + "/" + f.voiceID + "/00001.opus"
	// Paragraph 0 is word-level: first word from the segment start, last
	// word to its end.
	for _, want := range []string{
		`clipBegin="0.000s" clipEnd="0.400s"`, `clipBegin="0.400s" clipEnd="1.000s"`,
		// Paragraph 1 has no word timings: one paragraph-level entry.
		`<audio src="` + anchorClip + `" clipBegin="0.000s" clipEnd="1.500s"/>`,
		// The merge-group member plays the rest of the anchor's clip.
		`<audio src="` + anchorClip + `" clipBegin="1.500s" clipEnd="2.000s"/>`,
		`clipBegin="2.400s" clipEnd="3.000s"`,
	} {
		if !strings.Contains(smil, want) {
			t.Errorf("smil missing %s:\n%s", want, smil)
		}
	}
	if strings.Contains(smil, "00002.opus") {
		t.Error("merge-group member got a clip of its own")
	}

	// Chapter 1: the legacy WAV went in transcoded to Opus, mtime kept;
	// the ungenerated paragraph is text-only.
	c1 := f.chapters[1]
	clipName := "OEBPS/audio/" + c1.ID + "/" + f.voiceID + "/00000.opus"
	if head := readEntry(t, zr, clipName); !strings.HasPrefix(head, "OggS") {
		t.Errorf("legacy clip not transcoded: %q", head[:min(8, len(head))])
	}
	for _, file := range zr.File {
		if file.Name == clipName && !file.Modified.Equal(f.mtime) {
			t.Errorf("clip mtime %v, want %v", file.Modified, f.mtime)
		}
	}
	if strings.Count(readEntry(t, zr, "OEBPS/smil/c00001.smil"), "<par ") != 1 {
		t.Error("chapter 1 should have one overlay entry")
	}

	opf := readEntry(t, zr, "OEBPS/package.opf")
	for _, want := range []string{`media-overlay="mo-c00000"`, `<meta property="media:duration" refines="#mo-c00000">0:00:04.000</meta>`, `<meta property="media:duration">0:00:05.000</meta>`,
		`properties="cover-image"`, `media-type="audio/ogg; codecs=opus"`, `property="belongs-to-collection"`} {
		if !strings.Contains(opf, want) {
			t.Errorf("opf missing %s:\n%s", want, opf)
		}
	}

	var m Manifest
	if err := json.Unmarshal([]byte(readEntry(t, zr, manifestPath)), &m); err != nil {
		t.Fatal(err)
	}
	if !m.Full || len(m.Tables) == 0 {
		t.Errorf("manifest: %+v", m)
	}
	wantData := map[string]bool{
		"epubs/" + f.bookID + ".epub": false, "covers/" + f.bookID + ".jpg": false,
		"audio/" + f.bookID + "/" + c1.ID + "/" + f.voiceID + "/00000.opus": false,
	}
	for _, mf := range m.Files {
		if _, ok := wantData[mf.Data]; ok {
			wantData[mf.Data] = true
		}
		if strings.HasPrefix(mf.Data, "voice-refs/variants/") {
			wantData["variant"] = true
		}
	}
	for k, ok := range wantData {
		if !ok {
			t.Errorf("manifest has no file %s", k)
		}
	}
}

func TestPartialExport(t *testing.T) {
	f := newFixture(t)
	zr, raw := f.export(t, Options{Format: FormatEPUB, Chapters: []int{1}})
	if !strings.Contains(readEntry(t, zr, "OEBPS/package.opf"), "<dc:title>A Book (Chapter 2)</dc:title>") {
		t.Error("partial export title")
	}
	for _, file := range zr.File {
		if strings.HasPrefix(file.Name, dbDir) || strings.HasPrefix(file.Name, lectableDataDir) || strings.Contains(file.Name, "c00000") {
			t.Errorf("partial export contains %s", file.Name)
		}
	}
	_, err := f.src.Import(bytes.NewReader(raw), int64(len(raw)))
	if !errors.Is(err, ErrPartialExport) {
		t.Errorf("import partial: %v", err)
	}
}

func TestImport(t *testing.T) {
	f := newFixture(t)
	_, raw := f.export(t, Options{Format: FormatEPUB})

	if _, err := f.src.Import(bytes.NewReader(raw), int64(len(raw))); !errors.Is(err, ErrBookExists) {
		t.Errorf("import into its own library: %v", err)
	}

	dir := t.TempDir()
	st := openStore(t, dir)
	dst := Source{Store: st, Voices: narration.NewResolver(st), DataDir: dir}
	// This library already has a (different) clip for the same preset.
	book, _ := f.src.Store.GetBook(f.bookID)
	ref := audiopath.VoicePresetRefFile(dir, book.VoicePresetID)
	_ = os.MkdirAll(filepath.Dir(ref), 0o755)
	_ = os.WriteFile(ref, []byte("mine"), 0o644)

	if _, err := dst.Import(bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if b, _ := os.ReadFile(ref); string(b) != "mine" {
		t.Error("import overwrote an existing voice ref")
	}
	variant := audiopath.VoicePresetVariantFile(dir, book.VoicePresetID, "angry")
	if b, _ := os.ReadFile(variant); string(b) != "RIFF angry" {
		t.Error("variant not restored")
	}
	c0 := f.chapters[0]
	clip := audiopath.ParagraphFile(dir, f.bookID, c0.ID, f.voiceID, 1)
	if fi, err := os.Stat(clip); err != nil || !fi.ModTime().Equal(f.mtime) {
		t.Errorf("restored clip: %v %v", fi, err)
	}

	// The imported library exports the same book byte for byte in content.
	again := Source{Store: st, Voices: narration.NewResolver(st), DataDir: dir}
	opts := Options{Format: FormatEPUB}
	b1, err := f.src.Collect(f.bookID, opts)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := again.Collect(f.bookID, opts)
	if err != nil {
		t.Fatal(err)
	}
	if b1.Duration() != b2.Duration() || b1.MissingAudio != b2.MissingAudio || len(b2.Chapters) != 2 {
		t.Errorf("imported book differs: %v/%d vs %v/%d", b1.Duration(), b1.MissingAudio, b2.Duration(), b2.MissingAudio)
	}
}

func TestAllowedDataPath(t *testing.T) {
	for p, want := range map[string]bool{
		"audio/b1/c/v/00001.opus":  true,
		"voice-refs/x.wav":         true,
		"covers/b1.jpg":            true,
		"epubs/b1.epub":            true,
		"audio/b2/c/v/00001.opus":  false,
		"audio/b1/../b2/x":         false,
		"/etc/passwd":              false,
		"library.duckdb":           false,
		"covers/b1/../../x":        false,
		"epubs/b1.epub/../../evil": false,
	} {
		if got := allowedDataPath("b1", p); got != want {
			t.Errorf("allowedDataPath(%q) = %v", p, got)
		}
	}
}

func TestManager(t *testing.T) {
	f := newFixture(t)
	m := NewManager(f.src, nil)
	opts, err := m.Normalize(f.bookID, Options{Format: FormatEPUB, Chapters: []int{1, 0, 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Full() {
		t.Fatalf("every chapter selected should normalize to a full export: %v", opts.Chapters)
	}
	id := m.Request(f.bookID, opts)
	if list, _ := m.List(f.bookID); len(list) != 1 || !list[0].Pending || list[0].Artifact != nil {
		t.Fatalf("after Request: %+v", list)
	}

	// Chapter 1 has a paragraph without audio.
	if err := m.Build(t.Context(), f.bookID, opts, true); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("complete build of an incomplete book: %v", err)
	}
	if list, _ := m.List(f.bookID); len(list) != 1 || list[0].Pending || list[0].Error == "" {
		t.Fatalf("after failed build: %+v", list)
	}

	m.Request(f.bookID, opts)
	if err := m.Build(t.Context(), f.bookID, opts, false); err != nil {
		t.Fatal(err)
	}
	list, _ := m.List(f.bookID)
	if len(list) != 1 || list[0].ID != id || list[0].Pending || list[0].Error != "" || list[0].Stale || list[0].Artifact.MissingAudio != 1 {
		t.Fatalf("status %+v", list)
	}
	path, a, err := m.File(f.bookID, id)
	if err != nil || a.FileName != "A Book.epub" {
		t.Fatalf("File: %v %+v", err, a)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != a.SizeBytes {
		t.Errorf("artifact file: %v", err)
	}

	// A change to the book makes it stale.
	pid, _ := f.src.Store.GetParagraphIDByIdx(f.chapters[1].ID, 0)
	if _, err := f.src.Store.UpsertBookmark(pid, ""); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.List(f.bookID); !list[0].Stale {
		t.Error("not stale after a change")
	}

	if err := m.Delete(f.bookID, id); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.List(f.bookID); len(list) != 0 {
		t.Errorf("after delete: %+v", list)
	}
	if err := m.Delete(f.bookID, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if err := m.Delete(f.bookID, "../x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("path id: %v", err)
	}
}

func TestGroupPhrases(t *testing.T) {
	text := "One two, three four five six. Seven"
	locs := wordRe.FindAllStringIndex(text, -1)
	words := make([]wordSpan, len(locs))
	for i, loc := range locs {
		// 0.3s per word.
		words[i] = wordSpan{from: loc[0], to: loc[1], begin: float64(i) * 0.3, end: float64(i+1) * 0.3}
	}
	var got []string
	for _, g := range groupPhrases(text, words, 1.0) {
		got = append(got, text[g.from:g.to])
	}
	// "One two," breaks at the comma (0.6s >= half); "three four five six."
	// reaches 1s at "six."; the short "Seven" joins it.
	want := []string{"One two,", "three four five six. Seven"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("groupPhrases = %q, want %q", got, want)
	}
}

func TestPhraseExport(t *testing.T) {
	f := newFixture(t)
	zr, _ := f.export(t, Options{Format: FormatEPUB, WordLevel: true, PhraseSeconds: 0.9})
	smil := readEntry(t, zr, "OEBPS/smil/c00000.smil")
	// Paragraph 0 ("It began.", 1s) is one phrase - so paragraph-level; the
	// quote's words become "\"Hello there," (0.9s) + "friend.\"" (0.6s).
	if strings.Count(smil, "<seq id=\"seq-") != 1 {
		t.Errorf("expected only the quote to be phrase-level:\n%s", smil)
	}
	for _, want := range []string{`clipBegin="1.500s" clipEnd="2.400s"`, `clipBegin="2.400s" clipEnd="3.000s"`} {
		if !strings.Contains(smil, want) {
			t.Errorf("smil missing %s:\n%s", want, smil)
		}
	}
}

// A full export carries background music unless ExcludeMusic, which
// leaves the files out and hands the regions back pending, scoring kept.
func TestExcludeMusic(t *testing.T) {
	f := newFixture(t)
	st, dataDir, c0 := f.src.Store, f.src.DataDir, f.chapters[0]
	regions, err := st.AppendMusicRegions(c0.ID, []store.MusicRegionInput{{StartIdx: 0, Mood: "calm", Prompt: "piano", Ambience: "rain"}}, 2)
	if err != nil || len(regions) != 1 {
		t.Fatalf("AppendMusicRegions: %v %v", regions, err)
	}
	r := regions[0]
	if err := st.SetMusicRegionReady(r.ID, 60); err != nil {
		t.Fatal(err)
	}
	music := []string{
		audiopath.MusicRegionFile(dataDir, f.bookID, c0.ID, r.ID),
		audiopath.MusicRegionSeedFile(dataDir, f.bookID, c0.ID, r.ID),
		audiopath.AmbienceLoopFile(dataDir, f.bookID, "rain"),
	}
	for _, p := range music {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("music"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	check := func(opts Options, wantFiles bool, wantStatus string) {
		t.Helper()
		zr, _ := f.export(t, opts)
		var m Manifest
		if err := json.Unmarshal([]byte(readEntry(t, zr, manifestPath)), &m); err != nil {
			t.Fatal(err)
		}
		for _, p := range music {
			rel, _ := filepath.Rel(dataDir, p)
			found := false
			for _, mf := range m.Files {
				found = found || mf.Data == filepath.ToSlash(rel)
			}
			if found != wantFiles {
				t.Errorf("%+v: %s in export = %v", opts, rel, found)
			}
		}
		rows := readEntry(t, zr, dbDir+"music_regions.jsonl")
		if !strings.Contains(rows, `"status":"`+wantStatus+`"`) || !strings.Contains(rows, `"prompt":"piano"`) {
			t.Errorf("%+v: music_regions rows %s", opts, rows)
		}
	}
	check(Options{Format: FormatEPUB}, true, store.AudioReady)
	check(Options{Format: FormatEPUB, ExcludeMusic: true}, false, store.AudioPending)

	if (Options{Format: FormatEPUB, ExcludeMusic: true}).ID() == (Options{Format: FormatEPUB}).ID() {
		t.Error("ExcludeMusic doesn't change the artifact id")
	}
	if o, _ := (Options{Format: FormatEPUB, ExcludeMusic: true, Chapters: []int{0}}).Normalize(2); o.ExcludeMusic {
		t.Error("ExcludeMusic kept on a partial export")
	}
}

// A latent export ships clips as data - latents sidecars where they exist,
// audio otherwise - with a text-only EPUB, and imports restore them all.
func TestLatentExport(t *testing.T) {
	f := newFixture(t)
	dataDir, c0 := f.src.DataDir, f.chapters[0]
	clip0 := audiopath.ParagraphFile(dataDir, f.bookID, c0.ID, f.voiceID, 0)
	if err := os.WriteFile(latents.Sidecar(clip0), []byte("latents-0"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{Format: FormatEPUB, Latents: true}
	zr, raw := f.export(t, opts)
	var m Manifest
	if err := json.Unmarshal([]byte(readEntry(t, zr, manifestPath)), &m); err != nil {
		t.Fatal(err)
	}
	data := map[string]bool{}
	for _, mf := range m.Files {
		data[mf.Data] = true
	}
	rel := func(p string) string { r, _ := filepath.Rel(dataDir, p); return filepath.ToSlash(r) }
	clip1 := audiopath.ParagraphFile(dataDir, f.bookID, c0.ID, f.voiceID, 1)
	if !data[rel(latents.Sidecar(clip0))] || data[rel(clip0)] {
		t.Errorf("clip 0 should travel as latents only: %v", data)
	}
	if !data[rel(clip1)] {
		t.Errorf("clip 1 (no latents) should travel as audio: %v", data)
	}
	for _, zf := range zr.File {
		if strings.HasSuffix(zf.Name, ".smil") || strings.HasPrefix(zf.Name, oebps+"audio/") {
			t.Errorf("latent export's EPUB carries narration media: %s", zf.Name)
		}
	}
	if (Options{Format: FormatEPUB, Latents: true}).ID() == (Options{Format: FormatEPUB}).ID() {
		t.Error("Latents doesn't change the artifact id")
	}
	if o, _ := (Options{Format: FormatEPUB, Latents: true, Chapters: []int{0}}).Normalize(2); !o.Latents {
		t.Error("Latents dropped from a partial export (it imports as a standalone book)")
	}

	dir := t.TempDir()
	st := openStore(t, dir)
	dst := Source{Store: st, Voices: narration.NewResolver(st), DataDir: dir}
	if _, err := dst.Import(bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatalf("Import: %v", err)
	}
	got, err := os.ReadFile(latents.Sidecar(audiopath.ParagraphFile(dir, f.bookID, c0.ID, f.voiceID, 0)))
	if err != nil || string(got) != "latents-0" {
		t.Errorf("imported sidecar: %q %v", got, err)
	}
	if _, err := os.Stat(audiopath.ParagraphFile(dir, f.bookID, c0.ID, f.voiceID, 1)); err != nil {
		t.Errorf("imported audio clip: %v", err)
	}
}

// A partial latent export imports as its own book of just those chapters -
// new ids, renumbered, retitled - next to a full copy of the same book.
func TestPartialLatentExportIsStandalone(t *testing.T) {
	f := newFixture(t)
	_, full := f.export(t, Options{Format: FormatEPUB})
	opts, err := Options{Format: FormatEPUB, Latents: true, Chapters: []int{1}}.Normalize(2)
	if err != nil || !opts.Latents || opts.Full() {
		t.Fatalf("Normalize: %+v %v", opts, err)
	}
	_, part := f.export(t, opts)

	dir := t.TempDir()
	st := openStore(t, dir)
	dst := Source{Store: st, Voices: narration.NewResolver(st), DataDir: dir}
	if _, err := dst.Import(bytes.NewReader(full), int64(len(full))); err != nil {
		t.Fatalf("import full: %v", err)
	}
	m, err := dst.Import(bytes.NewReader(part), int64(len(part)))
	if err != nil {
		t.Fatalf("import partial: %v", err)
	}
	if m.BookID == f.bookID {
		t.Fatal("partial book reused the full book's id")
	}
	book, err := st.GetBook(m.BookID)
	if err != nil || book == nil || !strings.Contains(book.Title, "(chapter 2)") {
		t.Fatalf("partial book = %+v, %v", book, err)
	}
	ch, err := st.GetChapterByIdx(m.BookID, 0)
	if err != nil || ch == nil || ch.ID == f.chapters[1].ID {
		t.Fatalf("partial chapter 0 = %+v, %v", ch, err)
	}
	if extra, _ := st.GetChapterByIdx(m.BookID, 1); extra != nil {
		t.Error("partial book has a second chapter")
	}
	// The original's chapter 1 had a legacy WAV clip: it travels as audio,
	// under the new book and chapter ids.
	if _, err := os.Stat(audiopath.LegacyWAV(audiopath.ParagraphFile(dir, m.BookID, ch.ID, f.voiceID, 0))); err != nil {
		t.Errorf("partial book's clip not at its new path: %v", err)
	}
	if again, _ := st.GetBook(f.bookID); again == nil {
		t.Error("full copy disappeared")
	}
	if _, err := dst.Import(bytes.NewReader(part), int64(len(part))); !errors.Is(err, ErrBookExists) {
		t.Errorf("re-import of the same partial: %v", err)
	}
}
