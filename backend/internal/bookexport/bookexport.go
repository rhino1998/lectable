// Package bookexport packs a rendered book into one downloadable file and
// back.
//
// Export is two steps. Collect reads the library into a format-neutral
// Book: chapters in reading order, each with its display content and its
// playback timeline - Segments, one per paragraph with audio, each a time
// range inside a Clip (a narration file on disk) plus that range's word
// timings. A format writer then lays that out: today an EPUB 3 with Media
// Overlays (WriteEPUB), which any EPUB 3 reader with read-aloud support
// plays with paragraph- or word-level highlighting. The same Book is
// meant to feed an audio-only writer too (an Opus-in-MP4 .m4b: remux each
// chapter's clip ranges in order, chapter marks at Chapter boundaries) -
// nothing in the timeline is EPUB-specific.
//
// A full-book export also carries everything lectable itself needs to
// restore the book into another library (Import): the book's database
// rows, the source epub, voice reference clips and emotion variants,
// SFX/music/ambience audio, all under META-INF/lectable/, where EPUB
// readers never look. See archive.go for that layout, and Manager for how
// exports are built in the background and kept on disk for re-download.
package bookexport

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Format is an export file format.
type Format string

const (
	// FormatEPUB is an EPUB 3 with Media Overlays, narration clips
	// included as-is (Ogg Opus is an EPUB 3.3 core media type).
	FormatEPUB Format = "epub"
)

// Ext is f's file extension.
func (f Format) Ext() string {
	switch f {
	case FormatEPUB:
		return ".epub"
	}
	return ""
}

// ContentType is what f is served as.
func (f Format) ContentType() string {
	switch f {
	case FormatEPUB:
		return "application/epub+zip"
	}
	return "application/octet-stream"
}

// Valid reports whether f is a format this package can write.
func (f Format) Valid() bool { return f.Ext() != "" }

// Options selects what an export contains. Two exports with equal Options
// are the same artifact (see Options.ID) - rebuilding one replaces it.
type Options struct {
	Format Format
	// WordLevel gives every word its own overlay entry (and its own span
	// in the text), so a reader highlights word by word instead of
	// paragraph by paragraph. Paragraphs whose alignment doesn't match
	// their text word for word stay paragraph-level.
	WordLevel bool
	// PhraseSeconds (word-level only) groups words into phrases of at
	// least this long, breaking after punctuation where it can, instead of
	// one overlay entry per word. Some reading systems can't keep up with
	// very short entries - Chromium-based ones like Thorium only see the
	// audio clock every ~250ms, which at 2x speed is longer than most
	// words - so sync drifts; phrases keep the highlight moving without
	// relying on that precision. 0 is per word.
	PhraseSeconds float64
	// ExcludeMusic (full exports only) leaves background music out -
	// region clips, their seeds and ambience loops, typically most of a
	// scored book's size. Music regions keep their scoring and come back
	// pending, so an importing library renders the music again.
	ExcludeMusic bool
	// Chapters is the chapter indexes to include, sorted and deduplicated
	// (Normalize); empty means the whole book. Only a whole-book export
	// can be imported into another library.
	Chapters []int
	// Latents makes a lectable-to-lectable transfer
	// that ships models' latents instead of audio wherever they exist:
	// narration clips as their latents sidecars, ambience loops as theirs
	// (anything without latents still goes as audio). The EPUB part is
	// text-only; the receiving library decodes each clip the first time
	// it's played. Always as-is - an in-progress book moves with whatever
	// it has, the rest generated later. With Chapters set it imports as a
	// standalone book of just those chapters (new ids, "Title (chapters
	// ...)") - see collector.standalone.
	Latents bool
}

// Normalize sorts and deduplicates o.Chapters, and clears it when it
// names every one of chapterCount chapters (that's a full export).
func (o Options) Normalize(chapterCount int) (Options, error) {
	if !o.Format.Valid() {
		return o, fmt.Errorf("unknown export format %q", o.Format)
	}
	if o.PhraseSeconds < 0 || o.PhraseSeconds > 10 {
		return o, fmt.Errorf("phrase length %gs out of range (0-10s)", o.PhraseSeconds)
	}
	if !o.WordLevel {
		o.PhraseSeconds = 0
	}
	seen := map[int]bool{}
	var out []int
	for _, idx := range o.Chapters {
		if idx < 0 || idx >= chapterCount {
			return o, fmt.Errorf("chapter %d out of range (book has %d)", idx, chapterCount)
		}
		if !seen[idx] {
			seen[idx] = true
			out = append(out, idx)
		}
	}
	slices.Sort(out)
	if len(out) == chapterCount {
		out = nil
	}
	o.Chapters = out
	if !o.Full() {
		o.ExcludeMusic = false // a partial export carries no music anyway
	}
	return o, nil
}

// Full reports whether o covers the whole book.
func (o Options) Full() bool { return len(o.Chapters) == 0 }

// ID names the artifact o describes, stable across rebuilds.
func (o Options) ID() string {
	var b strings.Builder
	b.WriteString(string(o.Format))
	if o.WordLevel {
		b.WriteString("-words")
		if o.PhraseSeconds > 0 {
			b.WriteString("-p" + strconv.Itoa(int(o.PhraseSeconds*1000+0.5)))
		}
	}
	if o.Full() {
		b.WriteString("-all")
		if o.ExcludeMusic {
			b.WriteString("-nomusic")
		}
		if o.Latents {
			b.WriteString("-latents")
		}
		return b.String()
	}
	b.WriteString("-ch")
	if o.Latents {
		b.WriteString("-latents")
	}
	for _, r := range ChapterRanges(o.Chapters) {
		b.WriteString("-" + strconv.Itoa(r[0]))
		if r[1] != r[0] {
			b.WriteString("_" + strconv.Itoa(r[1]))
		}
	}
	return b.String()
}

// ChapterRanges groups sorted chapter indexes into inclusive [first, last]
// runs of consecutive indexes.
func ChapterRanges(idxs []int) [][2]int {
	var out [][2]int
	for _, i := range idxs {
		if n := len(out); n > 0 && out[n-1][1] == i-1 {
			out[n-1][1] = i
			continue
		}
		out = append(out, [2]int{i, i})
	}
	return out
}

// ChapterLabel describes a selection for people, 1-based: "Chapters 1–40,
// 45", or "" for the whole book.
func ChapterLabel(idxs []int) string {
	if len(idxs) == 0 {
		return ""
	}
	var parts []string
	for _, r := range ChapterRanges(idxs) {
		if r[0] == r[1] {
			parts = append(parts, strconv.Itoa(r[0]+1))
		} else {
			parts = append(parts, strconv.Itoa(r[0]+1)+"–"+strconv.Itoa(r[1]+1))
		}
	}
	if len(idxs) == 1 {
		return "Chapter " + parts[0]
	}
	return "Chapters " + strings.Join(parts, ", ")
}

// Book is a book ready to be written: metadata, the chapters being
// exported, and (for a full export) what lectable needs to restore it.
type Book struct {
	ID          string
	Title       string
	Author      string
	Language    string
	SeriesName  string
	SeriesIndex float64
	// Cover is the cover image, nil if the book has none.
	Cover *File

	Chapters []Chapter
	// MissingAudio counts exported paragraphs with no ready narration -
	// they're in the text, with no overlay entry.
	MissingAudio int

	// Lectable is everything only lectable reads back (archive.go).
	Lectable Lectable
}

// Duration is the book's total narration length.
func (b *Book) Duration() float64 {
	var d float64
	for i := range b.Chapters {
		d += b.Chapters[i].Duration()
	}
	return d
}

// Chapter is one exported chapter.
type Chapter struct {
	ID    string
	Idx   int
	Title string
	// Content is the chapter in display order: paragraphs interleaved
	// with images and scene breaks.
	Content []Block
}

// Duration is the chapter's narration length: its segments back to back.
func (c *Chapter) Duration() float64 {
	var d float64
	for _, s := range c.Segments() {
		d += s.End - s.Begin
	}
	return d
}

// Segments is the chapter's playback timeline - every paragraph with
// audio, in reading order.
func (c *Chapter) Segments() []*Segment {
	var out []*Segment
	for _, b := range c.Content {
		if b.Paragraph != nil && b.Paragraph.Audio != nil {
			out = append(out, b.Paragraph.Audio)
		}
	}
	return out
}

// BlockKind is what a Block holds.
type BlockKind int

const (
	BlockParagraph BlockKind = iota
	BlockImage
	BlockBreak
)

// Block is one item of a chapter's display content.
type Block struct {
	Kind      BlockKind
	Paragraph *Paragraph // BlockParagraph
	Image     *File      // BlockImage
}

// Paragraph is one paragraph row - the unit of narration. Inline
// paragraphs continue the previous one's visual paragraph (a quote split
// out of a narration sentence).
type Paragraph struct {
	ID     string
	Idx    int
	Text   string
	Inline bool
	// Audio is nil when the paragraph has no ready narration.
	Audio *Segment
}

// Segment is one paragraph's narration: [Begin, End) seconds of Clip.
// Most paragraphs own a whole clip; a scare-quote merge group shares one,
// each member a consecutive range of it.
type Segment struct {
	Clip  *Clip
	Begin float64
	End   float64
	// Words are the paragraph's aligned words, times in Clip's own
	// timeline (not relative to Begin). Empty when never aligned.
	Words []Word
}

// Word is one aligned word.
type Word struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// Clip is one narration file, shared by every segment cut from it.
type Clip struct {
	File
	VoiceID string
	// Legacy marks a pre-Opus WAV clip (internal/audiomaint hasn't
	// converted it yet); writers transcode it to Opus.
	Legacy bool
}

// File is a file on disk going into an export.
type File struct {
	// Path is where it is on disk.
	Path string
	// DataPath is Path relative to the library's DATA_DIR, slash-separated
	// - where Import puts it back.
	DataPath string
	ModTime  time.Time
	Size     int64
	// Ext is the extension (".jpg"), for files whose type matters to the
	// format (images).
	Ext string
}
