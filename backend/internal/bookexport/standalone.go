package bookexport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
)

// bookLocalTables are the exported tables whose rows belong to one book
// (and carry its book/chapter/paragraph/... ids) - not the series-scoped
// characters or the shared voice presets.
var bookLocalTables = []string{"books", "chapters", "paragraphs", "paragraph_audio", "images", "breaks", "bookmarks", "sfx", "music_regions"}

// standalone turns a partial latent export into its own book: only its
// chapters' rows, every book-local id replaced with a fresh one (so it can
// sit beside a full copy of the same book in the importing library), the
// chapters renumbered from 0, the title marked with the range, the reading
// position reset, and every file's data path rewritten to the new ids. The
// source epub is dropped - a chapter re-import from it wouldn't line up
// with the renumbered chapters.
//
// New ids are derived (sha256) from the source ids and the chapter set, so
// re-exporting the same chapters yields the same book id, and importing it
// twice is refused as an existing book.
func (c *collector) standalone(out *Book, idxs []int) {
	chosen := map[string]bool{}
	for _, ch := range out.Chapters {
		chosen[ch.ID] = true
	}
	rows := out.Lectable.Rows
	byChapter := func(r map[string]json.RawMessage) bool { return chosen[jsonString(r["chapter_id"])] }
	rows["chapters"] = filterRows(rows["chapters"], func(r map[string]json.RawMessage) bool { return chosen[jsonString(r["id"])] })
	rows["paragraphs"] = filterRows(rows["paragraphs"], byChapter)
	paragraphs := map[string]bool{}
	for _, raw := range rows["paragraphs"] {
		var r map[string]json.RawMessage
		if json.Unmarshal(raw, &r) == nil {
			paragraphs[jsonString(r["id"])] = true
		}
	}
	byParagraph := func(r map[string]json.RawMessage) bool { return paragraphs[jsonString(r["paragraph_id"])] }
	for _, t := range []string{"paragraph_audio", "bookmarks", "sfx"} {
		rows[t] = filterRows(rows[t], byParagraph)
	}
	for _, t := range []string{"images", "breaks", "music_regions"} {
		rows[t] = filterRows(rows[t], byChapter)
	}

	oldBookID := c.book.ID
	key := oldBookID + "|" + stringsFromInts(idxs)
	newBookID := derivedID(key, "book")
	ids := map[string]string{oldBookID: newBookID}
	for _, t := range []string{"chapters", "paragraphs", "images", "breaks", "bookmarks", "music_regions"} {
		for _, raw := range rows[t] {
			var r map[string]json.RawMessage
			if json.Unmarshal(raw, &r) == nil {
				if id := jsonString(r["id"]); id != "" {
					ids[id] = derivedID(newBookID, id)
				}
			}
		}
	}
	newIdx := map[int]int{}
	for i, idx := range idxs {
		newIdx[idx] = i
	}
	label := strings.ToLower(ChapterLabel(idxs))
	for _, t := range bookLocalTables {
		rows[t] = patchRows(rows[t], func(r map[string]json.RawMessage) {
			for k, v := range r {
				if s := jsonString(v); s != "" {
					if n, ok := ids[s]; ok {
						r[k], _ = json.Marshal(n)
					}
				}
			}
			switch t {
			case "books":
				r["title"], _ = json.Marshal(jsonString(r["title"]) + " (" + label + ")")
				for _, k := range []string{"pos_chapter_idx", "pos_paragraph_idx", "pos_seconds"} {
					if _, ok := r[k]; ok {
						r[k] = json.RawMessage(`0`)
					}
				}
			case "chapters":
				var idx int
				if json.Unmarshal(r["idx"], &idx) == nil {
					r["idx"], _ = json.Marshal(newIdx[idx])
				}
			}
		})
	}

	epub := "epubs/" + oldBookID + ".epub"
	files := out.Lectable.Files[:0]
	for _, f := range out.Lectable.Files {
		if f.DataPath == epub {
			continue
		}
		f.DataPath = remapPath(f.DataPath, ids)
		files = append(files, f)
	}
	out.Lectable.Files = files
	if out.Cover != nil {
		out.Cover.DataPath = remapPath(out.Cover.DataPath, ids)
	}
	for i := range out.Chapters {
		ch := &out.Chapters[i]
		ch.ID, ch.Idx = ids[ch.ID], newIdx[ch.Idx]
		for j := range ch.Content {
			if img := ch.Content[j].Image; img != nil {
				img.DataPath = remapPath(img.DataPath, ids)
			}
		}
	}
	out.ID = newBookID
	out.Title += " (" + label + ")"
}

// derivedID is a stable new 32-hex id for id under scope.
func derivedID(scope, id string) string {
	sum := sha256.Sum256([]byte(scope + "|" + id))
	return hex.EncodeToString(sum[:16])
}

// remapPath replaces ids in a data path: a whole segment equal to one
// (audio/<book>/<chapter>/...), or a file name's "<id>." prefix
// (music/<region>.seed.lat, covers/<book>.jpg).
func remapPath(p string, ids map[string]string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if n, ok := ids[s]; ok {
			segs[i] = n
			continue
		}
		if id, rest, ok := strings.Cut(s, "."); ok {
			if n, ok := ids[id]; ok {
				segs[i] = n + "." + rest
			}
		}
	}
	return strings.Join(segs, "/")
}

func stringsFromInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
