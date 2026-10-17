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
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/rhino1998/lectable/backend/internal/namekey"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
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
	// Decide asks every attribution by choice (AttributeOptions.Decide)
	// - LECTABLE_SPEAKER_ATTR_DECIDE, off by default (run.sh turns it on).
	Decide bool
	// Seed settles the lines the speech-tag rules decide before the model
	// sees the chapter - LECTABLE_SPEAKER_ATTR_SEED, off by default
	// (run.sh turns it on). Under Decide it is AttributeOptions.
	// SeedInReply, otherwise AttributeOptions.Seed.
	Seed bool
}

// minSlotNCtx is the least per-slot KV budget SlotNCtx ever returns -
// room for the largest prompt any pass sends together with its reply: a
// summary window, held to it by summaryPromptChars (~10K tokens of prompt
// beside a 2048-token reply). A pass whose prompt can grow with the book -
// a roster, carried-over notes - has to hold itself to the slot the same
// way: the worker fails a call that runs out of it (llamacpp.ErrContextFull).
const minSlotNCtx = 12288

// SlotNCtx is cmd/ttsworker's default for llmworker.Config.NCtx
// (LECTABLE_SPEAKER_LLM_CTX): the per-generation-slot share of the shared KV pool.
// The largest reply any pass asks for (MaxOutputTokens), but never below
// minSlotNCtx. This used to be MaxOutputTokens alone at 24576 (attribution's
// old JSON-for-every-line budget), reserving ~2x the KV cache - several GB
// of VRAM at MaxConcurrent 2 - for replies that never came close to it.
// Modern GGUF models train at very large native contexts (Qwen3: 262144),
// and a KV cache sized for that can fail outright to allocate next to a
// TTS model, which is why 0 ("the model's own") is never the default.
func SlotNCtx() int {
	return max(MaxOutputTokens(), minSlotNCtx)
}

// MaxOutputTokens is the largest maxTokens value any batch call in this
// package will ever pass to LLMGenerate - llmworker.Config sizes its shared KV-cache
// pool off this per concurrent generation slot (see its own NCtx doc
// comment), since that's the real per-slot worst case: a batch's own
// input prompt is always far smaller than its own maxTokens ceiling (see
// SlotNCtx, which never drops below minSlotNCtx to leave room for the
// prompt too), so budgeting
// maxTokens alone per slot leaves real margin for input too without
// needing a separate, harder-to-get-right prompt-length estimate.
func MaxOutputTokens() int {
	max := attributeMaxTokens
	for _, t := range []int{
		jsonPassMaxTokens,
		characterizeMaxTokens,
		emotionMaxTokens,
		pronunciationMaxTokens,
		musicDescribeMaxTokens,
		lightingMaxTokens,
	} {
		if t > max {
			max = t
		}
	}
	return max
}

// ConfigFromEnv reads LECTABLE_SPEAKER_LLM_NO_THINK - see NewClientFromEnv. The
// model-loading env vars (LECTABLE_SPEAKER_LLM_MODEL_PATH/GPU_LAYERS/CTX/
// MAX_CONCURRENT) are read by cmd/ttsworker/main.go instead now (see this
// package's own doc comment) - NewClientFromEnv still resolves the model
// path itself (ModelPathFromEnv), but only to decide whether the feature is
// available at all.
func ConfigFromEnv() Config {
	cfg := Config{}
	if v := strings.TrimSpace(os.Getenv("LECTABLE_SPEAKER_LLM_NO_THINK")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.NoThink = b
		}
	}
	if v := strings.TrimSpace(os.Getenv("LECTABLE_SPEAKER_ATTR_DECIDE")); v != "" {
		cfg.Decide, _ = strconv.ParseBool(v)
	}
	if v := strings.TrimSpace(os.Getenv("LECTABLE_SPEAKER_ATTR_SEED")); v != "" {
		cfg.Seed, _ = strconv.ParseBool(v)
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

// llmDecider is the optional half of an llmBackend that can answer
// closed-set questions by reading logits instead of generating
// (llmworker.Worker.LLMDecide).
type llmDecider interface {
	LLMDecide(ctx context.Context, systemPrompt, userPrompt string, steps []ttsproto.LLMDecideStep) ([]ttsproto.LLMDecision, error)
}

// Client is a thin caller-side wrapper around an llmBackend: batching,
// prompt-building, JSON-parsing, and malformed-response retry all live
// here (see AttributeChapter/CharacterizeVoice/
// EmotionChapter/ResolvePronunciation and generateAndParse below) -
// the model itself is neither loaded nor run by this package any more (see
// the package doc comment).
type Client struct {
	cfg Config
	llm llmBackend
}

// DefaultModelFile is the GGUF speaker attribution loads from ~/llm-models
// when LECTABLE_SPEAKER_LLM_MODEL_PATH isn't set - Qwen3.6-35B-A3B (a 3B-active MoE), Unsloth's
// UD-IQ4_XS with its MTP head (18.2GB; the same weights as the plain
// 16.5GB file, plus the layers MTP speculative decoding drafts with - see
// llmworker.Config.MTPDraft), so it runs exclusive of the audio models (see
// cmd/ttsworker's llmExclusive). Qwen3-4B-Instruct-2507 (Q4_K_M) was the
// default before 2026-09-28 and still works via the env var.
const DefaultModelFile = "Qwen3.6-35B-A3B-MTP-UD-IQ4_XS.gguf"

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

// ModelPathFromEnv returns LECTABLE_SPEAKER_LLM_MODEL_PATH, or DefaultModelPath() if
// it's unset. Shared by cmd/server (NewClientFromEnv's enablement gate) and
// cmd/ttsworker (what llmworker actually loads) so the two can't disagree.
func ModelPathFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("LECTABLE_SPEAKER_LLM_MODEL_PATH")); v != "" {
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
// doc comment. A batch parsed via parseAttributions
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
// a genuinely creative one. A call that already samples above
// retryTempBump is left at its own temp on retry instead: llama.cpp's own
// sampler already draws a fresh random seed per call there (see
// llamacpp-go/llamacpp/sampler.go's Seed doc comment - SamplerParams.Seed
// 0 means LLAMA_DEFAULT_SEED, entropy-seeded), so every retry already has
// natural variance without help.
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
// (attribution, characterization).
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
	// Known is the speaker a reader set on this dialogue line by hand
	// (store.ParagraphEdit), "" when it has none. AttributeChapter never
	// asks about it: the line is shown with its speaker as settled context
	// (who the conversation is between, whose turn came before), and Known
	// is its answer.
	Known string
	// KnownIndex is Known's speaker number when Known is a role label and
	// the reader's line carries one (store.Paragraph.SpeakerIndex), else 0.
	KnownIndex int
	// ContextOnly shows a dialogue line to the model without asking about
	// it or returning an answer for it - surrounding context for a run
	// that only re-asks some of a chapter's lines (httpapi's
	// attributeAroundEdits).
	ContextOnly bool
	// Seeded marks a Known set by the speech-tag rules (seedFromTags)
	// rather than by a reader - shown as a tag under AttributeOptions.
	// TaggedSeeds.
	Seeded bool
	// Forced is a seeded speaker under AttributeOptions.SeedInReply: the
	// line is shown and answered like any other, but decide writes this
	// answer into the reply instead of asking.
	Forced string
}

// maxBatchParagraphs caps how many paragraphs go into one completion
// request - keeps prompts within a reasonable context window regardless of
// chapter length, and keeps one malformed model response from losing
// attribution for an entire long chapter.
const maxBatchParagraphs = 40

// attributeParallelBatches is how many of a chapter's batches
// AttributeChapter sends at once. Matches llmworker's own default
// MaxConcurrent (2) - both of the worker's generation slots decode
// together in one batched call, so a single chapter's attribution (which
// the job queue already runs one at a time, in chapter order) keeps both
// busy instead of leaving one idle.
const attributeParallelBatches = 2

// seamContextLines is how far either side of a batch seam AttributeChapter
// looks for a conversation between numbered roles running across it, and
// how many of the earlier batch's lines the re-asked batch is shown.
const seamContextLines = 12

// batchContextLines is how many paragraphs before each batch are shown as
// unanswered context, so a batch's first lines aren't cut off from the
// turn-taking or tag right before them.
const batchContextLines = 3

// attributeMaxTokens bounds one attribution batch's reply: one short
// "idx: Name" line per dialogue line asked about (at most
// maxBatchParagraphs), well under a few hundred tokens in practice. Was
// 24576 when the reply was a JSON object for every line, narration
// included (~650 tokens and ~10s median per 40-line batch, measured), and
// earlier still had to leave room for a hybrid-thinking model's own
// reasoning trace - see the NoThink doc comment.
const attributeMaxTokens = 2048

// jsonPassMaxTokens bounds the JSON-array replies
// ScareQuoteChapter parse - the largest ever logged from either was ~8KB
// (~2.5K tokens).
const jsonPassMaxTokens = 4096

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
// knownDescriptions (nil-safe: a nil or missing entry just means no blurb)
// pairs some of knownCharacters' own names with a short identifying blurb -
// what the chapter's summary notes call them (Roster.forChapter) - included in
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
// indexes, also returned alongside out, is the speaker number of each line
// answered as a numbered role ("Guard 2 [role]" - out holds "Guard", indexes
// 2): the model numbers several unnamed people of one type talking in the
// same conversation so each can get their own voice. The numbers are only
// meaningful within a conversation - "Guard 2" here and in the next scene
// are not the same person. A line with no number has no entry.
//
// pronouns, also returned alongside out, is each final speaker's
// pronouns (see store.Character.Pronouns) where this call's own lines
// settled on one by strict majority - the model appends " | he"/" | she"/
// etc. to a reply line when the text establishes it. Never includes a
// role label: a "Guard" line may be any of several different people.
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
//
// Every raw answer goes through sanitizeSpeaker (non-names, group names,
// leaked prompt markup -> "Unknown") and is folded onto a known
// character's own name when it's one of their aliases or a case variant.
// Once every batch is done, RefineWithSpeechTags corrects the result
// against explicit speech tags in the chapter text - see refine.go.
func (c *Client) AttributeChapter(ctx context.Context, bookTitle, chapterTitle string, roster Roster, paragraphs []ParagraphInput, shouldPause func() bool) (out map[int]string, indexes map[int]int, roleNames map[string]bool, pronouns map[string]string, remaining []ParagraphInput, err error) {
	return c.AttributeChapterWith(ctx, bookTitle, chapterTitle, roster, paragraphs, shouldPause, AttributeOptions{})
}

// AttributeChapterWith is AttributeChapter with opts - see
// AttributeOptions, the experimental ways of asking that the attribution
// preview benches against the default.
func (c *Client) AttributeChapterWith(ctx context.Context, bookTitle, chapterTitle string, roster Roster, paragraphs []ParagraphInput, shouldPause func() bool, opts AttributeOptions) (out map[int]string, indexes map[int]int, roleNames map[string]bool, pronouns map[string]string, remaining []ParagraphInput, err error) {
	if opts.RedecideBelow > 0 {
		return c.attributeRedecided(ctx, bookTitle, chapterTitle, roster, paragraphs, shouldPause, opts)
	}
	ask := c.attributeBatch
	if opts.Decide || c.cfg.Decide {
		ask = c.attributeBatchDecide
	}
	if opts.SeedOnly {
		out := map[int]string{}
		for i, p := range seedFromTags(paragraphs, roster) {
			if p.Known != paragraphs[i].Known {
				out[p.Idx] = p.Known
			}
		}
		return out, map[int]int{}, map[string]bool{}, map[string]string{}, nil, nil
	}
	decide := opts.Decide || c.cfg.Decide
	switch {
	// The configured seed (live) is always written into decide's reply;
	// opts.Seed under decide is the old way, kept for the eval.
	case decide && (opts.SeedInReply || c.cfg.Seed && !opts.Seed):
		paragraphs = forcedFromSeeds(seedFromTags(paragraphs, roster))
	case opts.Seed || opts.SeedInReply || c.cfg.Seed:
		paragraphs = seedFromTags(paragraphs, roster)
	}
	roster.noUnknown = opts.NoUnknown
	roster.prevNext, roster.taggedSeeds = opts.PrevNext, opts.TaggedSeeds
	knownCharacters := roster.Names
	out = make(map[int]string, len(paragraphs))
	numberByIdx := make(map[int]int) // the model's own speaker number per line, see roleNumber
	isRoleByIdx := make(map[int]bool, len(paragraphs))
	pronounsByIdx := make(map[int]string, len(paragraphs))
	known := append([]string(nil), knownCharacters...)
	knownIsRole := make(map[string]bool, len(roster.Roles))
	for name, isRole := range roster.Roles {
		knownIsRole[name] = isRole
	}
	resolver := NewSpeakerNameResolver(knownCharacters, roster.Aliases).WithTitles(roster.World)
	isQuoteByIdx := make(map[int]bool, len(paragraphs))
	for _, p := range paragraphs {
		isQuoteByIdx[p.Idx] = p.IsQuote
	}

	finish := func(out map[int]string) (map[int]string, map[int]int, map[string]bool, map[string]string, []ParagraphInput, error) {
		canon := canonicalizeSpeakerNames(knownCharacters, out, knownIsRole)
		answered := maps.Clone(canon)
		// Votes go to the name the model itself answered (canonicalized),
		// before speech-tag refinement can move a line to someone else -
		// the pronouns were its judgment about that speaker, not theirs.
		votes := pronounVotes{}
		for idx, p := range pronounsByIdx {
			if !isRoleByIdx[idx] {
				votes.add(canon[idx], p)
			}
		}
		pronouns = votes.winners()
		tagResolver := NewSpeakerNameResolver(known, roster.Aliases).WithTitles(roster.World).WithFirstPerson(roster.FirstPerson)
		canon, _ = RefineWithSpeechTags(paragraphs, canon, tagResolver)
		// Established pronouns win over this call's votes, as they do
		// when the caller stores them.
		allPronouns := maps.Clone(pronouns)
		maps.Copy(allPronouns, roster.Pronouns)
		canon, _ = RefinePronounTags(paragraphs, canon, tagResolver, allPronouns)
		refined := maps.Clone(canon)
		roleNames = make(map[string]bool, len(isRoleByIdx))
		for idx, isRole := range isRoleByIdx {
			if !isRole {
				continue
			}
			if name := canon[idx]; name != "" && name != "Narrator" && name != "Unknown" {
				roleNames[name] = true
			}
		}
		roles := maps.Clone(knownIsRole)
		for name := range roleNames {
			roles[name] = true
		}
		// The roster's own names, not known: known has every raw answer
		// this call saw folded in, so "the older man" would count as a
		// name and pass through.
		canon = ResolveCastRefs(canon, roster.Cast)
		canon = resolveIdentities(canon, NewSpeakerNameResolver(knownCharacters, roster.Aliases).WithTitles(roster.World), roles)
		for _, name := range canon {
			if roles[name] {
				roleNames[name] = true
			}
		}
		// A number stays with the line only while the line is still the
		// role answer it came with: a tag refinement that moved the line
		// to someone else took it from a different speaker.
		indexes := make(map[int]int, len(numberByIdx))
		for idx, n := range numberByIdx {
			if roles[canon[idx]] && refined[idx] == answered[idx] {
				indexes[idx] = n
			}
		}
		// A role label that carries its own number - a chapter's cast lists
		// "Thug 1" and "Thug 2", and older rosters hold such names - is that
		// number of the plain label.
		nameResolver := NewSpeakerNameResolver(knownCharacters, roster.Aliases).WithTitles(roster.World)
		for idx, name := range canon {
			if !roles[name] {
				continue
			}
			label, n := SplitRoleIndex(name)
			if one, ok := splitRoleOne(name, maps.Keys(roles)); ok {
				label, n = one, 1
			}
			if n > 0 {
				label = nameResolver.Canonical(label)
				canon[idx], indexes[idx] = label, n
				roleNames[label] = true
			}
		}
		for name := range roleNames {
			if _, n := SplitRoleIndex(name); n > 0 {
				delete(roleNames, name)
			}
		}
		shareParagraphNumbers(paragraphs, canon, indexes)
		return canon, indexes, roleNames, pronouns, remaining, err
	}

	var starts []int
	for start := 0; start < len(paragraphs); start += maxBatchParagraphs {
		starts = append(starts, start)
	}

	// Up to attributeParallelBatches batches in flight at once, each
	// launched with a snapshot of the names known so far - a name one
	// in-flight batch discovers reaches every batch launched after it
	// finishes (canonicalizeSpeakerNames and the resolver fold any variant
	// spelling that slips through in the meantime). A rolling window
	// rather than fixed waves, so a narration-only batch (no model call at
	// all) never leaves the other slot idle.
	var (
		mu         sync.Mutex
		wg         sync.WaitGroup
		discovered []string // names first seen this call - listed in every later prompt
		stopAt     = len(starts)
		firstErr   error
	)
	// resolve is one raw answer's speaker and speaker number (roleNumber).
	resolve := func(idx int, a Attributed) (speaker string, role bool, number int) {
		answer, number := roleNumber(firstPersonAnswer(a.Speaker, roster.FirstPerson), a.Role, knownIsRole, resolver)
		speaker, role = sanitizeSpeaker(answer, a.Role)
		speaker = disallowNarratorForDialogue(isQuoteByIdx[idx], resolver.Canonical(speaker))
		if isSentinel(speaker) {
			return speaker, false, 0
		}
		return speaker, role, number
	}
	// record stores one line's answer. Callers hold mu.
	record := func(idx int, a Attributed) {
		speaker, role, number := resolve(idx, a)
		out[idx] = speaker
		delete(numberByIdx, idx)
		if number > 0 {
			numberByIdx[idx] = number
		}
		pronounsByIdx[idx] = a.Pronouns
		if !isSentinel(speaker) && !slices.Contains(known, speaker) {
			discovered = append(discovered, speaker)
		}
		known = addKnown(known, speaker)
		isRoleByIdx[idx] = role
		if role {
			knownIsRole[speaker] = true
		}
	}
	// batchRoster is the roster a batch launched now is shown. Callers
	// hold mu.
	batchRoster := func() Roster {
		return Roster{
			Names:        append([]string(nil), known...),
			Descriptions: roster.Descriptions,
			Roles:        maps.Clone(knownIsRole),
			Aliases:      roster.Aliases,
			Pronouns:     roster.Pronouns,
			Prominent:    append(append([]string(nil), roster.Prominent...), discovered...),
			// Once missing here, so no batch prompt ever said who "I" was
			// or showed scenes - the chapter-level fields reach each batch.
			Cast:        roster.Cast,
			Scenes:      roster.Scenes,
			FirstPerson: roster.FirstPerson,
			Corrections: roster.Corrections,
			noUnknown:   roster.noUnknown,
			confidence:  roster.confidence,
			prevNext:    roster.prevNext,
			taggedSeeds: roster.taggedSeeds,
		}
	}
	batchAnswer := func(start, end int) map[int]bool {
		answer := map[int]bool{}
		for _, p := range paragraphs[start:end] {
			if p.IsQuote && p.Known == "" && !p.ContextOnly {
				answer[p.Idx] = true
			}
		}
		return answer
	}
	sem := make(chan struct{}, attributeParallelBatches)
	for bi, start := range starts {
		if bi > 0 && shouldPause != nil && shouldPause() {
			mu.Lock()
			stopAt = min(stopAt, bi)
			mu.Unlock()
			break
		}
		sem <- struct{}{}
		mu.Lock()
		failed := firstErr != nil
		batchRoster := batchRoster()
		mu.Unlock()
		if failed {
			<-sem
			break
		}
		end := min(start+maxBatchParagraphs, len(paragraphs))
		answer := batchAnswer(start, end)
		lines := paragraphs[max(0, start-batchContextLines):end]
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			result, batchErr := ask(ctx, bookTitle, chapterTitle, batchRoster, lines, answer)
			mu.Lock()
			defer mu.Unlock()
			if batchErr != nil {
				if bi < stopAt {
					stopAt = bi
					firstErr = fmt.Errorf("paragraphs %d-%d: %w", paragraphs[start].Idx, paragraphs[end-1].Idx, batchErr)
				}
				return
			}
			for idx, a := range result {
				record(idx, a)
			}
		}()
	}
	wg.Wait()

	// Numbered roles across a batch seam. Batches run side by side, so the
	// one after a seam never saw which guard was "Guard 2" just before it
	// and starts again at 1 - the same man changing voice mid-conversation.
	// Where numbered role lines end one batch and that role speaks early in
	// the next, the next is asked again with the earlier lines shown as
	// settled, and its answers for that role replace the first ones. In
	// order, so each seam builds on the one before. One extra call per such
	// seam; a failed one keeps the first answers.
	for bi := 1; bi < min(stopAt, len(starts)) && ctx.Err() == nil; bi++ {
		start := starts[bi]
		end := min(start+maxBatchParagraphs, len(paragraphs))
		tail := append([]ParagraphInput(nil), paragraphs[max(0, start-seamContextLines):start]...)
		labels := map[string]bool{}
		for i, p := range tail {
			if sp := out[p.Idx]; p.IsQuote && p.Known == "" && sp != "" && !isSentinel(sp) {
				tail[i].Known, tail[i].KnownIndex = sp, numberByIdx[p.Idx]
				if numberByIdx[p.Idx] > 0 {
					labels[sp] = true
				}
			}
		}
		continues := false
		for _, p := range paragraphs[start:min(start+seamContextLines, end)] {
			continues = continues || labels[out[p.Idx]]
		}
		if !continues {
			continue
		}
		result, seamErr := ask(ctx, bookTitle, chapterTitle, batchRoster(), append(tail, paragraphs[start:end]...), batchAnswer(start, end))
		if seamErr != nil {
			continue
		}
		for idx, a := range result {
			if speaker, _, _ := resolve(idx, a); labels[speaker] || labels[out[idx]] {
				record(idx, a)
			}
		}
	}
	if stopAt < len(starts) {
		remaining = paragraphs[starts[stopAt]:]
	}
	err = firstErr
	// Reader-set lines join the result before finishing, so name
	// canonicalization and speech-tag refinement see them next to the
	// model's answers, and are put back after in case refinement moved one.
	for _, p := range paragraphs {
		if p.Known != "" {
			out[p.Idx] = p.Known
		}
	}
	canon, indexes, roleNames, pronouns, remaining, err := finish(out)
	for _, p := range paragraphs {
		if p.Known != "" {
			canon[p.Idx] = p.Known
			delete(indexes, p.Idx)
			if p.KnownIndex > 0 {
				indexes[p.Idx] = p.KnownIndex
			}
		}
	}
	return canon, indexes, roleNames, pronouns, remaining, err
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
func canonicalizeSpeakerNames(knownCharacters []string, raw map[int]string, roles map[string]bool) map[int]string {
	pool := map[string]bool{}
	for _, k := range knownCharacters {
		pool[k] = true
	}
	for _, v := range raw {
		if v != "" && v != "Narrator" && v != "Unknown" {
			pool[v] = true
		}
	}
	canon := canonMapFromPool(pool, roles)

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
// roles are the known role labels: a name is never folded into one of
// them ("Woman" is not "Refugee Woman" - a role is a type, not a person
// whose short name that would be), and one of them is never folded into a
// longer name.
func canonMapFromPool(pool map[string]bool, roles map[string]bool) map[string]string {
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
		if roles[short] {
			// Nor is a role label a short form of anyone's name: "Guard"
			// was folding into "The storm guard sergeant", who took every
			// guard's line in the scene (Spire's Spite ch. 62).
			continue
		}
		for _, long := range foldedNames {
			if long == short || roles[long] {
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
// (e.g. "The attendant"/"the attendant") or for which apostrophe they use
// ("Ve'delina"/"Ve’delina" - namekey.Fold) and maps every one of them to a
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
		lower := namekey.Fold(n)
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
// contiguously, as whole words somewhere inside long - by namekey.Fold
// (e.g. "Vesna" inside "Captain Vesna Thorne") - so two names merely
// sharing a run of letters at a non-word-boundary (like "Vesna" inside
// "Vesnathorpe") don't collapse.
func containsWholeName(long, short string) bool {
	lw := strings.Fields(namekey.Fold(long))
	sw := strings.Fields(namekey.Fold(short))
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

const systemPrompt = `You are a literary analysis assistant. Given numbered lines from a novel, identify who speaks each line of dialogue marked "(who?)".
Reply with ONLY plain text lines, no JSON, no code fence, no commentary: one line per "(who?)" line, made of that line's number, a colon and a space, then its speaker - adding " [role]" after a generic role label (see below), then " | " and the speaker's pronouns (see below) when known. For example:

12: Gandalf | he
14: Mira | she
15: Guard [role]
16: Unknown
21: Sailor 1 [role]
22: Sailor 2 [role]

Every "(who?)" line gets exactly one reply line. Lines without "(who?)" - narration, and dialogue outside the part you're asked about - are context only and never appear in your reply. Each number must be copied exactly from the number printed before that line.
Rules:
- speaker is the speaking character's short proper name (e.g. "Gandalf", not "the old wizard" or "he").
- speaker must always name exactly ONE person - never combine two or more names into a single value (e.g. "Toby and Jane", "Marcus & Elena", "Sam, Jake"), even when the line is addressed to multiple people, multiple characters are present in the scene, or more than one character seems to speak the same line at once. If a line is genuinely spoken by more than one character in unison, pick whichever one the narration credits as leading it, or use "Unknown" if that can't be determined from context - never invent a joint/combined speaker value.
- speaker must be the character's bare proper name only - strip away any descriptive or contextual modifier the surrounding narration happens to attach to them at that moment, even if it comes immediately before or after the name (e.g. "a now dressed Veronica" is just "Veronica"; "an exhausted Marcus" is just "Marcus"; "the newly arrived Captain Thorne" is just "Captain Thorne"). Those modifiers describe the character's state or situation in that instant, not their name - never fold one into the speaker value itself, even if it seems to help distinguish this appearance from another.
- Every "(who?)" line is spoken dialogue - NEVER answer "Narrator" for one, no matter who's speaking it. It always gets either a real character name, a role label (see below), or "Unknown".
- Reuse a name exactly as given in "Known characters" when the same character speaks again - never invent a variant spelling or a new label for a character already known. This applies within your own reply too: if the same character speaks more than once across these lines, use the exact same name for every one of their lines - never switch between a short form and a longer/titled form (e.g. "Vesna" vs. "Captain Vesna Thorne") for the same person. Matching a name against "Known characters", and matching two of your own lines against each other, is never sensitive to capitalization alone - "The attendant" and "the attendant" are the same character, not two different ones; once a name/role has an established spelling (from "Known characters", or from earlier in this same reply), keep using that exact spelling, capitalization included, rather than drifting to a differently-cased variant.
- A bare pronoun ("I"/"me"/"myself"/"he"/"she"/"him"/"her"/"they"/"them") is never a valid speaker value, even when context makes the referent obvious to you - resolve it to that character's actual proper name instead, or use exactly "Unknown" if the name genuinely can't be determined from context.
- A character's name appearing INSIDE the quoted words themselves does not mean that character is speaking - it almost always means the opposite: someone else is addressing them by name (e.g. "Well, Jake, I wasn't expecting that," or "Thank you, Jake," or a bare "Jake!") is being said TO Jake by another character, not BY Jake. Never pick a name as the speaker just because it's mentioned, vocative, or exclaimed inside the line - identify the actual speaker from the surrounding narration/context, and if that's genuinely unclear, use "Unknown" rather than defaulting to whoever's name appears in the text.
- If a dialogue line's speaker isn't a specific named individual, but the surrounding narration clearly gives a generic function/type for whoever's speaking instead (e.g. "the guard demanded," "a merchant called out," "one of the thugs replied"), use that type as speaker, followed by " [role]" - a short, generic, capitalized label with no article ("Guard", "Merchant", "Thug", never "a guard" or "the merchant"). A role label doesn't claim this is the same specific person every time it's used, only the same TYPE of minor, unindividuated speaker filling that function - reuse the same role label for every such speaker of that type throughout, the same way you'd reuse a real name, but understand it may cover several different unnamed people rather than one consistent individual. Only fall back to "Unknown" when even a generic type can't be determined. Never add " [role]" for a real named individual, including one only ever addressed by a title/epithet that clearly identifies one specific known person (e.g. "the captain" for a single established Captain Mocar) - that's a real individual, not a role, even though it reads like one; " [role]" is only for a genuinely interchangeable type.
- When two or more DIFFERENT unnamed people of the same type speak in the same conversation (two guards questioning someone, sailors calling to each other, several villagers answering in turn), number them so their lines can be told apart: "Guard 1 [role]", "Guard 2 [role]" - 1 for the first of them to speak, 2 for the next new one, and so on, each keeping their own number for every one of their lines in that conversation. The narration usually signals the switch ("the other guard said", "the taller one", "his companion", "a second voice", "the first man replied"). A type with only one such speaker in the conversation gets no number - plain "Guard [role]" - and a named character never gets one. The numbers only tell people apart within one conversation; they start again at 1 in the next. An entry in "Known characters" that already ends in a number ("Thug 1 [role]", "Thug 2 [role]") is one of several such people in this chapter: answer with it exactly as listed and never add a second number to it. A line shown with a numbered speaker in place of "(who?)" ("12 (Guard 2): …") tells you who that number is - keep the same numbers for the same people in the lines you answer.
- If a dialogue line's speaker is genuinely unclear from context, use exactly "Unknown" rather than guessing.
- Pronouns: after a named speaker, add " | " and ONE word - he, she, they, or it - for the pronoun the text uses for that character (e.g. "she said", "his voice", "the old man", "the queen"), or the pronouns listed for them in "Known characters". Omit the " | ..." part entirely when nothing establishes it - never guess from the name alone. Never add pronouns after "Unknown" or a " [role]" label. Pronouns can also help identify the speaker: a tag like "she said" can't belong to a character listed as "he/him".
- A blank line in "Lines:" marks a real paragraph break. Lines with NO blank line between them come from one continuous original paragraph - often a quote, then a short "X said" narration tag, then more dialogue right after it, but just as often a quote followed by a longer run of action/description/thought (several sentences, not just a short tag) before the next quote. In either case, the quote before that in-between narration and the quote after it are almost always the SAME speaker - the narration is a beat inside one character's continuous turn, not a hand-off to someone else, no matter how long that beat runs. This holds even when the in-between narration mentions another character by name or describes something that other character does - a passing mention or reaction shot does NOT make that other character the speaker of the quote that follows. Only attribute the quote after the gap to a different speaker when the in-between narration itself explicitly says someone else is now speaking (e.g. a tag like "Jake cut in" or "she interrupted" naming a different character than whoever spoke the line before it).
- Conversely, when a dialogue line is separated from the dialogue line before it by a blank line (a real paragraph break) with no narration tag anywhere between them naming who's speaking, default to treating that break itself as a signal the speaker changed, not a continuation of the same speaker - back-and-forth dialogue conventionally starts a new paragraph for each new speaker's turn. The new speaker is whoever the exchange is with: in a back-and-forth between two people, an untagged new paragraph is the other one of those two - never a third character who hasn't spoken or been named in the last several lines, just because they're present or known. Only keep the same speaker across a paragraph break like that when something in the surrounding context clearly supports it (e.g. a tag elsewhere in the passage naming them again, or the dialogue is obviously one uninterrupted speech continuing across a paragraph break rather than an exchange).
- Some entries in "Known characters" carry a short parenthetical description after their name (e.g. "Captain Mocar (a weathered, broad-shouldered sea captain...)"). That's there to help you recognize them when a narration tag names them by role or epithet instead of their proper name ("the captain said," "the old orc grunted") - if a tag's role/epithet clearly matches one character's own parenthetical description and no other known character fits better, attribute the line to that character's exact proper name, never the role/epithet itself (no " [role]" - this is a real individual, not a generic type). Still use "Unknown" if the match is genuinely ambiguous (e.g. the role could plausibly fit more than one known character, or nothing in "Known characters" matches it at all).
- Some entries in "Known characters" list other names that character goes by ("Bert (also called Albert, Al)"). A line whose speaker the text calls by any of those names belongs to that entry - answer with the entry's own main name ("Bert"), never the other name.
- speaker never names a group ("Toby and Jane", "the two clerks", "both of them") - pick the one person the narration credits, or use "Unknown".
- "Lines:" may contain "[Scene: ...]" markers where a scene starts, from notes on the chapter, naming where it is and who is present. Treat them as a hint: prefer someone present in the line's scene over someone the notes place only in another scene (they may have left, or be dead). The notes can be incomplete or wrong, though - when the lines themselves name someone as being there ("seeing the way Mira was watching me", "Tomas said"), the text wins over the notes, and a generic label in them ("woman", "Man") may stand for a known character the notes failed to name.
- A dialogue line printed with a name in place of "(who?)" (e.g. "12 (Mira): …") has a speaker the reader already confirmed. It is context, like narration: use it to follow the turn-taking and who is in the conversation, and never answer it.
- A "Reader corrections" list shows lines from earlier chapters that were attributed wrongly, each with the right speaker and the wrong answer. They are not among these lines - use them to learn how this book marks its speakers (a character's way of talking, who an epithet or title refers to, a mix-up to avoid), never copy an answer from them.
- Some entries in "Known characters" are marked "[role]" instead of (or alongside) a description - that means the name is itself a generic type label already established earlier (e.g. "Guard [role]"), not one consistent individual. Reuse it exactly, with " [role]", whenever a new line's speaker fits that same generic type, exactly as if it were a brand-new role label - never treat two different appearances of a "[role]" name as necessarily the same specific person.`

// Roster is what attribution knows about a book's characters going in:
// their names, an optional identifying blurb per name (Descriptions),
// which names are generic role labels (Roles), other names each one goes
// by (Aliases, canonical name -> aliases), and their established
// pronouns (Pronouns, see store.Character.Pronouns). Every map is
// nil-safe.
//
// Prominent (optional) are the names every batch's prompt lists even when
// its lines never mention them - a book's main cast, who speak on most
// pages behind a bare "he said". Everyone else in Names is listed only
// when the batch's own text mentions them (relevantNames): on a long
// series the full roster ran to a median 8.7K characters per batch -
// more than twice the lines being attributed - re-read on every batch.
type Roster struct {
	Names        []string
	Descriptions map[string]string
	Roles        map[string]bool
	Aliases      map[string][]string
	Pronouns     map[string]string
	Prominent    []string
	// Cast is the chapter's cast from its summary (WithCast), nil when it
	// has none: an answer that is one of a member's references ("the old
	// sailor") becomes their name.
	Cast []CastMember
	// Scenes are the chapter's scenes from its summary (httpapi's
	// withChapterCast), nil when it has none: a batch lists as Prominent
	// only who is present in the scenes its lines fall in (scenePresent).
	Scenes []Scene
	// FirstPerson is a first-person book's narrator (BookSetting.Narrator,
	// resolved onto the roster), "" for a third-person book. Every batch is
	// told who "I" is, an "I said" tag credits them, and an answer that
	// names the "I" without a name ("Protagonist", "Narrator" on a line of
	// dialogue) becomes them - see firstPersonAnswer.
	FirstPerson string
	// World are the story's own titles, races and nationalities
	// (store.WorldFact.Class): a word of a name that names no one by
	// itself - see SpeakerNameResolver.WithTitles.
	World WorldWords
	// Corrections are the reader's fixes to dialogue speakers in earlier
	// chapters of the book, in reading order. Each batch lists the few that
	// involve the characters its own lines are about (relevantCorrections).
	Corrections []Correction
	// noUnknown is AttributeOptions.NoUnknown, carried to every batch:
	// the prompt (attributionSystemPrompt) and the decide candidates offer
	// no "Unknown".
	noUnknown bool
	// confidence, when set, collects every decided line's confidence
	// (attributeRedecided).
	confidence *confidenceSink
	// prevNext and taggedSeeds are AttributeOptions.PrevNext and
	// TaggedSeeds, carried to every batch.
	prevNext, taggedSeeds bool
}

// Correction is one dialogue line whose speaker a reader corrected by
// hand (store.ParagraphEdit): the line, the speaker attribution had given
// it, and the reader's.
type Correction struct {
	ChapterIdx int
	Text       string
	Wrong      string
	Right      string
}

// maxBatchCorrections caps how many corrections one batch prompt lists,
// and maxCorrectionsPerPair how many of those may repeat one wrong->right
// mix-up, so a single often-fixed confusion doesn't fill the list.
const (
	maxBatchCorrections    = 6
	maxCorrectionsPerPair  = 2
	maxCorrectionTextChars = 200
)

// relevantCorrections picks the corrections worth showing a batch whose
// relevant names are names (plus the first-person narrator): those whose
// right or wrong speaker is one of them, nearest chapter first, capped at
// maxBatchCorrections. A correction to or from Narrator teaches nothing
// about who speaks and is skipped.
func relevantCorrections(corrections []Correction, names []string, firstPerson string) []Correction {
	if len(corrections) == 0 {
		return nil
	}
	relevant := make(map[string]bool, len(names)+1)
	for _, n := range names {
		relevant[strings.ToLower(n)] = true
	}
	if firstPerson != "" {
		relevant[strings.ToLower(firstPerson)] = true
	}
	perPair := map[[2]string]int{}
	var out []Correction
	for i := len(corrections) - 1; i >= 0 && len(out) < maxBatchCorrections; i-- {
		c := corrections[i]
		if c.Wrong == "" || c.Right == "" || c.Wrong == "Narrator" || c.Right == "Narrator" {
			continue
		}
		if !relevant[strings.ToLower(c.Right)] && !relevant[strings.ToLower(c.Wrong)] {
			continue
		}
		pair := [2]string{c.Wrong, c.Right}
		if perPair[pair] >= maxCorrectionsPerPair {
			continue
		}
		perPair[pair]++
		out = append(out, c)
	}
	return out
}

// clipText cuts s to at most n bytes on a rune boundary, marking the cut.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// firstPersonLabels are answers a model gives for a first-person
// narrator's own lines when it doesn't know (or ignores) their name -
// Qwen3.6 labeled 1,437 of Arise's lines "Protagonist".
var firstPersonLabels = map[string]bool{
	"i": true, "me": true, "myself": true, "protagonist": true, "the protagonist": true,
	"main character": true, "the main character": true, "mc": true, "narrator": true,
	"the narrator": true, "first-person narrator": true, "the first-person narrator": true,
	"narrator (i)": true, "hero": true, "the hero": true, "pov character": true,
}

// IsFirstPersonLabel reports whether name is a placeholder a model uses for
// a first-person narrator ("Protagonist", "Main character", "I") rather
// than anyone's name.
func IsFirstPersonLabel(name string) bool {
	return firstPersonLabels[strings.ToLower(strings.TrimSpace(name))]
}

// firstPersonAnswer maps a raw answer that stands for the "I" to the
// book's first-person narrator; other answers (and every answer in a
// third-person book) pass through.
func firstPersonAnswer(raw, firstPerson string) string {
	if firstPerson != "" && firstPersonLabels[strings.ToLower(strings.TrimSpace(raw))] {
		return firstPerson
	}
	return raw
}

// scenePresent narrows roster.Prominent (the chapter's cast, with a
// summary) to the scenes lines fall in: who is present in any of them,
// plus the first-person narrator and any cast member the summary placed in
// no scene at all (a voice on a phone, a presence it didn't pin down).
// Names outside those scenes are only listed when the batch's own text
// mentions them (relevantNames) - Arise ch. 8 gave the protagonist's
// friend's lines to a thug who'd been killed two scenes earlier. Prominent
// unchanged without scenes, or when the scenes found list nobody.
func (r Roster) scenePresent(lines []ParagraphInput) []string {
	if len(r.Scenes) == 0 || len(lines) == 0 {
		return r.Prominent
	}
	lo, hi := lines[0].Idx, lines[0].Idx
	for _, p := range lines {
		lo, hi = min(lo, p.Idx), max(hi, p.Idx)
	}
	here, placed := map[string]bool{}, map[string]bool{}
	found := false
	for i, sc := range r.Scenes {
		for _, n := range sc.Present {
			placed[strings.ToLower(n)] = true
		}
		end := hi + 1
		if i+1 < len(r.Scenes) {
			end = r.Scenes[i+1].StartIdx
		}
		// The first scene also covers anything before its start.
		if (i == 0 || sc.StartIdx <= hi) && end > lo {
			for _, n := range sc.Present {
				here[strings.ToLower(n)] = true
				found = true
			}
		}
	}
	if !found {
		return r.Prominent
	}
	var out []string
	for _, n := range r.Prominent {
		k := strings.ToLower(n)
		if here[k] || !placed[k] || n == r.FirstPerson {
			out = append(out, n)
		}
	}
	return out
}

// sceneStarting returns the scene a batch's line at idx opens - one
// starting after prevIdx (the line before it in the batch, -1 for the
// batch's first line, which gets the scene it falls in) and at or before
// idx - or nil.
func (r Roster) sceneStarting(prevIdx, idx int) *Scene {
	var at *Scene
	for i := range r.Scenes {
		sc := &r.Scenes[i]
		switch {
		case prevIdx < 0 && (sc.StartIdx <= idx || i == 0):
			at = sc
		case prevIdx >= 0 && sc.StartIdx > prevIdx && sc.StartIdx <= idx:
			at = sc
		}
	}
	if at == nil || len(at.Present) == 0 && at.Location == "" {
		return nil
	}
	return at
}

// sceneMarker renders sc for a "[Scene: ...]" marker in the Lines list.
func sceneMarker(sc *Scene) string {
	var parts []string
	if sc.Location != "" {
		parts = append(parts, sc.Location)
	}
	if len(sc.Present) > 0 {
		parts = append(parts, "present: "+strings.Join(sc.Present, ", "))
	}
	return strings.Join(parts, "; ")
}

// relevantNames picks which of roster.Names to list in a prompt for
// lines: every Prominent name (narrowed to the lines' scenes -
// scenePresent), plus any name, alias, or distinctive word
// of either (see SpeakerNameResolver's own token rules) that appears as a
// whole word in the lines' text. Keeps roster order.
func relevantNames(roster Roster, lines []ParagraphInput) []string {
	var text strings.Builder
	for _, p := range lines {
		text.WriteString(strings.ToLower(p.Text))
		text.WriteByte(' ')
	}
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(text.String(), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\'' && r != '’' && r != '-'
	}) {
		w = strings.TrimSuffix(strings.TrimSuffix(w, "’s"), "'s")
		words[strings.Trim(w, "'’-")] = true
	}
	mentioned := func(form string) bool {
		distinctive := false
		for _, w := range strings.Fields(strings.ToLower(form)) {
			w = strings.Trim(w, ".,'’")
			if len(w) < 3 || titleWords[w] {
				continue
			}
			distinctive = true
			if words[w] {
				return true
			}
		}
		// A name made only of title words - usually a role label like
		// "Guard" or "Captain" - counts when that word itself appears.
		if !distinctive {
			for _, w := range strings.Fields(strings.ToLower(form)) {
				if words[strings.Trim(w, ".,'’")] {
					return true
				}
			}
		}
		return false
	}
	prominent := map[string]bool{}
	for _, n := range roster.scenePresent(lines) {
		prominent[n] = true
	}
	for _, p := range lines {
		if p.Known != "" {
			prominent[p.Known] = true
		}
	}
	var out []string
	for _, name := range roster.Names {
		keep := prominent[name] || mentioned(name)
		for _, a := range roster.Aliases[name] {
			keep = keep || mentioned(a)
		}
		if keep {
			out = append(out, name)
		}
	}
	return out
}

// maxMissingFollowUps is how many extra rounds attributeBatch spends
// re-asking lines the model skipped. Measured on Spire's Spite chapters
// 5-9: the plain-text reply format silently left out ~10% of asked lines
// (a couple per 40-line batch) - the single biggest accuracy loss against
// the old answer-every-line JSON format until these follow-ups, each a
// short reply for just the missing lines.
const maxMissingFollowUps = 2

// attributeBatch asks for the speaker of every line in answer (paragraph
// Idx values, all within lines); every other line in lines is shown as
// context only. Skips the model entirely when answer is empty - a
// narration-only stretch has nothing to attribute. Lines the reply skips
// are asked again (only those marked) up to maxMissingFollowUps times,
// stopping early if a round answers none of them.
func (c *Client) attributeBatch(ctx context.Context, bookTitle, chapterTitle string, roster Roster, lines []ParagraphInput, answer map[int]bool) (map[int]Attributed, error) {
	out, err := c.attributeBatchOnce(ctx, bookTitle, chapterTitle, roster, lines, answer)
	if err != nil {
		return nil, err
	}
	for round := 0; round < maxMissingFollowUps; round++ {
		missing := make(map[int]bool)
		for idx := range answer {
			if _, ok := out[idx]; !ok {
				missing[idx] = true
			}
		}
		if len(missing) == 0 {
			break
		}
		more, err := c.attributeBatchOnce(ctx, bookTitle, chapterTitle, roster, lines, missing)
		if err != nil {
			break // keep what the first reply gave; the rest stay unattributed
		}
		for idx, a := range more {
			out[idx] = a
		}
		if len(more) == 0 {
			break
		}
	}
	return out, nil
}

func (c *Client) attributeBatchOnce(ctx context.Context, bookTitle, chapterTitle string, roster Roster, lines []ParagraphInput, answer map[int]bool) (map[int]Attributed, error) {
	if len(answer) == 0 {
		return map[int]Attributed{}, nil
	}
	return generateAndParse(ctx, c, roster.attributionSystemPrompt(), c.attributionUserPrompt(bookTitle, chapterTitle, roster, lines, answer), 0, attributeMaxTokens, func(content string) (map[int]Attributed, error) {
		return parseAttributionLines(content, answer)
	})
}

// attributionUserPrompt is an attribution batch's user prompt: the known
// characters relevant to lines, earlier corrections, and the numbered
// lines, those in answer marked "(who?)".
func (c *Client) attributionUserPrompt(bookTitle, chapterTitle string, roster Roster, lines []ParagraphInput, answer map[int]bool) string {
	knownCharacters, knownDescriptions, knownRoles := relevantNames(roster, lines), roster.Descriptions, roster.Roles
	var user strings.Builder
	fmt.Fprintf(&user, "Book: %s\nChapter: %s\n", bookTitle, chapterTitle)
	if len(knownCharacters) > 0 {
		entries := make([]string, len(knownCharacters))
		for i, name := range knownCharacters {
			var notes []string
			if p := roster.Pronouns[name]; p != "" {
				notes = append(notes, p)
			}
			if aliases := roster.Aliases[name]; len(aliases) > 0 {
				notes = append(notes, "also called "+strings.Join(aliases, ", "))
			}
			if desc := knownDescriptions[name]; desc != "" {
				notes = append(notes, oneLine(desc))
			}
			entries[i] = name
			if knownRoles[name] {
				entries[i] += " [role]"
			}
			if len(notes) > 0 {
				entries[i] += " (" + strings.Join(notes, "; ") + ")"
			}
		}
		fmt.Fprintf(&user, "Known characters: %s\n", strings.Join(entries, ", "))
	}

	if corrections := relevantCorrections(roster.Corrections, knownCharacters, roster.FirstPerson); len(corrections) > 0 {
		user.WriteString("Reader corrections from earlier chapters (lines attribution got wrong, with the right speaker):\n")
		for _, c := range corrections {
			fmt.Fprintf(&user, "- %s - %s, not %s\n", clipText(oneLine(c.Text), maxCorrectionTextChars), c.Right, c.Wrong)
		}
	}

	if roster.FirstPerson != "" {
		fmt.Fprintf(&user, "This book is narrated in the first person by %s: \"I\" and \"me\" in the narration are %s, so a line the narration tags \"I said\" is %s's, and a line tagged \"he said\" or \"she asked\" is someone else's.\n", roster.FirstPerson, roster.FirstPerson, roster.FirstPerson)
	}
	user.WriteString("\nLines:\n")
	for i, p := range lines {
		// A blank line marks a real paragraph break - see systemPrompt.
		// Never before the very first line of the batch, and never for a
		// split-out continuation of the paragraph right before it (that's
		// exactly what Inline means).
		if i > 0 && !p.Inline {
			user.WriteString("\n")
		}
		prevIdx := -1
		if i > 0 {
			prevIdx = lines[i-1].Idx
		}
		if sc := roster.sceneStarting(prevIdx, p.Idx); sc != nil {
			fmt.Fprintf(&user, "[Scene: %s]\n", sceneMarker(sc))
		}
		switch {
		case answer[p.Idx]:
			fmt.Fprintf(&user, "%d (who?): %s\n", p.Idx, oneLine(p.Text))
			if roster.prevNext {
				user.WriteString(prevNextNote(lines, i))
			}
		case p.Known != "" && p.IsQuote:
			known := knownLabel(p)
			if roster.taggedSeeds && p.Seeded {
				known = "tagged: " + known
			}
			fmt.Fprintf(&user, "%d (%s): %s\n", p.Idx, known, oneLine(p.Text))
		default:
			fmt.Fprintf(&user, "%d: %s\n", p.Idx, oneLine(p.Text))
		}
	}
	if c.cfg.NoThink {
		user.WriteString("\n/no_think")
	}
	return user.String()
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
	// Pronouns is the reply's own " | he" part, normalized (see
	// NormalizePronouns) - "" when the model gave none.
	Pronouns string
}

// attributionLineRe matches one reply line: "<idx>: <speaker>",
// tolerating a copied "(who?)" marker, a dash instead of a colon, and
// surrounding whitespace.
var attributionLineRe = regexp.MustCompile(`^\s*(\d+)\s*(?:\(who\?\))?\s*[:\-–]\s*(.+?)\s*$`)

// parseAttributionLines reads attributeBatch's plain-text reply, keeping
// only lines whose idx was asked about (answer). A speaker suffixed
// " [role]" is a role label (sanitizeSpeaker strips the marker); a
// trailing " | she" is that speaker's pronouns. A model
// that answers in the old JSON-array shape anyway still parses, via
// parseAttributions. Only a reply with no usable line at all is an error
// (so generateAndParse retries it) - a line the model skipped just stays
// unattributed, the same as a dropped JSON entry always did. A reply
// whose numbers are all wrong but which has exactly one line per asked
// line is read by position.
func parseAttributionLines(content string, answer map[int]bool) (map[int]Attributed, error) {
	out := make(map[int]Attributed, len(answer))
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "```") {
		if items, err := parseAttributions(content); err == nil {
			for idx, a := range items {
				if answer[idx] {
					out[idx] = a
				}
			}
		}
	}
	if len(out) == 0 {
		var unasked []Attributed // reply lines whose number wasn't asked about, in order
		for _, line := range strings.Split(content, "\n") {
			m := attributionLineRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			idx, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			speaker, pronouns := splitPronouns(m[2])
			speaker = strings.Trim(speaker, ` "'*.“”`)
			if speaker == "" {
				continue
			}
			a := Attributed{Speaker: speaker, Role: strings.Contains(strings.ToLower(speaker), "[role]"), Pronouns: pronouns}
			if !answer[idx] {
				unasked = append(unasked, a)
				continue
			}
			out[idx] = a
		}
		// Miscopied numbers: a reply with one line per asked line, none
		// under an asked number, is taken by position. A synopsis chapter's
		// only asked line, 50, came back as "49: …" on every retry and the
		// chapter's attribution failed for good (Arise: Explorer ch. 4).
		if len(out) == 0 && len(unasked) == len(answer) {
			asked := slices.Sorted(maps.Keys(answer))
			for i, a := range unasked {
				out[asked[i]] = a
			}
		}
	}
	if len(out) == 0 && len(answer) > 0 {
		return nil, fmt.Errorf("no speaker lines found in response: %s", truncate(content, 200))
	}
	return out, nil
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
	value, found := firstJSONValue(content, '[')
	if !found {
		return nil, fmt.Errorf("no JSON array found in response: %s", truncate(content, 200))
	}
	var items []attribution
	if err := unmarshalRepairedArray(value, &items); err != nil {
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

// CastVoice is another character's existing casting (store.Character's
// Name and Summary), shown to CharacterizeVoice so the new voice stays
// distinguishable from the people it shares scenes with - Spire's Spite's
// two leads, Fritz and Bert, were both cast "gruff, gravelly, middle-aged
// working-class British" when each was characterized alone.
type CastVoice struct {
	Name     string
	Instruct string
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

// characterizeMaxTokens bounds a characterization's generated JSON
// response - three fields ("evidence", then "instruct", roughly 35-60
// words, and "refLine", roughly 28-40; see characterizeSystemPrompt), not just
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

const characterizeSystemPrompt = `You are the casting director for an audiobook. From a character's dialogue and the narration describing them, cast the voice they speak with. Your casting is rendered by a voice-design model into one short reference clip, and every line the character ever says is cloned from that clip. Separate emotion variants (angry, afraid, whispering...) are made from it later, so the clip must be their ordinary resting voice, not their emotional range.

Reply with ONLY a JSON object, no other text:
{"evidence": "...", "instruct": "...", "refLine": "..."}

"evidence": one or two sentences, written first, quoting or citing what the material actually says about gender, age, origin/accent, species and size. Write "none" for anything the material doesn't show. If something happens to them during the story that changes how they sound for a while - an injury, illness, drink, a transformation - end with "Temporary, not their voice: ..." naming it. Base the other two fields on this, not on guesses, and keep everything named as temporary out of them.

"instruct": 35-60 words, starting "Speak as a ...", a direct casting note to a narrator. Cover, in this order:
1. Gender word and age group in the opening clause: "Speak as a teenage boy", "a young woman", "a middle-aged man", "an elderly woman". Age comes from evidence - a stated age, how others address them (boy, lad, old man, miss), their peers' ages, their station (apprentice, grandmother, veteran). Rank or authority is not age. With no evidence, choose young adult. A number of years is a human age only for a human: one of a long-lived kind (elf, dwarf, vampire, dragon, spirit, anyone the material calls ageless or centuries old) gets the age group their life among their own people shows - still serving, fighting or working beside younger adults is a seasoned adult ("a woman in her thirties", "a man in his forties"), however many decades they count, and not a young one when those years are more than a human lifetime's work; elderly only when the text calls them old, and never a child or teenager because their kind lives long.
2. Pitch, pace, and timbre/texture, each named concretely. Choose them from the character, not by default: children and small creatures sit higher and lighter, old voices thinner or rougher, big ones lower and fuller; quick talkers are quick. A grown adult, however young, has an adult's voice: give a high or girlish pitch only when the material describes one.
3. One specific accent (see below).
4. Their resting attitude as it colors ordinary speech: one or two traits that most of their lines share, in your own words from the material - not their most extreme moment, and not a stock adjective the material doesn't support. A joker should sound amused, a nervous character hesitant, a protagonist whose lines are mostly questions and quips should not come out as cold or haughty.
Describe only what can be heard. Leave out looks, clothes, eyes, scars, posture, and backstory unless they change the sound (a huge chest booms, a tiny creature squeaks, a lifelong stammer halts). Cast their own everyday voice, not a temporary form or state: armor, a helmet or mask, a transformation or battle form, a disguise, possession, or magic that changes them for a fight or a scene says nothing about how they normally sound - leave its size, species, and texture (metallic, echoing, towering, winged) out unless the material shows they are always that way. The same goes for something that happens to them during the story: an injury (broken teeth, a crushed throat, a split lip), illness, drunkenness, exhaustion, a gag. Lines slurred, lisped, or rasped because of it are that moment, not their voice: cast how they sounded before it, and say nothing about it in "instruct", not even that it is absent. Only a condition they have from their first line onward belongs in the casting. Never describe how the voice changes when angry, afraid, or excited; never name the character or a real person.

Gender: "Pronouns:" is authoritative - he/him is male, she/her is female, they/them and it/its take no gender word unless the description states one. With no pronouns, use explicit narration (he said, the woman); otherwise omit gender.

Accent: every character gets exactly one named accent (Scottish, Received Pronunciation, Cockney, Yorkshire, General American, Irish, German...). When the material ends with an "Accent (decided ...)" line, that is their accent: it was worked out from the evidence already, so name exactly it in "instruct", let it show in "refLine", and ignore the order below. Otherwise decide it in this order, stopping at the first that applies:
- An explicit signal in the material wins over everything below, including the setting notes: a stated origin, a described accent, dialect written into their speech (dinnae, y'all, ja), foreign words they use.
- "Origin clues" are narration sentences naming them near a nationality, language, or accent word. A nationality they are given ("a Danish archeologist") or a language they speak among their own people or under stress (swearing, praying, whispering in Danish) is an explicit origin signal: cast that country's accent speaking English. A language they merely learned, study, or talk about isn't, and a clue about someone else in the same sentence isn't theirs.
- In a real-world setting, a name clearly from another country than the story's place (a German name in London) is a signal too, a weaker one.
- Idiom of one national English recurring in their own lines is a signal too, even against the setting notes: British (mate, bloody, arse, bloke, oi, sodding), Scottish (aye, bairn, wee, lass, dinnae, och - Scottish unless the material places them in the north of England), or American (dude, gotten, mom, y'all - which is Southern American). "Idiom words across all their lines" counts these over everything they say, and decides it over the sample: one "aye" or "dude" in the sample is not an accent when the counts lean the other way.
- Otherwise use the setting notes' accent for the main characters' people; when it names several (a mixed cast), use the first. With no setting accent, pick the standard accent the dialogue's idiom points to ("mate", "bloody", "arse", "reckon" read standard British). Most of a cast shares one accent - choose a different one only on a signal.
- Slang, swearing, dropped g's, bad grammar, and being poor or rough are not regional signals: a working-class character keeps the cast's accent with a rougher, more casual register, not a new accent. Never use a Southern American accent unless the text places them in the American South. Name the accent only; don't list spelling or grammar features in "instruct".
- Non-human kinds with no signal of their own take the genre-standard voice: dwarf - Scottish; elf - refined RP; orc/goblin - guttural Cockney; giant/troll - broad rural; dragon or demon - plummy aristocratic RP (a beastly demon growls instead); fairy - light Irish; robot/AI/construct - flat, precisely enunciated, no accent; ghost - faint, old-fashioned English. Only for that kind, and only when the material establishes it - never invent a species.
- Non-human kinds, when established, also shape pitch and texture by size and nature: a hiss for a snake-folk, a resonant rumble for something huge, a thin quick voice for something tiny, an airy reverb for a spirit or angel. Size comes from what the material says of this character, never from the kind's name alone: an elf, an orc or a vampire is human-sized, with a human-range voice, unless the text says otherwise.

A "Note:" that the name is a ROLE label means several unnamed people shared it: cast the typical member of that type from traits that recur across lines, not one line's quirk. What happened to one of them - an injury, a described feature, a way of talking - belongs to that one person and never goes into the shared casting.

"World notes" are facts the reader has recorded, each filed under the word that brought it up ("kobold: ...") - about this story's world, or rules for every book. They are true of this story and outrank the genre defaults above and anything you would assume from a human. A note on what accent a kind has or how it sounds replaces the genre-standard voice. A note on how long a kind lives or how it ages says how to read an age the material gives this character: the age group is where that age falls in their own kind's lifespan, so an age that is old for their kind is an elderly voice, however few the years. Without an age, or other evidence of how old this character is, such a note changes nothing about their age group. A note about a kind applies only if the material establishes the character as one.

"Voice clues" are narration sentences about how they sound or how old they are, mostly the narration around their own lines. One that states a lasting quality (a deep voice, a lilting accent, "her usual drawl", "his normally steady voice", an age) is direct evidence for pitch, timbre, accent and age, and outranks what you would guess from the dialogue. One about a single moment (lowered, shaking, hoarse from shouting, put on as a disguise) is not their voice. A clue can be about someone else in the same sentence - use only what is said of this character.

"Their age, in their own words" lists lines of theirs that give their age, each with the line it answered in brackets when there was one. It is the strongest evidence of how old they are and outranks every guess - unless the number is plainly something else (a level, a count, a rank) or an age they were at some earlier time.

"First mentions" is the first narration that names them, and "Statements about them" are narration sentences found by search where their name is followed by a trait. In both, a line that describes someone they are looking at or talking to is not about them, and when this character copies or remarks on another person's voice or accent, that is the other person's.

"Already cast" lists the voices of other characters who share the most scenes with this one. Listeners must tell them apart by ear, so make this voice clearly different from each - in pitch, pace, or texture, not only in attitude - wherever the material leaves room. Never change gender, age group, or accent against the evidence to do it. They are other people: never reuse their wording, species, size, or traits - everything about this voice comes from this character's own material. Never mention them in "instruct" ("distinct from the British Steve"): the voice-design model reads only this one voice's description, so the difference must be in the pitch, pace, and texture you choose, not stated.

"refLine": 28-40 words, three sentences, that this character might say aloud on an ordinary calm day - in their own voice, vocabulary, and rhythm, but about nothing specific to the plot. It is read aloud by the voice-design model, so:
- Plain statements, at most one question, no exclamation marks, no shouting, no ellipses or dashes.
- No names, places, or invented words from the story.
- Show the accent through word choice and phrasing (aye, wee, reckon, mate, ja), never through apostrophe respellings ('ere, nothin', yer, fassster) - the reader model mispronounces those.
- Match their resting pace: short plain sentences for a curt voice, longer flowing ones for a measured one.
- Make it sound like this person and nobody else: their kind of humor, bluntness, formality, or warmth. Open with something only they would say, not a filler phrase.`

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
//
// pronouns (see store.Character.Pronouns; "" when unknown) goes on the
// user turn the same way - characterizeSystemPrompt treats it as the
// authoritative gender signal.
//
// evidence goes on the
// user turn section by section: "Their dialogue" (maxCharacterizeQuotes
// of Quotes), "First mentions", "Statements about
// them" (see TraitStatements), "Idiom words across all their lines",
// "Origin clues" (see OriginClues) and "Voice clues" (see VoiceClues).
//
// A casting that doesn't cast pronouns' gender (genderContradicts) is
// retried with a reminder; the last attempt's is kept either way, since
// Pronouns itself can be the thing that's wrong.
//
// costars (see httpapi.characterizeVoice: the characters sharing the most
// chapters with this one who are already characterized) go on the user
// turn as "Already cast".
func (c *Client) CharacterizeVoice(ctx context.Context, name string, isRole bool, pronouns string, setting *BookSetting, evidence CharacterEvidence, costars []CastVoice, opts CastingOptions) (instruct string, refLine string, err error) {
	quotes, origins, voiceClues := evidence.Quotes, evidence.Origins, evidence.VoiceClues
	introductions, traits := evidence.Introductions, evidence.Traits
	if len(quotes) > maxCharacterizeQuotes {
		// Evenly, not the first: the caller's sample is already spread
		// over the character's whole run.
		spread := make([]QuoteContext, maxCharacterizeQuotes)
		for i := range spread {
			spread[i] = quotes[i*len(quotes)/maxCharacterizeQuotes]
		}
		quotes = spread
	}

	var user strings.Builder
	fmt.Fprintf(&user, "Character: %s\n", name)
	if pronouns != "" {
		fmt.Fprintf(&user, "Pronouns: %s\n", pronouns)
	}
	if isRole {
		fmt.Fprintf(&user, "Note: %q is a generic role/type label, not one consistent individual - the sample below may be pooled from several different unnamed people who shared it.\n", name)
	}
	if lines := setting.castingLines(); lines != "" {
		fmt.Fprintf(&user, "\nBook setting notes:\n%s", lines)
	}
	if len(evidence.WorldNotes) > 0 {
		user.WriteString("\nWorld notes:\n")
		for _, n := range evidence.WorldNotes {
			fmt.Fprintf(&user, "- %s\n", oneLine(n))
		}
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
	if len(introductions) > 0 {
		user.WriteString("\nFirst mentions:\n")
		for _, in := range introductions {
			fmt.Fprintf(&user, "- %s\n", oneLine(truncate(in, maxIntroductionChars)))
		}
	}
	if len(traits) > 0 {
		user.WriteString("\nStatements about them:\n")
		for _, t := range traits {
			fmt.Fprintf(&user, "- %s\n", oneLine(t))
		}
	}
	if idioms := evidence.idiomLines(); idioms != "" {
		fmt.Fprintf(&user, "\nIdiom words across all their lines:\n%s", idioms)
	}
	if len(origins) > 0 {
		user.WriteString("\nOrigin clues:\n")
		for _, o := range origins {
			fmt.Fprintf(&user, "- %s\n", oneLine(o))
		}
	}
	if len(evidence.StatedAges) > 0 {
		user.WriteString("\nTheir age, in their own words:\n")
		for _, a := range evidence.StatedAges {
			fmt.Fprintf(&user, "- %s\n", a)
		}
	}
	if len(voiceClues) > 0 {
		user.WriteString("\nVoice clues:\n")
		for _, v := range voiceClues {
			fmt.Fprintf(&user, "- %s\n", oneLine(v))
		}
	}
	// A casting that copies a co-star is retried without that co-star in
	// "Already cast", and the last attempt shows none: the model can't
	// copy wording it isn't shown, where re-asking with the same prompt
	// usually copied again (the closer the two characters, the more
	// reliably) until the characterization failed for good. A
	// casting written without a co-star in view is the character's own
	// even when it comes out close to theirs, so only the co-stars shown
	// are checked.
	shown := costars
	copied := map[string]bool{}
	wrongGender, last := false, false
	searched, found := false, ""
	var accent *AccentDecision
	wrongAccent := false
	result, err := retryParse(func(attempt int) (string, error) {
		last = attempt == maxGenerateRetries
		if len(copied) > 0 {
			shown = nil
			if attempt < maxGenerateRetries {
				for _, cv := range costars {
					if !copied[cv.Name] {
						shown = append(shown, cv)
					}
				}
			}
		}
		prompt := user.String()
		if opts.Lookup != nil {
			// Searched once, from the material alone: a retry casts again
			// from the same results.
			if !searched {
				section, err := c.lookupSection(ctx, prompt, opts)
				if err != nil {
					return "", err
				}
				searched, found = true, section
			}
			prompt += found
		}
		// Decided once, from the material and what the searches found,
		// before the co-stars are in view: their accents are not evidence.
		if !opts.NoAccentStep && accent == nil && attempt == 0 {
			if accent = c.accentFor(ctx, name, prompt); accent != nil && opts.OnAccent != nil {
				opts.OnAccent(*accent)
			}
		}
		if accent != nil {
			prompt += accentSection(*accent)
		}
		if len(shown) > 0 {
			var cast strings.Builder
			cast.WriteString("\nAlready cast:\n")
			for _, cv := range shown {
				fmt.Fprintf(&cast, "- %s: %s\n", cv.Name, oneLine(cv.Instruct))
			}
			prompt += cast.String()
		}
		if wrongAccent {
			prompt += fmt.Sprintf("\nReminder: the accent is decided. \"instruct\" must name the %s accent and no other.\n", accent.Accent)
		}
		if wrongGender {
			prompt += fmt.Sprintf("\nReminder: their pronouns are %s. Open with that gender's word (man, woman, boy, girl); material describing anyone else is not this character.\n", pronouns)
		}
		temp := characterizeTemp
		if opts.Temp != nil {
			temp = *opts.Temp
		}
		// A retry at a temperature too low to sample differently would
		// only repeat the reply that failed.
		if attempt > 0 && temp < retryTempBump {
			temp = float32(attempt) * retryTempBump
		}
		return c.generate(ctx, characterizeSystemPrompt, prompt, temp, characterizeMaxTokens)
	}, func(content string) (characterization, error) {
		// The retry covers this validation too, not just JSON parsing - a
		// too-short instruct or refLine is the same kind of one-off model
		// hiccup (see parseCharacterization's own comment), and a fresh
		// generation often just doesn't hit it a second time.
		result, err := parseCharacterization(content)
		if err != nil {
			return result, err
		}
		for _, cv := range shown {
			if wordOverlap(result.Instruct, cv.Instruct) > maxCostarOverlap {
				copied[cv.Name] = true
				return characterization{}, fmt.Errorf("llm copied %s's casting: %q", cv.Name, result.Instruct)
			}
			if mentionsName(result.Instruct, cv.Name) {
				copied[cv.Name] = true
				return characterization{}, fmt.Errorf("llm named co-star %s in the casting: %q", cv.Name, result.Instruct)
			}
		}
		if !last && accent != nil && !castsAccent(result.Instruct, accent.Accent) {
			wrongAccent = true
			return characterization{}, fmt.Errorf("casting doesn't use the decided %s accent: %q", accent.Accent, result.Instruct)
		}
		if !last && genderContradicts(result.Instruct, pronouns) {
			wrongGender = true
			return characterization{}, fmt.Errorf("casting contradicts pronouns %s: %q", pronouns, result.Instruct)
		}
		return result, nil
	})
	if err != nil {
		return "", "", err
	}
	return result.Instruct, result.RefLine, nil
}

// characterizeTemp is CharacterizeVoice's sampling temperature: greedy.
// At the 0.3 it used to sample at, three castings of the same character
// from the same material disagreed on age group, accent or pitch for 10
// of 12 characters (Bert came out Southern American once, Ingrid British
// once); at 0, for 4 of 12, and the same 12 were no worse cast. Not fully
// repeatable even so - Sid still came out once with the voice she puts on.
const characterizeTemp float32 = 0

// CastingOptions are how CharacterizeVoice is run.
type CastingOptions struct {
	// Temp overrides characterizeTemp.
	Temp *float32
	// Lookup, when set, lets the model search the book before it casts
	// (lookupSection): it returns sentences of paragraphs containing
	// every one of terms.
	Lookup func(ctx context.Context, terms []string) ([]string, error)
	// OnLookup, when set, is told each lookup the model made and how many
	// sentences came back.
	OnLookup func(terms []string, found int)
	// NoAccentStep casts without deciding the accent first (decideAccent),
	// leaving it to the casting call as before - to tell what the step
	// changes.
	NoAccentStep bool
	// OnAccent, when set, is told the accent the step decided.
	OnAccent func(AccentDecision)
}

// maxLookups is how many searches the lookup plan may ask for.
const maxLookups = 6

// lookupPlanMaxTokens bounds the plan's reply: a short JSON list, plus a
// thinking model's trace.
const lookupPlanMaxTokens = 1536

// lookupPlanSystemPrompt asks only what to search for. Offered lookups
// inside the casting call itself, the model either searched categories
// ("age", "origin" - which the index answered with "page" and "original")
// or, told to search concrete words only where the material was silent,
// never searched at all and cast straight away: the casting instructions
// outweigh an optional detour. A call whose whole job is the search list
// can't skip it.
const lookupPlanSystemPrompt = `You help cast an audiobook voice. Below is the material gathered so far about one character. Before the voice is cast, the book can be searched for what the material does not state. Decide which of these the material leaves unstated: how old they are; where they are from or what accent they have; how their voice sounds; how big they are or what kind of being they are.

Reply with ONLY a JSON object, no other text:
{"lookup": [["Mara", "accent"], ["Mara", "Danish"]]}

Each lookup is the character's name (or what the text calls them) and ONE concrete word the book itself would print near it; the search returns sentences where both appear as whole words in one paragraph. Use words narration uses, never a category:
- age: "lad", "girl", "boy", "young", "old", "elderly", "grey", "wrinkled", "teenager", "years", "youth" - not "age"
- origin: "accent", a country or people you suspect from their name or lines ("Danish", "Greek", "Scottish"), "born", "homeland" - not "origin"
- voice: "voice", "deep", "rumbled", "squeaked", "lilt", "drawl", "rasp" - not "sound"
- size or kind: "tall", "huge", "tiny", "towering", or the kind you suspect ("dwarf", "fairy") - not "size" or "build"
At most 6 lookups, the most useful first. Do not look up what the material already states plainly. If it states everything, reply {"lookup": []}.`

// lookupSection lets the model search the book through opts.Lookup for
// what material (the casting turn so far) doesn't state: one call
// (lookupPlanSystemPrompt) lists what to search for, and what the
// searches found comes back as a "Searched in the book" section for the
// casting turn, "" when they found nothing.
func (c *Client) lookupSection(ctx context.Context, material string, opts CastingOptions) (string, error) {
	plan, err := c.generate(ctx, lookupPlanSystemPrompt, material, 0, lookupPlanMaxTokens)
	if err != nil {
		return "", err
	}
	var results strings.Builder
	for _, terms := range parseLookups(plan) {
		found, err := opts.Lookup(ctx, terms)
		if err != nil {
			return "", fmt.Errorf("lookup %q: %w", terms, err)
		}
		if opts.OnLookup != nil {
			opts.OnLookup(terms, len(found))
		}
		if len(found) == 0 {
			continue
		}
		fmt.Fprintf(&results, "%s:\n", strings.Join(terms, " + "))
		for _, f := range found {
			fmt.Fprintf(&results, "- %s\n", oneLine(f))
		}
	}
	if results.Len() == 0 {
		return "", nil
	}
	return "\nSearched in the book (found by their words, so some are about someone else; these add to the material above and never replace it - use one only where it plainly states something about this character):\n" + results.String(), nil
}

// parseLookups reads a {"lookup": [[...], ...]} reply, nil when content is
// anything else. At most maxLookups lookups of at most three terms each
// are kept.
func parseLookups(content string) [][]string {
	value, found := firstJSONValue(content, '{')
	if !found {
		return nil
	}
	var reply struct {
		Lookup [][]string `json:"lookup"`
	}
	if err := json.Unmarshal([]byte(value), &reply); err != nil {
		return nil
	}
	var out [][]string
	for _, terms := range reply.Lookup {
		var kept []string
		for _, t := range terms {
			if t = strings.TrimSpace(t); t != "" && len(kept) < 3 {
				kept = append(kept, t)
			}
		}
		if len(kept) > 0 && len(out) < maxLookups {
			out = append(out, kept)
		}
	}
	return out
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
// parseAttributions applies for its own array reply.
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
	value, found := firstJSONValue(content, '{')
	if !found {
		return characterization{}, fmt.Errorf("no JSON object found in response: %s", truncate(content, 200))
	}
	var result characterization
	if err := json.Unmarshal([]byte(value), &result); err != nil {
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

// maxCostarOverlap is the most of its words a characterization may share
// with a co-star's (wordOverlap) before it counts as a copy and is retried
// (without that co-star in view - see CharacterizeVoice):
// shown "Already cast" voices, the model once returned Mizhu's casting word
// for word for Sessi, a god ("a young male swampkin... prominent ears").
// Honest castings in one prompt style share less than half their words
// (at most 0.44 over the 276 pairs of 24 leads across three books).
const maxCostarOverlap = 0.6

// mentionsName reports whether text names name - in full, or by a word of
// it of 3+ letters that starts with a capital ("Steve" of "Steve Atherton")
// - as a whole word. Shown "Already cast" voices, the model took to writing
// "distinct from the British Steve and American Jonas" into the casting,
// which the voice-design model reads as part of the voice. A title word of
// the name doesn't count on its own: "Major General Franklin Hoenikker" as
// a co-star rejected every "General American accent".
func mentionsName(text, name string) bool {
	words := append([]string{name}, strings.Fields(name)...)
	for _, w := range words {
		w = strings.Trim(w, `.,;:!?"'()`)
		if len([]rune(w)) < 3 || !startsUpper(w) || IsTitleWord(w) {
			continue
		}
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(w) + `\b`).MatchString(text) {
			return true
		}
	}
	return false
}

// wordOverlap is the Jaccard similarity of a's and b's lowercase word sets.
func wordOverlap(a, b string) float64 {
	set := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, w := range strings.Fields(strings.ToLower(s)) {
			out[strings.Trim(w, ".,;:!?\"'()")] = true
		}
		return out
	}
	sa, sb := set(a), set(b)
	inter := 0
	for w := range sa {
		if sb[w] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// minCharacterizeWords/minRefLineWords - see parseCharacterization's own
// comment. Both sit well under the prompt's targets (instruct 35-60 words,
// refLine 28-40) to catch truncation, not stylistic brevity: a curt
// character's three short sentences legitimately come in under 20 words.
const minCharacterizeWords = 8
const minRefLineWords = 12

// maxRefLineWords is a Go-side backstop capping how long a characterized
// RefLine is ever actually saved at, independent of asking nicely in
// characterizeSystemPrompt's own ~28-40 word target - real, observed
// case: a model that ignored that target and produced a much longer
// refLine, which (combined with a slow/deliberate instructed pace) went on
// to render a reference clip that reproducibly OOM'd the ttsworker process
// on every one of that character's paragraphs (see
// voicerefs.maxRefClipSeconds' own doc comment, the other half of this
// defense - that one catches every cause including a slow Instruct
// inflating duration independent of word count, this one catches the
// LLM-output cause specifically, before it ever reaches a render at all).
// Set well above the prompt's own upper target (40) to only trim a
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
// backing AttributeChapter (systemPrompt), CharacterizeVoice (characterizeSystemPrompt),
// EmotionChapter (emotionDenseSystemPrompt - the one its decide call uses),
// ResolvePronunciation (pronunciationSystemPrompt), and ScoreMusic's
// describe call (musicDescribeSystemPrompt) - especially worth priming
// since ScoreMusic issues one describeMusicRegion call per region, all
// sharing that one prompt, each byte-identical
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
		characterizeSystemPrompt,
		accentSystemPrompt,
		emotionDenseSystemPrompt,
		directionSystemPrompt,
		pronunciationSystemPrompt,
		scareQuoteSystemPrompt,
		musicDescribeSystemPrompt,
		lightingSystemPrompt,
	}
}
