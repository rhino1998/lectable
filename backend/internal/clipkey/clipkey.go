// Package clipkey names a narration clip by what it says and how, rather
// than by where it sits (audiopath.ParagraphFile's book/chapter/voice/idx):
// two paragraphs with the same Recipe are the same clip, wherever they are.
//
// Identical lines close together must not sound identical, so a key also
// carries a take: lines repeating within RunWindow paragraphs of each other
// count up 0, 1, 2, ... (TakeAt), and anything further apart shares take 0.
// Measured on the library (196k paragraphs): 19.5k paragraphs repeat an
// earlier line of their book, 715 of them within 5 paragraphs - mostly
// speech tags ("Fritz said.", 839 times in one book) and stat-block cells.
//
// No dependencies beyond the standard library, and nothing here touches the
// store or the disk.
package clipkey

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// RunWindow is how many paragraphs back an identical line still counts as
// part of the same run, and so gets the next take instead of take 0.
const RunWindow = 5

// OrphanRetention is how long a clip no paragraph uses any more is kept
// in case the same reading is wanted again.
const OrphanRetention = 7 * 24 * time.Hour

// version is hashed into every key: bump it when a Recipe field changes
// meaning, so clips keyed the old way are never mistaken for the new.
const version = 1

// Recipe is everything that decides what a clip sounds like, short of the
// sampling itself.
type Recipe struct {
	// CloneModel is the clone model narrating the line (never "": resolve
	// voices.DefaultCloneModel first).
	CloneModel string
	// Reference identifies the reference clip cloned from - which preset,
	// and which emotion variant of it. Opaque here; the speaker and the
	// emotion reach a key only through it.
	Reference string
	Language  string
	// CloneInstruct is narration.ResolvedVoice.CloneInstruct.
	CloneInstruct string
	// Text is the generation text (store.Paragraph.ResolveGenerationText).
	Text string
	// AlignText is the plain text the clip's word timings are aligned
	// against. It differs from Text wherever pronunciation or a table
	// heading changed what is spoken, and the timings travel with the clip.
	AlignText string
}

// Key is the clip key for r's take'th reading (see TakeAt).
func (r Recipe) Key(take int) string {
	return r.sum(take)
}

// sum hashes every field length-prefixed, so no two recipes can collide by
// moving a boundary between fields.
func (r Recipe) sum(take int) string {
	h := sha256.New()
	var n [8]byte
	writeInt := func(v int) {
		binary.LittleEndian.PutUint64(n[:], uint64(int64(v)))
		h.Write(n[:])
	}
	writeInt(version)
	for _, f := range []string{r.CloneModel, r.Reference, r.Language, r.CloneInstruct, r.Text, r.AlignText} {
		writeInt(len(f))
		h.Write([]byte(f))
	}
	writeInt(take)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// TakeAt is the take of the line at paragraph idx of a chapter: 0, unless
// the nearest earlier paragraph holding the same line is at most RunWindow
// back, in which case one more than that paragraph's. A run is therefore a
// chain of repeats each close to the last, however long, and the count
// starts over after any gap wider than RunWindow.
//
// same reports whether paragraphs a and b (b < a) hold the same line; it is
// only asked about paragraphs from 0 up. A take depends only on the lines
// just before it, so an edit elsewhere in the chapter never changes it.
func TakeAt(idx int, same func(a, b int) bool) int {
	take := 0
	for cur := idx; ; take++ {
		prev := -1
		for j := cur - 1; j >= 0 && j >= cur-RunWindow; j-- {
			if same(cur, j) {
				prev = j
				break
			}
		}
		if prev < 0 {
			return take
		}
		cur = prev
	}
}
