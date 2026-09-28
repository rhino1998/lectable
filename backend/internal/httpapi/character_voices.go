package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// maxCharacterizeQuotes bounds how many of a character's quotes (across
// however many books in their series) get pulled for characterization -
// see Server.provisionCharacterVoice. Matches speakerattr's own internal
// cap - see its own doc comment for why this was lowered from an original
// 30 (a real, observed slow characterization task for a prominent
// character, traced to Store.QuotesForBooks' own pre-LIMIT sort cost, not
// the LLM call itself) - kept here too so the store query doesn't fetch
// more rows than will ever be used.
const maxCharacterizeQuotes = 20

// maxCharacterizeDescriptions is maxCharacterizeQuotes' counterpart for
// description paragraphs - matches speakerattr's own internal cap (see its
// own doc comment), kept here too so Store.DescriptionsForBooks doesn't
// fetch more rows than will ever be used.
const maxCharacterizeDescriptions = 10

// characterizeVoice asks the LLM (speakerattr.Client.CharacterizeVoice) to
// describe how char actually sounds, from a sample of *only their own*
// attributed dialogue plus narration that describes them (see
// store.QuotesForBooks/DescriptionsForBooks) gathered across every book in
// their series so far (see store.SeriesScope), and persists the result -
// both the casting instruction and its paired reference passage - to
// char.Summary/RefLine - the pure LLM+DB step behind a
// jobs.KindSpeakerCharacterization task (see jobs.Manager.RunCharacterization),
// shared by provisionCharacterVoice's lazy-provisioning flow and
// handleCharacterizeSpeaker's explicit "Regenerate" button.
//
// Always actually calls the LLM now, even when quotes and descriptions
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
	books, err := s.booksInScope(book)
	if err != nil {
		return "", fmt.Errorf("list scope books: %w", err)
	}
	bookIDs := make([]string, len(books))
	for i, b := range books {
		bookIDs[i] = b.ID
	}

	quotes, err := s.Store.QuotesForBooks(bookIDs, char.Name, maxCharacterizeQuotes)
	if err != nil {
		return "", fmt.Errorf("gather quotes: %w", err)
	}
	descriptions, err := s.Store.DescriptionsForBooks(bookIDs, char.Name, maxCharacterizeDescriptions)
	if err != nil {
		return "", fmt.Errorf("gather descriptions: %w", err)
	}
	speakerQuotes := make([]speakerattr.QuoteContext, len(quotes))
	for i, q := range quotes {
		speakerQuotes[i] = speakerattr.QuoteContext{Text: q.Text, Context: q.Context}
	}
	speakerDescriptions := make([]speakerattr.DescriptionContext, len(descriptions))
	for i, d := range descriptions {
		speakerDescriptions[i] = speakerattr.DescriptionContext{Text: d.Text}
	}

	instruct, refLine, err := s.Speaker.CharacterizeVoice(ctx, char.Name, char.IsRole, char.Pronouns, speakerQuotes, speakerDescriptions)
	if err != nil {
		return "", fmt.Errorf("characterize: %w", err)
	}
	if err := s.Store.SetCharacterSummary(char.ID, instruct, refLine); err != nil {
		return "", fmt.Errorf("save characterization: %w", err)
	}
	return instruct, nil
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

// normalizeCharacterVoiceVolume rescales presetID's just-rendered
// reference clip (voicerefs.NormalizeVolume) so its overall loudness
// matches book's own narrator voice - an auto-provisioned character's
// independent VoiceDesign render can otherwise land much louder or
// quieter than the book's voice, and that difference carries straight
// into every paragraph cloned from it. Best-effort and silent on failure,
// the same tolerance provisionCharacterVoiceAttempt/regenerateCharacterVoice
// already give the render itself - a reader still gets *a* voice for the
// character even if it can't be leveled against the narrator (e.g. the
// book's own voice hasn't rendered anything yet either).
//
// Calls voicerefs.EnsureFile directly rather than s.ensureVoiceRef -
// exactly like provisionCharacterVoiceAttempt/regenerateCharacterVoice's
// own render calls just above/below each call site, and for the same
// reason (see ensureVoiceRef's own doc comment): both callers can
// themselves already be running inside a dispatched poolDesign task, and
// s.ensureVoiceRef's RunVoiceProvision would deadlock nesting a second
// poolDesign task from inside the first.
func (s *Server) normalizeCharacterVoiceVolume(ctx context.Context, book *store.Book, presetID string) {
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		log.Printf("httpapi: normalize voice volume for preset %s: resolve book voice: %v", presetID, err)
		return
	}
	if bookVoice.PresetID == "" {
		return // fully custom instruct, no rendered clip to normalize against
	}
	if _, err := voicerefs.EnsureFile(ctx, s.TTS, s.DataDir, bookVoice.PresetID, bookVoice.Instruct, bookVoice.Seed, bookVoice.RefText, bookVoice.SpeedMultiplier, bookVoice.DesignModel); err != nil {
		log.Printf("httpapi: normalize voice volume for preset %s: ensure book voice reference clip: %v", presetID, err)
		return
	}
	if err := voicerefs.NormalizeVolume(s.DataDir, presetID, bookVoice.PresetID); err != nil {
		log.Printf("httpapi: normalize voice volume for preset %s: %v", presetID, err)
	}
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
	} else {
		s.normalizeCharacterVoiceVolume(ctx, book, preset.ID)
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
// holds poolGeneration active and un-drained (see taskqueue.Queue's own
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
	s.normalizeCharacterVoiceVolume(ctx, book, preset.ID)
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
	writeJSON(w, http.StatusOK, generateVoiceResponse{VoicePresetID: presetID, AudioURL: presetAudioURL(presetID)})
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
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
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
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
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
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
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
		refs, derr := s.Store.DeleteParagraphAudioForSpeaker(bookIDs, fresh.Name)
		if derr != nil {
			log.Printf("httpapi: could not invalidate %q's existing audio: %v", fresh.Name, derr)
			return fresh.Summary, invalidated, nil
		}
		for _, ref := range refs {
			if rerr := audiopath.RemoveClip(s.paragraphAudioPath(ref.BookID, ref.ChapterID, ref.VoiceID, ref.Idx)); rerr != nil {
				log.Printf("httpapi: remove stale audio file for %q: %v", fresh.Name, rerr)
			}
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
	if bookVoice, verr := s.Narration.BookVoice(book); verr != nil {
		log.Printf("httpapi: could not resolve book voice to provision %q: %v", fresh.Name, verr)
	} else {
		cloneModel := narration.EffectiveCloneModel(bookVoice)
		char := *fresh
		s.Jobs.EnqueueVoiceProvision(book.ID, char.ID, "regenerate", char.Name, func(ctx context.Context, attempt int) (string, error) {
			return s.regenerateCharacterVoice(ctx, book.ID, char, cloneModel, attempt)
		})
	}

	return fresh.Summary, invalidated, nil
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
		writeError(w, http.StatusServiceUnavailable, "speaker characterization is not configured (set SPEAKER_LLM_MODEL_PATH)")
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
	http.ServeFile(w, r, path)
}

type regenerateVariantRequest struct {
	Speaker string `json:"speaker"`
	Emotion string `json:"emotion"`
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
	if !emotions.Valid(req.Emotion) {
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
	voice, err := s.Narration.ForParagraph(book, speaker)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
		if name == speaker && p.EffectiveEmotion() == req.Emotion {
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
