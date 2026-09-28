package httpapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// chapterNotesDTO is one chapter's summary-pass notes (store.ChapterNotes):
// what happens, its scenes, and its cast.
type chapterNotesDTO struct {
	Summary string            `json:"summary"`
	Scenes  []summarySceneDTO `json:"scenes"`
	Cast    []castMemberDTO   `json:"cast"`
}

type summarySceneDTO struct {
	// StartIdx is the scene's first paragraph; it runs to the next scene's.
	StartIdx int      `json:"startIdx"`
	Location string   `json:"location"`
	Time     string   `json:"time"`
	Present  []string `json:"present"`
}

type castMemberDTO struct {
	Name     string   `json:"name"`
	Role     bool     `json:"role"`
	Pronouns string   `json:"pronouns"`
	Refs     []string `json:"refs"`
}

// chapterNotesResponse is GET .../notes' reply and the chapterNotes live
// topic's value. Notes is nil until the chapter has been summarized.
type chapterNotesResponse struct {
	Notes *chapterNotesDTO `json:"notes"`
}

func chapterNotesDTOFrom(n store.ChapterNotes) *chapterNotesDTO {
	out := &chapterNotesDTO{Summary: n.Summary, Scenes: []summarySceneDTO{}, Cast: []castMemberDTO{}}
	for _, sc := range n.Scenes {
		out.Scenes = append(out.Scenes, summarySceneDTO{StartIdx: sc.StartIdx, Location: sc.Location, Time: sc.Time, Present: nonNil(sc.Present)})
	}
	for _, m := range n.Cast {
		out.Cast = append(out.Cast, castMemberDTO{Name: m.Name, Role: m.Role, Pronouns: m.Pronouns, Refs: nonNil(m.Refs)})
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func storeNotesFrom(s speakerattr.ChapterSummary) store.ChapterNotes {
	out := store.ChapterNotes{Summary: s.Summary}
	for _, sc := range s.Scenes {
		out.Scenes = append(out.Scenes, store.SummaryScene{StartIdx: sc.StartIdx, Location: sc.Location, Time: sc.Time, Present: sc.Present})
	}
	for _, m := range s.Cast {
		out.Cast = append(out.Cast, store.CastMember{Name: m.Name, Role: m.Role, Pronouns: m.Pronouns, Refs: m.Refs})
	}
	return out
}

func speakerattrNotes(n *store.ChapterNotes) *speakerattr.ChapterSummary {
	if n == nil {
		return nil
	}
	out := &speakerattr.ChapterSummary{Summary: n.Summary}
	for _, sc := range n.Scenes {
		out.Scenes = append(out.Scenes, speakerattr.Scene{StartIdx: sc.StartIdx, Location: sc.Location, Time: sc.Time, Present: sc.Present})
	}
	for _, m := range n.Cast {
		out.Cast = append(out.Cast, speakerattr.CastMember{Name: m.Name, Role: m.Role, Pronouns: m.Pronouns, Refs: m.Refs})
	}
	return out
}

// chapterNotes is ch's summary-pass notes for a consuming pass, nil when it
// has none (never run, or the LLM isn't configured) - every consumer works
// without them, as before the pass existed.
func (s *Server) chapterNotes(ch *store.Chapter) *speakerattr.ChapterSummary {
	n, err := s.Store.GetChapterNotes(ch.ID)
	if err != nil {
		return nil
	}
	return speakerattrNotes(n)
}

// withChapterCast extends an attribution roster with ch's cast from its
// summary (speakerattr.Roster.WithCast), leaving out invalid names; r
// unchanged when the chapter has no summary.
func (s *Server) withChapterCast(r speakerattr.Roster, ch *store.Chapter, invalid invalidSpeakers) speakerattr.Roster {
	notes := s.chapterNotes(ch)
	if notes == nil || len(notes.Cast) == 0 {
		return r
	}
	var cast []speakerattr.CastMember
	for _, m := range notes.Cast {
		if !invalid.has(m.Name) {
			cast = append(cast, m)
		}
	}
	return r.WithCast(cast)
}

// summaryRoster is the known-characters roster the summary pass is shown -
// attribution's (see attributeChapter), minus descriptions: the book's
// series-wide valid characters with their roles, aliases and pronouns, and
// its most frequent speakers as always-listed.
func (s *Server) summaryRoster(book *store.Book, ch *store.Chapter) (speakerattr.Roster, error) {
	roster, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return speakerattr.Roster{}, err
	}
	existing, _ := splitInvalidCharacters(roster)
	r := speakerattr.Roster{Roles: map[string]bool{}, Aliases: map[string][]string{}, Pronouns: map[string]string{}}
	for _, c := range existing {
		r.Names = append(r.Names, c.Name)
		if c.IsRole {
			r.Roles[c.Name] = true
		}
		if len(c.Aliases) > 0 {
			r.Aliases[c.Name] = c.Aliases
		}
		if c.Pronouns != "" {
			r.Pronouns[c.Name] = c.Pronouns
		}
	}
	r.Prominent = s.prominentSpeakers(book.ID, ch.Idx, r.Names)
	return r, nil
}

// summarizeChapter is a jobs.KindSummary task's work: one chapter's summary
// pass (speakerattr.Client.SummarizeChapter), stored with its passes.summary
// flag (Store.SetChapterNotes). Changes no paragraph, so no audio goes stale;
// a pass that reads the notes picks them up the next time it runs.
func (s *Server) summarizeChapter(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	if len(all) == 0 {
		return 0, nil, s.Store.SetChapterNotes(ch.ID, store.ChapterNotes{})
	}
	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote}
	}
	roster, err := s.summaryRoster(book, ch)
	if err != nil {
		return 0, nil, err
	}
	sum, err := s.Speaker.SummarizeChapter(ctx, book.Title, ch.Title, s.bookSetting(ctx, book), roster, inputs)
	if err != nil {
		return 0, nil, fmt.Errorf("summarize: %w", err)
	}
	// Check the cast against the rest of the book: a new name or a role's
	// reference ("the storm guard sergeant") the book ties to a known
	// character becomes them. Best-effort - the summary stands without it.
	if ix, ierr := s.identityIndex(book, roster); ierr != nil {
		log.Printf("httpapi: identity index for book %s: %v", book.ID, ierr)
	} else {
		resolved, aliases, rerr := s.Speaker.ResolveCastIdentities(ctx, book.Title, sum, ix, roster)
		if rerr != nil {
			log.Printf("httpapi: identity checks for chapter %s: %v", ch.ID, rerr)
		} else {
			sum = resolved
			s.recordCastAliases(book, aliases)
		}
	}
	if err := s.Store.SetChapterNotes(ch.ID, storeNotesFrom(sum)); err != nil {
		return 0, nil, err
	}
	return len(sum.Scenes), nil, nil
}

// handleSummarizeChapter serves POST /api/books/{id}/chapters/{idx}/summarize:
// queues the chapter's summary pass, re-running it if it already has one.
func (s *Server) handleSummarizeChapter(w http.ResponseWriter, r *http.Request) {
	book, ch, ok := s.chapterLLMTarget(w, r)
	if !ok {
		return
	}
	s.Jobs.EnqueueSummary(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.summarizeChapter(ctx, book, ch)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// buildChapterNotes is the chapterNotes live topic's value and GET
// .../notes' reply.
func (s *Server) buildChapterNotes(bookID string, chapterIdx int) (chapterNotesResponse, error) {
	ch, err := s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		return chapterNotesResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if ch == nil {
		return chapterNotesResponse{}, httpError(http.StatusNotFound, "chapter not found")
	}
	n, err := s.Store.GetChapterNotes(ch.ID)
	if err != nil {
		return chapterNotesResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if n == nil {
		return chapterNotesResponse{}, nil
	}
	return chapterNotesResponse{Notes: chapterNotesDTOFrom(*n)}, nil
}

// handleGetChapterNotes serves GET /api/books/{id}/chapters/{idx}/notes.
func (s *Server) handleGetChapterNotes(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	v, err := s.buildChapterNotes(r.PathValue("id"), idx)
	writeBuilt(w, v, err)
}

// identityIndexTTL is how long a book's identity index (its text and known
// characters, for speakerattr's identity retrieval) is reused before being
// rebuilt - a whole-book summary run reuses one index across its chapters,
// while characters added since show up within minutes.
const identityIndexTTL = 10 * time.Minute

type cachedIdentityIndex struct {
	ix    *speakerattr.IdentityIndex
	built time.Time
}

// identityIndex is book's identity index - every paragraph's text and the
// roster's non-role characters with their aliases - cached per book for
// identityIndexTTL (Server.identityIndexes).
func (s *Server) identityIndex(book *store.Book, roster speakerattr.Roster) (*speakerattr.IdentityIndex, error) {
	if v, ok := s.identityIndexes.Load(book.ID); ok {
		if c := v.(cachedIdentityIndex); time.Since(c.built) < identityIndexTTL {
			return c.ix, nil
		}
	}
	all, err := s.Store.ListParagraphsRawForBook(book.ID)
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(all))
	for i, p := range all {
		texts[i] = p.Text
	}
	names := map[string][]string{}
	for _, n := range roster.Names {
		if !roster.Roles[n] {
			names[n] = roster.Aliases[n]
		}
	}
	ix := speakerattr.NewIdentityIndex(texts, names)
	s.identityIndexes.Store(book.ID, cachedIdentityIndex{ix: ix, built: time.Now()})
	return ix, nil
}

// recordCastAliases saves each name ResolveCastIdentities found to be
// another name for a known character as that character's alias, so
// attribution folds it back in from now on. Best-effort; a name that
// already belongs to another character is skipped (it's already known).
func (s *Server) recordCastAliases(book *store.Book, aliases []speakerattr.CastAlias) {
	scope := store.SeriesScope(book)
	for _, a := range aliases {
		if clean, _ := speakerattr.SanitizeSpeaker(a.Alias); clean != a.Alias {
			continue
		}
		if other, err := s.Store.GetCharacterByName(scope, a.Alias); err != nil || other != nil {
			continue
		}
		char, err := s.Store.GetCharacterByName(scope, a.Character)
		if err != nil || char == nil {
			continue
		}
		if err := s.Store.AddCharacterAliases(char.ID, a.Alias); err != nil {
			log.Printf("httpapi: record alias %q for %s: %v", a.Alias, a.Character, err)
		} else {
			log.Printf("httpapi: identity: %q is %s (alias recorded)", a.Alias, a.Character)
		}
	}
}
