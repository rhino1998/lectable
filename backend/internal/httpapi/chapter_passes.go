package httpapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// knownCharacterDescriptionMaxChars bounds how much of a known character's
// own tagged description text (see Store.DescriptionsForBooks) gets handed
// to attribution as identifying context - a short blurb, not the whole
// paragraph, since this rides along on every single attribution batch
// (unlike characterization's own much larger sample, gathered once per
// character, ever).
const knownCharacterDescriptionMaxChars = 160

// knownCharacterDescriptions gathers a short identifying blurb for each of
// existing's characters (their own first tagged description paragraph, if
// they have one - see Store.SetParagraphDescriptions/DescriptionsForBooks),
// keyed by name, for AttributeChapter's own "Known characters" prompt (see
// its own doc comment on why this exists): a narration tag often refers to
// someone by role or epithet rather than their proper name ("the captain
// said," "the old orc grunted") - a bare name list gives the model nothing
// to connect that back to who's actually known, but a one-line "what they
// look like/are" blurb often repeats the very role/epithet the narration
// itself uses, closing that gap. A character with no tagged description
// yet (most of them, especially early in a chapter, or a book where
// description-tagging hasn't run) is simply absent from the result -
// AttributeChapter treats that the same as "no blurb available," not an
// error.
func (s *Server) knownCharacterDescriptions(book *store.Book, existing []store.Character) (map[string]string, error) {
	if len(existing) == 0 {
		return nil, nil
	}
	books, err := s.booksInScope(book)
	if err != nil {
		return nil, err
	}
	bookIDs := make([]string, len(books))
	for i := range books {
		bookIDs[i] = books[i].ID
	}
	out := make(map[string]string, len(existing))
	for _, c := range existing {
		descs, err := s.Store.DescriptionsForBooks(bookIDs, c.Name, 1)
		if err != nil {
			return nil, err
		}
		if len(descs) == 0 {
			continue
		}
		out[c.Name] = truncateForPrompt(descs[0].Text, knownCharacterDescriptionMaxChars)
	}
	return out, nil
}

// truncateForPrompt trims s to at most maxChars runes for embedding in an
// LLM prompt, breaking at the last whitespace before the cutoff when
// possible so it doesn't end mid-word, and appending "..." when actually
// truncated.
func truncateForPrompt(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	cut := string(r[:maxChars])
	if i := strings.LastIndexAny(cut, " \t\n"); i > 0 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "..."
}

// invalidSpeakers is the set of names marked invalid (store.Character.
// Invalid) in a scope - matched case-insensitively, since the model
// re-emitting a rejected name with different casing ("he" vs "He") is
// exactly the kind of near-miss this exists to catch - except that a
// valid character's own exact name always wins over a case-folded
// invalid match.
type invalidSpeakers struct {
	folded map[string]bool
	valid  map[string]bool
}

// splitInvalidCharacters partitions existing into the characters an LLM
// pass may still offer as "Known characters" and the invalidSpeakers set
// its results must be checked against, so an invalid name is neither
// suggested to the model nor accepted back from it.
func splitInvalidCharacters(existing []store.Character) ([]store.Character, invalidSpeakers) {
	inv := invalidSpeakers{folded: map[string]bool{}, valid: map[string]bool{}}
	valid := make([]store.Character, 0, len(existing))
	for _, c := range existing {
		if c.Invalid {
			inv.folded[strings.ToLower(c.Name)] = true
			continue
		}
		inv.valid[c.Name] = true
		valid = append(valid, c)
	}
	return valid, inv
}

func (inv invalidSpeakers) has(name string) bool {
	return !inv.valid[name] && inv.folded[strings.ToLower(name)]
}

// attributeChapter runs speaker attribution (internal/speakerattr) over
// ch's paragraphs and persists the results: every newly-seen character
// name is registered (store.Store.UpsertCharacter, scoped to book's whole
// series - see store.SeriesScope - starting with no voice assigned) and
// every paragraph's Speaker is set. known is re-fetched from the store on
// every call (not cached across chapters or books) so a character
// discovered anywhere earlier in the series - not just earlier in this
// book - is always offered to the model as a name to reuse, keeping
// naming consistent across the whole series (see
// speakerattr.Client.AttributeChapter). Unlike an earlier version, this no
// longer characterizes or assigns a voice to newly-discovered characters
// itself - that now happens lazily, the first time a character's voice is
// actually needed for generation (see jobs.Manager.
// provisionMissingCharacterVoices/CharacterVoiceProvisioner,
// provisionCharacterVoice below), so a chapter with many brand-new
// characters attributes just as fast as one with none.
//
// onlyUnattributed restricts the paragraphs actually sent to the model to
// ones with no Speaker yet, instead of every paragraph in the chapter -
// false for every reader-triggered call (handleAttributeSpeakers: an
// explicit "Attribute speakers"/"Attribute all" click always means
// "redo the whole chapter," the same idempotent refresh it's always been,
// even for paragraphs already attributed - see SpeakersPage's own comment
// on that button), true only when this call is itself the automatic
// continuation of an earlier one that paused partway through (see
// speakerattr.Client.AttributeChapter's shouldPause and the requeue
// closure built below) - by the time that continuation runs, everything
// the first attempt actually finished is already persisted, so redoing it
// would just waste LLM calls repeating identical work.
//
// This satisfies AttributionFunc's shape directly (see jobs.Manager.
// EnqueueAttribution): requeue is non-nil exactly when speakerattr paused
// before reaching every paragraph, and calling it enqueues exactly this
// same continuation.
func (s *Server) attributeChapter(ctx context.Context, book *store.Book, ch *store.Chapter, onlyUnattributed bool) (attributed int, requeue func(), err error) {
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	paragraphs := all
	if onlyUnattributed {
		paragraphs = make([]store.Paragraph, 0, len(all))
		for _, p := range all {
			if p.Speaker == "" {
				paragraphs = append(paragraphs, p)
			}
		}
	}
	if len(paragraphs) == 0 {
		return 0, nil, nil
	}

	scope := store.SeriesScope(book)
	roster, err := s.Store.ListCharacters(scope)
	if err != nil {
		return 0, nil, err
	}
	existing, invalid := splitInvalidCharacters(roster)
	known := make([]string, len(existing))
	knownRoles := make(map[string]bool, len(existing))
	knownAliases := make(map[string][]string, len(existing))
	knownPronouns := make(map[string]string, len(existing))
	for i, c := range existing {
		known[i] = c.Name
		if c.IsRole {
			knownRoles[c.Name] = true
		}
		if len(c.Aliases) > 0 {
			knownAliases[c.Name] = c.Aliases
		}
		if c.Pronouns != "" {
			knownPronouns[c.Name] = c.Pronouns
		}
	}
	knownDescriptions, err := s.knownCharacterDescriptions(book, existing)
	if err != nil {
		return 0, nil, err
	}

	inputs := make([]speakerattr.ParagraphInput, len(paragraphs))
	for i, p := range paragraphs {
		// p.Inline (see store.Paragraph.Inline) means "continues the
		// immediately preceding paragraph in the chapter" - true only
		// meaningfully when that true predecessor (idx-1) is itself part
		// of inputs right next to it; onlyUnattributed can filter that
		// predecessor out (already attributed, and so already persisted,
		// in an earlier batch/dispatch of this same paused-and-resumed
		// run) while leaving this one in, which would otherwise silently
		// tell speakerattr.attributeBatch's "Lines:" formatting that this
		// paragraph continues whatever unrelated paragraph now happens to
		// precede it in the filtered list instead of starting a new
		// visible line - corrupting the model's own paragraph-boundary
		// signal for every batch after the first gap. Force it false
		// whenever the true predecessor isn't right there; a no-op for
		// the normal onlyUnattributed=false path, where paragraphs is
		// exactly all and every idx-1 predecessor is always present.
		inline := p.Inline
		if inline && (i == 0 || paragraphs[i-1].Idx != p.Idx-1) {
			inline = false
		}
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: inline, IsQuote: p.IsQuote && !p.ScareQuote}
	}

	// nil, not s.Jobs.HasHigherPriorityWork: every LLM pass (attribution,
	// direction, sfx, pronunciation, music) now runs each dispatched batch
	// through to completion instead of yielding mid-run - see this
	// package's own removal-of-pausing note by the direction/sfx/
	// pronunciation call site in directChapter for the full reasoning
	// (a whole-book preprocess phase dispatches each chapter's own pass
	// exactly once, with no automatic retry loop of its own, so a pause
	// under real contention could leave most of a bulk run's chapters
	// silently stuck half-done with nothing left in the queue to ever
	// pick them back up).
	attrRoster := speakerattr.Roster{Names: known, Descriptions: knownDescriptions, Roles: knownRoles, Aliases: knownAliases, Pronouns: knownPronouns, Prominent: s.prominentSpeakers(book.ID, ch.Idx, known)}
	speakers, roleNames, pronouns, remaining, attrErr := s.Speaker.AttributeChapter(ctx, book.Title, ch.Title, attrRoster, inputs, nil)

	paragraphByIdx := make(map[int]store.Paragraph, len(all))
	for _, p := range all {
		paragraphByIdx[p.Idx] = p
	}

	bySpeakerIdx := make(map[int]string, len(speakers))
	for idx, name := range speakers {
		if name == "" {
			continue
		}
		// Only an actual quoted-dialogue span may be attributed to a
		// character - narration/description about someone isn't them
		// speaking, whatever the model itself decided, and neither is a
		// quoted span already flagged as a scare quote (see
		// Store.SetParagraphScareQuotes, which forces those to Narrator
		// too). Enforced here, deterministically, rather than left to the
		// model's own prompt adherence (see speakerattr's own system
		// prompt, which already asks for this but can't guarantee it).
		if name != "Narrator" {
			if p, ok := paragraphByIdx[idx]; ok && (!p.IsQuote || p.ScareQuote) {
				name = "Narrator"
			}
		}
		// A name marked invalid is never (re)assigned or (re)created -
		// the line is still real dialogue, just not theirs, so it lands
		// on Unknown the same way handleDeleteCharacter's does.
		if invalid.has(name) || speakerattr.IsGroupSpeaker(name) {
			name = "Unknown"
		}
		bySpeakerIdx[idx] = name
		if name == "Narrator" || name == "Unknown" {
			continue
		}
		char, _, uerr := s.Store.UpsertCharacter(scope, name, roleNames[name])
		if uerr != nil {
			return 0, nil, uerr
		}
		// Attribution only fills pronouns in - an established value (an
		// earlier chapter's, or a reader's edit) always wins.
		if p := pronouns[name]; p != "" && char.Pronouns == "" && !char.IsRole {
			if perr := s.Store.SetCharacterPronouns(char.ID, p); perr != nil {
				return 0, nil, perr
			}
			delete(pronouns, name)
		}
	}
	// Persisted regardless of attrErr below - a batch failing partway
	// through a chapter (a real, observed failure mode: a small local
	// model occasionally blows its whole token budget on an unclosed
	// <think> trace and never produces JSON) shouldn't discard whatever
	// earlier batches in *this same call* actually did succeed at.
	if serr := s.Store.SetParagraphSpeakers(ch.ID, bySpeakerIdx); serr != nil {
		return 0, nil, serr
	}

	if attrErr != nil {
		// A genuine failure, not a pause - surfaced to the caller (logged
		// by jobs.Manager.handleResult) rather than auto-requeued, so a
		// batch that keeps failing the same way doesn't loop forever;
		// a reader can always retry explicitly.
		return len(bySpeakerIdx), nil, attrErr
	}
	if len(remaining) > 0 {
		requeue = func() {
			s.Jobs.EnqueueAttribution(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
				return s.attributeChapter(ctx, book, ch, true)
			})
		}
		return len(bySpeakerIdx), requeue, nil
	}
	// A full, uninterrupted run over the whole chapter just finished -
	// advance its pipeline progress persistently (see store.Passes' own
	// doc comment for why this is a stored fact, not derived from
	// paragraphs.speaker counts). Best-effort: a failure here shouldn't
	// undo the attribution work itself (bySpeakerIdx is already committed
	// above), just log rather than fail the call outright.
	if serr := s.Store.SetChapterAttributed(ch.ID); serr != nil {
		log.Printf("attributeChapter: could not advance chapter %s passes: %v", ch.ID, serr)
	}

	return len(bySpeakerIdx), nil, nil
}

// reattributeChapterSpeaker re-judges targetIdx - the paragraphs in ch
// currently credited to excludeName, the speaker "Auto Split" is trying
// to eliminate (see handleReattributeSpeaker) - and persists only those.
// No other paragraph in ch is touched, or even asked about:
//
//  1. A target that isn't real dialogue (narration, a scare quote) is
//     simply Narrator.
//  2. A target with an explicit speech tag naming someone else ("…,”
//     Bert said") takes that name - speakerattr.RefineWithSpeechTags, no
//     model call.
//  3. Every other target goes to the model in a window of surrounding
//     paragraphs centred on it, with only the targets marked to answer,
//     widening each round (speakerattr.Client.AttributeWithWideningContext)
//     until the answer isn't a rejected name. Targets close together
//     share a window.
//
// This used to re-run attribution over the whole chapter (every batch,
// every line, results discarded for everything but targetIdx) and only
// then widen around the leftovers - for a speaker with a handful of lines
// in a long chapter, most of that work was thrown away.
//
// excludeName is withheld from "Known characters", so the model has no
// established identity to reuse for it, and never accepted back for a
// target (nor any invalid or group name): a target still rejected at the
// widest window lands on "Unknown".
//
// Satisfies jobs.ReattributionFunc's shape directly (see
// jobs.Manager.EnqueueReattribution); it never pauses, so requeue is
// always nil.
func (s *Server) reattributeChapterSpeaker(ctx context.Context, book *store.Book, ch *store.Chapter, speakerKey, excludeName string, targetIdx map[int]bool) (reattributed int, requeue func(), err error) {
	if len(targetIdx) == 0 {
		return 0, nil, nil
	}
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}

	scope := store.SeriesScope(book)
	roster, err := s.Store.ListCharacters(scope)
	if err != nil {
		return 0, nil, err
	}
	existing, invalid := splitInvalidCharacters(roster)
	known := make([]string, 0, len(existing))
	knownRoles := make(map[string]bool, len(existing))
	knownAliases := make(map[string][]string, len(existing))
	knownPronouns := make(map[string]string, len(existing))
	for _, c := range existing {
		if c.Name == excludeName {
			continue
		}
		known = append(known, c.Name)
		if c.IsRole {
			knownRoles[c.Name] = true
		}
		if len(c.Aliases) > 0 {
			knownAliases[c.Name] = c.Aliases
		}
		if c.Pronouns != "" {
			knownPronouns[c.Name] = c.Pronouns
		}
	}
	knownDescriptions, err := s.knownCharacterDescriptions(book, existing)
	if err != nil {
		return 0, nil, err
	}
	delete(knownDescriptions, excludeName)

	inputs := make([]speakerattr.ParagraphInput, len(all))
	paragraphByIdx := make(map[int]store.Paragraph, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote}
		paragraphByIdx[p.Idx] = p
	}
	isDialogue := func(idx int) bool {
		p, ok := paragraphByIdx[idx]
		return ok && p.IsQuote && !p.ScareQuote
	}
	rejected := func(name string) bool {
		return name == "" || name == excludeName || invalid.has(name) || speakerattr.IsGroupSpeaker(name)
	}
	resolver := speakerattr.NewSpeakerNameResolver(known, knownAliases)
	// tagFix runs the speech-tag rules over every dialogue line's current
	// speaker (targets as given, the rest as stored - context for the
	// continuation rule) and returns what it says about the targets.
	tagFix := func(targetSpeakers map[int]string) map[int]string {
		current := make(map[int]string, len(all))
		for _, p := range all {
			if isDialogue(p.Idx) {
				current[p.Idx] = p.Speaker
			}
		}
		for idx, name := range targetSpeakers {
			current[idx] = name
		}
		refined, _ := speakerattr.RefineWithSpeechTags(inputs, current, resolver)
		out := make(map[int]string, len(targetSpeakers))
		for idx := range targetSpeakers {
			out[idx] = refined[idx]
		}
		return out
	}

	speakers := make(map[int]string, len(targetIdx))
	roleNames := map[string]bool{}
	pending := make(map[int]string)
	for idx := range targetIdx {
		if isDialogue(idx) {
			pending[idx] = excludeName
		} else {
			speakers[idx] = "Narrator"
		}
	}
	var retry []int
	for idx, name := range tagFix(pending) {
		if rejected(name) {
			retry = append(retry, idx)
		} else {
			speakers[idx] = name
		}
	}
	sort.Ints(retry)
	var attrErr error
	if len(retry) > 0 {
		attrRoster := speakerattr.Roster{Names: known, Descriptions: knownDescriptions, Roles: knownRoles, Aliases: knownAliases, Pronouns: knownPronouns, Prominent: s.prominentSpeakers(book.ID, ch.Idx, known)}
		widened, widenedRoles, werr := s.Speaker.AttributeWithWideningContext(ctx, book.Title, ch.Title, attrRoster, inputs, retry, rejected)
		if werr != nil {
			attrErr = fmt.Errorf("auto split %q: chapter %d: %w", excludeName, ch.Idx, werr)
		}
		for name := range widenedRoles {
			roleNames[name] = true
		}
		// Whatever the widening pass settled, run the tag rules once more
		// over it (a newly settled line can now anchor a continuation).
		settled := make(map[int]string, len(retry))
		for _, idx := range retry {
			if name, ok := widened[idx]; ok {
				settled[idx] = name
			} else if werr == nil {
				settled[idx] = "Unknown" // rejected even at the widest window
			}
		}
		for idx, name := range tagFix(settled) {
			speakers[idx] = name
		}
	}

	bySpeakerIdx := make(map[int]string, len(speakers))
	for idx, name := range speakers {
		if name != "Narrator" && !isDialogue(idx) {
			name = "Narrator"
		}
		if name != "Narrator" && rejected(name) {
			name = "Unknown"
		}
		bySpeakerIdx[idx] = name
		if name == "Narrator" || name == "Unknown" {
			continue
		}
		if _, _, uerr := s.Store.UpsertCharacter(scope, name, roleNames[name]); uerr != nil {
			return 0, nil, uerr
		}
	}
	// Persisted regardless of attrErr - keep whatever actually succeeded;
	// a target the failed widening never reached keeps excludeName, so a
	// retry of this same Auto Split picks it up again.
	if serr := s.Store.SetParagraphSpeakers(ch.ID, bySpeakerIdx); serr != nil {
		return 0, nil, serr
	}
	return len(bySpeakerIdx), nil, attrErr
}

// describeChapter runs speakerattr.Client.DescribeChapter over every
// paragraph in chapter and persists which ones describe a character
// (Store.SetParagraphDescriptions), upserting any newly-discovered
// character it names along the way - the same deterministic is_quote gate
// attributeChapter already applies to Speaker applies here to Describes,
// since narration *about* someone isn't the same as them speaking, whatever
// the model output. The body of describeChapterForJob (a
// jobs.KindDescription task). Any error returned is specifically DescribeChapter's own (a genuine LLM/
// parse failure, not a pause - see its own doc comment) or a persistence
// failure; per-character UpsertCharacter failures along the way are
// individually logged and skipped rather than failing the whole call, the
// same best-effort-within-a-best-effort treatment attributeChapter's own
// speaker-upsert loop doesn't need (a single UpsertCharacter failure there
// already fails outright, but this call site never blocks anything more
// important on getting every single character registered).
func (s *Server) describeChapter(ctx context.Context, book *store.Book, ch *store.Chapter, all []store.Paragraph) error {
	scope := store.SeriesScope(book)
	roster, err := s.Store.ListCharacters(scope)
	if err != nil {
		return fmt.Errorf("list characters: %w", err)
	}
	existing, invalid := splitInvalidCharacters(roster)
	known := make([]string, len(existing))
	for i, c := range existing {
		known[i] = c.Name
	}

	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline}
	}

	descriptions, _, describeErr := s.Speaker.DescribeChapter(ctx, book.Title, ch.Title, known, inputs, nil)
	// Persisted below regardless of describeErr - descriptions still holds
	// whatever batches completed before a failing one, same "persist what
	// actually succeeded" treatment attributeChapter gives a partial
	// AttributeChapter error; describeErr itself is still returned at the
	// end, after that persistence, so a caller sees the failure without
	// losing the real progress this call already made.

	paragraphByIdx := make(map[int]store.Paragraph, len(all))
	for _, p := range all {
		paragraphByIdx[p.Idx] = p
	}

	byDescribesIdx := make(map[int][]string, len(descriptions))
	for idx, names := range descriptions {
		// Only an actual narration paragraph may describe someone - the
		// same is_quote gate attributeChapter applies to Speaker, and for
		// the same reason: a quoted-dialogue span can't simultaneously be
		// a description of someone else, whatever the model output. No
		// found-paragraph fallback trusting the model: a description is
		// only ever kept when this paragraph is positively confirmed
		// non-quote.
		p, found := paragraphByIdx[idx]
		if !found || p.IsQuote {
			continue
		}
		// An invalid name is dropped outright rather than remapped -
		// there's no "Unknown" equivalent for who a description is about.
		kept := make([]string, 0, len(names))
		for _, name := range names {
			if !invalid.has(name) {
				kept = append(kept, name)
			}
		}
		names = kept
		if len(names) == 0 {
			continue
		}
		byDescribesIdx[idx] = names
		for _, name := range names {
			// A description can register a character not yet in scope -
			// see DescribeChapter's own doc comment: a character can be
			// discovered by how they're described before they ever speak.
			if _, _, uerr := s.Store.UpsertCharacter(scope, name, false); uerr != nil {
				log.Printf("describeChapter: upsert character %q for chapter %s: %v", name, ch.ID, uerr)
			}
		}
	}
	if serr := s.Store.SetParagraphDescriptions(ch.ID, byDescribesIdx); serr != nil {
		return fmt.Errorf("persist descriptions: %w", serr)
	}
	return describeErr
}

// scareQuoteChapter runs speakerattr.Client.ScareQuoteChapter over every
// quoted paragraph in chapter and persists which ones are scare quotes
// (Store.SetParagraphScareQuotes) - describeChapter's own sibling, the
// body of scareQuoteChapterForJob (a jobs.KindScareQuote task).
//
// Invalidates every paragraph in the inline paragraph set (see
// expandToInlineSets) of any paragraph whose own ScareQuote flag actually
// changed this run - never the whole chapter, unlike directChapter's own
// blunt "tagged anything -> invalidate everything" choice for delivery
// tags, but not narrower than that either: see expandToInlineSets' own
// doc comment for why a changed paragraph's Inline neighbors need
// invalidating too, not just the paragraph itself.
func (s *Server) scareQuoteChapter(ctx context.Context, book *store.Book, ch *store.Chapter, all []store.Paragraph) error {
	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote}
	}

	scareQuotes, _, scareQuoteErr := s.Speaker.ScareQuoteChapter(ctx, book.Title, ch.Title, inputs, nil)
	// Persisted below regardless of scareQuoteErr, same "keep whatever
	// batches actually completed" treatment describeChapter gives its own
	// partial-failure case.

	byIdx := make(map[int]bool, len(scareQuotes))
	var changed []store.Paragraph
	for _, p := range all {
		want := scareQuotes[p.Idx]
		if want == p.ScareQuote {
			continue
		}
		byIdx[p.Idx] = want
		p.ScareQuote = want
		changed = append(changed, p)
	}
	if serr := s.Store.SetParagraphScareQuotes(ch.ID, byIdx); serr != nil {
		return fmt.Errorf("persist scare quotes: %w", serr)
	}
	s.invalidateParagraphAudio(book, s.expandToInlineSets(ch.ID, changed))
	return scareQuoteErr
}

// expandToInlineSets expands paragraphs to the union (deduped by
// paragraph ID) of each one's own inline paragraph set
// (jobs.Manager.InlineParagraphSet) - the full run reachable by following
// Inline in either direction.
//
// A ScareQuote flag changing on any one paragraph can change whether its
// whole Inline-linked run is eligible for scare-quote merging (see
// jobs.Manager.scareQuoteMergeGroup), so every member's audio needs
// invalidating, not just the paragraph whose flag actually changed.
// Growing a merge group self-heals without this - handleMergedResult
// rewrites every member's audio the next time the changed paragraph is
// dispatched, whatever their prior status - but *shrinking* one doesn't:
// once the changed paragraph regenerates alone (its flag no longer keeps
// the group eligible), its former merge-mates are left exactly as they
// were - the anchor still holding the old, now-wrong multi-paragraph
// recording on disk with `AudioReady` status, a former pointer member
// still pointing into it - and neither is ever re-examined, since nothing
// else marks an already-`AudioReady` paragraph as needing generation
// again.
func (s *Server) expandToInlineSets(chapterID string, paragraphs []store.Paragraph) []store.Paragraph {
	seen := make(map[string]bool, len(paragraphs))
	var out []store.Paragraph
	for _, p := range paragraphs {
		for _, member := range s.Jobs.InlineParagraphSet(chapterID, p) {
			if seen[member.ID] {
				continue
			}
			seen[member.ID] = true
			out = append(out, member)
		}
	}
	return out
}

// invalidateParagraphAudio resets each of paragraphs' own already-generated
// audio (if any) back to pending under its own currently-resolved voice,
// removing its on-disk file too - scareQuoteChapter's own scoped
// invalidation and handleSetParagraphScareQuote's manual-toggle
// counterpart. Best-effort: a resolution/delete failure for one paragraph
// is logged and skipped rather than failing the whole batch, since this
// always runs after the real state change (the ScareQuote flag itself) has
// already been persisted - a paragraph whose stale audio couldn't be
// cleared here just means a reader has to notice and hit "regenerate"
// themselves, not a lost update.
func (s *Server) invalidateParagraphAudio(book *store.Book, paragraphs []store.Paragraph) {
	for _, p := range paragraphs {
		voice, err := s.Narration.ForParagraph(book, p.Speaker)
		if err != nil {
			log.Printf("invalidateParagraphAudio: resolve voice for paragraph %s: %v", p.ID, err)
			continue
		}
		voiceID := voice.VoiceID()
		if err := audiopath.RemoveClip(audiopath.ParagraphFile(s.DataDir, book.ID, p.ChapterID, voiceID, p.Idx)); err != nil {
			log.Printf("invalidateParagraphAudio: remove audio file for paragraph %s: %v", p.ID, err)
		}
		if err := s.Store.ResetParagraphAudio(p.ID, voiceID); err != nil {
			log.Printf("invalidateParagraphAudio: reset paragraph %s: %v", p.ID, err)
		}
	}
}

// directChapter runs the emotion pass (speakerattr.Client.EmotionChapter)
// over chapter's paragraphs and persists each dialogue line's label
// (Store.SetParagraphEmotions) - "direction" in every name here (the
// KindSpeechDirection task, Passes.Direction, handleTagDirections) is
// kept from the Higgs delivery-tag passes this replaced. Runs under any
// clone model: an emotion reaches TTS as an emotion-variant reference
// clip (internal/voicerefs.EnsureVariantFile, cloned from at generation
// time by jobs.Manager.generate), not as model-specific markup. Never
// chained automatically after attributeChapter - it's independently
// triggered (handleTagDirections, the preprocess direction phase, or
// jobs.Manager.speechDirectionDependency when the book's SpeechDirection
// is on), and needs nothing from attribution.
//
// Every processed paragraph's stored label is rewritten - a line the model
// now calls neutral has an old label cleared - but only paragraphs whose
// *effective* emotion (store.Paragraph.EffectiveEmotion) actually changed,
// or that still carry legacy Higgs tags from before this pass existed
// (Store.ClearLegacyTags), get their audio invalidated.
//
// onlyIdx restricts the paragraphs actually sent to the model to those
// whose Idx is in the set - nil for every reader-triggered call (a whole-
// chapter run), non-nil only for the automatic continuation of an earlier
// run that stopped partway (see the requeue closure below). It can't be
// re-derived from stored state: a neutral line is indistinguishable from
// one never checked.
//
// shouldPause is nil, not s.Jobs.HasHigherPriorityWork: a whole-book
// preprocess phase dispatches each chapter's pass exactly once with no
// retry loop of its own, and under contention most chapters would pause
// after their first batch and never be picked up again.
func (s *Server) directChapter(ctx context.Context, book *store.Book, ch *store.Chapter, onlyIdx map[int]bool) (labeled int, requeue func(), err error) {
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	paragraphs := all
	if onlyIdx != nil {
		paragraphs = make([]store.Paragraph, 0, len(onlyIdx))
		for _, p := range all {
			if onlyIdx[p.Idx] {
				paragraphs = append(paragraphs, p)
			}
		}
	}
	if len(paragraphs) == 0 {
		return 0, nil, nil
	}

	inputs := make([]speakerattr.ParagraphInput, len(paragraphs))
	for i, p := range paragraphs {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote}
	}
	labels, remaining, labelErr := s.Speaker.EmotionChapter(ctx, book.Title, ch.Title, inputs, nil)

	// Persisted regardless of labelErr - a batch failing partway shouldn't
	// discard what earlier batches in this same call labeled.
	notReached := make(map[int]bool, len(remaining))
	for _, p := range remaining {
		notReached[p.Idx] = true
	}
	updates := make(map[int]string)
	var changed []store.Paragraph
	for _, p := range paragraphs {
		if notReached[p.Idx] {
			continue
		}
		label := labels[p.Idx]
		if label == p.Emotion {
			continue
		}
		updates[p.Idx] = label
		before := p.EffectiveEmotion()
		p.Emotion = label
		if p.EffectiveEmotion() != before {
			changed = append(changed, p)
		}
	}
	if serr := s.Store.SetParagraphEmotions(ch.ID, updates); serr != nil {
		return 0, nil, serr
	}
	// Audio generated under the Higgs tag passes this replaced still has
	// their emotion/sfx/pause tags baked in, even where the emotion label
	// itself didn't change - invalidate those lines too (once: the legacy
	// tags are cleared as they're found).
	if legacy, lerr := s.Store.ClearLegacyTags(ch.ID); lerr != nil {
		log.Printf("directChapter: clear legacy tags for chapter %s: %v", ch.ID, lerr)
	} else if len(legacy) > 0 {
		inChanged := make(map[int]bool, len(changed))
		for _, p := range changed {
			inChanged[p.Idx] = true
		}
		byIdx := make(map[int]store.Paragraph, len(all))
		for _, p := range all {
			byIdx[p.Idx] = p
		}
		for _, idx := range legacy {
			if p, ok := byIdx[idx]; ok && !inChanged[idx] {
				if e, ok := updates[idx]; ok {
					p.Emotion = e
				}
				changed = append(changed, p)
			}
		}
	}
	if len(changed) > 0 {
		// Expanded to scare-quote merge groups: a member's text changing
		// makes the group's shared clip stale.
		s.invalidateParagraphAudio(book, s.expandToInlineSets(ch.ID, changed))
	}

	if labelErr != nil {
		// A genuine failure, not a pause - don't mark the chapter directed
		// yet, so a later re-run doesn't skip it.
		return len(labels), nil, labelErr
	}
	if len(remaining) > 0 {
		remainingIdx := make(map[int]bool, len(remaining))
		for _, p := range remaining {
			remainingIdx[p.Idx] = true
		}
		requeue = func() {
			s.Jobs.EnqueueDirection(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
				return s.directChapter(ctx, book, ch, remainingIdx)
			})
		}
		return len(labels), requeue, nil
	}
	// A full, uninterrupted run over the whole chapter (possibly across
	// several continuations) - best-effort, since the labels themselves are
	// already committed.
	if serr := s.Store.SetChapterDirected(ch.ID); serr != nil {
		log.Printf("directChapter: could not advance chapter %s passes: %v", ch.ID, serr)
	}
	return len(labels), nil, nil
}

// pronounceChapter runs speakerattr.Client.ResolvePronunciation
// (ambiguous-abbreviation disambiguation, e.g. "Dr." -> "Doctor" vs
// "Drive") over chapter's paragraphs and persists the substitutions
// (Store.SetParagraphPronunciation) - its own pass (store.Passes.
// Pronunciation, jobs.KindPronunciation), split out of directChapter: a
// plain word substitution reads correctly under every clone model, so
// unlike direction tagging it isn't gated on Higgs. Invalidates the
// generated audio of every paragraph it finds or clears a substitution in (and
// their scare-quote merge groups - see Store.DeleteParagraphAudioForIdxs),
// and marks
// Passes.Pronunciation only on a full, uninterrupted run - directChapter's
// exact contract, including onlyIdx's meaning (non-nil only for a paused
// run's own continuation).
func (s *Server) pronounceChapter(ctx context.Context, book *store.Book, ch *store.Chapter, onlyIdx map[int]bool) (resolved int, requeue func(), err error) {
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	paragraphs := all
	if onlyIdx != nil {
		paragraphs = make([]store.Paragraph, 0, len(onlyIdx))
		for _, p := range all {
			if onlyIdx[p.Idx] {
				paragraphs = append(paragraphs, p)
			}
		}
	}
	if len(paragraphs) == 0 {
		return 0, nil, nil
	}
	inputs := make([]speakerattr.ParagraphInput, len(paragraphs))
	for i, p := range paragraphs {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote}
	}

	pronunciation, remaining, resolveErr := s.Speaker.ResolvePronunciation(ctx, book.Title, ch.Title, inputs, nil)

	// Every paragraph this run actually reached gets its stored
	// substitutions replaced - including clearing a stale list the new
	// pass no longer produces (ResolvePronunciation omits paragraphs with
	// nothing to substitute). Only a paragraph whose substitutions
	// actually changed loses its audio; one resolved to the same list it
	// already had keeps it.
	unreached := make(map[int]bool, len(remaining))
	for _, p := range remaining {
		unreached[p.Idx] = true
	}
	updates := make(map[int][]pronounce.Substitution, len(paragraphs))
	var touched []int
	for _, p := range paragraphs {
		if unreached[p.Idx] {
			continue
		}
		subs := pronunciation[p.Idx]
		if !slices.Equal(subs, []pronounce.Substitution(p.Pronunciation)) {
			updates[p.Idx] = subs
			touched = append(touched, p.Idx)
		}
	}
	// Persisted regardless of any error - see directChapter's own "persist
	// what succeeded" note.
	if serr := s.Store.SetParagraphPronunciation(ch.ID, updates); serr != nil {
		return 0, nil, serr
	}
	if len(touched) > 0 {
		refs, derr := s.Store.DeleteParagraphAudioForIdxs(book.ID, ch.ID, touched)
		if derr != nil {
			log.Printf("pronounceChapter: invalidate audio for chapter %s: %v", ch.ID, derr)
		}
		for _, ref := range refs {
			if rerr := audiopath.RemoveClip(s.paragraphAudioPath(ref.BookID, ref.ChapterID, ref.VoiceID, ref.Idx)); rerr != nil {
				log.Printf("pronounceChapter: remove stale audio file: %v", rerr)
			}
		}
	}
	if resolveErr != nil {
		return len(pronunciation), nil, resolveErr
	}
	if len(remaining) > 0 {
		remainingIdx := make(map[int]bool, len(remaining))
		for _, p := range remaining {
			remainingIdx[p.Idx] = true
		}
		requeue = func() {
			s.Jobs.EnqueuePronunciation(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
				return s.pronounceChapter(ctx, book, ch, remainingIdx)
			})
		}
		return len(pronunciation), requeue, nil
	}
	if serr := s.Store.SetChapterPronounced(ch.ID); serr != nil {
		log.Printf("pronounceChapter: could not advance chapter %s passes: %v", ch.ID, serr)
	}
	return len(pronunciation), nil, nil
}

// pronounceChapterForJob matches jobs.ChapterPronouncer's signature, wired
// in as such from NewRouter so jobs.Manager.pronunciationDependency can
// resolve a chapter's pronunciation lazily ahead of generating it -
// directChapterForJob's exact counterpart.
func (s *Server) pronounceChapterForJob(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	return s.pronounceChapter(ctx, book, ch, nil)
}

// directChapterForJob matches jobs.ChapterDirector's signature, wired in as
// such from NewRouter so jobs.Manager (speechDirectionBlockerFor) can
// direction-tag a chapter lazily the moment a clone/design task first
// discovers it isn't tagged yet, rather than only ever via an explicit
// "Tag directions" click (handleTagDirections). onlyIdx is always nil - a
// fresh, full-chapter run, never itself a paused continuation - directChapter's
// own pause/resume handling (its own requeue closure, re-invoking
// EnqueueDirection directly) works identically regardless of who queued
// the task it's running as.
func (s *Server) directChapterForJob(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	return s.directChapter(ctx, book, ch, nil)
}

// attributeChapterForJob matches jobs.ChapterAttributor's signature, wired
// in as such from NewRouter so jobs.Manager (attributionOrderDependency)
// can attribute an earlier, not-yet-attributed chapter lazily the moment a
// later chapter in the same book first discovers it needs to wait on it -
// see attributionOrderDependency's own doc comment for why chapter order
// matters here, rather than only ever via an explicit "Attribute speakers"
// click (handleAttributeSpeakers). onlyUnattributed is always false - a
// fresh, full-chapter run, never itself a paused continuation -
// attributeChapter's own pause/resume handling (its own requeue closure,
// re-invoking EnqueueAttribution directly) works identically regardless of
// who queued the task it's running as.
func (s *Server) attributeChapterForJob(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	return s.attributeChapter(ctx, book, ch, false)
}

// scareQuoteChapterForJob is a jobs.KindScareQuote task's work (matching
// jobs.ChapterScareQuoter, and wired in as such from NewRouter so
// jobs.Manager.scareQuoteDependency can create one lazily ahead of a
// chapter's attribution/description tagging): runs scareQuoteChapter over
// every paragraph and, only on a clean full pass, sets
// Passes.ScareQuote. Always re-runs regardless of Passes.ScareQuote - an
// explicit "Retag scare quotes" click means redo it.
func (s *Server) scareQuoteChapterForJob(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	if err := s.scareQuoteChapter(ctx, book, ch, all); err != nil {
		return 0, nil, err
	}
	if err := s.Store.SetChapterScareQuoted(ch.ID); err != nil {
		log.Printf("scareQuoteChapterForJob: could not advance chapter %s passes: %v", ch.ID, err)
	}
	return 0, nil, nil
}

// describeChapterForJob is a jobs.KindDescription task's work:
// describeChapter over every paragraph, setting Passes.Description only on
// a clean full pass. scareQuoteChapterForJob's shape; the task itself waits
// on the chapter's scare-quote tagging (jobs.Manager.scareQuoteDependency).
func (s *Server) describeChapterForJob(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	if err := s.describeChapter(ctx, book, ch, all); err != nil {
		return 0, nil, err
	}
	if err := s.Store.SetChapterDescribed(ch.ID); err != nil {
		log.Printf("describeChapterForJob: could not advance chapter %s passes: %v", ch.ID, err)
	}
	return 0, nil, nil
}

// handleAttributeSpeakers enqueues speaker attribution for one chapter,
// explicitly triggered by the reader (not automatic) - see
// backend/CLAUDE.md's "Speaker attribution" section. Fire-and-forget
// (202 Accepted, no result body): attribution runs the LLM over a whole
// chapter in batches and can take minutes, and SpeakersPage's "Attribute
// all" fires one of these per unattributed chapter in a book at once, so
// blocking this request on the actual run (as an earlier version did)
// meant holding up to one HTTP connection open per chapter in the book for
// as long as the whole book took to attribute - see
// jobs.Manager.EnqueueAttribution's own doc comment for the full story.
// Progress is observable via GET /api/jobs and by refetching this book's
// speaker table once a chapter's task clears.
func (s *Server) handleAttributeSpeakers(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker attribution is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	s.Jobs.EnqueueAttribution(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.attributeChapter(ctx, book, ch, false)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// handleTagDirections enqueues speech-direction tagging for one chapter,
// explicitly triggered by the reader - see directChapter's own doc comment
// for why this isn't chained after attribution automatically the way
// describeChapter is. Fire-and-forget (202 Accepted, no result body),
// mirroring handleAttributeSpeakers exactly: tagging runs the LLM over a
// whole chapter in batches and can take a while, so this shouldn't hold a
// request open for it - see jobs.Manager.EnqueueAttribution's own doc
// comment for the fuller story behind that shape. Progress is observable
// via GET /api/jobs.
func (s *Server) handleTagDirections(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speech direction tagging is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	s.Jobs.EnqueueDirection(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.directChapter(ctx, book, ch, nil)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// handleRetagDescriptions queues a KindDescription task for one chapter -
// the Speakers page's own "Retag descriptions" button. Always re-runs, even
// if the chapter was already described. Fire-and-forget (202), the same
// shape as handleAttributeSpeakers; the task waits on the chapter's
// scare-quote tagging first (jobs.Manager.scareQuoteDependency), and
// progress shows up on the Jobs dashboard.
func (s *Server) handleRetagDescriptions(w http.ResponseWriter, r *http.Request) {
	book, ch, ok := s.chapterLLMTarget(w, r)
	if !ok {
		return
	}
	s.Jobs.EnqueueDescription(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.describeChapterForJob(ctx, book, ch)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// handleRetagScareQuotes queues a KindScareQuote task for one chapter -
// handleRetagDescriptions' counterpart. Always re-runs, even if the chapter
// was already tagged.
func (s *Server) handleRetagScareQuotes(w http.ResponseWriter, r *http.Request) {
	book, ch, ok := s.chapterLLMTarget(w, r)
	if !ok {
		return
	}
	s.Jobs.EnqueueScareQuote(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.scareQuoteChapterForJob(ctx, book, ch)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// handleResolvePronunciation queues a KindPronunciation task for one
// chapter - handleRetagScareQuotes' counterpart, for any clone model.
// Always re-runs, even if the chapter was already resolved.
func (s *Server) handleResolvePronunciation(w http.ResponseWriter, r *http.Request) {
	book, ch, ok := s.chapterLLMTarget(w, r)
	if !ok {
		return
	}
	s.Jobs.EnqueuePronunciation(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.pronounceChapter(ctx, book, ch, nil)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// chapterLLMTarget resolves handleRetagDescriptions/handleRetagScareQuotes/
// handleResolvePronunciation's shared {id}/{idx} path params to a book and chapter, writing the error
// response itself (503 without a speaker LLM, 400/404/500 otherwise) and
// reporting ok=false when the caller should just return.
func (s *Server) chapterLLMTarget(w http.ResponseWriter, r *http.Request) (*store.Book, *store.Chapter, bool) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker attribution is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return nil, nil, false
	}
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return nil, nil, false
	}
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return nil, nil, false
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return nil, nil, false
	}
	return book, ch, true
}

// rosterResolver builds a speakerattr.SpeakerNameResolver over roster's
// valid characters and their aliases.
func rosterResolver(roster []store.Character) *speakerattr.SpeakerNameResolver {
	existing, _ := splitInvalidCharacters(roster)
	names := make([]string, len(existing))
	aliases := make(map[string][]string, len(existing))
	for i, c := range existing {
		names[i] = c.Name
		if len(c.Aliases) > 0 {
			aliases[c.Name] = c.Aliases
		}
	}
	return speakerattr.NewSpeakerNameResolver(names, aliases)
}

// prominentSpeakersCount is how many of a book's most frequent speakers
// every attribution prompt lists regardless of whether the batch's own
// text names them - see speakerattr.Roster.Prominent.
const prominentSpeakersCount = 12

// recentChapters is how many chapters before the one being attributed
// prominentSpeakers draws its "still on stage" speakers from.
const recentChapters = 2

// prominentSpeakers returns the known names every attribution prompt for
// chapterIdx lists regardless of its own text: up to
// prominentSpeakersCount with the most dialogue lines in bookID so far
// (most first), plus everyone who spoke in the recentChapters chapters
// right before it - a character on stage from the last scene is often
// referred to only by epithet ("the glowing figure intoned") and would
// otherwise be missing from a prompt whose lines never name them. Empty
// on a book with nothing attributed yet - every batch then just lists
// whoever its own lines mention. Best-effort: a query failure only costs
// prompt context, so it's logged, not returned.
func (s *Server) prominentSpeakers(bookID string, chapterIdx int, known []string) []string {
	names := s.topSpeakers(bookID, known)
	recent, err := s.Store.RecentChapterSpeakers(bookID, chapterIdx, recentChapters)
	if err != nil {
		log.Printf("httpapi: recent speakers for book %s: %v", bookID, err)
		return names
	}
	for _, n := range recent {
		if slices.Contains(known, n) && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return names
}

func (s *Server) topSpeakers(bookID string, known []string) []string {
	counts, err := s.Store.SpeakerLineCounts(bookID)
	if err != nil {
		log.Printf("httpapi: speaker line counts for book %s: %v", bookID, err)
		return nil
	}
	var names []string
	for _, n := range known {
		if counts[n] > 0 {
			names = append(names, n)
		}
	}
	sort.SliceStable(names, func(i, j int) bool { return counts[names[i]] > counts[names[j]] })
	if len(names) > prominentSpeakersCount {
		names = names[:prominentSpeakersCount]
	}
	return names
}
