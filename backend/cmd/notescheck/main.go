// Command notescheck audits chapter summary notes against the chapter text,
// offline: for every scene, which cast members it lists as present never
// appear in its paragraphs (by name, alias, a distinctive word of either, or
// one of their references), and whether they speak there anyway - by
// attreval/gold.json on the eval chapters, by the stored attribution
// elsewhere. Needs the backend on :8080.
//
// usage (from backend/): go run ./cmd/notescheck [-v] book:chapter ...
//
//	book is arise, spire, jake, devourer, reclaimer, crusader or midsummer;
//	chapter may be a range (spire:40-60).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
)

const base = "http://localhost:8080/api/books/"

var books = map[string]string{
	"arise": "b225960dc6812f3a8bfc534cb64e94b1", "spire": "c228368c10437fa0e83b436a7042b375",
	"jake": "3fe07ab5323c39550cf03bbde77d4245", "devourer": "51e31b816791643a21968b390451abf1",
	"reclaimer": "02ced1c06b09cb91c192e4bdf1069e49", "crusader": "6a461581fd82e3ac2a5200ae2a2e7261",
	"midsummer": "0482d9419cccfe387d38f08784206c6e",
}

func get(path string, v any) error {
	resp, err := http.Get(base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type paragraph struct {
	Idx     int    `json:"idx"`
	Text    string `json:"text"`
	IsQuote bool   `json:"isQuote"`
	Speaker string `json:"speaker"`
}

type notes struct {
	Scenes []struct {
		StartIdx int      `json:"startIdx"`
		Location string   `json:"location"`
		Present  []string `json:"present"`
	} `json:"scenes"`
	Cast []struct {
		Name     string   `json:"name"`
		Role     bool     `json:"role"`
		Pronouns string   `json:"pronouns"`
		Refs     []string `json:"refs"`
	} `json:"cast"`
}

// mentions builds a matcher for one cast member: the full name, each alias,
// each distinctive word of those (3+ letters, not a title word), and each
// reference, as whole words, case-insensitively.
func mentions(name string, aliases, refs []string) *regexp.Regexp {
	var alts []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s != "" {
			alts = append(alts, regexp.QuoteMeta(strings.ToLower(s)))
		}
	}
	for _, form := range append([]string{name}, aliases...) {
		add(form)
		for _, w := range strings.Fields(form) {
			w = strings.Trim(w, ".,'’")
			if len([]rune(w)) >= 3 && !speakerattr.IsTitleWord(w) {
				add(w)
			}
		}
	}
	for _, r := range refs {
		add(r)
	}
	if len(alts) == 0 {
		return nil
	}
	return regexp.MustCompile(`(?:^|[^\p{L}])(?:` + strings.Join(alts, "|") + `)(?:$|[^\p{L}])`)
}

func main() {
	verbose := flag.Bool("v", false, "print each flagged scene's opening lines")
	flag.Parse()
	gold := map[string]string{}
	if raw, err := os.ReadFile("attreval/gold.json"); err == nil {
		_ = json.Unmarshal(raw, &gold)
	}
	aliasCache := map[string]map[string][]string{}
	narrators := map[string]string{}
	totalScenes, totalMembers, flagged, flaggedSpeaks := 0, 0, 0, 0
	for _, arg := range flag.Args() {
		book, rng, _ := strings.Cut(arg, ":")
		id := books[book]
		lo, hi := rng, rng
		if a, b, ok := strings.Cut(rng, "-"); ok {
			lo, hi = a, b
		}
		from, _ := strconv.Atoi(lo)
		to, _ := strconv.Atoi(hi)
		if _, ok := aliasCache[book]; !ok {
			var sp []struct {
				Name    string   `json:"name"`
				Aliases []string `json:"aliases"`
			}
			_ = get(id+"/speakers", &sp)
			m := map[string][]string{}
			for _, s := range sp {
				m[strings.ToLower(s.Name)] = s.Aliases
			}
			aliasCache[book] = m
			var st struct {
				Setting *struct {
					Narrator string `json:"narrator"`
				} `json:"setting"`
			}
			_ = get(id+"/setting", &st)
			if st.Setting != nil {
				narrators[book] = st.Setting.Narrator
			}
		}
		for ch := from; ch <= to; ch++ {
			var nr struct {
				Notes *notes `json:"notes"`
			}
			if err := get(fmt.Sprintf("%s/chapters/%d/notes", id, ch), &nr); err != nil || nr.Notes == nil || len(nr.Notes.Scenes) == 0 {
				continue
			}
			var chapter struct {
				Paragraphs []paragraph `json:"paragraphs"`
			}
			if err := get(fmt.Sprintf("%s/chapters/%d", id, ch), &chapter); err != nil {
				continue
			}
			ps := chapter.Paragraphs
			sort.Slice(ps, func(i, j int) bool { return ps[i].Idx < ps[j].Idx })
			refs := map[string][]string{}
			for _, m := range nr.Notes.Cast {
				refs[strings.ToLower(m.Name)] = m.Refs
			}
			scenes := nr.Notes.Scenes
			for si, sc := range scenes {
				start, end := sc.StartIdx, 1<<30
				if si == 0 {
					start = 0
				}
				if si+1 < len(scenes) {
					end = scenes[si+1].StartIdx
				}
				totalScenes++
				var text strings.Builder
				speaks := map[string]string{} // lower name -> "gold"/"stored"
				for _, p := range ps {
					if p.Idx < start || p.Idx >= end {
						continue
					}
					text.WriteString(strings.ToLower(p.Text))
					text.WriteString("\n")
					if !p.IsQuote {
						continue
					}
					if g, ok := gold[fmt.Sprintf("%s/%d/%d", book, ch, p.Idx)]; ok {
						speaks[strings.ToLower(g)] = "gold"
					} else if p.Speaker != "" && speaks[strings.ToLower(p.Speaker)] == "" {
						speaks[strings.ToLower(p.Speaker)] = "stored"
					}
				}
				for _, name := range sc.Present {
					totalMembers++
					if name == narrators[book] {
						continue
					}
					low := strings.ToLower(name)
					re := mentions(name, aliasCache[book][low], refs[low])
					if re != nil && re.MatchString(text.String()) {
						continue
					}
					flagged++
					how := speaks[low]
					if how != "" {
						flaggedSpeaks++
					}
					fmt.Printf("%s ch%d scene@%d-%s %-24s speaks=%-6s | %s\n", book, ch, start, endLabel(end), name, how, sc.Location)
					if *verbose {
						n := 0
						for _, p := range ps {
							if p.Idx >= start && p.Idx < end && n < 4 {
								t := p.Text
								if len(t) > 110 {
									t = t[:110]
								}
								fmt.Printf("      ¶%d %s\n", p.Idx, t)
								n++
							}
						}
					}
				}
			}
		}
	}
	fmt.Printf("\nscenes %d, present entries %d, unevidenced %d (%d of them speak in the scene)\n", totalScenes, totalMembers, flagged, flaggedSpeaks)
}

func endLabel(end int) string {
	if end == 1<<30 {
		return "end"
	}
	return strconv.Itoa(end)
}
