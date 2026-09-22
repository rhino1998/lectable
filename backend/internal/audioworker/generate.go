package audioworker

import (
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

// GenerateRequest clones cloneModel's voice, given the caller's own already-
// rendered reference clip (raw samples, not a cache lookup - the Go backend
// owns and persists reference clips on its own disk now; this worker holds
// no per-preset state at all, only per-clone_model loaded model instances).
// audio.cpp's own session-level cache (see cloneFamily.sessionOptions)
// transparently dedupes repeated (reference audio, reference text) pairs,
// so resending the same reference clip on every call is fine.
//
// Deliberately carries no way to override cloneFamily.defaultRequestOptions'
// own "max_tokens" per call - tried once (a per-request override scaled to
// a voice's own observed pace), and reverted: any value requested above
// higgsMaxTokens's own default reproducibly segfaulted audiocpp_session_run
// deep inside audio.cpp itself (confirmed via three separate crash traces,
// all at the exact same native call site, every one of them from a *raised*
// max_tokens - never reproduced at the flat 8192 default this app now
// always sends).
//
// Session reuse after a before-EOC overflow (Worker.Generate's own
// isMaxTokensOverflow handling): the actual safety gate, read directly from
// audio.cpp's own source (engine/models/higgs_audio_tts/generator.cpp), is
// bucketed_initial_cache_steps(prompt_steps, max_tokens) matching between
// consecutive calls on the same warm Session, *and* the reference audio
// matching (generator.cpp's reference_kv_cache_hit) - not max_tokens
// equality alone. prompt_steps depends on that call's own text+reference
// length, which legitimately differs call to call regardless of max_tokens
// being pinned constant - this app already exercises "consecutive warm
// generate() calls with differing prompt_steps" routinely and safely for
// any paragraph over session.cpp's own ~1024-char chunk_text_request
// boundary (this app sends paragraphs up to ~4000 chars - see
// higgsMaxTokens' own comment - so a several-chunk paragraph is several
// sequential generator_->generate() calls against the same warm generator
// within one Session.Run()), but only ever in the all-chunks-succeed case,
// where each chunk's decode completes cleanly (reference_kv_ready_ ends up
// reflecting a real, finished decode) before the next chunk reuses the
// cache.
//
// A before-EOC overflow is different: the decode loop is cut short
// mid-generation, yet reference_kv_ready_ was already set true right after
// prefill (before that decode loop even starts - see the doc comment on
// isMaxTokensOverflow's own call site), so whatever picks this pooled
// Session up next can still take the warm retain_prefix() path against a
// KV-cache that was never actually driven to a clean, finished state for
// this reference - true whether that next caller is a same-request retry
// (prompt_steps identical, definitely safe - the one case this app's own
// 3x automatic retry actually exercises) or, since this session simply
// returns to the shared pool rather than being evicted, some unrelated
// paragraph that happens to share both this reference and this bucket
// (prompt_steps close enough that bucketed_initial_cache_steps lands the
// same) - a combination none of the three confirmed crash traces above
// actually covers (those were all about a *differing* max_tokens value on
// an identical-request retry, a scenario that can no longer occur at all
// now that max_tokens is fixed everywhere), but isn't provably safe either,
// just uncrashed so far. Accepted deliberately, for the real throughput
// this reuse buys (no cold reference-prefill for whoever picks the session
// up next) - revert Worker.Generate to evicting via a fresh
// newCloneSessionLocked call (see git history for the removed
// replacePooledSession, worker.go) if this ever turns out to matter live.
// See jobs.Manager.generateClone's own text-bisection fallback (splitting
// an overflowing paragraph and generating each half separately, both still
// at this same safe default) for how the overflow itself is handled on the
// Go side either way.
type GenerateRequest struct {
	Text          string
	CloneModel    string
	RefSamples    []float32
	RefSampleRate int
	RefChannels   int
	RefText       string
	Language      string
	// Instruct is a clone-time style instruction sent alongside RefSamples
	// (breeze_tts's own "instructed voice cloning" - see cloneFamily.
	// instructOption's own doc comment) - "" (every caller but the
	// instructed-character-voices path) sends nothing extra, same as
	// before this field existed. Silently ignored for any family with no
	// instructOption of its own, the same "caller may resend something
	// this family can't use" shape noReference already has for RefSamples.
	Instruct string
	// GuidanceScale, when non-empty, overrides fam.instructGuidanceScale
	// for this one call - lets a caller (httpapi.handleTestCloneInstruct,
	// the voice/character editor's "test with another voice as a base"
	// control) experiment with different values live, without a server
	// restart/env var change. "" (every caller but that one) defers to the
	// family's own configured default. Same "only applied alongside an
	// actual instruction" scoping as instructGuidanceScale itself - see
	// its own doc comment; ignored entirely on a call with no Instruct.
	GuidanceScale string
}

// isMaxTokensOverflow reports whether err is Higgs generation running out
// of its own max_tokens budget before reaching EOC ("...reached max_tokens
// (N) before EOC for this text chunk...") - see GenerateRequest's own doc
// comment for why a session that just hit this isn't safe to keep reusing
// warm.
func isMaxTokensOverflow(err error) bool {
	return err != nil && strings.Contains(err.Error(), "before EOC for this text chunk")
}

// Generate mirrors clone_backends/audiocpp.py's AudioCppBackend.generate().
func (w *Worker) Generate(req GenerateRequest) (*audiocpp.AudioBuffer, error) {
	w.touch()
	familyKey, ok := cloneModelFamilies[req.CloneModel]
	if !ok {
		return nil, fmt.Errorf("unsupported clone_model %q", req.CloneModel)
	}
	fam := cloneFamilies[familyKey]

	lc, err := w.getCloneModel(req.CloneModel)
	if err != nil {
		return nil, err
	}
	// Reserved for the rest of this call - see loadedClone.inFlight's own
	// doc comment for why this has to cover checkoutSession/Run too, not
	// just the getCloneModel call itself: eviction/UnloadCloneModels must
	// never close this clone_model (or a session on it) while it's still
	// in use here.
	defer w.releaseCloneModel(lc)

	lang := languageOption(req.Language)
	if fam.languageTag != nil {
		lang = fam.languageTag(lang, req.Text)
	}
	if fam.noLanguageOption {
		lang = ""
	}

	text := req.Text
	if fam.wrapText != nil {
		text = fam.wrapText(text)
	}

	request := audiocpp.NewRequest()
	defer request.Close()
	request.SetText(text, lang)
	if fam.noReference {
		// No voice-cloning capability at all (a single fixed built-in
		// voice) - skip SetVoiceAudio/refTextOption entirely rather than
		// sending a reference clip this family has no request option to
		// accept in the first place. Whatever refAudio/refText the caller
		// resent for this clone_model (same as every other family - see
		// GenerateRequest's own doc comment) is simply ignored.
	} else {
		refTextOption := fam.refTextOption
		if refTextOption == "" {
			refTextOption = "reference_text"
		}
		request.SetVoiceAudio(req.RefSamples, req.RefSampleRate, req.RefChannels)
		if !fam.noRefText {
			request.SetOption(refTextOption, req.RefText)
		}
	}
	if fam.estimateDuration {
		request.SetOption("duration_sec", formatSeconds(estimateCloneDuration(req)))
	}
	if fam.instructOption != "" && req.Instruct != "" {
		request.SetOption(fam.instructOption, req.Instruct)
	}
	for k, v := range fam.defaultRequestOptions {
		request.SetOption(k, v)
	}
	// Applied after defaultRequestOptions so this instruct-specific
	// override always wins over any family-wide default for the same key -
	// moot today (no family sets its own default guidance_scale on the
	// clone path), but keeps the more specific setting authoritative if
	// that ever changes. req.GuidanceScale (a live, per-call override - see
	// its own doc comment) takes priority over the family's own configured
	// default when both are set.
	if fam.instructOption != "" && req.Instruct != "" {
		gs := fam.instructGuidanceScale
		if req.GuidanceScale != "" {
			gs = req.GuidanceScale
		}
		if gs != "" {
			request.SetOption("guidance_scale", gs)
		}
	}
	if err := request.Err(); err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// Check out one of this clone_model's pooled sessions rather than
	// contending on a single process-wide lock - see loadedClone/Config.
	// ClonePoolSize's own doc comments. Sessions beyond whatever's already
	// idle are created lazily, on demand, up to the pool cap - see
	// Worker.checkoutSession - so this only actually blocks once that cap
	// is reached and every session is already checked out, the same
	// backpressure a shared lock gave, just with up to ClonePoolSize
	// callers actually running Session.Run (including audio.cpp's own
	// per-call graph rebuild - the real CPU-bound cost here, see
	// backend/CLAUDE.md) concurrently instead of one at a time.
	session, err := w.checkoutSession(lc)
	if err != nil {
		return nil, err
	}
	result, err := session.Run(request)
	if err != nil {
		if isMaxTokensOverflow(err) {
			// Kept warm rather than evicted - see GenerateRequest's own doc
			// comment for exactly what this does and doesn't rule out.
			log.Printf("audioworker: %s session hit a max-tokens overflow - kept warm in the pool rather than evicted", req.CloneModel)
		}
		lc.avail <- session
		return nil, fmt.Errorf("run: %w", err)
	}
	lc.avail <- session
	defer result.Close()

	audio, err := result.Audio()
	if err != nil {
		return nil, fmt.Errorf("read audio: %w", err)
	}
	if audio == nil {
		return nil, fmt.Errorf("generation produced no audio")
	}
	return audio, nil
}

// estimateCloneDuration sizes a fixed-duration family's output (see
// cloneFamily.estimateDuration) for req.Text: the reference clip's own
// seconds-per-character pace, measured from its samples and transcript,
// times req.Text's length - so a voice cloned from a slow, deliberate
// reference gets proportionally more time than a brisk one. Falls back to
// aukCharsPerSecond when there's no transcript to measure a pace from.
func estimateCloneDuration(req GenerateRequest) float64 {
	var refSeconds float64
	if req.RefSampleRate > 0 && req.RefChannels > 0 {
		refSeconds = float64(len(req.RefSamples)) / float64(req.RefChannels) / float64(req.RefSampleRate)
	}
	return textSeconds(req.Text, refSeconds, speechChars(req.RefText))
}

// textSeconds estimates how long text takes to speak at refSeconds per
// refChars characters, or at aukCharsPerSecond if either is zero - never
// less than one second.
func textSeconds(text string, refSeconds float64, refChars int) float64 {
	perChar := 1 / aukCharsPerSecond
	if refSeconds > 0 && refChars > 0 {
		perChar = refSeconds / float64(refChars)
	}
	return math.Max(1, perChar*float64(speechChars(text)))
}

// speechChars counts text's characters with runs of whitespace collapsed
// to one, so indentation or line breaks in a paragraph don't inflate its
// estimated duration.
func speechChars(text string) int {
	return utf8.RuneCountInString(strings.Join(strings.Fields(text), " "))
}

func formatSeconds(s float64) string {
	return strconv.FormatFloat(s, 'f', 2, 64)
}
