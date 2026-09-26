package bookexport

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// ErrBookNotFound is Collect's error for a book that doesn't exist.
var ErrBookNotFound = errors.New("book not found")

// Source is the library an export reads from.
type Source struct {
	Store   store.Store
	Voices  *narration.Resolver
	DataDir string
}

// Collect reads bookID into a Book per opts (already Normalized): every
// selected chapter's content, each paragraph's narration under the voice
// it resolves to right now (narration.BookVoices - exactly what the
// reader plays; clips cached under other voices are left behind), and for
// a full export everything Import needs (archive.go).
func (src Source) Collect(bookID string, opts Options) (*Book, error) {
	book, err := src.Store.GetBook(bookID)
	if err != nil {
		return nil, err
	}
	if book == nil {
		return nil, ErrBookNotFound
	}
	voices, err := src.Voices.BookVoices(book)
	if err != nil {
		return nil, err
	}
	idxs := opts.Chapters
	if opts.Full() {
		n, err := src.Store.CountChapters(bookID)
		if err != nil {
			return nil, err
		}
		for i := range n {
			idxs = append(idxs, i)
		}
	}

	out := &Book{
		ID: book.ID, Title: book.Title, Author: book.Author, Language: book.Language,
		SeriesName: book.SeriesName, SeriesIndex: book.SeriesIndex,
	}
	if book.CoverExt != "" {
		out.Cover = src.file(audiopath.CoverFile(src.DataDir, book.ID, book.CoverExt), book.CoverExt)
	}
	c := collector{src: src, book: book, voices: voices, full: opts.Full(), noMusic: opts.ExcludeMusic, latents: opts.Latents,
		exported: map[[2]string]bool{}, usedVoices: map[string]narration.ResolvedVoice{}, ambience: map[string]bool{}}
	for _, idx := range idxs {
		ch, err := src.Store.GetChapterByIdx(bookID, idx)
		if err != nil {
			return nil, err
		}
		if ch == nil {
			return nil, fmt.Errorf("chapter %d not found", idx)
		}
		chapter, err := c.chapter(ch)
		if err != nil {
			return nil, fmt.Errorf("chapter %d: %w", idx, err)
		}
		out.Chapters = append(out.Chapters, chapter)
	}
	out.MissingAudio = c.missing
	if c.full || c.latents {
		if err := c.lectable(out); err != nil {
			return nil, err
		}
	}
	if !c.full && c.latents {
		c.standalone(out, opts.Chapters)
	}
	return out, nil
}

type collector struct {
	src    Source
	book   *store.Book
	voices *narration.BookVoices
	full   bool
	// noMusic leaves background music (clips, seeds, ambience loops) out
	// of a full export - see Options.ExcludeMusic.
	noMusic bool
	// latents ships latents sidecars in place of audio - Options.Latents.
	latents bool

	missing int
	// exported is every (paragraph id, voice id) whose clip went in -
	// the paragraph_audio rows a full export keeps.
	exported map[[2]string]bool
	// usedVoices is every voice a segment was narrated in, by voice id.
	usedVoices map[string]narration.ResolvedVoice
	// extras are a full export's lectable-only files (SFX, music).
	extras   []File
	ambience map[string]bool
}

func (c *collector) chapter(ch *store.Chapter) (Chapter, error) {
	st := c.src.Store
	paragraphs, err := st.ListParagraphsRaw(ch.ID)
	if err != nil {
		return Chapter{}, err
	}
	images, err := st.ListImages(ch.ID)
	if err != nil {
		return Chapter{}, err
	}
	breaks, err := st.ListBreaks(ch.ID)
	if err != nil {
		return Chapter{}, err
	}

	voiceByParagraph := make(map[string]narration.ResolvedVoice, len(paragraphs))
	idsByVoice := map[string][]string{}
	for _, p := range paragraphs {
		v, err := c.voices.ForSpeaker(p.Speaker)
		if err != nil {
			return Chapter{}, err
		}
		voiceByParagraph[p.ID] = v
		idsByVoice[v.VoiceID()] = append(idsByVoice[v.VoiceID()], p.ID)
	}
	states := make(map[string]store.AudioState, len(paragraphs))
	for vid, ids := range idsByVoice {
		byID, err := st.ParagraphAudioStatuses(ids, vid)
		if err != nil {
			return Chapter{}, err
		}
		for id, s := range byID {
			states[id] = s
		}
	}

	clips := map[string]*Clip{} // by path - a merge group's members share one
	type positioned struct {
		position int
		block    Block
	}
	items := make([]positioned, 0, len(paragraphs)+len(images)+len(breaks))
	for _, p := range paragraphs {
		para := &Paragraph{ID: p.ID, Idx: p.Idx, Text: p.Text, Inline: p.Inline}
		v := voiceByParagraph[p.ID]
		if s, ok := states[p.ID]; ok && s.Status == store.AudioReady && s.DurationSeconds > 0 {
			para.Audio = c.segment(ch, p, v, s, clips)
		}
		if para.Audio == nil {
			c.missing++
		} else {
			c.exported[[2]string{p.ID, v.VoiceID()}] = true
			c.usedVoices[v.VoiceID()] = v
		}
		items = append(items, positioned{p.Position, Block{Kind: BlockParagraph, Paragraph: para}})
	}
	for _, img := range images {
		if f := c.src.file(audiopath.ImageFile(c.src.DataDir, ch.BookID, ch.ID, img.ID, img.Ext), img.Ext); f != nil {
			items = append(items, positioned{img.Position, Block{Kind: BlockImage, Image: f}})
		}
	}
	for _, b := range breaks {
		items = append(items, positioned{b.Position, Block{Kind: BlockBreak}})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].position < items[j].position })

	out := Chapter{ID: ch.ID, Idx: ch.Idx, Title: ch.Title, Content: make([]Block, len(items))}
	for i, it := range items {
		out.Content[i] = it.block
	}
	if c.full || c.latents {
		if err := c.chapterExtras(ch, paragraphs); err != nil {
			return Chapter{}, err
		}
	}
	return out, nil
}

// segment is a ready paragraph's narration. A scare-quote merge group
// member (PointerOffset > 0) has no file of its own: its audio is the
// range of the group anchor's clip starting PointerSeconds in. nil if
// the clip isn't on disk.
func (c *collector) segment(ch *store.Chapter, p store.Paragraph, v narration.ResolvedVoice, s store.AudioState, clips map[string]*Clip) *Segment {
	path := audiopath.ParagraphFile(c.src.DataDir, ch.BookID, ch.ID, v.VoiceID(), p.Idx-s.PointerOffset)
	clip, ok := clips[path]
	if !ok {
		resolved := audiopath.Resolve(path)
		f := c.src.file(resolved, audiopath.ClipExt)
		if f == nil {
			return nil
		}
		// A legacy WAV goes in transcoded, under the .opus name it'll
		// have once converted.
		f.DataPath = c.src.dataPath(path)
		clip = &Clip{File: *f, VoiceID: v.VoiceID(), Legacy: resolved != path}
		clips[path] = clip
		if c.latents {
			// The clip travels as data, not EPUB media: its latents where
			// it has them (decoded on first play by the receiving
			// library), else the audio file itself.
			if _, err := os.Stat(latents.Sidecar(path)); err == nil {
				c.addExtra(latents.Sidecar(path))
			} else {
				c.addExtra(resolved)
			}
		}
	}
	seg := &Segment{Clip: clip, Begin: s.PointerSeconds, End: s.PointerSeconds + s.DurationSeconds}
	if s.WordTimings != "" {
		_ = json.Unmarshal([]byte(s.WordTimings), &seg.Words) // unaligned: stays paragraph-level
	}
	return seg
}

// chapterExtras adds ch's SFX and music files to a full export.
func (c *collector) chapterExtras(ch *store.Chapter, paragraphs []store.Paragraph) error {
	ids := make([]string, len(paragraphs))
	for i, p := range paragraphs {
		ids[i] = p.ID
	}
	sfx, err := c.src.Store.ParagraphSFXStates(ids)
	if err != nil {
		return err
	}
	for _, p := range paragraphs {
		if sfx[p.ID].Status != store.AudioReady {
			continue
		}
		c.addExtra(audiopath.Resolve(audiopath.SFXFile(c.src.DataDir, ch.BookID, ch.ID, p.Idx)))
	}
	if c.noMusic {
		return nil
	}
	regions, err := c.src.Store.ListMusicRegions(ch.ID)
	if err != nil {
		return err
	}
	for _, r := range regions {
		c.addExtra(audiopath.Resolve(audiopath.MusicRegionFile(c.src.DataDir, ch.BookID, ch.ID, r.ID)))
		c.addExtra(audiopath.MusicRegionSeedFile(c.src.DataDir, ch.BookID, ch.ID, r.ID))
		c.addExtra(audiopath.MusicRegionSeedLatentsFile(c.src.DataDir, ch.BookID, ch.ID, r.ID))
		if r.Ambience != "" && !c.ambience[r.Ambience] {
			c.ambience[r.Ambience] = true
			loop := audiopath.AmbienceLoopFile(c.src.DataDir, ch.BookID, r.Ambience)
			if _, err := os.Stat(latents.Sidecar(loop)); err == nil && c.latents {
				c.addExtra(latents.Sidecar(loop)) // rebuilt when next needed (jobs.ambienceLoop)
			} else {
				c.addExtra(loop)
			}
		}
	}
	return nil
}

func (c *collector) addExtra(path string) {
	if f := c.src.file(path, ""); f != nil {
		c.extras = append(c.extras, *f)
	}
}

// lectable fills a full export's Lectable: the database rows, the source
// epub, every voice's reference clip and emotion variants, and the
// SFX/music files gathered per chapter.
func (c *collector) lectable(out *Book) error {
	rows, err := c.src.Store.ExportBookRows(c.book.ID, store.SeriesScope(c.book))
	if err != nil {
		return err
	}
	rows["paragraph_audio"] = filterRows(rows["paragraph_audio"], func(r map[string]json.RawMessage) bool {
		return c.exported[[2]string{jsonString(r["paragraph_id"]), jsonString(r["voice_id"])}]
	})
	// A render in flight when the export ran never finishes in the
	// library it's imported into; its file isn't in the export either.
	rows["sfx"] = patchRows(rows["sfx"], func(r map[string]json.RawMessage) {
		if jsonString(r["status"]) == store.AudioGenerating {
			r["status"] = json.RawMessage(`""`)
		}
	})
	rows["music_regions"] = patchRows(rows["music_regions"], func(r map[string]json.RawMessage) {
		// Without its files, a region keeps its scoring (the LLM work) but
		// goes back to pending, so the importing library renders it again.
		if c.noMusic || jsonString(r["status"]) == store.AudioGenerating {
			r["status"] = json.RawMessage(`"` + store.AudioPending + `"`)
		}
		if c.noMusic {
			r["error"] = json.RawMessage(`""`)
			r["duration_seconds"] = json.RawMessage(`0`)
		}
	})
	out.Lectable.Rows = rows

	var files []File
	if f := c.src.file(audiopath.SourceEpubFile(c.src.DataDir, c.book.ID), ".epub"); f != nil {
		files = append(files, *f)
	}
	for _, id := range c.presetIDs(rows["voice_presets"]) {
		if f := c.src.file(audiopath.VoicePresetRefFile(c.src.DataDir, id), ".wav"); f != nil {
			files = append(files, *f)
		}
		dir := audiopath.VoicePresetVariantDir(c.src.DataDir, id)
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.Type().IsRegular() && e.Name()[0] != '.' {
				if f := c.src.file(filepath.Join(dir, e.Name()), ""); f != nil {
					files = append(files, *f)
				}
			}
		}
	}
	out.Lectable.Files = append(files, c.extras...)

	for vid, v := range c.usedVoices {
		out.Lectable.Voices = append(out.Lectable.Voices, VoiceInfo{
			VoiceID: vid, PresetID: v.PresetID, Instruct: v.Instruct, CloneInstruct: v.CloneInstruct,
			Language: v.Language, CloneModel: v.CloneModel,
		})
	}
	sort.Slice(out.Lectable.Voices, func(i, j int) bool { return out.Lectable.Voices[i].VoiceID < out.Lectable.Voices[j].VoiceID })
	return nil
}

// presetIDs is every preset the book can narrate with: its own, each
// voice a segment used (built-in presets have no row), and every custom
// preset its characters are assigned under any clone model - so another
// library can keep generating, and switch clone model, in the same voices.
func (c *collector) presetIDs(presetRows []json.RawMessage) []string {
	var ids []string
	add := func(id string) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	add(c.book.VoicePresetID)
	for _, v := range c.usedVoices {
		add(v.PresetID)
	}
	for _, raw := range presetRows {
		var r map[string]json.RawMessage
		if json.Unmarshal(raw, &r) == nil {
			add(jsonString(r["id"]))
		}
	}
	slices.Sort(ids)
	return ids
}

// file describes path for an export, nil if it isn't a readable regular
// file.
func (src Source) file(path, ext string) *File {
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	return &File{Path: path, DataPath: src.dataPath(path), ModTime: fi.ModTime(), Size: fi.Size(), Ext: ext}
}

func (src Source) dataPath(path string) string {
	rel, err := filepath.Rel(src.DataDir, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(rel)
}

func jsonString(raw json.RawMessage) string {
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func filterRows(list []json.RawMessage, keep func(map[string]json.RawMessage) bool) []json.RawMessage {
	var out []json.RawMessage
	for _, raw := range list {
		var r map[string]json.RawMessage
		if json.Unmarshal(raw, &r) == nil && keep(r) {
			out = append(out, raw)
		}
	}
	return out
}

func patchRows(list []json.RawMessage, patch func(map[string]json.RawMessage)) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(list))
	for _, raw := range list {
		var r map[string]json.RawMessage
		if json.Unmarshal(raw, &r) != nil {
			out = append(out, raw)
			continue
		}
		patch(r)
		b, err := json.Marshal(r)
		if err != nil {
			out = append(out, raw)
			continue
		}
		out = append(out, b)
	}
	return out
}
