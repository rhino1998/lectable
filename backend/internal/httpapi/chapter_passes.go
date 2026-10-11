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

	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

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

// attributionRun is one speaker-attribution pass's unpersisted result -
// see runAttribution.
type attributionRun struct {
	speakers map[int]string
	// indexes is each numbered role line's speaker number - see
	// speakerattr.Client.AttributeChapter.
	indexes   map[int]int
	roleNames map[string]bool
	pronouns  map[string]string
	remaining []speakerattr.ParagraphInput
	err       error // a batch failure; speakers still holds what finished
	scope     string
	invalid   invalidSpeakers
}

// attributionOptions says how much of the reader's own speaker corrections
// (store.ParagraphEdit) a run is shown. attributeChapter shows all of them;
// the preview can hold some back so a score against those same corrections
// stays meaningful.
type attributionOptions struct {
	// given reports whether a corrected line in this chapter is shown with
	// its speaker (speakerattr.ParagraphInput.Known) rather than asked.
	given func(paragraphIdx int) bool
	// noCorrections leaves out earlier chapters' corrections
	// (speakerattr.Roster.Corrections).
	noCorrections bool
	// contextOnly (nil for none) marks paragraphs shown to the model but
	// not asked about (speakerattr.ParagraphInput.ContextOnly) -
	// attributeAroundEdits' surrounding context.
	contextOnly func(paragraphIdx int) bool
}

// allCorrections is attributeChapter's attributionOptions: every correction
// the reader made is shown.
var allCorrections = attributionOptions{given: func(int) bool { return true }}

// runAttribution runs speaker attribution over paragraphs (all is the whole
// chapter) with the series roster, the book's first-person narrator, the
// chapter's summary notes and the reader's corrections (opts), persisting
// nothing - attributeChapter stores the result, previewAttribution only
// reports it.
func (s *Server) runAttribution(ctx context.Context, book *store.Book, ch *store.Chapter, all, paragraphs []store.Paragraph, opts attributionOptions) (attributionRun, error) {
	scope := store.SeriesScope(book)
	roster, err := s.Store.ListCharacters(scope)
	if err != nil {
		return attributionRun{}, err
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
	// The reader's speaker corrections: this chapter's are shown as settled
	// lines (opts.given), earlier chapters' as examples of past mistakes.
	// Later chapters' are left out - a first pass runs in reading order and
	// wouldn't have them, and the preview's scores stay honest.
	edits, err := s.Store.ParagraphEdits(book.ID, store.EditSpeaker)
	if err != nil {
		return attributionRun{}, err
	}
	given := map[int]string{}
	var corrections []speakerattr.Correction
	for _, e := range edits {
		switch {
		case e.ChapterIdx == ch.Idx && opts.given != nil && opts.given(e.ParagraphIdx):
			given[e.ParagraphIdx] = speakerLabel(e.Value)
		case e.ChapterIdx < ch.Idx && !opts.noCorrections:
			corrections = append(corrections, speakerattr.Correction{ChapterIdx: e.ChapterIdx, Text: e.Text, Wrong: speakerLabel(e.Original), Right: speakerLabel(e.Value)})
		}
	}
	// A play's lines come with their speaker (store.Paragraph.
	// SourceSpeaker): settled like a reader's correction, never asked.
	for _, p := range all {
		if _, corrected := given[p.Idx]; p.SourceSpeaker != "" && !corrected {
			given[p.Idx] = p.SourceSpeaker
		}
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
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: inline, IsQuote: p.IsQuote && !p.ScareQuote && !p.Unvoiced()}
		if inputs[i].IsQuote {
			inputs[i].Known = given[p.Idx]
			if inputs[i].Known != "" && knownRoles[inputs[i].Known] {
				inputs[i].KnownIndex = p.SpeakerIndex
			}
			inputs[i].ContextOnly = opts.contextOnly != nil && opts.contextOnly(p.Idx)
		}
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
	attrRoster := speakerattr.Roster{Names: known, Roles: knownRoles, Aliases: knownAliases, Pronouns: knownPronouns, Prominent: s.prominentSpeakers(book.ID, ch.Idx, known)}
	attrRoster = s.withFirstPerson(attrRoster, book)
	// The chapter's cast from its summary, when it has one: listed in every
	// batch with what the chapter calls them, so a batch that only says
	// "the smith" or "he" still sees who's in the scene.
	attrRoster = s.withChapterCast(attrRoster, ch, invalid, all)
	attrRoster.Corrections = corrections
	speakers, indexes, roleNames, pronouns, remaining, attrErr := s.Speaker.AttributeChapter(ctx, book.Title, ch.Title, attrRoster, inputs, nil)
	return attributionRun{speakers: speakers, indexes: indexes, roleNames: roleNames, pronouns: pronouns, remaining: remaining, err: attrErr, scope: scope, invalid: invalid}, nil
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
// mode attributeUnattributed restricts the paragraphs actually sent to the
// model to ones with no Speaker yet, instead of every paragraph in the
// chapter (attributeFull) - attributeFull for every reader-triggered call (handleAttributeSpeakers: an
// explicit "Attribute speakers"/"Attribute all" click always means
// "redo the whole chapter," the same idempotent refresh it's always been,
// even for paragraphs already attributed - see SpeakersPage's own comment
// on that button), attributeUnattributed only when this call is itself the automatic
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
//
// attributeAroundEdits re-asks only the dialogue near the reader's
// corrected lines - see aroundEditsScope. It never advances the chapter's
// pass flag: the chapter was attributed already, and this is a touch-up.
func (s *Server) attributeChapter(ctx context.Context, book *store.Book, ch *store.Chapter, mode attributionMode) (attributed int, requeue func(), err error) {
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	paragraphs := all
	opts := allCorrections
	switch mode {
	case attributeUnattributed:
		paragraphs = make([]store.Paragraph, 0, len(all))
		for _, p := range all {
			if p.Speaker == "" {
				paragraphs = append(paragraphs, p)
			}
		}
	case attributeAroundEdits:
		edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditSpeaker)
		if err != nil {
			return 0, nil, err
		}
		paragraphs, opts.contextOnly = aroundEditsScope(all, edited)
	}
	if len(paragraphs) == 0 {
		return 0, nil, nil
	}

	run, err := s.runAttribution(ctx, book, ch, all, paragraphs, opts)
	if err != nil {
		return 0, nil, err
	}
	scope, invalid := run.scope, run.invalid
	speakers, roleNames, pronouns, remaining, attrErr := run.speakers, run.roleNames, run.pronouns, run.remaining, run.err
	roleNames = s.realRoles(book, roleNames)
	// A line the reader corrected by hand keeps their speaker (see
	// store.ParagraphEdit) - a re-run would otherwise undo the fix.
	edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditSpeaker)
	if err != nil {
		return 0, nil, err
	}
	for idx := range edited {
		delete(speakers, idx)
	}
	// So does a line the book itself names the speaker of - a play's.
	for _, p := range all {
		if p.SourceSpeaker != "" {
			delete(speakers, p.Idx)
		}
	}

	paragraphByIdx := make(map[int]store.Paragraph, len(all))
	for _, p := range all {
		paragraphByIdx[p.Idx] = p
	}

	bySpeakerIdx := make(map[int]string, len(speakers))
	// A speaker number is only stored on a line whose speaker ended up a
	// role: it tells unnamed people of one type apart, nothing else.
	indexes := make(map[int]int, len(run.indexes))
	var newChars []store.Character // created by this run - see absorbShortNames
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
			if p, ok := paragraphByIdx[idx]; ok && (!p.IsQuote || p.ScareQuote || p.Unvoiced()) {
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
		char, created, uerr := s.Store.UpsertCharacter(scope, name, roleNames[name])
		if uerr != nil {
			return 0, nil, uerr
		}
		// The row's own spelling: the lookup is case-insensitive, so
		// "woman" lands on an existing "Woman".
		bySpeakerIdx[idx] = char.Name
		if n := run.indexes[idx]; n > 0 && char.IsRole {
			indexes[idx] = n
		}
		if created {
			newChars = append(newChars, char)
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
	if serr := s.Store.SetParagraphSpeakersIndexed(ch.ID, bySpeakerIdx, indexes); serr != nil {
		return 0, nil, serr
	}
	// A full name met for the first time absorbs a first-name-only
	// character made earlier ("Lars" into "Lars Machmüller") - after the
	// lines above are stored, so this chapter's own "Lars" lines move too.
	for _, c := range newChars {
		s.absorbShortNames(book, c)
	}

	if attrErr != nil {
		// A genuine failure, not a pause - surfaced to the caller (logged
		// by jobs.Manager.handleResult) rather than auto-requeued, so a
		// batch that keeps failing the same way doesn't loop forever;
		// a reader can always retry explicitly.
		return len(bySpeakerIdx), nil, attrErr
	}
	if len(remaining) > 0 {
		next := attributeUnattributed
		if mode == attributeAroundEdits {
			next = attributeAroundEdits
		}
		requeue = func() {
			s.Jobs.EnqueueAttribution(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
				return s.attributeChapter(ctx, book, ch, next)
			})
		}
		return len(bySpeakerIdx), requeue, nil
	}
	if mode == attributeAroundEdits {
		return len(bySpeakerIdx), nil, nil
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

// attributionMode is which of a chapter's lines attributeChapter asks about.
type attributionMode int

const (
	// attributeFull asks every dialogue line.
	attributeFull attributionMode = iota
	// attributeUnattributed asks only lines with no speaker yet - the
	// continuation of a run that stopped partway.
	attributeUnattributed
	// attributeAroundEdits asks only the lines near the reader's corrected
	// ones (aroundEditsScope).
	attributeAroundEdits
)

// aroundEditsAsk is how many paragraphs either side of a corrected line
// attributeAroundEdits re-asks, and aroundEditsContext how many either
// side it shows as context - the exchange a correction sits in is usually
// a handful of turns, and a line at the edge of the asked stretch still
// needs the turns before it.
const (
	aroundEditsAsk     = 8
	aroundEditsContext = 20
)

// aroundEditsScope picks what attributeAroundEdits sends: every paragraph
// within aroundEditsContext of a corrected line (edited, by idx), in
// chapter order, with contextOnly true for those farther than
// aroundEditsAsk. Corrected lines themselves go in too - runAttribution
// shows them with the reader's speaker. Nothing when nothing is corrected.
func aroundEditsScope(all []store.Paragraph, edited map[int]bool) ([]store.Paragraph, func(int) bool) {
	dist := map[int]int{}
	for i, p := range all {
		if !edited[p.Idx] {
			continue
		}
		for j := max(0, i-aroundEditsContext); j <= min(len(all)-1, i+aroundEditsContext); j++ {
			d := j - i
			if d < 0 {
				d = -d
			}
			if cur, ok := dist[all[j].Idx]; !ok || d < cur {
				dist[all[j].Idx] = d
			}
		}
	}
	var out []store.Paragraph
	for _, p := range all {
		if _, ok := dist[p.Idx]; ok {
			out = append(out, p)
		}
	}
	return out, func(idx int) bool { return dist[idx] > aroundEditsAsk }
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
	// A line the reader corrected by hand isn't the model's to move
	// (store.ParagraphEdit).
	edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditSpeaker)
	if err != nil {
		return 0, nil, err
	}
	if len(edited) > 0 {
		kept := make(map[int]bool, len(targetIdx))
		for idx := range targetIdx {
			if !edited[idx] {
				kept[idx] = true
			}
		}
		targetIdx = kept
	}
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

	inputs := make([]speakerattr.ParagraphInput, len(all))
	paragraphByIdx := make(map[int]store.Paragraph, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote && !p.Unvoiced()}
		paragraphByIdx[p.Idx] = p
	}
	isDialogue := func(idx int) bool {
		p, ok := paragraphByIdx[idx]
		return ok && p.IsQuote && !p.ScareQuote && !p.Unvoiced()
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
		attrRoster := speakerattr.Roster{Names: known, Roles: knownRoles, Aliases: knownAliases, Pronouns: knownPronouns, Prominent: s.prominentSpeakers(book.ID, ch.Idx, known)}
		attrRoster = s.withFirstPerson(attrRoster, book)
		// The chapter's cast from its summary, when it has one: listed in every
		// batch with what the chapter calls them, so a batch that only says
		// "the smith" or "he" still sees who's in the scene.
		attrRoster = s.withChapterCast(attrRoster, ch, invalid, all)
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

	roleNames = s.realRoles(book, roleNames)
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
		char, _, uerr := s.Store.UpsertCharacter(scope, name, roleNames[name])
		if uerr != nil {
			return 0, nil, uerr
		}
		bySpeakerIdx[idx] = char.Name // the row's own spelling
	}
	// Persisted regardless of attrErr - keep whatever actually succeeded;
	// a target the failed widening never reached keeps excludeName, so a
	// retry of this same Auto Split picks it up again.
	if serr := s.Store.SetParagraphSpeakers(ch.ID, bySpeakerIdx); serr != nil {
		return 0, nil, serr
	}
	return len(bySpeakerIdx), nil, attrErr
}

// scareQuoteChapter runs speakerattr.Client.ScareQuoteChapter over every
// quoted paragraph in chapter and persists which ones are scare quotes
// (Store.SetParagraphScareQuotes) - the
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
		// A play's speech is never a scare quote: it has no quote marks
		// to misread, and its speaker is the book's own word.
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && p.SourceSpeaker == ""}
	}

	scareQuotes, _, scareQuoteErr := s.Speaker.ScareQuoteChapter(ctx, book.Title, ch.Title, inputs, nil)
	// Persisted below regardless of scareQuoteErr: whatever batches
	// completed are kept.

	// A line the reader marked or unmarked by hand keeps their call
	// (store.ParagraphEdit) - the pass is additive, so an unmarked line
	// would otherwise be flagged again on the next run.
	edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditScareQuote)
	if err != nil {
		return err
	}
	byIdx := make(map[int]bool, len(scareQuotes))
	var changed []store.Paragraph
	for _, p := range all {
		if edited[p.Idx] || p.SourceSpeaker != "" {
			continue
		}
		want := scareQuotes[p.Idx]
		// Additive: a run only ever adds flags. The pass misses far more
		// scare quotes than it invents (its precision is high - see
		// speakerattr.scareQuoteCandidates), so a re-run not finding one
		// again is no evidence it was wrong; clearing it would hand a real
		// scare quote back to attribution as dialogue. A reader's manual
		// edit or a pass Reset still clears.
		if !want || p.ScareQuote {
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
	chapterIdx := map[string]int{}
	for _, p := range paragraphs {
		idx, ok := chapterIdx[p.ChapterID]
		if !ok {
			idx = narration.NoChapter
			if ch, err := s.chapterByID(p.ChapterID); err == nil && ch != nil {
				idx = ch.Idx
			}
			chapterIdx[p.ChapterID] = idx
		}
		voice, err := s.Narration.ForParagraph(book, p.Speaker, p.SpeakerIndex, idx)
		if err != nil {
			log.Printf("invalidateParagraphAudio: resolve voice for paragraph %s: %v", p.ID, err)
			continue
		}
		voiceID := voice.VoiceID()
		// The row is all there is to reset: its clip is a shared one,
		// kept or swept by the store (see jobs' sharedclips.go).
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
	return s.directChapterRange(ctx, book, ch, onlyIdx, false)
}

// directChapterRange is directChapter with partial: a window run (jobs.ChapterWindowPass)
// that labels only onlyIdx and neither marks the chapter's pass done nor
// requeues what it didn't reach.
func (s *Server) directChapterRange(ctx context.Context, book *store.Book, ch *store.Chapter, onlyIdx map[int]bool, partial bool) (labeled int, requeue func(), err error) {
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
		if onlyIdx != nil && !partial {
			// The rest of a chapter whose windows already covered it all.
			if serr := s.Store.SetChapterDirected(ch.ID); serr != nil {
				log.Printf("directChapter: could not advance chapter %s passes: %v", ch.ID, serr)
			}
		}
		return 0, nil, nil
	}

	inputs := make([]speakerattr.ParagraphInput, len(paragraphs))
	for i, p := range paragraphs {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote && !p.Unvoiced()}
	}
	labels, remaining, labelErr := s.Speaker.EmotionChapter(ctx, book.Title, ch.Title, inputs, nil)

	// Persisted regardless of labelErr - a batch failing partway shouldn't
	// discard what earlier batches in this same call labeled.
	notReached := make(map[int]bool, len(remaining))
	for _, p := range remaining {
		notReached[p.Idx] = true
	}
	// A line the reader set by hand keeps their emotion (store.ParagraphEdit).
	edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditEmotion)
	if err != nil {
		return 0, nil, err
	}
	updates := make(map[int]string)
	var changed []store.Paragraph
	for _, p := range paragraphs {
		if notReached[p.Idx] || edited[p.Idx] {
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
	if partial {
		// A window run ahead of the chapter's own pass: that pass finishes
		// the chapter and marks it done.
		return len(labels), nil, nil
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
	return s.pronounceChapterRange(ctx, book, ch, onlyIdx, false)
}

// pronounceChapterRange is pronounceChapter with partial: a window run (jobs.ChapterWindowPass)
// that labels only onlyIdx and neither marks the chapter's pass done nor
// requeues what it didn't reach.
func (s *Server) pronounceChapterRange(ctx context.Context, book *store.Book, ch *store.Chapter, onlyIdx map[int]bool, partial bool) (resolved int, requeue func(), err error) {
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
		if onlyIdx != nil && !partial {
			// The rest of a chapter whose windows already covered it all.
			if serr := s.Store.SetChapterPronounced(ch.ID); serr != nil {
				log.Printf("pronounceChapter: could not advance chapter %s passes: %v", ch.ID, serr)
			}
		}
		return 0, nil, nil
	}
	inputs := make([]speakerattr.ParagraphInput, len(paragraphs))
	for i, p := range paragraphs {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote && !p.Unvoiced()}
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
		if _, derr := s.Store.DeleteParagraphAudioForIdxs(book.ID, ch.ID, touched); derr != nil {
			log.Printf("pronounceChapter: invalidate audio for chapter %s: %v", ch.ID, derr)
		}
	}
	if resolveErr != nil {
		return len(pronunciation), nil, resolveErr
	}
	if partial {
		// A window run ahead of the chapter's own pass: that pass finishes
		// the chapter and marks it done.
		return len(pronunciation), nil, nil
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
	return s.attributeChapter(ctx, book, ch, attributeFull)
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
//
// ?scope=edits re-asks only the dialogue around the chapter's corrected
// lines (attributeAroundEdits), with the corrections shown - "I fixed a
// few, sort out the rest". 409 no_edits when the chapter has none.
func (s *Server) handleAttributeSpeakers(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker attribution is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	mode := attributeFull
	switch scope := r.URL.Query().Get("scope"); scope {
	case "", "all":
	case "edits":
		mode = attributeAroundEdits
	default:
		writeError(w, http.StatusBadRequest, "scope must be all or edits, not "+strconv.Quote(scope))
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
	if mode == attributeAroundEdits {
		edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditSpeaker)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(edited) == 0 {
			writeErrorCode(w, http.StatusConflict, errNoEdits, "this chapter has no corrected speakers to work around")
			return
		}
	}

	s.Jobs.EnqueueAttribution(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.attributeChapter(ctx, book, ch, mode)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// handleTagDirections enqueues speech-direction tagging for one chapter,
// explicitly triggered by the reader - see directChapter's own doc comment
// for why this isn't chained after attribution automatically.
// Fire-and-forget (202 Accepted, no result body),
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
	return s.chapterTarget(w, r)
}

// chapterTarget resolves the {id}/{idx} book and chapter a request names,
// writing the error reply itself when it can't.
func (s *Server) chapterTarget(w http.ResponseWriter, r *http.Request) (*store.Book, *store.Chapter, bool) {
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
