// Package speakerattr attributes each paragraph of a chapter to whichever
// character speaks it (or "Narrator" for prose) by prompting a
// general-purpose LLM. Client is a thin caller-side wrapper (batching,
// JSON-parsing, malformed-response retry, all the domain logic below) - the
// actual model (a GGUF file, embedded via llamacpp-go, a cgo binding around
// llama.cpp) is loaded and run by internal/llmworker, hosted inside the
// separate ttsworker process alongside internal/audioworker's own TTS
// clone models (see backend/CLAUDE.md's "ttsworker / audioworker" section).
// llama.cpp itself carries none of audio.cpp's confirmed native leak - it
// lives there anyway so both native model families share one process's
// VRAM budget and can be idle-unloaded against each other rather than both
// sitting permanently resident on top of whatever the other needs.
// Client talks to it the same way the rest of the backend talks to
// ttsworker's TTS endpoints - see the llmBackend interface below, and
// internal/ttsworker.Manager.LLMGenerate for the real (HTTP) implementation
// cmd/server wires in.
//
// This is a deliberate choice over an encoder-based NLP pipeline like
// BookNLP/ModernBookNLP: every actively-maintained open-source audiobook
// generator surveyed while designing this (Alexandria, AudioBard,
// audiobook-creator, storycast, Castwright) independently converged on
// LLM-prompted speaker tagging rather than BookNLP-style
// entity/coref/quote models, and recent literary-NLP research agrees -
// "Evaluating LLMs for Quotation Attribution in Literary Texts" (NAACL
// 2025) found prompted Llama-3 beats BookNLP's own encoder baseline. The
// tradeoff is speed (an LLM call per chapter batch vs. a sub-second encoder
// pass), which is acceptable here since attribution is a one-time,
// explicitly-triggered preprocessing step (httpapi's
// POST .../attribute-speakers), not something run on every read.
//
// See internal/narration for how an attributed paragraph's Speaker then
// maps to an actual narration voice.
package speakerattr

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Config carries the caller-side prompting knobs that stay in this package
// even though the model itself moved to internal/llmworker (see the
// package doc comment) - unlike ModelPath/NGPULayers/NCtx, which are the
// ttsworker process's own concern now (see llmworker.Config, wired up by
// cmd/ttsworker/main.go from the same SPEAKER_LLM_* env vars this package
// used to read directly).
type Config struct {
	// NoThink appends Qwen3's documented "/no_think" marker to every
	// attribution batch's user turn, asking a hybrid-thinking model (see
	// llmworker.stripThinking) to skip its <think>...</think> reasoning
	// trace for that turn - a per-message instruction the model itself
	// honors, unlike enable_thinking, which is a chat-template Jinja
	// variable the raw llama_chat_apply_template C API has no way to pass
	// at all. Appended to the user prompt rather than the system prompt so
	// it doesn't require a second, differently-primed system-prompt
	// variant (see llmworker.Config.SystemPrompts, keyed on the exact
	// system-prompt string). Off by default - see ConfigFromEnv.
	NoThink bool
}

// DefaultNCtx comfortably covers one attribution/emotion batch (up
// to maxBatchParagraphs/emotionBatchParagraphs
// paragraphs of prose plus that pass's own maxTokens of generated JSON)
// or one characterization call (up to maxCharacterizeQuotes quotes plus
// characterizeMaxTokens of generated instruction) - exported so cmd/
// ttsworker/main.go, which now owns SPEAKER_LLM_CTX's own default (see
// llmworker.Config.NCtx), doesn't need to duplicate this number. Doubled
// from an original 12288 alongside attributeMaxTokens/
// characterizeMaxTokens (and the since-removed direction/sfx passes' own
// budgets) all roughly doubling too - this is
// llmworker.Config.NCtx's own *per-slot* context size (see its doc
// comment), so doubling it doubles this model's total KV-cache footprint
// (already multiplied by SPEAKER_LLM_MAX_CONCURRENT slots, independent of
// this constant) - a real but modest VRAM cost for a ~4B-class model,
// worth it to stop clipping longer chapters/more dialogue-heavy batches
// against the old ceiling. Modern GGUF models increasingly train with a
// very large native context (e.g. Qwen3's 262144), and defaulting to that
// (llmworker.Config.NCtx's own zero value) is a real footgun - a KV cache
// sized for it can fail outright to allocate ("failed to allocate buffer
// for kv cache") alongside another already-VRAM-resident model, for a
// batch-of-40-paragraphs task that never needed anywhere near that many
// tokens anyway. 0 is still available by setting SPEAKER_LLM_CTX=0
// explicitly, for a model that genuinely needs more than DefaultNCtx.
const DefaultNCtx = 49152

// MaxOutputTokens is the largest maxTokens value any batch call in this
// package will ever pass to LLMGenerate - llmworker.Config sizes its shared KV-cache
// pool off this per concurrent generation slot (see its own NCtx doc
// comment), since that's the real per-slot worst case: a batch's own
// input prompt is always far smaller than its own maxTokens ceiling (see
// DefaultNCtx's own doc comment, which already sizes for input+output
// combined and is comfortably larger than any real prompt), so budgeting
// maxTokens alone per slot leaves real margin for input too without
// needing a separate, harder-to-get-right prompt-length estimate.
func MaxOutputTokens() int {
	max := attributeMaxTokens
	for _, t := range []int{
		characterizeMaxTokens,
		emotionMaxTokens,
		pronunciationMaxTokens,
		musicMaxTokens,
		musicDescribeMaxTokens,
	} {
		if t > max {
			max = t
		}
	}
	return max
}

// ConfigFromEnv reads SPEAKER_LLM_NO_THINK - see NewClientFromEnv. The
// model-loading env vars (SPEAKER_LLM_MODEL_PATH/GPU_LAYERS/CTX/
// MAX_CONCURRENT) are read by cmd/ttsworker/main.go instead now (see this
// package's own doc comment) - NewClientFromEnv still resolves the model
// path itself (ModelPathFromEnv), but only to decide whether the feature is
// available at all.
func ConfigFromEnv() Config {
	cfg := Config{}
	if v := strings.TrimSpace(os.Getenv("SPEAKER_LLM_NO_THINK")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.NoThink = b
		}
	}
	return cfg
}

// llmBackend is the model call Client actually needs - satisfied by
// *internal/ttsworker.Manager (the real, HTTP-backed implementation
// cmd/server wires in via NewClientFromEnv/NewClient) and directly by
// *internal/llmworker.Worker too (used by cmd/benchattr, a standalone
// benchmarking tool with no ttsworker process of its own to talk to - see
// its own main.go). A small local interface rather than a concrete
// dependency on either package keeps this package free of any import that
// would drag llamacpp-go (transitively, via llmworker) into cmd/server -
// the same "never link libaudiocpp.so into cmd/server" shape
// backend/CLAUDE.md's hard invariant already established for TTS, now
// covering the LLM too even though nothing enforces it as strictly (unlike
// audio.cpp, llama.cpp carries no confirmed leak - see this package's own
// doc comment for why it still lives in ttsworker anyway).
type llmBackend interface {
	LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error)
}

// Client is a thin caller-side wrapper around an llmBackend: batching,
// prompt-building, JSON-parsing, and malformed-response retry all live
// here (see AttributeChapter/DescribeChapter/CharacterizeVoice/
// EmotionChapter/ResolvePronunciation and generateAndParse below) -
// the model itself is neither loaded nor run by this package any more (see
// the package doc comment).
type Client struct {
	cfg Config
	llm llmBackend
}

// DefaultModelFile is the GGUF speaker attribution loads from ~/llm-models
// when SPEAKER_LLM_MODEL_PATH isn't set - Qwen3-4B-Instruct-2507, Q4_K_M.
const DefaultModelFile = "qwen3-4b-instruct-2507-q4_k_m.gguf"

// DefaultModelPath returns ~/llm-models/DefaultModelFile (or just
// llm-models/DefaultModelFile, relative to the working directory, if the
// home directory can't be determined).
func DefaultModelPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("llm-models", DefaultModelFile)
	}
	return filepath.Join(home, "llm-models", DefaultModelFile)
}

// ModelPathFromEnv returns SPEAKER_LLM_MODEL_PATH, or DefaultModelPath() if
// it's unset. Shared by cmd/server (NewClientFromEnv's enablement gate) and
// cmd/ttsworker (what llmworker actually loads) so the two can't disagree.
func ModelPathFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("SPEAKER_LLM_MODEL_PATH")); v != "" {
		return v
	}
	return DefaultModelPath()
}

// NewClientFromEnv returns nil if ModelPathFromEnv's file doesn't exist -
// speaker attribution is an optional feature (see httpapi's s.Speaker ==
// nil check), not a hard dependency of the rest of the app, so a box
// without the default model just runs with it disabled. Checking here
// (rather than only inside cmd/ttsworker, which is what actually loads the
// model - see the package doc comment) is deliberate: it lets cmd/server
// decide whether the feature is enabled at startup, without a round trip
// to ttsworker. Both processes run on the same machine, so the file check
// is valid for the worker too.
func NewClientFromEnv(llm llmBackend) *Client {
	if _, err := os.Stat(ModelPathFromEnv()); err != nil {
		return nil
	}
	return NewClient(ConfigFromEnv(), llm)
}

func NewClient(cfg Config, llm llmBackend) *Client {
	return &Client{cfg: cfg, llm: llm}
}

// Close is a no-op kept only for source compatibility with existing call
// sites (cmd/server's own defer, tests constructing a zero-value Client) -
// the model this Client used to own directly now lives in llmworker,
// inside the separate ttsworker process, with its own lifecycle
// (idle-unload, watchdog restart) entirely independent of this Client's
// own.
func (c *Client) Close() {}

// generate runs one independent single-turn chat completion (system + user
// message in, assistant reply text out) via c.llm - see llmBackend and
// internal/llmworker.Worker.LLMGenerate for where the actual model call,
// primed-system-prompt reuse, and multi-sequence-batched concurrency now
// live. Unlike this method's own previous in-process implementation, it no
// longer serializes concurrent callers behind a mutex - c.llm's own
// implementation (ttsworker.Manager.LLMGenerate, proxying to llmworker)
// allows up to llmworker.Config.MaxConcurrent calls to run genuinely
// concurrently, sharing the one loaded model via real multi-sequence
// batching rather than each paying for (and blocking behind) its own
// exclusive turn.
func (c *Client) generate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return c.llm.LLMGenerate(ctx, systemPrompt, userPrompt, temp, maxTokens)
}

// maxGenerateRetries bounds how many times generateAndParse re-asks the
// model for the same batch after its own response fails to parse/validate
// - a malformed JSON array (a stray token, a truncated response cut off
// by maxTokens) is usually a one-off model hiccup, not a systemic
// failure, and asking again (a fresh generation under the same sampling
// params can easily come out well-formed the second time) is far cheaper
// than losing an entire batch's worth of already-completed work - see
// generateAndParse's own callers, which previously abandoned every
// remaining paragraph in a chapter over exactly this kind of single
// malformed response (confirmed in production: one bad JSON array failed
// a whole KindSpeechDirection task outright, with no automatic retry).
// Small and fixed rather than configurable: this is meant to smooth over
// a rare hiccup, not paper over a systemically broken prompt/model
// combination - if every attempt fails, something is actually wrong and
// the caller should still hear about it.
const maxGenerateRetries = 2

// retryTempBump is the sampling temperature substituted for a batch's own
// temp on retry attempt 1 (the first retry), but only when that batch
// normally runs at temp 0 (greedy/deterministic) - see retryParse's own
// doc comment. A batch parsed via parseAttributions/parseDescribeItems
// runs at temp 0 specifically because greedy decoding
// suits a structured-JSON task better than sampling does (less
// hallucination, more consistent formatting) - confirmed in production,
// this determinism is exactly what makes a malformed response like a
// dropped closing quote (see parseAttributions' own doc comment) a
// *guaranteed* repeat failure under the old "just call generate again"
// retry, since the same prompt at temp 0 always decodes the same tokens.
// Bumping just the retry attempts off greedy gives them an actual chance
// at a different (hopefully well-formed) token sequence, without giving
// up temp 0's benefits on the common, successful-first-try path. Kept
// low - this is meant to nudge a deterministic call off whichever exact
// token sequence caused the failure, not turn a structured-JSON task into
// a genuinely creative one. A batch that already samples above 0
// (CharacterizeVoice, temp 0.3) is left at its own temp on retry instead:
// llama.cpp's own sampler already draws a fresh random seed per call
// there (see llamacpp-go/llamacpp/sampler.go's Seed doc comment -
// SamplerParams.Seed 0 means LLAMA_DEFAULT_SEED, entropy-seeded), so
// every retry already has natural variance without help.
//
// Escalates by another full retryTempBump on each attempt beyond the
// first (attempt 2 gets 2*retryTempBump, and so on, via generateAndParse's
// own callTemp := float32(attempt) * retryTempBump) rather than staying
// flat across every retry - a real, observed production case (a chapter's
// own speech-direction batch containing a bare quoted exclamation, e.g.
// "What . . . ?", as one paragraph's entire text) kept producing the same
// kind of malformed JSON at the flat 0.2 bump on both retries, tying up
// this batch permanently across every single top-level task attempt for
// hours. A still-low first nudge preserves temp 0's own formatting
// benefits on the common single-retry-fixes-it case, while a batch
// stubborn enough to need a second retry gets more room to actually land
// on a different, well-formed token sequence instead of repeating the same
// near-miss twice.
//
// That original case was a (since-removed) speech-direction batch; this
// escalation applies to whichever batches are still JSON-parsed
// (attribution, description, characterization).
const retryTempBump = 0.2

// generateAndParse calls c.generate, then parse on its own output - a thin
// per-call wrapper around retryParse (below), which holds the actual
// retry loop in a form that doesn't need a real *Client to exercise in a
// test. Only the first attempt (attempt 0) uses temp as given; see
// retryTempBump for what each retry attempt after it escalates to instead.
// A low-but-nonzero temp (below retryTempBump) still escalates the same
// way, never dropping below its own starting value.
func generateAndParse[T any](ctx context.Context, c *Client, systemPrompt, userPrompt string, temp float32, maxTokens int, parse func(content string) (T, error)) (T, error) {
	return retryParse(func(attempt int) (string, error) {
		callTemp := temp
		if attempt > 0 && temp < retryTempBump {
			callTemp = max(temp, float32(attempt)*retryTempBump)
		}
		return c.generate(ctx, systemPrompt, userPrompt, callTemp, maxTokens)
	}, parse)
}

// retryParse calls generate, then parse on its own output, retrying the
// *whole* round trip (calling generate again, not just re-parsing the
// same broken text) up to maxGenerateRetries times when parse itself
// returns an error - see maxGenerateRetries' own doc comment for the full
// reasoning. generate receives the 0-based attempt number so a caller
// like generateAndParse can vary sampling params (see retryTempBump) on a
// retry without this loop needing to know anything about temperature
// itself. Never retries a generate error itself (a context
// cancellation or a genuine model/inference failure) - only a
// parse/validate failure on an otherwise-successful generation, since
// retrying a cancelled context is pointless and a different kind of
// problem than a malformed response.
func retryParse[T any](generate func(attempt int) (string, error), parse func(content string) (T, error)) (T, error) {
	var zero T
	var lastErr error
	for attempt := 0; attempt <= maxGenerateRetries; attempt++ {
		content, err := generate(attempt)
		if err != nil {
			return zero, err
		}
		result, perr := parse(content)
		if perr == nil {
			return result, nil
		}
		lastErr = perr
		if attempt < maxGenerateRetries {
			log.Printf("speakerattr: response failed to parse, retrying (attempt %d/%d): %v", attempt+1, maxGenerateRetries+1, perr)
		}
	}
	return zero, fmt.Errorf("giving up after %d attempts: %w", maxGenerateRetries+1, lastErr)
}

// ParagraphInput is one paragraph offered up for attribution.
type ParagraphInput struct {
	Idx  int
	Text string
	// Inline mirrors store.Paragraph.Inline - true when this is a split-
	// out continuation of the immediately preceding ParagraphInput (a
	// quote/narration segment split out of one mixed source paragraph),
	// rather than the start of a new one. attributeBatch uses this to mark
	// where the model's own view of "Lines:" should show a paragraph
	// break, so it isn't handed a flat list of fragments with no
	// indication that e.g. a quote, a "Bramwell said" tag, and another
	// quote right after it all came from one continuous beat.
	Inline bool
	// IsQuote mirrors store.Paragraph.IsQuote - true when this paragraph
	// is an actual quoted-dialogue span rather than narration/description.
	// EmotionChapter only ever labels a line with this set - narration is
	// shown to the model as context but always stays neutral.
	IsQuote bool
}

// maxBatchParagraphs caps how many paragraphs go into one completion
// request - keeps prompts within a reasonable context window regardless of
// chapter length, and keeps one malformed model response from losing
// attribution for an entire long chapter.
const maxBatchParagraphs = 40

// attributeMaxTokens bounds one attribution batch's generated JSON - a
// batch of maxBatchParagraphs {"idx":N,"speaker":"Name"} entries fits
// comfortably within this even for long character names, with headroom
// left over for a "hybrid thinking" model's own <think>...</think>
// reasoning trace before the JSON itself (see characterizeMaxTokens' doc
// comment - same reasoning applies here). Doubled from an original 12288
// after real production failures on longer chapters/more dialogue-heavy
// batches ran into it - see DefaultNCtx's own doc comment, bumped
// alongside this so the larger budget actually has context-window room to
// use, not just a bigger ceiling that gets clipped anyway.
const attributeMaxTokens = 24576

// AttributeChapter attributes chapterTitle's paragraphs to speakers,
// batching in groups of maxBatchParagraphs. knownCharacters (already-seen
// character names, typically from earlier chapters of the same book) is
// passed in every batch's prompt, and grows as each batch's own newly-seen
// names are folded in, so the model reuses one consistent name for a
// recurring character across the whole call instead of drifting to a
// variant spelling mid-chapter. Returns a map from paragraph Idx to speaker
// name ("Narrator" for prose, "Unknown" if the model itself couldn't tell) -
// a paragraph the model dropped or that failed to parse is simply absent
// from the result rather than failing the whole call.
//
// See DescribeChapter for the separate, single-purpose call that tags which
// narration paragraphs describe a character - folding that into this same
// call (asking the model for both a speaker judgment and a description
// judgment per line in one pass) was tried and measurably hurt precision
// (see cmd/benchattr's own -describe-only flag and its git history): asking
// two judgments per line across a large batch made the model lapse into
// blanket-tagging nearly every line touching a salient character as a
// "description," rather than exercising real per-line judgment. Isolating
// it into its own smaller-batched, single-purpose call fixed that.
//
// knownDescriptions (nil-safe: a nil or missing entry just means no blurb)
// pairs some of knownCharacters' own names with a short identifying blurb -
// typically DescribeChapter's own earlier output for them (see
// httpapi.knownCharacterDescriptions, the only real caller) - included in
// the "Known characters" prompt line specifically so a narration tag that
// refers to someone by role/epithet rather than name ("the captain said,"
// "the old orc grunted") has something to match against: the blurb often
// repeats the same role/epithet the narration itself uses. Speaker itself
// is always resolved to the character's own proper name regardless (see
// systemPrompt) - the blurb is read-only context, never a value this
// returns.
//
// knownRoles (nil-safe) marks which of knownCharacters' own names are
// generic role/type labels rather than persistent individuals (see
// store.Character.IsRole) - fed back into the "Known characters" prompt
// line as a "[role]" marker (attributeBatch) so the model reuses an
// already-established role label consistently instead of guessing whether
// it's meeting the same specific person again. roleNames, returned
// alongside out, is the read side of the same fact: every final
// (canonicalized) speaker name this call itself judged to be a role at
// least once - a caller persists it via store.UpsertCharacter's own isRole
// parameter the same way it persists out via Store.SetParagraphSpeakers.
//
// shouldPause (nil-safe: nil never pauses) is checked between batches -
// never before the first one, so a single dispatch of this call always
// makes at least some real progress rather than a run of back-to-back
// higher-priority arrivals starving it forever. If it returns true,
// AttributeChapter stops immediately rather than starting another batch,
// returning every batch it *did* complete (in the same map it would have
// returned anyway - out reflects real, finished work, not a rollback) plus
// the paragraphs it never got to (remaining, in their original input
// order) so the caller can persist what's genuinely done and requeue just
// the rest, later, behind whatever shouldPause is now reporting instead of
// making that more urgent work wait out however many batches were left -
// see jobs.Manager.HasHigherPriorityWork and
// httpapi.attributeChapter's own use of this. remaining is also non-nil
// (though for a different reason - a genuine failure, not a pause) when
// err != nil: whichever batch failed and anything after it, so the
// caller's persisted progress still only reflects paragraphs that actually
// got a real result. canonicalizeSpeakerNames runs once, at the very end,
// over whatever was actually completed this call - a paused run's later
// resumption (a separate AttributeChapter call) re-canonicalizes its own
// batches independently rather than against the full chapter's eventual
// name pool, which in rare cases (a short-form name's only "canonicalizing"
// longer mention lands in a not-yet-reached batch) can leave a short form
// uncollapsed until a later explicit re-attribution - the same
// best-effort, reader-correctable tradeoff canonicalizeSpeakerNames' own
// doc comment already describes for cross-chapter/book name variants.
func (c *Client) AttributeChapter(ctx context.Context, bookTitle, chapterTitle string, knownCharacters []string, knownDescriptions map[string]string, knownRoles map[string]bool, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int]string, roleNames map[string]bool, remaining []ParagraphInput, err error) {
	out = make(map[int]string, len(paragraphs))
	isRoleByIdx := make(map[int]bool, len(paragraphs))
	known := append([]string(nil), knownCharacters...)
	knownIsRole := make(map[string]bool, len(knownRoles))
	for name, isRole := range knownRoles {
		knownIsRole[name] = isRole
	}
	isQuoteByIdx := make(map[int]bool, len(paragraphs))
	for _, p := range paragraphs {
		isQuoteByIdx[p.Idx] = p.IsQuote
	}

	finish := func(out map[int]string) (map[int]string, map[string]bool, []ParagraphInput, error) {
		canon := canonicalizeSpeakerNames(knownCharacters, out)
		roleNames = make(map[string]bool, len(isRoleByIdx))
		for idx, isRole := range isRoleByIdx {
			if !isRole {
				continue
			}
			if name := canon[idx]; name != "" && name != "Narrator" && name != "Unknown" {
				roleNames[name] = true
			}
		}
		return canon, roleNames, remaining, err
	}

	for start := 0; start < len(paragraphs); start += maxBatchParagraphs {
		if start > 0 && shouldPause != nil && shouldPause() {
			remaining = paragraphs[start:]
			return finish(out)
		}
		end := start + maxBatchParagraphs
		if end > len(paragraphs) {
			end = len(paragraphs)
		}
		batch := paragraphs[start:end]

		attributions, batchErr := c.attributeBatch(ctx, bookTitle, chapterTitle, known, knownDescriptions, knownIsRole, batch)
		if batchErr != nil {
			remaining = paragraphs[start:]
			err = fmt.Errorf("paragraphs %d-%d: %w", batch[0].Idx, batch[len(batch)-1].Idx, batchErr)
			return finish(out)
		}
		for idx, a := range attributions {
			speaker := normalizeBarePronoun(a.Speaker)
			speaker = disallowNarratorForDialogue(isQuoteByIdx[idx], speaker)
			out[idx] = speaker
			known = addKnown(known, speaker)
			if a.Role && speaker != "" && speaker != "Narrator" && speaker != "Unknown" {
				isRoleByIdx[idx] = true
				knownIsRole[speaker] = true
			}
		}
	}
	return finish(out)
}

// cleanDescribes trims and dedupes a raw "describes" list from one line's
// own attribution, dropping anything that isn't a real, usable proper name:
// blanks, the Speaker sentinels ("Narrator"/"Unknown"), and bare pronouns a
// model might emit instead of leaving someone unnamed (see systemPrompt's
// own "leave them out ... rather than inventing a name or using a generic
// label/pronoun" rule, which this backstops the same way
// normalizeBarePronoun backstops the equivalent Speaker rule). Returns nil
// (never an empty non-nil slice) when nothing survives, so a paragraph
// with no real description collapses to the same "no entry" shape
// AttributeChapter's caller already treats as "describes no one".
func cleanDescribes(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, name := range raw {
		name = strings.TrimSpace(name)
		if name == "" || name == "Narrator" || name == "Unknown" {
			continue
		}
		lower := strings.ToLower(name)
		switch lower {
		case "i", "me", "myself", "he", "she", "they", "him", "her", "them", "his", "hers", "their":
			continue
		}
		// A real proper name never starts with an article - a generic role
		// label like "the middle-aged man" or "a woman in the crowd" does,
		// even after heavy physical detail (a real, observed failure: the
		// model tagged "the middle-aged man" as a describes-target for a
		// paragraph that never gave him an actual name).
		if strings.HasPrefix(lower, "the ") || strings.HasPrefix(lower, "a ") || strings.HasPrefix(lower, "an ") {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeBarePronoun maps a raw speaker value that's just a bare
// pronoun - first-person ("I"/"me"/"myself") or third-person ("he"/"she"/
// "him"/"her"/"they"/"them") - back to "Unknown". systemPrompt's own "a
// bare pronoun is never a valid speaker value" rule isn't perfectly
// followed, and the model sometimes leaves the pronoun itself in place -
// whether a first-person narrator referring to themselves or a
// third-person reference to someone else - instead of resolving it to the
// actual character's proper name or falling back to "Unknown" as
// instructed. "Unknown" is the only safe fallback for either: this
// package no longer has any "the narrator's own dialogue is Narrator"
// special case for a bare pronoun to resolve to (see
// disallowNarratorForDialogue) - a bare pronoun carries no name identity
// of its own, first- or third-person alike. Applied to each batch's raw
// result as soon as it comes back, before it's folded into known/out - so
// a stray pronoun never gets offered to a later batch as a "known
// character" to reuse (addKnown), and never reaches
// canonicalizeSpeakerNames, which would otherwise treat it as a real name
// candidate for whole-word merging against an actual character's name.
func normalizeBarePronoun(speaker string) string {
	switch strings.ToLower(strings.TrimSpace(speaker)) {
	case "i", "me", "myself", "he", "she", "him", "her", "they", "them":
		return "Unknown"
	default:
		return speaker
	}
}

// disallowNarratorForDialogue enforces systemPrompt's own "'Narrator' is
// reserved exclusively for narration/description - never for a line of
// spoken dialogue, no matter who's speaking it" rule in Go, the same
// "don't just ask nicely, enforce it" pattern this package uses elsewhere
// (e.g. deliverytags' own insertion-diffing invariant). isQuote is the
// paragraph's own ParagraphInput.IsQuote - true for a quoted span not
// (yet) flagged as a scare quote (callers pass store.Paragraph.IsQuote &&
// !ScareQuote; a scare quote that ScareQuoteChapter hasn't caught yet
// still looks like dialogue here, and gets fixed up once it is - see
// store.Store.SetParagraphScareQuotes). If the model still returns
// "Narrator" for one
// (whichever way it got there - unable to determine the speaker, or an
// older "first-person narrator's own dialogue is Narrator" framing this
// package no longer uses at all), this maps it to "Unknown" instead - the
// same safe fallback systemPrompt already asks the model to reach for
// itself when a dialogue line's speaker is genuinely unclear.
func disallowNarratorForDialogue(isQuote bool, speaker string) string {
	if isQuote && speaker == "Narrator" {
		return "Unknown"
	}
	return speaker
}

// canonicalizeSpeakerNames catches the case the system prompt's own
// "reuse a name exactly" instruction doesn't fully prevent: an
// autoregressive model drifting to a shorter or differently-titled variant
// of a name it already used earlier in the *same* reply (nothing stops
// this mid-generation - "Known characters" only lists names from before
// this call), or reusing a variant of a name established in an earlier
// chapter/book instead of knownCharacters' own exact spelling. Any name
// that appears as a whole-word run inside a longer name already seen
// (either in knownCharacters or elsewhere in raw) is folded into that
// longer name - e.g. a reply using both "Vesna" and "Captain Vesna Thorne"
// for the same person collapses every occurrence to "Captain Vesna
// Thorne". Best-effort, not exact: two genuinely distinct characters who
// happen to share a title or surname (rare, but possible - "Captain
// Thorne" and "Old Man Thorne") would also collapse under this heuristic;
// there's no reliable way to tell them apart from names alone, and this
// errs toward fewer duplicate character rows over occasional over-merging,
// which a reader can still correct manually afterward.
func canonicalizeSpeakerNames(knownCharacters []string, raw map[int]string) map[int]string {
	pool := map[string]bool{}
	for _, k := range knownCharacters {
		pool[k] = true
	}
	for _, v := range raw {
		if v != "" && v != "Narrator" && v != "Unknown" {
			pool[v] = true
		}
	}
	canon := canonMapFromPool(pool)

	out := make(map[int]string, len(raw))
	for idx, name := range raw {
		if c, ok := canon[name]; ok {
			out[idx] = c
		} else {
			out[idx] = name
		}
	}
	return out
}

// canonMapFromPool builds a name -> canonical-name map from every name in
// pool: first folding case-only variants of the same name/role together
// (see caseFoldPool's own doc comment - "The attendant" and "the attendant"
// are the same character, not two different ones), then, on that
// case-deduplicated set, folding any name that's a whole-word run inside a
// longer name also in the set into that longer name (see
// canonicalizeSpeakerNames' own doc comment for that second heuristic and
// its trade-offs) - the shared core behind canonicalizeSpeakerNames and
// DescribeChapter's own canonicalizeDescribes.
func canonMapFromPool(pool map[string]bool) map[string]string {
	names := make([]string, 0, len(pool))
	for n := range pool {
		names = append(names, n)
	}

	caseFold := caseFoldPool(names)
	folded := make(map[string]bool, len(caseFold))
	for _, c := range caseFold {
		folded[c] = true
	}
	foldedNames := make([]string, 0, len(folded))
	for n := range folded {
		foldedNames = append(foldedNames, n)
	}
	// Longest first, so a short name is tested against every longer
	// candidate (including other, already-merged names) before deciding
	// it stands on its own.
	sort.Slice(foldedNames, func(i, j int) bool { return len(foldedNames[i]) > len(foldedNames[j]) })

	foldedCanon := make(map[string]string, len(foldedNames))
	for _, short := range foldedNames {
		foldedCanon[short] = short
		for _, long := range foldedNames {
			if long == short {
				continue
			}
			if len(long) > len(short) && containsWholeName(long, short) {
				foldedCanon[short] = foldedCanon[long]
				break
			}
		}
	}

	canon := make(map[string]string, len(names))
	for _, n := range names {
		canon[n] = foldedCanon[caseFold[n]]
	}
	return canon
}

// caseFoldPool groups names that are identical except for capitalization
// (e.g. "The attendant"/"the attendant") and maps every one of them to a
// single chosen spelling for its group - the "don't distinguish characters
// by case" enforcement systemPrompt's own "never sensitive to
// capitalization alone" rule asks for, backstopped here the same
// "don't just ask nicely, enforce it" way normalizeBarePronoun/
// disallowNarratorForDialogue already enforce their own prompt rules in Go.
// A dedicated pass ahead of canonMapFromPool's own longest-first
// whole-word-containment merge, not something that merge can absorb on its
// own: two names identical except for case are, by definition, the exact
// same length, so containsWholeName's own len(long) > len(short)
// requirement never even considers them.
//
// Within a case-insensitive group, the spelling with an uppercase first
// letter wins over an all-lowercase one (matching how a role label is
// already asked to read - systemPrompt's own "capitalized, no article"
// rule - and how a real proper name conventionally reads either way);
// ties (including an all-lowercase-only group) fall back to names' own
// sorted order, so the result is deterministic given the same input
// regardless of map iteration order.
func caseFoldPool(names []string) map[string]string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	byLower := make(map[string][]string, len(sorted))
	var lowerOrder []string
	for _, n := range sorted {
		lower := strings.ToLower(n)
		if _, seen := byLower[lower]; !seen {
			lowerOrder = append(lowerOrder, lower)
		}
		byLower[lower] = append(byLower[lower], n)
	}

	caseFold := make(map[string]string, len(sorted))
	for _, lower := range lowerOrder {
		variants := byLower[lower]
		canonical := variants[0]
		for _, v := range variants[1:] {
			if startsUpper(v) && !startsUpper(canonical) {
				canonical = v
			}
		}
		for _, v := range variants {
			caseFold[v] = canonical
		}
	}
	return caseFold
}

// startsUpper reports whether s's first rune is uppercase - caseFoldPool's
// own tiebreak for which spelling in a case-insensitive group of names
// becomes that group's canonical one.
func startsUpper(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r)
}

// containsWholeName reports whether short's words appear, in order and
// contiguously, as whole words somewhere inside long - case-insensitive
// (e.g. "Vesna" inside "Captain Vesna Thorne") - so two names merely
// sharing a run of letters at a non-word-boundary (like "Vesna" inside
// "Vesnathorpe") don't collapse.
func containsWholeName(long, short string) bool {
	lw := strings.Fields(strings.ToLower(long))
	sw := strings.Fields(strings.ToLower(short))
	if len(sw) == 0 || len(sw) > len(lw) {
		return false
	}
	for i := 0; i+len(sw) <= len(lw); i++ {
		match := true
		for j, w := range sw {
			if lw[i+j] != w {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func addKnown(known []string, name string) []string {
	if name == "" || name == "Narrator" || name == "Unknown" {
		return known
	}
	for _, k := range known {
		if k == name {
			return known
		}
	}
	return append(known, name)
}

const systemPrompt = `You are a literary analysis assistant. Given numbered lines from a novel, identify who speaks each one.
Reply with ONLY a JSON array, no other text: [{"idx": <line number>, "speaker": "<name>", "role": <true or false>}, ...], one entry per input line, in any order.
Rules:
- If a line is narration/description rather than spoken dialogue, its speaker is exactly "Narrator".
- If a line is dialogue, speaker is the speaking character's short proper name (e.g. "Gandalf", not "the old wizard" or "he").
- speaker must always name exactly ONE person - never combine two or more names into a single value (e.g. "Toby and Jane", "Marcus & Elena", "Sam, Jake"), even when the line is addressed to multiple people, multiple characters are present in the scene, or more than one character seems to speak the same line at once. If a line is genuinely spoken by more than one character in unison, pick whichever one the narration credits as leading it, or use "Unknown" if that can't be determined from context - never invent a joint/combined speaker value.
- speaker must be the character's bare proper name only - strip away any descriptive or contextual modifier the surrounding narration happens to attach to them at that moment, even if it comes immediately before or after the name (e.g. "a now dressed Veronica" is just "Veronica"; "an exhausted Marcus" is just "Marcus"; "the newly arrived Captain Thorne" is just "Captain Thorne"). Those modifiers describe the character's state or situation in that instant, not their name - never fold one into the speaker value itself, even if it seems to help distinguish this appearance from another.
- "Narrator" is reserved exclusively for actual narration/description text (the rule above) - NEVER use it for a line of spoken dialogue, no matter who's speaking it. A dialogue line always gets either a real character name, a role label (see below), or "Unknown" - "Narrator" is never a valid answer for one.
- Reuse a name exactly as given in "Known characters" when the same character speaks again - never invent a variant spelling or a new label for a character already known. This applies within your own reply too: if the same character speaks more than once across these lines, use the exact same name for every one of their lines - never switch between a short form and a longer/titled form (e.g. "Vesna" vs. "Captain Vesna Thorne") for the same person. Matching a name against "Known characters", and matching two of your own lines against each other, is never sensitive to capitalization alone - "The attendant" and "the attendant" are the same character, not two different ones; once a name/role has an established spelling (from "Known characters", or from earlier in this same reply), keep using that exact spelling, capitalization included, rather than drifting to a differently-cased variant.
- A bare pronoun ("I"/"me"/"myself"/"he"/"she"/"him"/"her"/"they"/"them") is never a valid speaker value, even when context makes the referent obvious to you - resolve it to that character's actual proper name instead, or use exactly "Unknown" if the name genuinely can't be determined from context.
- A character's name appearing INSIDE the quoted words themselves does not mean that character is speaking - it almost always means the opposite: someone else is addressing them by name (e.g. "Well, Jake, I wasn't expecting that," or "Thank you, Jake," or a bare "Jake!") is being said TO Jake by another character, not BY Jake. Never pick a name as the speaker just because it's mentioned, vocative, or exclaimed inside the line - identify the actual speaker from the surrounding narration/context, and if that's genuinely unclear, use "Unknown" rather than defaulting to whoever's name appears in the text.
- If a dialogue line's speaker isn't a specific named individual, but the surrounding narration clearly gives a generic function/type for whoever's speaking instead (e.g. "the guard demanded," "a merchant called out," "one of the thugs replied"), set "role": true and use that type as speaker - a short, generic, capitalized label with no article ("Guard", "Merchant", "Thug", never "a guard" or "the merchant"). A role label doesn't claim this is the same specific person every time it's used, only the same TYPE of minor, unindividuated speaker filling that function - reuse the same role label for every such speaker of that type throughout, the same way you'd reuse a real name, but understand it may cover several different unnamed people rather than one consistent individual. Only fall back to "Unknown" (with "role" false) when even a generic type can't be determined. Never set "role": true for "Narrator" or for a real named individual, including one only ever addressed by a title/epithet that clearly identifies one specific known person (e.g. "the captain" for a single established Captain Mocar) - that's a real individual, not a role, even though it reads like one; "role" is only for a genuinely interchangeable type. Omit "role" (or set it false) for every other line.
- If a dialogue line's speaker is genuinely unclear from context, use exactly "Unknown" rather than guessing.
- A blank line in "Lines:" marks a real paragraph break. Lines with NO blank line between them come from one continuous original paragraph - often a quote, then a short "X said" narration tag, then more dialogue right after it, but just as often a quote followed by a longer run of action/description/thought (several sentences, not just a short tag) before the next quote. In either case, the quote before that in-between narration and the quote after it are almost always the SAME speaker - the narration is a beat inside one character's continuous turn, not a hand-off to someone else, no matter how long that beat runs. This holds even when the in-between narration mentions another character by name or describes something that other character does - a passing mention or reaction shot does NOT make that other character the speaker of the quote that follows. Only attribute the quote after the gap to a different speaker when the in-between narration itself explicitly says someone else is now speaking (e.g. a tag like "Jake cut in" or "she interrupted" naming a different character than whoever spoke the line before it).
- Conversely, when a dialogue line is separated from the dialogue line before it by a blank line (a real paragraph break) with no narration tag anywhere between them naming who's speaking, default to treating that break itself as a signal the speaker changed, not a continuation of the same speaker - back-and-forth dialogue conventionally starts a new paragraph for each new speaker's turn. Only keep the same speaker across a paragraph break like that when something in the surrounding context clearly supports it (e.g. a tag elsewhere in the passage naming them again, or the dialogue is obviously one uninterrupted speech continuing across a paragraph break rather than an exchange).
- Some entries in "Known characters" carry a short parenthetical description after their name (e.g. "Captain Mocar (a weathered, broad-shouldered sea captain...)"). That's there to help you recognize them when a narration tag names them by role or epithet instead of their proper name ("the captain said," "the old orc grunted") - if a tag's role/epithet clearly matches one character's own parenthetical description and no other known character fits better, attribute the line to that character's exact proper name, never the role/epithet itself (and "role" stays false - this is a real individual, not a generic type). Still use "Unknown" if the match is genuinely ambiguous (e.g. the role could plausibly fit more than one known character, or nothing in "Known characters" matches it at all).
- Some entries in "Known characters" are marked "[role]" instead of (or alongside) a description - that means the name is itself a generic type label already established earlier (e.g. "Guard [role]"), not one consistent individual. Reuse it exactly, with "role": true, whenever a new line's speaker fits that same generic type, exactly as if it were a brand-new role label - never treat two different appearances of a "[role]" name as necessarily the same specific person.`

func (c *Client) attributeBatch(ctx context.Context, bookTitle, chapterTitle string, knownCharacters []string, knownDescriptions map[string]string, knownRoles map[string]bool, batch []ParagraphInput) (map[int]Attributed, error) {
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	if len(knownCharacters) > 0 {
		entries := make([]string, len(knownCharacters))
		for i, name := range knownCharacters {
			desc := knownDescriptions[name]
			switch {
			case knownRoles[name] && desc != "":
				entries[i] = fmt.Sprintf("%s [role] (%s)", name, oneLine(desc))
			case knownRoles[name]:
				entries[i] = fmt.Sprintf("%s [role]", name)
			case desc != "":
				entries[i] = fmt.Sprintf("%s (%s)", name, oneLine(desc))
			default:
				entries[i] = name
			}
		}
		fmt.Fprintf(&user, "Known characters: %s\n", strings.Join(entries, ", "))
	}
	user.WriteString("\nLines:\n")
	for i, p := range batch {
		// A blank line marks a real paragraph break - see systemPrompt.
		// Never before the very first line of the batch, and never for a
		// split-out continuation of the paragraph right before it (that's
		// exactly what Inline means).
		if i > 0 && !p.Inline {
			user.WriteString("\n")
		}
		fmt.Fprintf(&user, "%d: %s\n", p.Idx, oneLine(p.Text))
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}

	return generateAndParse(ctx, c, systemPrompt, user.String(), 0, attributeMaxTokens, parseAttributions)
}

type attribution struct {
	Idx     int    `json:"idx"`
	Speaker string `json:"speaker"`
	Role    bool   `json:"role"`
}

// Attributed is one line's resolved attribution: Speaker, plus whether the
// model marked it a generic role/type label rather than one consistent
// named individual (see systemPrompt's own "role" rule and
// store.Character.IsRole, which this ultimately feeds).
type Attributed struct {
	Speaker string
	Role    bool
}

// parseAttributions extracts the JSON array from content, tolerating models
// that wrap it in prose or a markdown code fence despite the system prompt
// asking for JSON only - common enough with smaller local models to be
// worth handling rather than failing the whole batch over it. Also goes
// through unmarshalRepairedArray, not a bare json.Unmarshal, so a dropped-
// closing-quote response (see repairDroppedClosingQuoteInArray's own doc
// comment) gets one repair attempt before being treated as a genuine parse
// failure.
func parseAttributions(content string) (map[int]Attributed, error) {
	start := strings.IndexByte(content, '[')
	end := strings.LastIndexByte(content, ']')
	if start == -1 || end == -1 || end < start {
		return nil, fmt.Errorf("no JSON array found in response: %s", truncate(content, 200))
	}
	var items []attribution
	if err := unmarshalRepairedArray(content[start:end+1], &items); err != nil {
		return nil, fmt.Errorf("parse JSON array: %w", err)
	}
	out := make(map[int]Attributed, len(items))
	for _, it := range items {
		speaker := strings.TrimSpace(it.Speaker)
		if speaker == "" {
			continue
		}
		out[it.Idx] = Attributed{Speaker: speaker, Role: it.Role}
	}
	return out, nil
}

// QuoteContext is one of a character's own attributed dialogue lines, plus
// (when available) the narration immediately following it in the source
// text - see CharacterizeVoice and store.QuoteContext, which this mirrors
// (kept as its own type rather than importing store, matching this
// package's existing rule of taking plain values in, not store types -
// see ParagraphInput).
type QuoteContext struct {
	Text    string
	Context string
}

// DescriptionContext is one paragraph of narration that physically or
// personality-describes a character (see AttributionResult.Describes and
// store.DescriptionContext, which this mirrors) - unlike QuoteContext, this
// is narration *about* the character, not their own dialogue, so it carries
// no separate Context field: the description text itself already is the
// narration.
type DescriptionContext struct {
	Text string
}

// maxCharacterizeQuotes bounds how many of a character's quotes (across
// however many books in their series) get pulled for characterization -
// matched by httpapi's own maxCharacterizeQuotes, which bounds the store
// query that gathers them. Lowered from an original 30: for a prominent
// character in a long-running series, Store.QuotesForBooks' own ORDER BY
// (book position, then chapter/paragraph) can't be satisfied by an index
// together with its WHERE clause, so the query still has to gather and
// sort every one of that character's matching quotes across the whole
// series before LIMIT ever trims it down - a real, observed slow
// characterization task (high CPU, no GPU activity yet - the LLM call
// hadn't even started) traced back to exactly this for a character with
// far more than 30 real appearances. A smaller cap bounds that same
// pre-LIMIT cost, not just the prompt size.
const maxCharacterizeQuotes = 20

// maxCharacterizeDescriptions bounds how many of a character's description
// paragraphs get pulled for characterization - matched by httpapi's own
// maxCharacterizeDescriptions. Lower than maxCharacterizeQuotes: a
// description paragraph typically runs several sentences (versus one quoted
// line each), and a handful of genuine physical/personality descriptions
// says everything this step needs - unlike dialogue, where a bigger sample
// helps establish a consistent speaking style. Lowered alongside
// maxCharacterizeQuotes - see its own doc comment for why (Store.
// DescriptionsForBooks carries the same pre-LIMIT sort cost).
const maxCharacterizeDescriptions = 10

// characterizeMaxTokens bounds a characterization's generated JSON
// response - now two fields ("instruct", roughly 40-90 words, and
// "refLine", roughly 30-50 words; see characterizeSystemPrompt), not just
// one short paragraph, so this needs real headroom beyond either field's
// own size alone. A "hybrid thinking" model (Qwen3's own chat template
// defaults to this, with no flag here to ask it not to - see
// stripThinking) spends a further chunk of this budget on an unrequested
// <think>...</think> reasoning trace before it ever gets to the JSON
// itself, on top of that - a character sampled from a larger, more varied
// dialogue quote set (up to maxCharacterizeQuotes) tends to produce a
// longer trace before it settles on an answer, which is exactly what was
// running into this budget in practice; doubled from an original 2560 to
// give that trace real room without starving the actual JSON answer (see
// parseCharacterization's own doc comment for what happens when it does).
const characterizeMaxTokens = 5120

const characterizeSystemPrompt = `You are a casting director for an audiobook. Given a character's name and, depending on what's available, a sample of their own spoken dialogue and/or narration that describes them, produce two things: a casting instruction for how a narrator should voice them, and a short reference passage that lets a voice-design tool actually render that voice well.
Reply with ONLY a JSON object, no other text: {"instruct": "...", "refLine": "..."}.

For "instruct", write ONE short paragraph (roughly 40-90 words) instructing a narrator how to voice them: apparent gender, approximate age range (a bucket like child, teenager, young adult, middle-aged, or elderly, not a literal number), accent/register, the personality or tone that should come through in delivery, and a strong physical/vibe impression - build, posture, energy, the kind of presence they'd have in a room - so the instruction paints a fuller picture of who's speaking, not just how their voice sounds.
Whenever the source material signals the speaker isn't an ordinary human - a named fantasy or sci-fi race or species (elf, dwarf, orc, alien), a non-corporeal or non-biological entity (ghost, spirit, demon, robot, AI, construct), or a non-human animal given speech - name that kind of creature/entity explicitly, early in the instruction (e.g. "Speak as a gravelly-voiced dwarf...", "Speak as a flat, synthetic-sounding AI...", "Speak as a shrieking, otherworldly ghost..."), and let it inform pitch/timbre/pace choices a step beyond ordinary human vocal ranges where the material supports it (e.g. a booming subterranean weight for a stone giant, a thin metallic buzz for a robot, an airy reverberation for a ghost). Whenever the material also gives (or strongly implies) a size for that creature/entity - tiny, human-scale, or towering/massive, e.g. a pixie, a giant, a dragon, a house-sized construct - state that size too and let it shape the voice the same way (a thin, high, quick voice for something tiny; a huge, resonant, slow-rolling one for something towering), rather than voicing every non-human character at an ordinary human scale by default. Treat all of this as fantasy/sci-fi worldbuilding, not real-world plausibility - never hedge or rationalize a fantastical trait away. Default to human, unhedged, at ordinary human scale, when nothing suggests otherwise; do not invent a race/species/size where the text is silent.
Write "instruct" as a direct instruction to a narrator, in the form "Speak as a ...", the same way an audiobook voice-casting note would read - not a description of the character as a story figure.
Describe the voice across specific, concrete dimensions rather than one or two vague adjectives - a vague, single-word impression ("a nice voice", "distinctive") produces a generic-sounding result, while naming pitch (e.g. "low-pitched", "a bright upper register"), pace (e.g. "an unhurried, measured pace", "a quick, clipped cadence"), and timbre/texture (e.g. "warm and smooth", "raspy", "breathy", "magnetic", "mellow", "a gravelly weight underneath") together gives a narrator something they can actually perform consistently. Cover at least pitch, pace, and timbre/texture explicitly, alongside personality/tone, rather than leaning on personality words alone. Never stack repeated or piled-up intensifiers ("very, very energetic", "extremely extremely deep") to signal strength - repetition doesn't add precision; reach for one stronger, more specific word or phrase instead (e.g. "booming and intense" rather than "very very loud").
Never describe the voice by comparing it to a real, identifiable person - a celebrity, actor, public figure, or narrator, by name (e.g. "sounds like Morgan Freeman", "a David Attenborough quality") - even if the dialogue or description evokes one. A small, locally-run voice-design model has no reliable knowledge of what any specific real person actually sounds like, so a name like that is dead weight at best and a distracting mismatch at worst; translate whatever impression that comparison is standing in for directly into the same pitch/pace/timbre/personality vocabulary above instead (e.g. a "deep, commanding, gravelly bass-baritone with unhurried gravitas" rather than "a James Earl Jones voice").
Whenever there is any gender signal at all, state it explicitly and unambiguously as one word in that opening "Speak as a [word] ..." clause - "male"/"female", or "man"/"woman"/"boy"/"girl" if age makes that the more natural fit - before any age/accent/tone words, the same way the built-in presets read (e.g. "Speak as a calm, warm middle-aged male narrator..."). Do not rely on pronouns, a gendered name, or descriptive words like "gruff" or "delicate" to imply gender on their own - one of those explicit words must actually appear, every time there is a signal to base it on, so this is consistent from one character to the next rather than only sometimes stated outright.
Every character gets a specific nationality- or region-based accent named explicitly somewhere in "instruct" - never the generic word "accent" alone, and never left out entirely, even for an ordinary human with nothing distinctive about them otherwise. Whenever the source material gives an accent, regional, or national signal - a stated nationality/hometown/origin, a description of how they sound ("spoke with a thick Scottish burr", "his clipped British vowels"), dialect/phonetic spelling written into their own dialogue ("y'all", "dinnae", dropped g's), or a name/setting that strongly suggests a place or culture of origin - name that specific accent or regional voice explicitly (e.g. "with a Scottish accent", "a Southern American drawl", "a clipped British accent") rather than the generic word "accent" alone. This is for audiobook narration, so a reasonable, tasteful inference from a name or setting is fine when nothing more explicit is available. When the material gives no signal at all - no name, setting, or dialogue cue to infer from - still name a specific accent rather than omitting one: reach for whatever's most plausible given the story's own general setting (e.g. a General American accent for an otherwise-unmarked contemporary American setting, a standard/neutral British accent for an unmarked British one), falling back to a plain, unremarkable General American accent as the last resort default when the setting itself gives nothing to go on either.
When a character is a named fantasy/sci-fi race or species, or a non-corporeal/non-biological entity (per the paragraph above) and the text itself gives no accent/regional signal of its own, fall back on the accent (or, for a couple of kinds, deliberate lack of one) audiobook narration conventionally gives that kind, rather than defaulting to an unaccented voice by default: dwarves - a thick Scottish accent; elves - a refined, mid-Atlantic or English RP accent; orcs and goblins - a rough, guttural, working-class or Cockney-inflected accent; giants and trolls - a broad, rustic, rural accent; halflings/hobbits - a warm West Country (English countryside) accent; dragons - a plummy, aristocratic English RP accent; vampires - a refined Eastern European (Transylvanian) or aristocratic RP accent; fairies/pixies - a light, airy Irish lilt; robots/AI/constructs - a flat, neutral, unaccented, precisely-enunciated register with no regional coloring at all, the deliberate absence of an accent being the convention itself; ghosts/spirits - a faint, old-fashioned English accent evoking a bygone era, airy and half-whispered; demons - a refined, urbane, aristocratic English RP accent for anything articulate and menacing-but-composed, or a guttural, growling, non-regional voice for anything more beastly/feral. These are the expected, standard defaults for each kind, not a hesitant last resort - apply them confidently whenever the race/species/entity itself is established and the text is silent on accent specifically, the same way a real casting director would reach for the genre-standard choice rather than leave it vague. An explicit signal actually in the text (a stated nationality, dialect spelling, or described accent) still wins when one is present; absent both that and a race/species match here, fall back to the general rule above (infer from name/setting, or default to a plain General American accent) rather than ever leaving accent out - every character, human or not, ends up with one specific accent named in "instruct".
When the material signals a more specific non-human texture beyond the broad creature/entity categories above, layer in the matching delivery quality alongside whatever accent/pitch/size choices already apply, applying it with the same confidence as the accent defaults above: a snake-like or reptilian creature - a light sibilant hiss threaded through the delivery; an insectoid or hive-mind entity - a faint buzzing, subtly doubled-voice quality; an angelic or divine being - an airy, reverberant, breathy quality. Apply one of these whenever the material signals that specific kind (a named reptilian/snake race, a swarm/hive framing, an angel/seraph/divine messenger called out as such) - still never invent the underlying kind itself where the text is silent, the same restraint the broader creature/entity rule above already asks for, but don't hedge on the texture once that kind is established.
Independent of race/species, when the material casts a character in a clearly-stated narrative archetype or role, confidently layer in the delivery convention audiobook narration typically gives that role, on top of (not instead of) whatever gender/age/accent/creature choices already apply: a wizard, sage, or elder mentor figure - slow, breathy, weathered gravitas; a monarch or noble - crisp, poised, unhurried diction, regardless of race; a pirate or sailor - a gruff, weathered, seafaring drawl; a story's clearly-established villain or antagonist - a lower-pitched, unhurried, deliberate menace. Apply the villain convention only when the material explicitly frames the character as the story's antagonist or an established evildoer (a stated role like "the villain" or "the Dark Lord", or a clear pattern of intentional wrongdoing directed at the protagonist(s)) - never for a character who is merely morally gray, rude, or on the opposite side of an ordinary disagreement; skip the convention entirely rather than guess when it's unclear. As with every other convention above, an explicit voice/accent/personality signal actually in the text still wins over the archetype default when present - but once the role itself is clearly established, apply its convention rather than leaving the delivery generic.
You may be given source material under two separate headings: "Their dialogue" (lines they actually say) and "Description of them" (narration about their appearance/personality, not spoken by them - not their own words). When "Description of them" is present, it's usually the most reliable source for apparent gender/age, physical/vibe impression, and creature/species/entity type - lean on it directly for those rather than guessing from dialogue alone. Base personality and tone primarily on how they actually talk (dialogue) and on any personality/temperament the description narration itself states, not on assumptions from their name in either section.
Some dialogue lines end with a bracketed [narrator: ...] note - that's the surrounding narration right after that line, the same kind of physical/gender/age/accent signal as a "Description of them" entry, and can be mined the same way (a crossed-arm stance, a tired slump, a booming gesture, a described accent). Never use narration, bracketed or in its own section, to infer personality or tone beyond what it or the dialogue actually shows.
If, and only if, no dialogue or description gives any gender signal at all, omit that word and describe the voice without committing to a gender rather than guessing one.
The user turn may open with a note that this name is a generic ROLE/TYPE label (e.g. "Guard", "Thug", "Merchant"), not one consistent individual - when present, the dialogue/description sample below may be pooled from several different unnamed people who happened to share that label at different points in the story, not one person's own consistent voice. In that case, cast the general TYPE the label names rather than any one line's specific personality quirks: base gender/age/accent/archetype conventions on whatever the label and the pooled sample suggest about that kind of person in general (a "Guard" reads as a stern, dutiful, working-class voice; a "Merchant" reads as an affable, transactional patter), and favor whichever traits recur across multiple sample lines over a single outlier line's own idiosyncrasy. Still follow every rule above exactly the same way - a role gets a full, specific casting instruction and reference passage, just one aimed at the type rather than a unique individual.
For "refLine", write a separate short passage (roughly 30-50 words, two or three sentences - short, deliberately: this becomes a rendered audio reference clip, and a longer passage risks pushing that render past what generation can handle) of plain, generic narration - NOT the character's own dialogue and NOT specific to their story - written purely to give a voice-design tool something to perform "instruct"'s voice with. Match its sentence rhythm and punctuation to the voice "instruct" describes: short, direct sentences or exclamations/questions for an energetic, intense, or commanding voice; longer, smooth, descriptive sentences for a calm or measured one - whatever best fits the pace and energy "instruct" itself calls for, within that same 30-50 word budget. A flat, neutral sentence undersells an energetic voice description just as much as a breathless one undersells a calm narrator, so the passage's own rhythm needs to actually invite the delivery "instruct" asks for. Still use a real variety of words and sounds across the passage, not one repeated phrase or sound, so it works well as a general-purpose voice reference rather than just for one moment - think of it as a short scene-setting passage a narrator would read as a warm-up in that voice, not a line of the character's own dialogue.
Actively write toward whatever specific accent "instruct" names, not just toward its pace/energy: spell words the way that accent is conventionally rendered in prose (eye dialect/phonetic respelling - e.g. "dinnae"/"cannae"/"wee" for Scottish, dropped g's and "y'all" for a Southern American drawl, "gonna"/"innit"/dropped h's for a Cockney accent, "da"/"dis"/"dat" for a heavy Eastern European accent), and reach for that accent's own typical sentence structure and phrasing habits (word order, contractions, characteristic filler words/interjections), not just its vocabulary in isolation. Layer the same treatment on top for any other speech-pattern trait "instruct" calls out beyond accent - a stammer, a verbal tic ("y'know", "if you catch my meaning"), unusually formal/archaic phrasing for nobility or an old-fashioned character, clipped fragment-heavy speech for a curt/military voice, and so on - so the passage reads as that specific character's own speech pattern, not neutral prose with an accent label attached. Keep it legible and performable rather than a parody-thick wall of apostrophes: respell/restructure enough that the accent and speech pattern are unmistakable on the page, without tipping into being hard to actually read aloud.
Neither field should repeat the character's name or include a heading.`

// CharacterizeVoice asks the LLM to describe how name should sound, from a
// sample of their own attributed dialogue (quotes - narration *about* them
// is otherwise excluded, except each quote's own immediate narration tag,
// carried in QuoteContext itself, and descriptions, gathered separately by
// the caller, see httpapi.characterizeVoice) plus a sample of narration that
// physically/personality-describes them (descriptions) - gender,
// approximate age, tone, speaking style - returning two things: instruct, a
// short natural-language voice-design instruction suitable to use verbatim
// as an internal/voices-style Instruct string, and refLine, a short generic
// reference passage written to match instruct's own pace/energy (see
// characterizeSystemPrompt) - suitable to use verbatim as that same voice
// preset's RefText, in place of voices.DefaultRefText, so the rendered
// voice actually gets to perform the delivery instruct asks for rather than
// being read a flat, unrelated passage regardless of how the character is
// meant to sound.
//
// Still runs even when both quotes and descriptions are empty - name alone
// (always sent, below) is enough for the system prompt's own "no signal at
// all" fallback rules to produce a plausible, generic casting instruction
// from (default to human/ordinary scale, a name/setting-plausible accent,
// no gender committed to unless the name itself is unambiguous - see
// characterizeSystemPrompt's own rules for each). Used to return ("", "",
// nil) instead for this case (nothing to characterize from yet), which
// left a character with zero attributed dialogue and zero descriptions -
// a real, observed case: an orphaned character record whose only
// attributed paragraph was later reattributed/canonicalized to a
// different name - permanently stuck: every later characterization pass
// hit the same empty-material early return, silently, forever, since
// there's no error to retry or terminal state to persist either. Now
// always produces *something* real to work with instead, even if it's
// necessarily generic without any real material behind it.
//
// isRole (see store.Character.IsRole) flags name as a generic role/type
// label rather than one consistent individual - passed through as a note
// on the user turn (characterizeSystemPrompt's own rule for it) rather
// than a second system-prompt variant, the same NoThink/no_think shape
// AttributeChapter's own Config.NoThink already uses, since it's a
// per-call fact about this one character, not a global prompting choice.
func (c *Client) CharacterizeVoice(ctx context.Context, name string, isRole bool, quotes []QuoteContext, descriptions []DescriptionContext) (instruct string, refLine string, err error) {
	if len(quotes) > maxCharacterizeQuotes {
		quotes = quotes[:maxCharacterizeQuotes]
	}
	if len(descriptions) > maxCharacterizeDescriptions {
		descriptions = descriptions[:maxCharacterizeDescriptions]
	}

	var user strings.Builder
	fmt.Fprintf(&user, "Character: %s\n", name)
	if isRole {
		fmt.Fprintf(&user, "Note: %q is a generic role/type label, not one consistent individual - the sample below may be pooled from several different unnamed people who shared it.\n", name)
	}
	if len(quotes) > 0 {
		user.WriteString("\nTheir dialogue:\n")
		for _, q := range quotes {
			fmt.Fprintf(&user, "- %s", oneLine(q.Text))
			if q.Context != "" {
				fmt.Fprintf(&user, " [narrator: %s]", oneLine(q.Context))
			}
			user.WriteString("\n")
		}
	}
	if len(descriptions) > 0 {
		user.WriteString("\nDescription of them:\n")
		for _, d := range descriptions {
			fmt.Fprintf(&user, "- %s\n", oneLine(d.Text))
		}
	}

	// generateAndParse's retry covers this validation step too, not just
	// JSON parsing elsewhere in this package - a too-short instruct or
	// refLine is the same kind of one-off model hiccup (see
	// parseCharacterization's own comment), and a fresh generation attempt
	// often just doesn't hit it a second time.
	result, err := generateAndParse(ctx, c, characterizeSystemPrompt, user.String(), 0.3, characterizeMaxTokens, parseCharacterization)
	if err != nil {
		return "", "", err
	}
	return result.Instruct, result.RefLine, nil
}

// characterization is CharacterizeVoice's own parsed JSON response shape -
// see characterizeSystemPrompt's {"instruct": "...", "refLine": "..."}
// reply format.
type characterization struct {
	Instruct string `json:"instruct"`
	RefLine  string `json:"refLine"`
}

// parseCharacterization extracts and validates CharacterizeVoice's JSON
// object, tolerating models that wrap it in prose or a markdown code fence
// despite the system prompt asking for JSON only - the same tolerance
// parseAttributions/parseDescribeItems apply for their own array replies.
// A short fragment like "Speak as a" for instruct, or a one-sentence
// fragment for refLine, is worse than an empty result: it would still pass
// as non-empty and get saved permanently onto the character's voice preset
// (see httpapi.provisionCharacterVoice), silently producing a voice that's
// never actually characterized, or a reference clip too short to render
// well, rather than visibly failing to characterize at all. This happens
// when a "hybrid thinking" model's own <think> trace (see stripThinking)
// eats most of characterizeMaxTokens' budget and generation hits maxTokens
// moments into the real answer - stripThinking already treats a trace that
// never closes as "nothing usable"; this catches the case where it did
// close, but left too little of the budget behind for the answer to
// finish. minCharacterizeWords/minRefLineWords are well under what even a
// terse but complete instruct/refLine would ever come in at.
func parseCharacterization(content string) (characterization, error) {
	start := strings.IndexByte(content, '{')
	end := strings.LastIndexByte(content, '}')
	if start == -1 || end == -1 || end < start {
		return characterization{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var result characterization
	if err := json.Unmarshal([]byte(content[start:end+1]), &result); err != nil {
		return characterization{}, fmt.Errorf("parse JSON object: %w", err)
	}
	result.Instruct = strings.TrimSpace(result.Instruct)
	result.RefLine = strings.TrimSpace(result.RefLine)
	if words := strings.Fields(result.Instruct); len(words) < minCharacterizeWords {
		return characterization{}, fmt.Errorf("llm returned an implausibly short characterization (%q) - likely truncated mid-answer", result.Instruct)
	}
	if words := strings.Fields(result.RefLine); len(words) < minRefLineWords {
		return characterization{}, fmt.Errorf("llm returned an implausibly short reference line (%q) - likely truncated mid-answer", result.RefLine)
	}
	result.RefLine = trimRefLineToSentences(result.RefLine, maxRefLineWords)
	return result, nil
}

// minCharacterizeWords/minRefLineWords - see parseCharacterization's own
// comment. minRefLineWords is lower than refLine's own ~30-50 word target
// for the same "catch truncation, not stylistic brevity" reason
// minCharacterizeWords sits under instruct's ~40-90 word target.
const minCharacterizeWords = 8
const minRefLineWords = 20

// maxRefLineWords is a Go-side backstop capping how long a characterized
// RefLine is ever actually saved at, independent of asking nicely in
// characterizeSystemPrompt's own ~30-50 word target - real, observed
// case: a model that ignored that target and produced a much longer
// refLine, which (combined with a slow/deliberate instructed pace) went on
// to render a reference clip that reproducibly OOM'd the ttsworker process
// on every one of that character's paragraphs (see
// voicerefs.maxRefClipSeconds' own doc comment, the other half of this
// defense - that one catches every cause including a slow Instruct
// inflating duration independent of word count, this one catches the
// LLM-output cause specifically, before it ever reaches a render at all).
// Set a bit above the prompt's own upper target (50) to only trim a
// genuine overshoot, not ordinary stylistic length.
const maxRefLineWords = 65

// sentenceBoundary matches one sentence (including its own closing
// punctuation and any immediately-trailing closing quote/bracket) within a
// larger passage - trimRefLineToSentences' own building block.
var sentenceBoundary = regexp.MustCompile(`[^.!?]+[.!?]+["'\)\]]*`)

// trimRefLineToSentences trims s down to at most maxWords words, cutting
// only at a full sentence boundary - never mid-sentence, since a dangling
// fragment is unusable for VoiceDesign to actually perform. Always keeps
// at least the first sentence, even if that sentence alone already
// exceeds maxWords - a single long sentence over budget still renders
// fine; it's the accumulation of several that inflates a clip toward the
// failure this guards against. Returns s unchanged if it contains no
// recognizable sentence boundary at all (defensive fallback - shouldn't
// happen given parseCharacterization's own minRefLineWords check already
// ran first).
func trimRefLineToSentences(s string, maxWords int) string {
	sentences := sentenceBoundary.FindAllString(s, -1)
	if len(sentences) == 0 {
		return s
	}
	kept := []string{sentences[0]}
	words := len(strings.Fields(sentences[0]))
	for _, sent := range sentences[1:] {
		n := len(strings.Fields(sent))
		if words+n > maxWords {
			break
		}
		kept = append(kept, sent)
		words += n
	}
	var out strings.Builder
	for i, sent := range kept {
		if i > 0 {
			out.WriteByte(' ')
		}
		out.WriteString(strings.TrimSpace(sent))
	}
	return out.String()
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// SystemPrompts returns every distinct system-prompt string this package's
// Client will ever call generate with - the fixed, package-level constants
// backing AttributeChapter (systemPrompt), DescribeChapter
// (describeSystemPrompt), CharacterizeVoice (characterizeSystemPrompt),
// EmotionChapter (emotionSystemPrompt),
// ResolvePronunciation (pronunciationSystemPrompt), and ScoreMusic's own
// two passes (musicSystemPrompt, musicDescribeSystemPrompt) - the latter
// especially worth priming since ScoreMusic now issues one
// describeMusicRegion call per region, all sharing that one prompt, each
// byte-identical
// across every call for the life of the process. cmd/ttsworker/main.go
// passes this to llmworker.Config.SystemPrompts so each one gets primed
// (decoded once, into its own permanently-resident sequence) at model-load
// time - see that field's own doc comment for why priming has to happen
// with the full set known up front, rather than lazily as new prompts show
// up the way this package's old, single-sequence primedFor/tryPrimed used
// to.
func SystemPrompts() []string {
	return []string{
		systemPrompt,
		describeSystemPrompt,
		characterizeSystemPrompt,
		emotionSystemPrompt,
		pronunciationSystemPrompt,
		scareQuoteSystemPrompt,
		musicSystemPrompt,
		musicDescribeSystemPrompt,
	}
}
