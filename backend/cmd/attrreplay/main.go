// Command attrreplay replays saved attreval runs (data/attreval-runs/
// <label>.json) through attribution's deterministic post-pass under test -
// today speakerattr.RefinePronounTags - offline, with no LLM call, and
// scores before/after against attreval/gold.json, printing every line it
// changed as FIXED/BROKE/CHANGED. A rule can be tried against every saved
// run in seconds instead of a GPU-bound eval run. Needs the backend on
// :8080 for chapter text, the roster and the book setting.
//
// -castpronouns merges each chapter's summary-notes cast pronouns the way
// attribution does (speakerattr.Roster.WithCast).
//
// -stored book:from-to replays over those chapters' stored attribution
// instead (no gold): every change is printed for reading against the text.
//
// usage (from backend/): go run ./cmd/attrreplay [-castpronouns] [-stored book:from-to] [runs...]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
)

const base = "http://localhost:8080/api/books/"

var books = map[string]string{"arise": "b225960dc6812f3a8bfc534cb64e94b1", "spire": "c228368c10437fa0e83b436a7042b375", "jake": "3fe07ab5323c39550cf03bbde77d4245"}

func get(path string, v any) {
	resp, err := http.Get(base + path)
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		panic(err)
	}
}

type runChapter struct {
	Book    string `json:"book"`
	Chapter int    `json:"chapter"`
	Scored  bool   `json:"scored"`
	Lines   []struct {
		ParagraphIdx int    `json:"paragraphIdx"`
		Text         string `json:"text"`
		Proposed     string `json:"proposed"`
		Given        bool   `json:"given"`
		Current      string `json:"current"`
	} `json:"lines"`
}

type roster struct {
	resolver *speakerattr.SpeakerNameResolver
	pronouns map[string]string
	names    []string
	aliases  map[string][]string
}

func main() {
	castPronouns := flag.Bool("castpronouns", false, "merge each chapter's notes cast pronouns as attribution does")
	stored := flag.String("stored", "", "book:from-to - replay over the stored attribution of those chapters instead of saved runs (no gold; prints every change)")
	flag.Parse()
	gold := map[string]string{}
	raw, _ := os.ReadFile("attreval/gold.json")
	_ = json.Unmarshal(raw, &gold)
	rosters := map[string]roster{}
	for name, id := range books {
		var sp []struct {
			Name     string   `json:"name"`
			Pronouns string   `json:"pronouns"`
			Aliases  []string `json:"aliases"`
		}
		get(id+"/speakers", &sp)
		var names []string
		al := map[string][]string{}
		pr := map[string]string{}
		for _, s := range sp {
			names = append(names, s.Name)
			al[s.Name] = s.Aliases
			if s.Pronouns != "" {
				pr[s.Name] = s.Pronouns
			}
		}
		var setting struct {
			Setting *struct {
				Narrator string `json:"narrator"`
			} `json:"setting"`
		}
		get(id+"/setting", &setting)
		narr := ""
		if setting.Setting != nil {
			narr = setting.Setting.Narrator
		}
		rosters[name] = roster{speakerattr.NewSpeakerNameResolver(names, al).WithFirstPerson(narr), pr, names, al}
	}
	if *stored != "" {
		replayStored(*stored, rosters, *castPronouns)
		return
	}
	for _, label := range flag.Args() {
		var run []runChapter
		if err := json.Unmarshal(mustRead(filepath.Join("data/attreval-runs", label+".json")), &run); err != nil {
			panic(err)
		}
		before, after, n, fixed, broke := 0, 0, 0, 0, 0
		for _, ch := range run {
			if !ch.Scored {
				continue
			}
			var chapter struct {
				Paragraphs []struct {
					Idx        int    `json:"idx"`
					Text       string `json:"text"`
					IsQuote    bool   `json:"isQuote"`
					Inline     bool   `json:"inline"`
					ScareQuote bool   `json:"scareQuote"`
				} `json:"paragraphs"`
			}
			get(fmt.Sprintf("%s/chapters/%d", books[ch.Book], ch.Chapter), &chapter)
			speakers := map[int]string{}
			givenIdx := map[int]string{}
			for _, l := range ch.Lines {
				if l.Given {
					givenIdx[l.ParagraphIdx] = l.Current
					continue
				}
				speakers[l.ParagraphIdx] = l.Proposed
			}
			paras := make([]speakerattr.ParagraphInput, len(chapter.Paragraphs))
			for i, p := range chapter.Paragraphs {
				paras[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote, Known: givenIdx[p.Idx]}
			}
			r := rosters[ch.Book]
			pronouns := r.pronouns
			if *castPronouns {
				pronouns = withCastPronouns(books[ch.Book], ch.Chapter, r)
			}
			out, changes := speakerattr.RefinePronounTags(paras, speakers, r.resolver, pronouns)
			text := map[int]string{}
			for _, l := range ch.Lines {
				text[l.ParagraphIdx] = l.Text
			}
			for idx, s := range speakers {
				g, ok := gold[fmt.Sprintf("%s/%d/%d", ch.Book, ch.Chapter, idx)]
				if !ok {
					continue
				}
				n++
				if same(s, g) {
					before++
				}
				if same(out[idx], g) {
					after++
				}
			}
			for _, c := range changes {
				g := gold[fmt.Sprintf("%s/%d/%d", ch.Book, ch.Chapter, c.Idx)]
				mark := "CHANGED"
				if same(c.To, g) {
					mark, fixed = "FIXED", fixed+1
				} else if same(c.From, g) {
					mark, broke = "BROKE", broke+1
				}
				t := text[c.Idx]
				if len(t) > 70 {
					t = t[:70]
				}
				fmt.Printf("%-7s %s ch%d ¶%d %s: %s -> %s (gold %s) | %s\n", mark, ch.Book, ch.Chapter, c.Idx, c.Reason, c.From, c.To, g, t)
			}
		}
		fmt.Printf("%s: %d -> %d of %d (fixed %d, broke %d)\n\n", label, before, after, n, fixed, broke)
	}
}

func same(a, b string) bool { return strings.EqualFold(a, b) }

func mustRead(p string) []byte {
	b, err := os.ReadFile(p)
	if err != nil {
		panic(err)
	}
	return b
}

// withCastPronouns is the roster's pronouns after attribution merges in
// chapter's notes cast (speakerattr.Roster.WithCast).
func withCastPronouns(bookID string, chapter int, r roster) map[string]string {
	var nr struct {
		Notes *struct {
			Cast []struct {
				Name     string   `json:"name"`
				Role     bool     `json:"role"`
				Pronouns string   `json:"pronouns"`
				Refs     []string `json:"refs"`
			} `json:"cast"`
		} `json:"notes"`
	}
	get(fmt.Sprintf("%s/chapters/%d/notes", bookID, chapter), &nr)
	if nr.Notes == nil {
		return r.pronouns
	}
	var cast []speakerattr.CastMember
	for _, m := range nr.Notes.Cast {
		cast = append(cast, speakerattr.CastMember{Name: m.Name, Role: m.Role, Pronouns: m.Pronouns, Refs: m.Refs})
	}
	return speakerattr.Roster{Names: r.names, Aliases: r.aliases, Pronouns: r.pronouns}.WithCast(cast).Pronouns
}

// replayStored runs the rule over chapters' stored attribution (spec
// "book:from-to") and prints every line it would change, for reading
// against the text where there's no gold.
func replayStored(spec string, rosters map[string]roster, castPronouns bool) {
	book, rng, _ := strings.Cut(spec, ":")
	a, b, _ := strings.Cut(rng, "-")
	from, _ := strconv.Atoi(a)
	to, _ := strconv.Atoi(b)
	r := rosters[book]
	for ch := from; ch <= to; ch++ {
		var chapter struct {
			Paragraphs []struct {
				Idx        int    `json:"idx"`
				Text       string `json:"text"`
				IsQuote    bool   `json:"isQuote"`
				Inline     bool   `json:"inline"`
				ScareQuote bool   `json:"scareQuote"`
				Speaker    string `json:"speaker"`
			} `json:"paragraphs"`
		}
		get(fmt.Sprintf("%s/chapters/%d", books[book], ch), &chapter)
		paras := make([]speakerattr.ParagraphInput, len(chapter.Paragraphs))
		speakers := map[int]string{}
		text := map[int]string{}
		for i, p := range chapter.Paragraphs {
			paras[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote}
			if paras[i].IsQuote && p.Speaker != "" {
				speakers[p.Idx] = p.Speaker
			}
			text[p.Idx] = p.Text
		}
		pronouns := r.pronouns
		if castPronouns {
			pronouns = withCastPronouns(books[book], ch, r)
		}
		_, changes := speakerattr.RefinePronounTags(paras, speakers, r.resolver, pronouns)
		for _, c := range changes {
			t := text[c.Idx]
			if next := nextText(chapter.Paragraphs, c.Idx); next != "" {
				t += " " + next
			}
			if len(t) > 110 {
				t = t[:110]
			}
			fmt.Printf("%s ch%d ¶%d: %s -> %s | %s\n", book, ch, c.Idx, c.From, c.To, t)
		}
	}
}

func nextText[P any](ps []P, idx int) string {
	raw, _ := json.Marshal(ps)
	var list []struct {
		Idx  int    `json:"idx"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &list)
	for i, p := range list {
		if p.Idx == idx && i+1 < len(list) {
			return list[i+1].Text
		}
	}
	return ""
}
