package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/namekey"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// Roster review: the pass that looks over a roster many chapters of
// attribution built up and proposes fixes - one person under two names, an
// alias that is somebody else's name, a description never marked as a
// role, a name that names nobody (see speakerattr's rosterreview.go for
// how each is found). It never changes the roster itself: every finding is
// a store.RosterProposal the reader accepts or skips on the Speakers page,
// since a merge can't be undone. Proposals can also be filed from outside
// (POST .../roster-review/proposals), which is how a stronger reader than
// the local model adds its own.

// rosterFixKind mirrors speakerattr.RosterFixKind on the wire.
type rosterFixKind = speakerattr.RosterFixKind

// rosterProposalDTO is one proposed fix to a book's roster.
type rosterProposalDTO struct {
	ID   string        `json:"id"`
	Kind rosterFixKind `json:"kind"`
	// SubjectID/Subject is the character it changes; TargetID/Target the
	// one it is merged into, or whose name an alias collides with ("" when
	// none).
	SubjectID string `json:"subjectId"`
	Subject   string `json:"subject"`
	TargetID  string `json:"targetId"`
	Target    string `json:"target"`
	// SubjectLines/TargetLines are how many lines of dialogue each has
	// across the roster's books.
	SubjectLines int `json:"subjectLines"`
	TargetLines  int `json:"targetLines"`
	// Detail is the alias to remove (remove_alias) or split off
	// (split_alias), or the subject's new name (rename, rename_role).
	Detail string `json:"detail"`
	Reason string `json:"reason"`
	// Evidence is the passages from the book that show it.
	Evidence  []string                   `json:"evidence"`
	Source    store.RosterProposalSource `json:"source"`
	Status    store.RosterProposalStatus `json:"status"`
	CreatedAt int64                      `json:"createdAt"`
}

// rosterReviewResponse is GET .../roster-review's reply and the
// rosterReview live topic's value.
type rosterReviewResponse struct {
	Proposals []rosterProposalDTO `json:"proposals"`
}

// createRosterProposalRequest is POST .../roster-review/proposals' body.
// Characters are given by name (or alias), as the roster spells them.
type createRosterProposalRequest struct {
	Kind    rosterFixKind `json:"kind"`
	Subject string        `json:"subject"`
	// Target is the character to merge Subject into (merge only).
	Target string `json:"target,omitempty"`
	// Detail is the alias to remove or split off (remove_alias and
	// split_alias), or the new name (rename, and rename_role - renamed and
	// marked a role).
	Detail   string   `json:"detail,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// maxRosterPairsPerRun bounds one review's LLM calls: the pairs with the
// most lines at stake are read first, and the next run takes up where this
// one stopped (pairs already decided are never asked again).
const maxRosterPairsPerRun = 40

func rosterPairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return "pair:" + a + ":" + b
}

// rosterFixKey is a rule fix's dedupe key: one proposal per alias (removing
// it or splitting it off), and one each per character for its role and its
// validity.
func rosterFixKey(kind rosterFixKind, subjectID, targetID, detail string) string {
	switch kind {
	case speakerattr.RosterMerge:
		return rosterPairKey(subjectID, targetID)
	case speakerattr.RosterRemoveAlias, speakerattr.RosterSplitAlias:
		return "alias:" + subjectID + ":" + strings.ToLower(strings.TrimSpace(detail))
	case speakerattr.RosterMarkRole:
		return "role:" + subjectID
	case speakerattr.RosterRename, speakerattr.RosterRenameRole:
		return "rename:" + subjectID
	default:
		return "invalid:" + subjectID
	}
}

// rosterLineCounts sums each speaker's lines of dialogue over books.
func (s *Server) rosterLineCounts(books []store.Book) (map[string]int, error) {
	total := map[string]int{}
	for _, b := range books {
		counts, err := s.Store.SpeakerLineCounts(b.ID)
		if err != nil {
			return nil, err
		}
		for name, n := range counts {
			total[name] += n
		}
	}
	return total, nil
}

// rosterEntries is scope's valid characters as review entries.
func rosterEntries(chars []store.Character, lines map[string]int) []speakerattr.RosterEntry {
	out := make([]speakerattr.RosterEntry, 0, len(chars))
	for _, c := range chars {
		if c.Invalid {
			continue
		}
		out = append(out, speakerattr.RosterEntry{ID: c.ID, Name: c.Name, Aliases: c.Aliases, Pronouns: c.Pronouns, IsRole: c.IsRole, Lines: lines[c.Name]})
	}
	return out
}

// ReviewRoster is the KindRosterReview task (and all of cmd/rosterreview):
// propose every fix the rules find in book's roster, then take up to
// maxRosterPairsPerRun pairs of names that may be one person and propose a
// merge for each whose second name the book never writes
// (speakerattr.RosterPair.UnwrittenName) or that the text shows to be one
// (speakerattr.Client.JudgeSamePerson; with a semantic index, names the
// series uses alike need less shown - rosterUsage). A pair shown to be
// two people is ruled out - and the alias one has of the other proposed
// for removal; a pair the passages don't settle is ruled out too, unless
// it may yet be settled by how its names are used (rosterUsage.alike's
// outdone).
func (s *Server) ReviewRoster(ctx context.Context, book *store.Book) error {
	scope := store.SeriesScope(book)
	books, err := s.booksInScope(book)
	if err != nil {
		return err
	}
	chars, err := s.Store.ListCharacters(scope)
	if err != nil {
		return err
	}
	lines, err := s.rosterLineCounts(books)
	if err != nil {
		return err
	}
	entries := rosterEntries(chars, lines)
	// The world's own titles ("Prognugator"), read like the fixed ones.
	titles := s.titles(book)
	existing, err := s.Store.ListRosterProposals(scope)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, p := range existing {
		known[p.DedupeKey] = true
	}

	insertFix := func(fix speakerattr.RosterFix) error {
		known[rosterFixKey(fix.Kind, fix.Subject.ID, fix.Target.ID, fix.Detail)] = true
		_, err := s.Store.InsertRosterProposal(store.RosterProposal{
			Scope: scope, Kind: string(fix.Kind), SubjectID: fix.Subject.ID, Subject: fix.Subject.Name,
			TargetID: fix.Target.ID, Target: fix.Target.Name, Detail: fix.Detail, Reason: fix.Reason, Evidence: fix.Evidence,
			Source: store.RosterFromRule, DedupeKey: rosterFixKey(fix.Kind, fix.Subject.ID, fix.Target.ID, fix.Detail),
		})
		return err
	}

	// A first name into the one full name that has it - what dedupe merges
	// outright (shortNameTarget), here a proposal like the rest.
	byID := make(map[string]speakerattr.RosterEntry, len(entries))
	for _, e := range entries {
		byID[e.ID] = e
	}
	for _, c := range chars {
		// Not a rank ("Baron" into "Baron Sanguis"), nor what someone is
		// the something of ("Issa" into "Priest of Issa").
		if t := shortNameTarget(c, chars, titles); t != nil && !speakerattr.IsBareTitle(c.Name, titles) && !strings.Contains(strings.ToLower(t.Name), " of ") {
			if err := insertFix(speakerattr.RosterFix{Kind: speakerattr.RosterMerge, Subject: byID[c.ID], Target: byID[t.ID],
				Reason: fmt.Sprintf("%q is in no other character's name", c.Name)}); err != nil {
				return err
			}
		}
	}

	allPairs := speakerattr.RosterPairs(entries, titles)
	var pairs []speakerattr.RosterPair
	for _, p := range allPairs {
		if known[rosterPairKey(p.A.ID, p.B.ID)] {
			continue
		}
		if pairs = append(pairs, p); len(pairs) == maxRosterPairsPerRun {
			break
		}
	}
	usage := s.newRosterUsage(allPairs, pairs)

	// One pass over the text, a book at a time (a long series doesn't fit
	// comfortably in memory all at once): how each would-be kind word is
	// written, a few passages for each pair, and how the paragraphs
	// writing each of a pair's names read (rosterUsage).
	kinds := speakerattr.NewWordCaseCounts(speakerattr.RosterKindCandidates(entries, titles))
	passages := make([]speakerattr.PairPassages, len(pairs))
	for _, b := range books {
		if err := ctx.Err(); err != nil {
			return err
		}
		all, err := s.Store.ListParagraphsRawForBook(b.ID)
		if err != nil {
			return err
		}
		texts := make([]string, len(all))
		for i, p := range all {
			texts[i] = p.Text
			kinds.Add(p.Text)
		}
		ix := speakerattr.NewIdentityIndex(texts, nil)
		for i, p := range pairs {
			ea, eb := p.Entries()
			found := ix.PairMentions(ea, eb)
			passages[i] = passages[i].Append(ix.Passages(found, ea, eb))
			usage.add(b.ID, all, p, found)
		}
		usage.addBook(b.ID, all, ix)
	}
	// outdone pairs read alike but less so than a rival pairing: left
	// unsettled, they are asked about again once the roster has changed.
	outdone := make([]bool, len(pairs))
	for i, p := range pairs {
		passages[i].Alike, outdone[i] = usage.alike(scope, p)
	}

	for _, fix := range speakerattr.RosterRuleFixes(entries, kinds, titles) {
		if err := insertFix(fix); err != nil {
			return err
		}
	}

	for i, p := range pairs {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := store.RosterProposal{
			Scope: scope, Kind: string(speakerattr.RosterMerge), SubjectID: p.B.ID, Subject: p.B.Name, TargetID: p.A.ID, Target: p.A.Name,
			Source: store.RosterFromLLM, Status: store.RosterRuledOut, DedupeKey: rosterPairKey(p.A.ID, p.B.ID),
		}
		if fix, ok := p.UnwrittenName(passages[i]); ok {
			if err := insertFix(fix); err != nil {
				return err
			}
			continue
		}
		if s.Speaker == nil {
			continue // left for a run with a model
		}
		verdict, evidence := speakerattr.VerdictUnclear, ""
		if passages[i].Empty() {
			row.Reason = "the book has no passage naming both, or none naming each"
		} else if verdict, evidence, err = judgePair(ctx, s.Speaker, book.Title, p, passages[i]); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// One pair's failed call doesn't cost the rest; it is asked
			// again next run.
			log.Printf("httpapi: roster review %s: %q / %q: %v", scope, p.A.Name, p.B.Name, err)
			continue
		}
		switch verdict {
		case speakerattr.VerdictSame:
			row.Status, row.Reason, row.Evidence = store.RosterPending, "the text uses both names for one person", []string{evidence}
			if passages[i].Alike {
				row.Reason = "the book uses both names the same way, for one person"
				if evidence == "" {
					// The model quoted nothing that is in the text: show
					// each name as the book writes it.
					row.Evidence = slices.Concat(firstOf(passages[i].OnlyA), firstOf(passages[i].OnlyB))
					if len(row.Evidence) < 2 {
						row.Evidence = firstOf(passages[i].Both)
					}
				}
			}
		case speakerattr.VerdictDifferent:
			row.Reason, row.Evidence = "the text shows two people", []string{evidence}
			if p.Alias != "" {
				other := p.A
				if other.ID == p.AliasHolder.ID {
					other = p.B
				}
				if _, err := s.Store.InsertRosterProposal(store.RosterProposal{
					// The text shows the two apart, so lines of the one may
					// sit under the other: split, not only remove.
					Scope: scope, Kind: string(speakerattr.RosterSplitAlias), SubjectID: p.AliasHolder.ID, Subject: p.AliasHolder.Name,
					TargetID: other.ID, Target: other.Name, Detail: p.Alias, Reason: other.Name + " is another character",
					Evidence: []string{evidence}, Source: store.RosterFromLLM,
					DedupeKey: rosterFixKey(speakerattr.RosterSplitAlias, p.AliasHolder.ID, "", p.Alias),
				}); err != nil {
					return err
				}
			}
		default:
			if outdone[i] {
				continue
			}
			if row.Reason == "" {
				row.Reason = "the passages don't settle it"
			}
		}
		if _, err := s.Store.InsertRosterProposal(row); err != nil {
			return err
		}
	}
	return nil
}

// firstOf is the first of passages, if there is one.
func firstOf(passages []string) []string {
	return passages[:min(len(passages), 1)]
}

func judgePair(ctx context.Context, c *speakerattr.Client, title string, p speakerattr.RosterPair, found speakerattr.PairPassages) (speakerattr.SamePersonVerdict, string, error) {
	a, b := p.Entries()
	return c.JudgeSamePerson(ctx, title, a, b, found)
}

// rosterProposalApplies reports whether a pending proposal can still be
// applied to the roster as it now stands: its characters still there and
// valid, and the thing it would change not already so.
func rosterProposalApplies(p store.RosterProposal, byID map[string]store.Character) bool {
	subject, ok := byID[p.SubjectID]
	if !ok || subject.Invalid {
		return false
	}
	switch speakerattr.RosterFixKind(p.Kind) {
	case speakerattr.RosterMerge:
		target, ok := byID[p.TargetID]
		return ok && !target.Invalid && target.ID != subject.ID
	case speakerattr.RosterRemoveAlias, speakerattr.RosterSplitAlias:
		return slices.ContainsFunc([]string(subject.Aliases), func(a string) bool { return namekey.Equal(a, p.Detail) })
	case speakerattr.RosterMarkRole:
		return !subject.IsRole
	case speakerattr.RosterInvalidate:
		return true
	case speakerattr.RosterRename, speakerattr.RosterRenameRole:
		if p.Kind == string(speakerattr.RosterRename) && subject.Name == p.Detail ||
			p.Kind == string(speakerattr.RosterRenameRole) && subject.Name == p.Detail && subject.IsRole {
			return false
		}
		// Someone else has the name by now: that is a merge, not this.
		for _, c := range byID {
			if c.ID != subject.ID && !c.Invalid && namekey.Equal(c.Name, p.Detail) {
				return false
			}
		}
		return p.Detail != ""
	}
	return false
}

// rosterReview lists bookID's roster's proposals: the pending ones that
// still apply, the most lines at stake first - or, with all, every row as
// stored (decided and ruled-out ones too), oldest first.
func (s *Server) rosterReview(bookID string, all bool) (rosterReviewResponse, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return rosterReviewResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return rosterReviewResponse{}, httpError(http.StatusNotFound, "book not found")
	}
	scope := store.SeriesScope(book)
	proposals, err := s.Store.ListRosterProposals(scope)
	if err != nil {
		return rosterReviewResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	out := rosterReviewResponse{Proposals: []rosterProposalDTO{}}
	if len(proposals) == 0 {
		return out, nil
	}
	chars, err := s.Store.ListCharacters(scope)
	if err != nil {
		return rosterReviewResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	byID := make(map[string]store.Character, len(chars))
	for _, c := range chars {
		byID[c.ID] = c
	}
	books, err := s.booksInScope(book)
	if err != nil {
		return rosterReviewResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	lines, err := s.rosterLineCounts(books)
	if err != nil {
		return rosterReviewResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	for _, p := range proposals {
		if !all && (p.Status != store.RosterPending || !rosterProposalApplies(p, byID)) {
			continue
		}
		dto := rosterProposalDTO{
			ID: p.ID, Kind: rosterFixKind(p.Kind), SubjectID: p.SubjectID, Subject: p.Subject, TargetID: p.TargetID, Target: p.Target,
			Detail: p.Detail, Reason: p.Reason, Evidence: p.Evidence, Source: p.Source, Status: p.Status, CreatedAt: p.CreatedAt,
		}
		// The roster's current spelling, when the character is still there.
		if c, ok := byID[p.SubjectID]; ok {
			dto.Subject = c.Name
		}
		if c, ok := byID[p.TargetID]; ok {
			dto.Target = c.Name
		}
		dto.SubjectLines, dto.TargetLines = lines[dto.Subject], lines[dto.Target]
		if dto.Evidence == nil {
			dto.Evidence = []string{}
		}
		out.Proposals = append(out.Proposals, dto)
	}
	if !all {
		sort.SliceStable(out.Proposals, func(i, j int) bool { return out.Proposals[i].SubjectLines > out.Proposals[j].SubjectLines })
	}
	return out, nil
}

// buildRosterReview is the rosterReview live topic's value.
func (s *Server) buildRosterReview(bookID string) (rosterReviewResponse, error) {
	return s.rosterReview(bookID, false)
}

// handleGetRosterReview serves GET /api/books/{id}/roster-review - see
// docs/api.md.
func (s *Server) handleGetRosterReview(w http.ResponseWriter, r *http.Request) {
	v, err := s.rosterReview(r.PathValue("id"), r.URL.Query().Get("all") == "1")
	writeBuilt(w, v, err)
}

// handleRunRosterReview serves POST /api/books/{id}/roster-review/run:
// queues a review of the book's roster. The rosterReview live topic carries
// what it proposes.
func (s *Server) handleRunRosterReview(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	s.Jobs.EnqueueRosterReview(book, store.SeriesScope(book), func(ctx context.Context) error {
		return s.ReviewRoster(ctx, book)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// rosterCharacter resolves name to one of chars by name, then by alias,
// ignoring case; nil when none (or an invalid one) has it.
func rosterCharacter(chars []store.Character, name string) *store.Character {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	for i := range chars {
		if !chars[i].Invalid && namekey.Equal(chars[i].Name, name) {
			return &chars[i]
		}
	}
	for i := range chars {
		if !chars[i].Invalid && slices.ContainsFunc([]string(chars[i].Aliases), func(a string) bool { return namekey.Equal(a, name) }) {
			return &chars[i]
		}
	}
	return nil
}

// handleCreateRosterProposal serves POST
// /api/books/{id}/roster-review/proposals: files a proposal from outside
// the review pass, pending like any other. 400 for one that names no
// character of the roster or wouldn't change anything; 409 when the same
// fix was already proposed (whatever became of it).
func (s *Server) handleCreateRosterProposal(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req createRosterProposalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	scope := store.SeriesScope(book)
	chars, err := s.Store.ListCharacters(scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	subject := rosterCharacter(chars, req.Subject)
	if subject == nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("no character named %q in this roster", req.Subject))
		return
	}
	p := store.RosterProposal{
		ID: store.NewID(), Scope: scope, Kind: string(req.Kind), SubjectID: subject.ID, Subject: subject.Name,
		Reason: strings.TrimSpace(req.Reason), Evidence: req.Evidence, Source: store.RosterFromAgent,
	}
	switch req.Kind {
	case speakerattr.RosterMerge:
		target := rosterCharacter(chars, req.Target)
		if target == nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("no character named %q in this roster", req.Target))
			return
		}
		p.TargetID, p.Target = target.ID, target.Name
	case speakerattr.RosterRemoveAlias, speakerattr.RosterSplitAlias, speakerattr.RosterRename, speakerattr.RosterRenameRole:
		p.Detail = strings.Join(strings.Fields(req.Detail), " ")
	case speakerattr.RosterMarkRole, speakerattr.RosterInvalidate:
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown kind %q", req.Kind))
		return
	}
	byID := map[string]store.Character{}
	for _, c := range chars {
		byID[c.ID] = c
	}
	if !rosterProposalApplies(p, byID) {
		writeError(w, http.StatusBadRequest, "that wouldn't change the roster as it stands")
		return
	}
	p.DedupeKey = rosterFixKey(req.Kind, p.SubjectID, p.TargetID, p.Detail)
	inserted, err := s.Store.InsertRosterProposal(p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !inserted {
		writeError(w, http.StatusConflict, "that fix was already proposed")
		return
	}
	writeJSON(w, http.StatusCreated, idResponse{ID: p.ID})
}

// pendingRosterProposal loads the pending proposal a decision route names,
// with the book it was reached through; it writes the error itself.
func (s *Server) pendingRosterProposal(w http.ResponseWriter, r *http.Request) (*store.Book, *store.RosterProposal, bool) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return nil, nil, false
	}
	p, err := s.Store.GetRosterProposal(r.PathValue("proposalId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if p == nil || p.Scope != store.SeriesScope(book) {
		writeError(w, http.StatusNotFound, "proposal not found")
		return nil, nil, false
	}
	if p.Status != store.RosterPending {
		writeError(w, http.StatusConflict, "proposal already decided")
		return nil, nil, false
	}
	return book, p, true
}

// applyRosterProposal makes the change p proposes. A merge keeps the
// merged-away name as the target's alias unless noAlias; an alias is split
// off (splitCharacterAliases) rather than only removed when split.
func (s *Server) applyRosterProposal(book *store.Book, p store.RosterProposal, byID map[string]store.Character, noAlias, split bool) error {
	subject := byID[p.SubjectID]
	switch speakerattr.RosterFixKind(p.Kind) {
	case speakerattr.RosterMerge:
		return s.mergeCharacterInto(book, subject, byID[p.TargetID], !subject.IsRole && !noAlias)
	case speakerattr.RosterRemoveAlias, speakerattr.RosterSplitAlias:
		if split {
			_, err := s.splitCharacterAliases(book, &subject, []string{p.Detail})
			return err
		}
		kept := slices.DeleteFunc(slices.Clone([]string(subject.Aliases)), func(a string) bool { return namekey.Equal(a, p.Detail) })
		return s.Store.SetCharacterAliases(subject.ID, kept)
	case speakerattr.RosterMarkRole:
		return s.Store.SetCharacterRole(subject.ID, true)
	case speakerattr.RosterInvalidate:
		return s.Store.SetCharacterInvalid(subject.ID, true)
	case speakerattr.RosterRename, speakerattr.RosterRenameRole:
		roster := make([]store.Character, 0, len(byID))
		for _, c := range byID {
			roster = append(roster, c)
		}
		if err := s.renameCharacterChecked(book, subject, roster, p.Detail); err != nil {
			return err
		}
		if speakerattr.RosterFixKind(p.Kind) == speakerattr.RosterRenameRole {
			return s.Store.SetCharacterRole(subject.ID, true)
		}
		return nil
	}
	return fmt.Errorf("unknown proposal kind %q", p.Kind)
}

// handleAcceptRosterProposal serves POST
// /api/books/{id}/roster-review/proposals/{proposalId}/accept: makes the
// proposed change. A merge moves the character's lines across the whole
// series, keeps its name as the other's alias (not with ?alias=0, for a
// name that was never theirs) and deletes its voice - it can't be undone.
// An alias is split off - made a character of its own, the subject's lines
// re-judged for theirs (handleSplitCharacterAlias) - for a split_alias
// proposal, or a remove_alias one with ?split=1; ?split=0 only removes it.
// 409 when the proposal was already decided, or no longer
// applies to the roster.
func (s *Server) handleAcceptRosterProposal(w http.ResponseWriter, r *http.Request) {
	book, p, ok := s.pendingRosterProposal(w, r)
	if !ok {
		return
	}
	chars, err := s.Store.ListCharacters(p.Scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := make(map[string]store.Character, len(chars))
	for _, c := range chars {
		byID[c.ID] = c
	}
	if !rosterProposalApplies(*p, byID) {
		writeError(w, http.StatusConflict, "the roster has changed and this no longer applies")
		return
	}
	split := speakerattr.RosterFixKind(p.Kind) == speakerattr.RosterSplitAlias
	if q := r.URL.Query().Get("split"); q != "" {
		split = q == "1"
	}
	if err := s.applyRosterProposal(book, *p, byID, r.URL.Query().Get("alias") == "0", split); err != nil {
		writeBuildError(w, err)
		return
	}
	if err := s.Store.SetRosterProposalStatus(p.ID, store.RosterAccepted); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// handleSkipRosterProposal serves POST
// /api/books/{id}/roster-review/proposals/{proposalId}/skip: declines the
// proposal, which is then never made again.
func (s *Server) handleSkipRosterProposal(w http.ResponseWriter, r *http.Request) {
	_, p, ok := s.pendingRosterProposal(w, r)
	if !ok {
		return
	}
	if err := s.Store.SetRosterProposalStatus(p.ID, store.RosterSkipped); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}
