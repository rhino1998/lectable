package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// maxVoiceClues is how many voice clues characterization is shown.
const maxVoiceClues = 8

// characterizeVoice asks the LLM how char actually sounds (castCharacter)
// and persists the result - the casting instruction and its paired
// reference passage to char.Summary/RefLine - the pure LLM+DB step behind a
// jobs.KindSpeakerCharacterization task (see jobs.Manager.RunCharacterization),
// shared by provisionCharacterVoice's lazy-provisioning flow and
// handleCharacterizeSpeaker's explicit "Regenerate" button.
//
// Always actually calls the LLM now, even when quotes
// come back empty - speakerattr.Client.CharacterizeVoice's own doc comment
// covers why (the name alone is enough for its system prompt's "no signal
// at all" fallback rules), and this is the layer that used to short-
// circuit before ever reaching that call at all when both were empty,
// returning ("", nil) as a normal "nothing to characterize from yet"
// state - which, for a character whose paragraphs were all later
// reattributed/canonicalized elsewhere, was actually permanent: every
// future characterization pass hit this same early return again, silently
// forever, never persisting a Summary and never erroring either.
func (s *Server) characterizeVoice(ctx context.Context, book *store.Book, char store.Character) (string, error) {
	cast, err := s.castCharacter(ctx, book, char, castOptions{})
	if err != nil {
		return "", err
	}
	if err := s.Store.SetCharacterSummary(char.ID, cast.instruct, cast.refLine); err != nil {
		return "", fmt.Errorf("save characterization: %w", err)
	}
	return cast.instruct, nil
}

// casting is castCharacter's result.
type casting struct {
	instruct string
	refLine  string
	// lookups are the searches the casting call made, each with how many
	// sentences it found ("Scylla + Greek: 3").
	lookups []string
}

// Lookup result sizes.
const (
	// lookupResults is how many matching paragraphs one lookup returns.
	lookupResults = 6
	// lookupSentenceChars caps each one.
	lookupSentenceChars = 400
	// lookupReplyChars caps the reply shown after a matching question.
	lookupReplyChars = 200
	// lookupPool is how many index matches are read for the whole-word
	// check to keep lookupResults of.
	lookupPool = 200
)

// Retrieval sizes for casting's evidence (speakerattr.CharacterEvidence).
const (
	// castingQuotes is how many of the character's lines are read; the
	// casting call is shown an even spread of 20 of them.
	castingQuotes = 40
	// castingIntroductions is how many of the first narration paragraphs
	// naming them it reads.
	castingIntroductions = 3
	// castingTraitPool is how many trait-phrase matches are read for
	// speakerattr.TraitStatements to choose castingTraits from.
	castingTraitPool = 400
	castingTraits    = 14
	// castingStatedAges is how many of their own lines giving their age
	// (speakerattr.StatedAges) it reads.
	castingStatedAges = 3
)

// lookupExcerpts answers one of the casting call's lookups
// (speakerattr.CastingOptions.Lookup): up to lookupResults excerpts, spread
// over the series, of paragraphs where every one of terms appears as a
// whole word. A lookup that names nobody is given the character's own
// name (names[0]) - without it the search finds everyone. The index
// matches substrings ("age" finds "page", "origin" "original"), which is
// what the first version returned, so each hit is checked again here; the
// excerpt is cut around the last term, the specific word rather than the
// name. A question that matches is shown with the line answering it
// (store.LookupMatch.Reply): "how old are you, Yen?" matches Yen + old,
// and the age is in her answer.
func (s *Server) lookupExcerpts(bookIDs, names, terms []string) ([]string, error) {
	named := false
	for _, t := range terms {
		for _, n := range names {
			if strings.Contains(strings.ToLower(n), strings.ToLower(t)) || strings.Contains(strings.ToLower(t), strings.ToLower(n)) {
				named = true
			}
		}
	}
	if !named {
		terms = append([]string{names[0]}, terms...)
	}
	found, err := s.Store.ParagraphsContainingAll(bookIDs, terms, lookupPool)
	if err != nil {
		return nil, err
	}
	words := make([]*regexp.Regexp, len(terms))
	for i, t := range terms {
		words[i] = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(t) + `\b`)
	}
	var kept []string
	for _, f := range found {
		whole := true
		for _, w := range words {
			if !w.MatchString(f.Text) {
				whole = false
				break
			}
		}
		if !whole {
			continue
		}
		excerpt := speakerattr.Excerpt(f.Text, terms[len(terms)-1], lookupSentenceChars)
		if f.Reply != "" && strings.Contains(excerpt, "?") {
			who := f.ReplySpeaker
			if who == "" || who == "Unknown" {
				who = "the reply"
			}
			reply := strings.Join(strings.Fields(f.Reply), " ")
			if len(reply) > lookupReplyChars {
				reply = reply[:lookupReplyChars] + "..."
			}
			excerpt += fmt.Sprintf(" [%s: %s]", who, reply)
		}
		kept = append(kept, excerpt)
	}
	if len(kept) <= lookupResults {
		return kept, nil
	}
	out := make([]string, lookupResults)
	for i := range out {
		out[i] = kept[i*len(kept)/lookupResults]
	}
	return out, nil
}

// castOptions are the preview's variations on castCharacter; the zero
// value is what characterization does.
type castOptions struct {
	// temp overrides the casting call's sampling temperature.
	temp *float32
	// noLookup casts from the gathered material alone, without letting
	// the model search the book first (speakerattr.CastingOptions.Lookup).
	noLookup bool
	// notes, when set, are the casting call's "World notes"
	// (speakerattr.CharacterEvidence.WorldNotes) in place of the stored
	// facts the character's material brings up (worldNotes).
	notes []string
}

// castCharacter gathers char's material across their series and casts
// their voice (speakerattr.Client.CharacterizeVoice), storing nothing.
// The material is retrieved by search rather than only sampled: the first
// narration naming them and their trait statements ("Bert was a ...")
// through the paragraphs_fts index (Store.NarrationMatching), voice and
// origin clues (narrationClues), idiom counts over every line they speak,
// and a spread of their lines. The model then searches the book itself
// for what that leaves unstated (lookupExcerpts) before it casts.
func (s *Server) castCharacter(ctx context.Context, book *store.Book, char store.Character, opts castOptions) (casting, error) {
	books, err := s.booksInScope(book)
	if err != nil {
		return casting{}, fmt.Errorf("list scope books: %w", err)
	}
	bookIDs := make([]string, len(books))
	for i, b := range books {
		bookIDs[i] = b.ID
	}

	evidence, err := s.describedEvidence(books, bookIDs, char)
	if err != nil {
		return casting{}, err
	}
	names := append([]string{char.Name}, char.Aliases...)
	setting := s.bookSetting(ctx, book)

	lines, err := s.Store.SpokenLinesForBooks(bookIDs, char.Name)
	if err != nil {
		return casting{}, fmt.Errorf("gather lines: %w", err)
	}
	evidence.Idioms = speakerattr.IdiomCounts{}
	spoken := make([]speakerattr.SpokenLine, len(lines))
	for i, l := range lines {
		evidence.Idioms.Add(l.Text)
		spoken[i] = speakerattr.SpokenLine{Text: l.Text, Asked: l.Asked}
	}
	if !char.IsRole {
		// A role's lines are several people's.
		evidence.StatedAges = speakerattr.StatedAges(spoken, castingStatedAges)
	}

	costars, err := s.characterizedCostars(bookIDs, char)
	if err != nil {
		return casting{}, fmt.Errorf("gather co-stars: %w", err)
	}
	if evidence.WorldNotes = opts.notes; evidence.WorldNotes == nil {
		if evidence.WorldNotes, err = s.worldNotes(book, char, evidence); err != nil {
			return casting{}, fmt.Errorf("gather world notes: %w", err)
		}
	}

	castingOpts := speakerattr.CastingOptions{Temp: opts.temp}
	var lookups []string
	if !opts.noLookup {
		castingOpts.Lookup = func(_ context.Context, terms []string) ([]string, error) {
			return s.lookupExcerpts(bookIDs, names, terms)
		}
		castingOpts.OnLookup = func(terms []string, found int) {
			lookups = append(lookups, fmt.Sprintf("%s: %d", strings.Join(terms, " + "), found))
		}
	}
	instruct, refLine, err := s.Speaker.CharacterizeVoice(ctx, char.Name, char.IsRole, char.Pronouns, setting, evidence, costars, castingOpts)
	if err != nil {
		return casting{}, fmt.Errorf("characterize: %w", err)
	}
	return casting{instruct: instruct, refLine: refLine, lookups: lookups}, nil
}

// describedEvidence gathers the part of char's casting material that is
// found by search across their series (books; bookIDs are the same
// books): a spread of their lines with the narration around each, the
// first narration naming them, their trait statements, and their voice and
// origin clues.
func (s *Server) describedEvidence(books []store.Book, bookIDs []string, char store.Character) (speakerattr.CharacterEvidence, error) {
	quotes, err := s.Store.QuotesForBooks(bookIDs, char.Name, castingQuotes)
	if err != nil {
		return speakerattr.CharacterEvidence{}, fmt.Errorf("gather quotes: %w", err)
	}
	evidence := speakerattr.CharacterEvidence{Quotes: make([]speakerattr.QuoteContext, len(quotes))}
	for i, q := range quotes {
		evidence.Quotes[i] = speakerattr.QuoteContext{Text: q.Text, Context: q.Context}
	}
	names := append([]string{char.Name}, char.Aliases...)
	origins, voiceClues, err := s.narrationClues(books, bookIDs, char)
	if err != nil {
		return evidence, fmt.Errorf("gather narration clues: %w", err)
	}
	evidence.VoiceClues = voiceClues
	for _, o := range origins {
		// An accent word makes a sentence both kinds of clue.
		if !slices.Contains(voiceClues, o) {
			evidence.Origins = append(evidence.Origins, o)
		}
	}
	if evidence.Introductions, err = s.Store.NarrationMatching(bookIDs, names, castingIntroductions); err != nil {
		return evidence, fmt.Errorf("gather introductions: %w", err)
	}
	traits, err := s.Store.NarrationMatching(bookIDs, speakerattr.TraitPhrases(names), castingTraitPool)
	if err != nil {
		return evidence, fmt.Errorf("gather trait statements: %w", err)
	}
	evidence.Traits = speakerattr.TraitStatements(traits, names, castingTraits)
	return evidence, nil
}

// worldNotes returns the world facts in force for book (store.BookWorldFacts)
// that char's gathered material brings up, as the casting call's "World
// notes". The material is what is said *of* them: their names, the
// narration around their lines, and the narration evidence. What they
// say is left out - mentioning the king doesn't make the speaker one, and
// with it nearly every lead was shown the facts about royalty.
func (s *Server) worldNotes(book *store.Book, char store.Character, evidence speakerattr.CharacterEvidence) ([]string, error) {
	facts, err := store.BookWorldFacts(s.Store, book)
	if err != nil || len(facts) == 0 {
		return nil, err
	}
	notes := make([]speakerattr.WorldNote, len(facts))
	for i, f := range facts {
		notes[i] = speakerattr.WorldNote{Key: f.Key, Aliases: f.Aliases, Fact: f.Fact}
	}
	texts := append([]string{char.Name}, char.Aliases...)
	for _, q := range evidence.Quotes {
		texts = append(texts, q.Context)
	}
	texts = append(texts, evidence.Introductions...)
	texts = append(texts, evidence.Traits...)
	texts = append(texts, evidence.VoiceClues...)
	texts = append(texts, evidence.Origins...)
	return speakerattr.MatchWorldNotes(notes, texts...), nil
}

// castingWorldNotes returns the world facts in force for book that a
// casting of char brings up - their names and the casting's own words,
// which name their kind ("an adult kobold woman") - for the calls that
// rewrite a casting for another age. nil when the facts can't be read.
func (s *Server) castingWorldNotes(book *store.Book, char store.Character, casting string) []string {
	facts, err := store.BookWorldFacts(s.Store, book)
	if err != nil {
		log.Printf("httpapi: world facts for %q's casting: %v", char.Name, err)
		return nil
	}
	notes := make([]speakerattr.WorldNote, len(facts))
	for i, f := range facts {
		notes[i] = speakerattr.WorldNote{Key: f.Key, Aliases: f.Aliases, Fact: f.Fact}
	}
	return speakerattr.MatchWorldNotes(notes, append(append([]string{char.Name}, char.Aliases...), casting)...)
}

// maxOriginClues is how many origin clues characterization is shown.
const maxOriginClues = 6

// narrationClues returns char's speakerattr.OriginClues and VoiceClues
// across books (their series, in order; bookIDs are the same books), from
// each book's cached clue paragraphs and the narration following char's
// own lines.
func (s *Server) narrationClues(books []store.Book, bookIDs []string, char store.Character) (origins, voice []string, err error) {
	var originParas, voiceParas []string
	for i := range books {
		c, err := s.bookClueParagraphs(&books[i])
		if err != nil {
			return nil, nil, err
		}
		originParas = append(originParas, c.origin...)
		voiceParas = append(voiceParas, c.voice...)
	}
	tags, err := s.Store.SpeechTagsForBooks(bookIDs, char.Name)
	if err != nil {
		return nil, nil, err
	}
	names := append([]string{char.Name}, char.Aliases...)
	roster, err := s.Store.ListCharacters(char.Scope)
	if err != nil {
		return nil, nil, err
	}
	var others []string
	for _, o := range roster {
		if o.ID != char.ID && !o.Invalid {
			others = append(others, o.Name)
			others = append(others, o.Aliases...)
		}
	}
	shared := speakerattr.SharedNames(others)
	return speakerattr.OriginClues(originParas, names, shared, maxOriginClues), speakerattr.VoiceClues(tags, voiceParas, names, shared, char.Pronouns, maxVoiceClues), nil
}

// cachedClueParagraphs is one book's speakerattr.OriginParagraphs (every
// paragraph) and VoiceParagraphs (narration only - "lower your voice" in
// someone's speech says nothing of how anyone sounds).
type cachedClueParagraphs struct {
	origin, voice []string
	built         time.Time
}

// bookClueParagraphs returns book's clue paragraphs, cached for
// identityIndexTTL (Server.originParagraphs) - a bulk recast reads
// every book of the series once per character.
func (s *Server) bookClueParagraphs(book *store.Book) (cachedClueParagraphs, error) {
	if v, ok := s.originParagraphs.Load(book.ID); ok {
		if c := v.(cachedClueParagraphs); time.Since(c.built) < identityIndexTTL {
			return c, nil
		}
	}
	all, err := s.Store.ListParagraphsRawForBook(book.ID)
	if err != nil {
		return cachedClueParagraphs{}, err
	}
	texts := make([]string, len(all))
	var narration []string
	for i, p := range all {
		texts[i] = p.Text
		if !p.IsQuote {
			narration = append(narration, p.Text)
		}
	}
	c := cachedClueParagraphs{origin: speakerattr.OriginParagraphs(texts), voice: speakerattr.VoiceParagraphs(narration), built: time.Now()}
	s.originParagraphs.Store(book.ID, c)
	return c, nil
}

// maxCharacterizeCostars is how many already-cast co-stars characterization
// is shown, and costarCandidates how many co-speakers are looked at to find
// them (some have no summary yet, or are roles or invalid).
const (
	maxCharacterizeCostars = 5
	costarCandidates       = 15
)

// characterizedCostars returns the casting of the characters who share the
// most chapters with char (Store.CoSpeakersForBooks) and already have one -
// named, valid characters only, since a role's pooled voice isn't one
// person a listener has to tell apart. Characters are usually
// characterized in order of appearance, so leads see each other; a
// Regenerate sees the whole cast.
func (s *Server) characterizedCostars(bookIDs []string, char store.Character) ([]speakerattr.CastVoice, error) {
	names, err := s.Store.CoSpeakersForBooks(bookIDs, char.Name, costarCandidates)
	if err != nil {
		return nil, err
	}
	var out []speakerattr.CastVoice
	for _, name := range names {
		other, err := s.Store.GetCharacterByName(char.Scope, name)
		if err != nil {
			return nil, err
		}
		if other == nil || other.ID == char.ID || other.Summary == "" || other.Invalid || other.IsRole {
			continue
		}
		out = append(out, speakerattr.CastVoice{Name: other.Name, Instruct: other.Summary})
		if len(out) == maxCharacterizeCostars {
			break
		}
	}
	return out, nil
}

// lockCharacterProvision serializes provisionCharacterVoice per
// characterID (across every clone model, and every caller/goroutine that
// might race one - a lazy provision from jobs.Manager's own background
// goroutine, another lazy provision for a different clone model, or an
// explicit "Regenerate" click) via Server.provisionLocks, a keyed mutex.
// This is what actually prevents two concurrent callers from both
// deciding "no voice yet" and each creating their own duplicate preset -
// jobs.Manager.RunCharacterization's own per-characterID dedup, used
// inside provisionCharacterVoice below, only covers the LLM
// characterization step by itself; without this, two callers that joined
// (or separately ran) that step back to back would still both fall
// through to CreateVoicePreset afterward. Returns the unlock func; call
// it (typically via defer) once done.
func (s *Server) lockCharacterProvision(characterID string) func() {
	muAny, _ := s.provisionLocks.LoadOrStore(characterID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// characterRefText picks the reference passage a character's auto-created
// voice preset should be rendered from: characterized.RefLine, written by
// the same speakerattr.Client.CharacterizeVoice call as its Summary so its
// pace/energy actually matches the voice Summary describes (see
// store.Character.RefLine), falling back to voices.DefaultRefText only as
// a defensive default - RefLine should never legitimately be empty
// whenever Summary isn't, since CharacterizeVoice always returns both
// together.
func characterRefText(characterized store.Character) string {
	if characterized.RefLine != "" {
		return characterized.RefLine
	}
	return voices.DefaultRefText
}

// provisionCharacterVoice matches jobs.CharacterVoiceProvisioner's fixed
// signature - see provisionCharacterVoiceAttempt's own doc comment for what
// it actually does. Always attempt 0 (no seed bump ever possible) because
// this exact interface value is what jobs.Manager.provisionMissingCharacterVoices
// calls directly, outside of any RunVoiceProvision/EnqueueVoiceProvision
// task (so there's no t.attempt to plumb through even if this path did
// retry) - and, separately, provisionCharacterVoiceAttempt's own reference-
// clip render errors are logged and swallowed rather than surfaced/retried
// on this path already (see its own doc comment), so attempt would be inert
// here regardless. A caller that *does* run inside a KindVoiceProvision
// task (characters.go's own handlers, pipeline.go) calls
// provisionCharacterVoiceAttempt directly instead, passing the task's real
// attempt through.
func (s *Server) provisionCharacterVoice(ctx context.Context, bookID string, char store.Character, cloneModel string) (string, error) {
	return s.provisionCharacterVoiceAttempt(ctx, bookID, char, cloneModel, 0)
}

// provisionCharacterVoiceAttempt ensures char has an assigned voice for
// cloneModel specifically (a voice preset clones through one specific
// model and isn't portable to another - see store.Character's doc
// comment), characterizing them first (characterizeVoice, above) if
// they aren't yet and then creating + assigning a freshly-cloned preset.
// attempt is the calling KindVoiceProvision task's own retry count (0 on
// the first try) - threaded through to renderWithSeedBump so a retry after
// a numerically-unstable VoiceDesign crash ("no finite logits") perturbs
// the seed instead of reproducing the exact same crash on every attempt;
// see renderWithSeedBump's own doc comment.
// Characterization itself is shared across every clone model, so it only
// runs once, ever, per character (when char.Summary is still empty) - a
// character already characterized under one model reuses that same
// instruct text when a voice is created for another, rather than
// re-describing them (and potentially drifting) on every new model
// encountered.
func (s *Server) provisionCharacterVoiceAttempt(ctx context.Context, bookID string, char store.Character, cloneModel string, attempt int) (string, error) {
	if presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel); err != nil {
		return "", err
	} else if presetID != "" {
		return presetID, nil // already provisioned - the common case after the first call
	}

	unlock := s.lockCharacterProvision(char.ID)
	defer unlock()

	// Re-check now that this call actually holds the lock: a concurrent
	// caller (any clone model) may have provisioned this exact
	// (character, cloneModel) pair between the check above and acquiring
	// the lock.
	if presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel); err != nil {
		return "", err
	} else if presetID != "" {
		return presetID, nil
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return "", err
	}
	if book == nil {
		return "", fmt.Errorf("book %s not found", bookID)
	}

	characterized, err := s.ensureCharacterized(ctx, book, char)
	if err != nil {
		return "", err
	}
	if characterized.Summary == "" {
		return "", nil // defensive only now - see ensureCharacterized
	}
	if (book.InstructCharacterVoices() || book.InstructAllCharacterVoices()) && cloneModel == voices.InstructedCloneModel {
		// This book resolves this character straight to its own narrator
		// voice for this clone model, styled by their own characterization,
		// rather than a dedicated auto-provisioned preset (see
		// internal/narration.Resolver.ResolveCharacterVoice's own
		// InstructUnassigned/InstructAll branches) - characterizing them
		// (above) is still real, needed work (their own Summary/RefLine now
		// exist for the Speakers page and for the resolver to read),
		// there's just no preset left to create or assign here. "" is this
		// function's own established "no preset for this character/model"
		// return - see provisionMissingCharacterVoices' own matching check,
		// which skips calling this at all once Summary is already set, so
		// this branch only actually runs once per character, the same as
		// preset creation below would.
		return "", nil
	}

	preset, err := s.Store.CreateVoicePreset(char.Name, characterized.Summary, characterRefText(characterized), store.RandomSeed(), 1.0, voices.DefaultDesignModel)
	if err != nil {
		return "", fmt.Errorf("create voice preset: %w", err)
	}
	// Render its reference clip now so the preset is immediately usable;
	// if this fails (e.g. the worker is briefly down), it's still assigned
	// below and self-heals on first actual generation - see
	// jobs.Manager.generate's own voicerefs.EnsureFile call.
	if _, err := s.renderWithSeedBump(preset.ID, preset.Seed, attempt, func(effectiveSeed int) (string, error) {
		return voicerefs.EnsureFile(ctx, s.TTS, s.DataDir, preset.ID, preset.Instruct, effectiveSeed, preset.RefText, preset.SpeedMultiplier, preset.DesignModel)
	}); err != nil {
		log.Printf("httpapi: render reference clip for character %q: %v", char.Name, err)
	}
	if err := s.Store.SetCharacterVoice(char.ID, cloneModel, preset.ID); err != nil {
		return "", fmt.Errorf("assign auto-created voice: %w", err)
	}
	return preset.ID, nil
}

// characterizeCharacterForJob matches jobs.CharacterCharacterizer's
// signature, wired in as such from NewRouter so jobs.Manager
// (characterizationBlockerFor) can characterize a speaker lazily the
// moment a clone/design/provision task first discovers they aren't
// characterized yet, rather than only ever via an explicit "Characterize"/
// "Regenerate" click (handleCharacterizeSpeakers) or provisionCharacterVoice's
// own lazy-provisioning path (ensureCharacterized). Calls
// recharacterizeAndInvalidate/characterizeVoice directly, the same pair
// handleCharacterizeSpeakers itself calls - NOT ensureCharacterized, which
// goes through the blocking jobs.Manager.RunCharacterization task-queue
// call: this function runs *as* a KindSpeakerCharacterization task's own
// runLLM body (see characterizationBlockerFor), so routing back through
// RunCharacterization here would have that task block on itself, waiting
// for a second characterization of the same character to join and
// complete a task that can't finish until this call returns - a
// self-deadlock. invalidation is a harmless no-op the first time a
// character is ever characterized (recharacterizeAndInvalidate only finds
// existing voice presets to invalidate, and a never-characterized
// character has none yet - see provisionCharacterVoice, which only creates
// one after characterization succeeds).
func (s *Server) characterizeCharacterForJob(ctx context.Context, bookID string, char store.Character) error {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return err
	}
	if book == nil {
		return fmt.Errorf("book %s not found", bookID)
	}
	_, _, err = s.recharacterizeAndInvalidate(ctx, book, char.ID, func(ctx context.Context) error {
		_, err := s.characterizeVoice(ctx, book, char)
		return err
	})
	return err
}

// ensureCharacterized returns char with its own Summary/RefLine populated,
// characterizing it first (via jobs.Manager.RunCharacterization, same as an
// explicit "Regenerate" click - see characterizeVoice) if Summary is still
// empty. Shared by provisionCharacterVoiceAttempt and regenerateCharacterVoice
// below, both of which need a character's instruct text (and its paired
// refLine - see store.Character.RefLine) before they can create or render a
// voice from it. characterizeVoice now always actually characterizes (name
// alone is enough - see its own doc comment), so a zero store.Character
// (empty Summary), nil return here is no longer the normal "no material
// yet" case it used to be - both callers below still check for it
// defensively, but it should only ever fire on a genuine, already-logged
// characterization failure now.
//
// Always re-checks char.ID against the store first, rather than trusting
// the caller-supplied char.Summary directly - every real caller reaches
// this from inside a dispatched KindVoiceProvision task's own runProvision
// closure (provisionCharacterVoiceAttempt, this function's only caller),
// which captured its own char snapshot back when handleGenerateCharacterVoice(s)/
// handleRegenerateCharacterVoices/provisionMissingCharacterVoices first
// enqueued it - not when it actually dispatched. jobs.Manager's own
// characterizationDependency already blocks a KindVoiceProvision task from
// dispatching at all until a character's characterization genuinely
// finishes (see that function's own doc comment), so by the time this
// runs, the store row is normally already up to date even when the
// snapshot handed in isn't. Trusting the stale snapshot instead would call
// RunCharacterization a second time here - fatally: that's a blocking call
// pushing a poolLLM task from *inside* an already-dispatched, in-flight
// poolGeneration task, which can never actually dispatch while this task
// holds poolGeneration active and un-drained (see the scheduler's own
// "one pool at a time" doc comment) - a real deadlock, the same class
// referenceClipDependency's own doc comment describes fixing for
// poolDesign, just reintroduced here via stale caller data bypassing that
// same dependency mechanism instead of a missing one.
func (s *Server) ensureCharacterized(ctx context.Context, book *store.Book, char store.Character) (store.Character, error) {
	if fresh, err := s.Store.GetCharacter(char.ID); err != nil {
		return store.Character{}, err
	} else if fresh != nil && fresh.Summary != "" {
		return *fresh, nil
	}
	if err := s.Jobs.RunCharacterization(ctx, book.ID, char.ID, char.Name, func(ctx context.Context) error {
		_, err := s.characterizeVoice(ctx, book, char)
		return err
	}); err != nil {
		return store.Character{}, err
	}
	fresh, err := s.Store.GetCharacter(char.ID)
	if err != nil {
		return store.Character{}, err
	}
	if fresh == nil {
		return store.Character{}, nil
	}
	return *fresh, nil
}

// regenerateCharacterVoice force-rerenders char's reference clip for
// cloneModel via voicerefs.Regenerate, creating a voice for them first (via
// provisionCharacterVoice's same characterize-then-create path) if they
// don't have one yet for this model. Unlike provisionCharacterVoice, this
// never takes the "already provisioned, return unchanged" fast path - it's
// SpeakersPage's "Regenerate all voices" button, for "give every character
// a fresh take on their existing voice" regardless of whether they already
// have a cached ref clip, not "make sure everyone has *a* voice." Never
// touches char.Summary/Instruct itself - see recharacterizeAndInvalidate
// for changing what a voice sounds like; this only asks for a new render
// of however it's already described.
func (s *Server) regenerateCharacterVoice(ctx context.Context, bookID string, char store.Character, cloneModel string, attempt int) (string, error) {
	unlock := s.lockCharacterProvision(char.ID)
	defer unlock()

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return "", err
	}
	if book == nil {
		return "", fmt.Errorf("book %s not found", bookID)
	}

	presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel)
	if err != nil {
		return "", err
	}

	var preset *store.VoicePreset
	if presetID == "" {
		characterized, err := s.ensureCharacterized(ctx, book, char)
		if err != nil {
			return "", err
		}
		if characterized.Summary == "" {
			return "", nil // defensive only now - see ensureCharacterized
		}
		created, err := s.Store.CreateVoicePreset(char.Name, characterized.Summary, characterRefText(characterized), store.RandomSeed(), 1.0, voices.DefaultDesignModel)
		if err != nil {
			return "", fmt.Errorf("create voice preset: %w", err)
		}
		preset = &created
		if err := s.Store.SetCharacterVoice(char.ID, cloneModel, preset.ID); err != nil {
			return "", fmt.Errorf("assign auto-created voice: %w", err)
		}
	} else {
		preset, err = s.Store.GetVoicePreset(presetID)
		if err != nil {
			return "", err
		}
		if preset == nil {
			return "", fmt.Errorf("voice preset %s not found", presetID)
		}
	}

	if _, err := s.renderWithSeedBump(preset.ID, preset.Seed, attempt, func(effectiveSeed int) (string, error) {
		return voicerefs.Regenerate(ctx, s.TTS, s.DataDir, preset.ID, preset.Instruct, effectiveSeed, preset.RefText, preset.SpeedMultiplier, preset.DesignModel)
	}); err != nil {
		return "", fmt.Errorf("regenerate reference clip: %w", err)
	}
	return preset.ID, nil
}

// handleGenerateCharacterVoice force-provisions a voice for one character
// right now, rather than waiting for the lazy path
// (jobs.Manager.provisionMissingCharacterVoices) to trigger it the next
// time their dialogue is actually generated - the Speakers page's own
// "Generate voice" button, for previewing/setting up a character's voice
// ahead of time. Idempotent: if they already have one for this book's
// clone model, provisionCharacterVoice's own fast path just returns it
// unchanged. 409 if there still isn't enough dialogue to characterize
// them from yet (provisionCharacterVoice returns "", nil in that case,
// same as the lazy path silently no-ops on).
func (s *Server) handleGenerateCharacterVoice(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	characterID := r.PathValue("characterId")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}

	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)

	// Routed through jobs.Manager.RunVoiceProvision (a KindVoiceProvision
	// task), not called directly - this used to bypass the job queue
	// entirely, so an explicit "Generate voice" click competed for the
	// ttsworker process uncoordinated with (and outside the concurrency
	// limit protecting) actual paragraph generation and every other kind of
	// voice work, same class of bug handleTestVoiceDesign's own doc comment
	// describes fixing for the "Test" button. Same "generate" mode/dedup
	// key handleGenerateCharacterVoices (the batch button) and the lazy
	// path (provisionMissingCharacterVoices) already use, so all three
	// correctly join into one queued provision instead of racing.
	presetID, err := s.Jobs.RunVoiceProvision(r.Context(), book.ID, char.ID, "generate", char.Name, func(ctx context.Context, attempt int) (string, error) {
		return s.provisionCharacterVoiceAttempt(ctx, book.ID, *char, cloneModel, attempt)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "could not generate voice: "+err.Error())
		return
	}
	if presetID == "" {
		writeError(w, http.StatusConflict, "not enough dialogue attributed to this character yet to characterize a voice")
		return
	}
	writeJSON(w, http.StatusOK, generateVoiceResponse{VoicePresetID: presetID, AudioURL: s.versionedPresetAudioURL(presetID)})
}

// handleGenerateCharacterVoices batch-provisions a voice for every given
// character in one request - SpeakersPage's "Generate all voices",
// fire-and-forget (202 Accepted, no result body), the same rationale as
// handleCharacterizeSpeakers: one request that survives the page closing,
// not N blocking requests capped by the browser's own per-origin
// connection limit.
//
// Each character's provisionCharacterVoice call runs as its own
// jobs.Manager KindVoiceProvision task (see EnqueueVoiceProvision) rather
// than a bare detached goroutine (an earlier version of this handler used
// one): a goroutine calling straight into the TTS worker bypasses
// poolGeneration's own concurrency limit and, more importantly, the
// worker's poolGeneration/poolLLM mutual exclusion (see worker's own
// fill) that keeps this app's two GPU-bound subsystems - the TTS worker
// and the in-process speakerattr LLM - from running at the same time and
// competing for VRAM. EnqueueVoiceProvision's own doc comment covers why
// this is poolGeneration rather than poolLLM despite
// provisionCharacterVoice itself enqueuing a KindSpeakerCharacterization
// (poolLLM) task internally for an uncharacterized character. Already-
// provisioned characters no-op harmlessly, same as the single-character
// button - see provisionCharacterVoice's own doc comment; a character with
// no dialogue/description attributed yet now still gets characterized
// (from its name alone) and provisioned like any other, rather than
// silently no-opping. Unknown/already-deleted
// character ids are skipped, not an error, same reasoning as
// handleCharacterizeSpeakers.
func (s *Server) handleGenerateCharacterVoices(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)

	var body characterIDsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	queued := 0
	for _, characterID := range body.CharacterIDs {
		found, err := s.Store.GetCharacter(characterID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Bulk actions skip a character marked invalid - see
		// store.Character.Invalid.
		if found == nil || found.Invalid {
			continue
		}
		char := *found
		s.Jobs.EnqueueVoiceProvision(book.ID, char.ID, "generate", char.Name, func(ctx context.Context, attempt int) (string, error) {
			return s.provisionCharacterVoiceAttempt(ctx, book.ID, char, cloneModel, attempt)
		})
		queued++
	}

	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
}

// handleRegenerateCharacterVoices batch-forces a fresh reference-clip
// render for every given character in one request - SpeakersPage's
// "Regenerate all voices", distinct from handleGenerateCharacterVoices:
// that one only provisions characters with no voice yet (idempotent,
// no-ops on an already-provisioned one), this one re-renders regardless of
// whether a cached ref already exists - see regenerateCharacterVoice. Same
// EnqueueVoiceProvision/KindVoiceProvision shape and rationale as
// handleGenerateCharacterVoices (its own doc comment covers the
// poolGeneration-not-poolLLM reasoning, which applies identically here -
// regenerateCharacterVoice can also enqueue a KindSpeakerCharacterization
// task internally, for a character with no voice yet at all).
func (s *Server) handleRegenerateCharacterVoices(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)

	var body characterIDsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	queued := 0
	for _, characterID := range body.CharacterIDs {
		found, err := s.Store.GetCharacter(characterID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Bulk actions skip a character marked invalid - see
		// store.Character.Invalid.
		if found == nil || found.Invalid {
			continue
		}
		char := *found
		s.Jobs.EnqueueVoiceProvision(book.ID, char.ID, "regenerate", char.Name, func(ctx context.Context, attempt int) (string, error) {
			return s.regenerateCharacterVoice(ctx, book.ID, char, cloneModel, attempt)
		})
		queued++
	}

	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
}

// handleCharacterizeSpeaker force re-runs characterization for one
// character - the Speakers page's own (and the reader-view annotation
// legend's own) explicit "Regenerate" button, distinct from
// provisionCharacterVoice's own lazy, characterize-once-ever behavior.
// Unlike that path, this always re-runs even if char.Summary is already
// set (a deliberate "redo this with a fuller sample now that there's more
// dialogue" ask - see backend/CLAUDE.md's "Speaker attribution" section).
//
// Dispatched through jobs.Manager.RunCharacterization - same
// KindSpeakerCharacterization task shape (and same TierUrgent/poolLLM
// admission, and dedup-by-characterID against an already-in-flight run)
// its own batch sibling handleCharacterizeSpeakers and the lazy path
// (ensureCharacterized) both already use - rather than calling
// characterizeVoice directly against the request's own context, which an
// earlier version of this handler did: that bypassed the job queue
// entirely, so a manual "Regenerate" click showed up nowhere on the Jobs
// dashboard, couldn't join (or be joined by) a concurrent run for the same
// character, and - the sharper bug - never respected poolGeneration/
// poolLLM's mutual exclusion (see internal/jobs' own "Layout" section in
// backend/CLAUDE.md), so it could run the speakerattr LLM at the exact
// same time as ordinary TTS generation, the two GPU-bound subsystems this
// app is otherwise careful to never let contend for VRAM at once.
// RunCharacterization still blocks this request until the task finishes
// (a single character's own characterization is one short LLM call, the
// same "fine to hold the connection open" reasoning EnqueueAttribution's
// own doc comment gives for why *that* one doesn't), so the response body
// (summary/voiceInvalidated) is unchanged.
//
// See recharacterizeAndInvalidate for what happens after a successful
// re-characterization (every clone model's assigned voice preset for the
// character gets invalidated, not just rebuilt eagerly).
func (s *Server) handleCharacterizeSpeaker(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	characterID := r.PathValue("characterId")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}

	var summary string
	var invalidated bool
	if err := s.Jobs.RunCharacterization(r.Context(), book.ID, char.ID, char.Name, func(ctx context.Context) error {
		var err error
		summary, invalidated, err = s.recharacterizeAndInvalidate(ctx, book, char.ID, func(ctx context.Context) error {
			_, err := s.characterizeVoice(ctx, book, *char)
			return err
		})
		return err
	}); err != nil {
		writeError(w, http.StatusBadGateway, "characterization failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, characterizeResponse{Summary: summary, VoiceInvalidated: invalidated})
}

// handlePreviewCharacterization runs a character's characterization
// (castCharacter) and returns the casting without storing it, so a change
// to the prompts or the retrieval can be judged against the stored casting
// without discarding anyone's voice or audio. ?temp= sets the casting
// call's sampling temperature and ?lookup=0 casts without the
// book lookups.
func (s *Server) handlePreviewCharacterization(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	characterID := r.PathValue("characterId")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil {
		writeError(w, http.StatusNotFound, "character not found")
		return
	}

	opts := castOptions{noLookup: r.URL.Query().Get("lookup") == "0"}
	for _, n := range strings.Split(r.URL.Query().Get("notes"), "\n") {
		if n = strings.TrimSpace(n); n != "" {
			opts.notes = append(opts.notes, n)
		}
	}
	if raw := r.URL.Query().Get("temp"); raw != "" {
		temp, err := strconv.ParseFloat(raw, 32)
		if err != nil || temp < 0 || temp > 2 {
			writeError(w, http.StatusBadRequest, "temp must be a number from 0 to 2")
			return
		}
		t := float32(temp)
		opts.temp = &t
	}
	var cast casting
	if err := s.Jobs.RunCharacterization(r.Context(), book.ID, char.ID, char.Name, func(ctx context.Context) error {
		var err error
		cast, err = s.castCharacter(ctx, book, *char, opts)
		return err
	}); err != nil {
		writeError(w, http.StatusBadGateway, "characterization failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, characterizePreviewResponse{
		Summary:        cast.instruct,
		RefLine:        cast.refLine,
		Lookups:        cast.lookups,
		CurrentSummary: char.Summary,
	})
}

// recharacterizeAndInvalidate runs runFn (a CharacterizationFunc closing
// over the character being re-characterized - see characterizeVoice) and,
// if it produced a non-empty Summary, invalidates every clone model's
// assigned voice preset for that character (see handleCharacterizeSpeaker's
// original doc comment on why every clone model, not just one book's own
// resolved one - Summary is shared series/model-wide, see store.Character)
// so each is rebuilt fresh (voicerefs.EnsureFile) the next time it's
// actually needed - not re-rendered eagerly here. Each preset's own
// Instruct and RefText are both updated from the fresh characterization
// (fresh.Summary/characterRefText(*fresh)) - RefLine is Summary's own
// paired reference passage (see store.Character.RefLine), so a
// recharacterize that changes Summary needs the preset's RefText to catch
// up too, not just Instruct, or the rebuilt clip would keep reading the
// character's previous (or default) reference passage against their new
// casting instruction.
//
// Deliberately called *from inside* the CharacterizationFunc passed to
// jobs.RunCharacterization/EnqueueCharacterization, not after it returns:
// that closure runs against the worker's own long-lived context regardless
// of whether the triggering HTTP request is still around (see
// EnqueueCharacterization's own doc comment), but code living outside it -
// as this invalidation step originally did, directly in
// handleCharacterizeSpeaker - only ever ran if RunCharacterization returned
// normally to a still-listening caller. A request cancelled between
// characterizeVoice finishing and that step running used to leave a
// character with an up-to-date Summary but a stale cached voice preset
// until the next successful re-run; running it in here instead closes that
// gap for both the single-character and batch paths.
func (s *Server) recharacterizeAndInvalidate(ctx context.Context, book *store.Book, characterID string, runFn func(ctx context.Context) error) (summary string, invalidated bool, err error) {
	if err := runFn(ctx); err != nil {
		return "", false, err
	}

	fresh, err := s.Store.GetCharacter(characterID)
	if err != nil {
		return "", false, err
	}
	if fresh == nil {
		return "", false, fmt.Errorf("character %s no longer exists", characterID)
	}
	if fresh.Summary == "" {
		return "", false, nil
	}

	presetIDs, err := s.Store.VoicePresetIDsForCharacter(fresh.ID)
	if err != nil {
		return fresh.Summary, false, err
	}
	for _, presetID := range presetIDs {
		preset, err := s.Store.GetVoicePreset(presetID)
		if err != nil {
			return fresh.Summary, invalidated, err
		}
		// nil for a built-in preset (GetVoicePreset only knows custom,
		// DB-backed ones - see store.go's voice_presets table comment, and
		// a reader can manually assign a character a built-in via
		// handleSetCharacterVoice) - nothing to invalidate there.
		if preset == nil {
			continue
		}
		if err := s.Store.UpdateVoicePreset(preset.ID, preset.Name, fresh.Summary, characterRefText(*fresh), preset.Seed, preset.SpeedMultiplier, preset.DesignModel); err != nil {
			return fresh.Summary, invalidated, err
		}
		if err := voicerefs.Delete(s.DataDir, preset.ID); err != nil {
			log.Printf("httpapi: delete cached reference clip for character %q's voice %s: %v", fresh.Name, preset.ID, err)
			continue
		}
		invalidated = true
	}

	s.recastStages(ctx, book, *fresh)

	// A custom preset's own instruct just changed (invalidated), so every
	// paragraph already generated in this character's old voice - across
	// however many books in their series they've spoken in, not just this
	// one, since character voices are series-wide (see store.SeriesScope)
	// - no longer reflects who they actually sound like now. Best-effort:
	// logged, not surfaced as a request failure, since the characterization
	// itself (this function's actual job) already succeeded by this point;
	// worst case some stale audio lingers reachable until manually cleared,
	// same as before this existed.
	if invalidated {
		bookScope, serr := s.booksInScope(book)
		if serr != nil {
			log.Printf("httpapi: could not resolve series scope to invalidate %q's existing audio: %v", fresh.Name, serr)
			return fresh.Summary, invalidated, nil
		}
		bookIDs := make([]string, len(bookScope))
		for i, b := range bookScope {
			bookIDs[i] = b.ID
		}
		if _, derr := s.Store.DeleteParagraphAudioForSpeaker(bookIDs, fresh.Name); derr != nil {
			log.Printf("httpapi: could not invalidate %q's existing audio: %v", fresh.Name, derr)
			return fresh.Summary, invalidated, nil
		}
	}

	// Every characterization run - not just one that actually invalidated
	// an existing preset - is followed by a KindVoiceProvision task in
	// "regenerate" mode (not "generate": that mode no-ops the instant a
	// preset already exists - see provisionCharacterVoice - which would
	// skip re-rendering the very clip this function just deleted above).
	// regenerateCharacterVoice covers both cases in one call: creates and
	// assigns a fresh preset for a character who's never had one, or
	// re-renders the existing one's now-stale reference clip - so this
	// eagerly pre-warms the character's voice instead of leaving it to be
	// lazily rendered inline the next time generation actually needs it
	// (voicerefs.EnsureFile's own self-heal, still there as a fallback if
	// this fails). taskBlockedBySpeakerCharacterization/kindPriority (see
	// internal/jobs) already make sure this waits behind - and loses
	// same-tier ties to - the characterization task it's queued from, so
	// it can't itself race ahead and provision from a stale Summary.
	// Resolved against this book's own clone model, same as the Speakers
	// page's own explicit "Generate/Regenerate voice" buttons - a
	// character voiced under another clone model too stays on its own
	// lazy self-heal path.
	//
	// When the character already has a custom preset for this clone model -
	// just invalidated above - the re-render is queued as that preset's
	// own base-clip task (jobs.Manager.EnqueueReferenceClip) rather than a
	// "regenerate" keyed by character: lines already queued in their voice
	// look for exactly that task, so they (and their emotion variants) wait
	// for the new clip. Under the character key they couldn't see it, and
	// raced it - rendering the base inline from the old casting, cloning
	// from that, and building variants the new clip then wiped.
	if bookVoice, verr := s.Narration.BookVoice(book); verr != nil {
		log.Printf("httpapi: could not resolve book voice to provision %q: %v", fresh.Name, verr)
	} else {
		cloneModel := narration.EffectiveCloneModel(bookVoice)
		char := *fresh
		presetID, perr := s.Store.CharacterVoiceForModel(char.ID, cloneModel)
		if perr != nil {
			log.Printf("httpapi: look up %q's voice for %s: %v", char.Name, cloneModel, perr)
		}
		if custom, _ := s.Store.GetVoicePreset(presetID); presetID != "" && custom != nil {
			s.Jobs.EnqueueReferenceClip(book.ID, presetID, char.Name)
		} else {
			s.Jobs.EnqueueVoiceProvision(book.ID, char.ID, "regenerate", char.Name, func(ctx context.Context, attempt int) (string, error) {
				return s.regenerateCharacterVoice(ctx, book.ID, char, cloneModel, attempt)
			})
		}
	}

	return fresh.Summary, invalidated, nil
}

// recastStages rewrites the casting of each of char's age stages
// (store.CharacterStage) from their casting as it now stands: a stage's
// instruction is the character's own rewritten for an age, so a recast
// leaves it describing whoever they were cast as before - and since a
// stage's voice is keyed on that instruction (narration.StageVoice), the
// stage's lines would have gone on playing the old voice. One AgeCasting
// call per stage, inside the characterization task that recast them. A
// stage whose rewrite fails is left with no instruction, which plays the
// character's own new voice until the stage is set again; an instruction
// the reader had written by hand is replaced like any other.
func (s *Server) recastStages(ctx context.Context, book *store.Book, char store.Character) {
	if char.IsRole || s.Speaker == nil {
		return
	}
	stages, err := s.Store.ListCharacterStages(char.Scope)
	if err != nil {
		log.Printf("httpapi: list %q's stages to recast them: %v", char.Name, err)
		return
	}
	for _, st := range stages {
		if st.CharacterID != char.ID {
			continue
		}
		instruct, err := s.Speaker.AgeCasting(ctx, char.Summary, st.Age, s.castingWorldNotes(book, char, char.Summary))
		if err != nil {
			log.Printf("httpapi: recast %q at %d: %v", char.Name, st.Age, err)
			instruct = ""
		}
		if _, err := s.Store.SetCharacterStage(st.CharacterID, st.BookID, st.ChapterID, st.Age, instruct, st.JumpID); err != nil {
			log.Printf("httpapi: save %q's recast stage at %d: %v", char.Name, st.Age, err)
		}
	}
}

// handleCharacterizeSpeakers batch-enqueues re-characterization for a list
// of characters in one request - SpeakersPage's "Recharacterize all",
// fire-and-forget (202 Accepted, no result body) via
// jobs.Manager.EnqueueCharacterization rather than one blocking
// POST .../characterize per character (see that method's own doc comment
// for why: the browser's per-origin connection cap plus a page reload
// mid-burst could otherwise silently drop whichever characters' requests
// hadn't gone out yet). Unknown/already-deleted character ids are skipped,
// not an error - a stale id in the client's own list (e.g. a character
// merged/deleted by another tab moments earlier) shouldn't fail the rest
// of the batch. Progress is observable the same way as any other
// characterization - GET /api/jobs, and refetching this book's speaker
// table once each one's task clears.
func (s *Server) handleCharacterizeSpeakers(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	var body characterIDsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	queued := 0
	for _, characterID := range body.CharacterIDs {
		found, err := s.Store.GetCharacter(characterID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Bulk actions skip a character marked invalid - see
		// store.Character.Invalid.
		if found == nil || found.Invalid {
			continue
		}
		char := *found
		s.Jobs.EnqueueCharacterization(book.ID, char.ID, char.Name, func(ctx context.Context) error {
			_, _, err := s.recharacterizeAndInvalidate(ctx, book, char.ID, func(ctx context.Context) error {
				_, err := s.characterizeVoice(ctx, book, char)
				return err
			})
			return err
		})
		queued++
	}

	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
}

// handleVariantAudio serves one emotion variant of a voice preset's
// reference clip (see voicerefs.EnsureVariantFile) - the Speakers page's
// per-emotion preview. 404 until it's been rendered.
func (s *Server) handleVariantAudio(w http.ResponseWriter, r *http.Request) {
	presetID, emotion := r.PathValue("presetId"), r.PathValue("emotion")
	if !emotions.Valid(emotion) || strings.ContainsAny(presetID, "/\\.") {
		writeError(w, http.StatusNotFound, "variant not found")
		return
	}
	path := audiopath.VoicePresetVariantFile(s.DataDir, presetID, emotion)
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "variant not found")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	serveVoiceClip(w, r, path)
}

// handleRoleVariantAudio serves the reference clip of a role voice's
// numbered sibling (narration.MemberVoice - a derived preset id, with no
// stored preset to look up) - the Speakers page's per-number preview. 404
// until it's been rendered.
func (s *Server) handleRoleVariantAudio(w http.ResponseWriter, r *http.Request) {
	presetID := r.PathValue("presetId")
	if !strings.Contains(presetID, "~") || strings.ContainsAny(presetID, "/\\.") {
		writeError(w, http.StatusNotFound, "role variant not found")
		return
	}
	path := audiopath.VoicePresetRefFile(s.DataDir, presetID)
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, "role variant not found")
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	serveVoiceClip(w, r, path)
}

type regenerateVariantRequest struct {
	Speaker string `json:"speaker"`
	// Emotion is the variant to render again; empty is the voice's own
	// reference clip, the neutral one (see regenerateNeutral).
	Emotion string `json:"emotion"`
	// SpeakerIndex picks one numbered member of a role
	// (store.Paragraph.SpeakerIndex) - its sibling voice's variant, and
	// only its lines. 0 for the speaker's own voice.
	SpeakerIndex int `json:"speakerIndex,omitempty"`
	// StageID picks the voice the speaker has from one of their stages on
	// (store.CharacterStage) - its variant, and only that stage's lines.
	StageID string `json:"stageId,omitempty"`
}

// regenerateNeutral is handleRegenerateVariant with no emotion: a fresh
// render of the reference clip one of speaker's voices clones from (voice,
// as resolved for req). Their own preset's is rendered again in place
// (regenerateCharacterVoice, queued); a stage's moves to its next take
// (store.CharacterStage.Take), which is another voice as far as audio
// goes, so that stage's lines - emotional ones too - generate again. A
// role's numbered sibling has no take of its own: it is its role's
// casting under a seed fixed by its number. 202 on success.
func (s *Server) regenerateNeutral(w http.ResponseWriter, book *store.Book, speaker string, req regenerateVariantRequest, voice narration.ResolvedVoice) {
	char, err := s.Store.GetCharacterByName(store.SeriesScope(book), speaker)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if char == nil || char.Name != speaker {
		writeError(w, http.StatusBadRequest, "this speaker has no voice of their own to render again")
		return
	}
	if req.StageID != "" {
		st, err := s.Store.GetCharacterStage(req.StageID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if st == nil || st.CharacterID != char.ID {
			writeError(w, http.StatusNotFound, "stage not found")
			return
		}
		if err := s.Store.NextCharacterStageTake(st.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if st, err = s.Store.GetCharacterStage(st.ID); err == nil && st != nil {
			s.renderStageRef(book, *char, *st)
		}
		writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
		return
	}
	cloneModel := narration.EffectiveCloneModel(voice)
	presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if presetID == "" {
		writeError(w, http.StatusBadRequest, "this speaker has no voice of their own to render again")
		return
	}
	if voice.PresetID != presetID {
		writeError(w, http.StatusBadRequest, "a numbered member's voice follows its role's - render the role's again instead")
		return
	}
	c := *char
	s.Jobs.EnqueueVoiceProvision(book.ID, c.ID, "regenerate", c.Name, func(ctx context.Context, attempt int) (string, error) {
		return s.regenerateCharacterVoice(ctx, book.ID, c, cloneModel, attempt)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// handleRegenerateVariant re-renders the emotion variant speaker's lines in
// bookID clone from (whichever preset they currently generate under - see
// narration.Resolver.ForParagraph): deletes the old take (and any failure
// marker), resets this book's audio for that speaker's lines in that
// emotion to pending, and queues the new render right away so it can be
// previewed. Another book sharing the same preset keeps its already-
// generated audio until regenerated there. 202 on success.
func (s *Server) handleRegenerateVariant(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	var req regenerateVariantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Emotion != "" && !emotions.Valid(req.Emotion) {
		writeError(w, http.StatusBadRequest, "unknown emotion "+strconv.Quote(req.Emotion))
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
	speaker := req.Speaker
	if speaker == "" {
		speaker = "Narrator"
	}
	if req.SpeakerIndex < 0 || req.SpeakerIndex > store.MaxSpeakerIndex {
		writeError(w, http.StatusBadRequest, "unknown speaker number")
		return
	}
	// The voice whose variant this is: the speaker's own (or their
	// number's), or with StageID the one they have from that stage on.
	voices, err := s.Narration.BookVoices(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	chapterIdx, err := s.Store.ChapterIdxByID(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	at := narration.NoChapter
	if req.StageID != "" {
		found := false
		for _, spans := range voices.Stages {
			for _, span := range spans {
				if span.Stage.ID == req.StageID {
					at, found = span.FromIdx, true
				}
			}
		}
		if !found {
			writeError(w, http.StatusNotFound, "stage not found in this book")
			return
		}
	}
	voice, err := voices.ForSpeaker(speaker, req.SpeakerIndex, at)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.Emotion == "" {
		s.regenerateNeutral(w, book, speaker, req, voice)
		return
	}
	if voice.PresetID == "" {
		writeError(w, http.StatusBadRequest, "this speaker's voice has no reference clip to vary")
		return
	}
	if err := voicerefs.DeleteVariant(s.DataDir, voice.PresetID, req.Emotion); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	paragraphs, err := s.Store.ListParagraphsRawForBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var stale []store.Paragraph
	for _, p := range paragraphs {
		name := p.Speaker
		if name == "" {
			name = "Narrator"
		}
		if name != speaker || p.EffectiveEmotion() != req.Emotion {
			continue
		}
		// Re-voiced only when it generates under the regenerated variant's
		// preset - another number of the role, or another stage, has its own.
		if v, err := voices.ForSpeaker(p.Speaker, p.SpeakerIndex, chapterIdx[p.ChapterID]); err == nil && v.PresetID == voice.PresetID {
			stale = append(stale, p)
		}
	}
	s.invalidateParagraphAudio(book, stale)

	s.Jobs.EnqueueEmotionVariant(bookID, speaker, voicerefs.BaseClip{
		PresetID:        voice.PresetID,
		Instruct:        voice.Instruct,
		Seed:            voice.Seed,
		RefText:         voice.RefText,
		SpeedMultiplier: voice.SpeedMultiplier,
		DesignModel:     voice.DesignModel,
	}, req.Emotion)
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}
