# backend

Go service owning the library: epub parsing, storage, background TTS
generation, TTS/LLM orchestration (voice presets, reference-clip
rendering/caching, speed adjustment, speaker-attribution prompting/
batching), and the HTTP API the frontend talks to. This module also builds
`cmd/ttsworker` - a second, separate binary that's the only thing here
linking against `libaudiocpp.so` **or** `libllama.so`.

## ttsworker / audioworker / llmworker - why model inference is a separate process

audio.cpp (the C++ engine doing TTS inference, wrapped by `../audiocpp-go`)
has a confirmed native memory leak - found via heaptrack against a real
workload, in `higgs_audio_tts`'s KV-cache/decode-graph reconstruction path
(ASan can't even get a clean trace here: it crashes inside the HIP driver's
own init on this box, and an `RTLD_DEEPBIND` workaround just traded that
crash for a different allocator-mismatch one - heaptrack, not ASan, is what
actually pinned this down). Root-caused by reading audio.cpp's own source
(`src/models/higgs_audio_tts/ar.cpp`): `HiggsGenerator`'s prefill graph is
torn down and rebuilt on *every single* `generate()` call (its shape
depends on `prompt_steps`, i.e. the input text's own length, so it's
essentially never the same shape twice - `generator.cpp` unconditionally
does `prefill_graph_.reset(); prefill_graph_ = make_unique<...>(); ...run();
prefill_graph_.reset();` every call), and its decode graph/KV cache rebuild
whenever the bucketed cache size changes or grows mid-generation on a long
paragraph. All five of the graph-owning classes' destructors in that file
(`HiggsARDecodeGraph::Impl`, `HiggsARPrefillGraph::Impl`, and the
long-reference-prefill-only `EmbeddingGraph`/`LayerGraph`/`FinalGraph`) call
`engine::core::release_backend_graph_resources(runtime->backend(), graph)` -
the **2-argument overload**, which defaults its `evict_cuda_graph_cache`
parameter to `false` (see that function's own doc comment in
`src/framework/core/backend.cpp`: deliberate, for families that rebuild
*same-shape* graphs between requests, so they can keep a warm CUDA/HIP-graph
cache). Higgs's graphs are essentially never same-shape, yet every one of
its own call sites was left on that `false` default - unlike ~56 other call
sites elsewhere in audio.cpp (including the shared `greedy_qwen_decoder.cpp`
most *other* families reuse for their own KV-cache decode loop instead of
hand-rolling one the way Higgs does), which already pass `true` for exactly
this reason. Net effect: since `QWEN_TTS_AUDIOCPP_BACKEND=hip` here and this
box's audio.cpp build has `GGML_HIP_GRAPHS=ON`, every paragraph rendered
through Higgs (this app's default clone model) leaves one more captured
HIP-graph-cache entry permanently resident, never evicted.

**Patched, but only in this box's local checkout, deliberately left
uncommitted there**: adding `, true` as a third argument to all five call
sites above, in `/home/rhino/audio.cpp` (a separate git repo from this one -
`git diff` there shows the fix; it is *not* committed or upstreamed). An A/B
test (same `ttsworker` binary, swapping `LD_LIBRARY_PATH` between a
pre-fix and post-fix build of just `libaudiocpp.so`, 50 `/generate` calls
each against `audiocpp-higgs-4b` with varying-length text) measured
steady-state RSS growth dropping from ~1.37 MB/call (unpatched) to
~0.50 MB/call (patched) - roughly a 2.7x reduction, the right direction and
magnitude for a slow trickle that eventually trips `RSSLimitBytes` on a real
book, though the residual ~0.5MB/call wasn't run long enough (only 50 calls)
to be certain it's pure allocator noise rather than a smaller remaining
leak. **Any fresh clone or reset of the `/home/rhino/audio.cpp` checkout
loses this patch** - reapply the `, true` change to all five call sites in
`src/models/higgs_audio_tts/ar.cpp` before trusting a rebuilt
`libaudiocpp.so` not to leak again.

**A second, distinct failure mode, also patched only in the same local
checkout**: Higgs occasionally fails to reach EOC and grinds on for far
longer than the text warrants - observed live (repeated `/generate` calls
against real book paragraphs, `audiocpp-higgs-4b`, this box's real GPU) as
single calls taking 58-108s against a ~1-4s norm, one of them producing
~194s of audio for a 26-word sentence, another actually exhausting
`max_tokens` (8192) outright. Each such call jumped RSS by hundreds of MB
to +3.3GB in one shot - bursty, not the steady per-call trickle described
above - and since `Worker.runMu` serializes every `Generate`/`Design`/
`Align` call in the process against this box's one GPU, a single stuck call
blocks every other request queued behind it for its entire duration, not
just its own caller's. Also measured live: unloading the stuck session
afterward (`POST /unload`, the same path `IdleUnloadAfter`/
`evictCloneModel` take) recovered only ~1% of the accumulated RSS
(32MB of 2.68GB), and reloading afterward landed *above* the pre-unload
level, not below it - only a full `ttsworker` process restart actually
returns RSS to baseline (confirmed: matched the fresh-process baseline
exactly). So periodic session-level unload/reload is not a viable
mitigation for either failure mode - the RSS-threshold watchdog restart
below is, and stays load-bearing regardless of either patch.

**The fix**: `HiggsGenerator::generate()`'s AR decode loop
(`src/models/higgs_audio_tts/generator.cpp`) now bails out early - through
the exact same `"...before EOC for this text chunk"` error message/suffix
`max_tokens` exhaustion already throws, so callers that pattern-match on it
(this app's own `Worker.Generate`/`isMaxTokensOverflow`, see
`internal/audioworker/generate.go`) need no changes - once
`result.delayed_frames` crosses a per-call budget derived from *that call's
own reference clip*: its codec frames per character of its own transcript,
projected onto the input text's length, times a multiplier. Exposed as the
`higgs_audio_tts.decode_frame_budget_multiplier` session option
(`session.h`/`session.cpp`; default 4.0, floored at 128 frames, capped at
`max_tokens`, `<= 0` disables it entirely) and threaded through
`HiggsGenerator`'s constructor (`generator.h`). Deliberately reference-pace-
derived rather than a flat wall-clock or token-count number: a voice cloned
from a slow, deliberate reference delivery legitimately needs more frames
per word than a brisk one does (see `higgsMaxTokens`' own comment below for
the confirmed-live case that still overflowed even 8192), so a single flat
bound either has to be loose enough for the app's slowest voice - too loose
to catch a short paragraph's runaway quickly for every other voice - or
risks false-failing that voice's own legitimately long output. This app's
own Go side no longer threads a multiplier through for this - dropped
after the real, observed OOM failures (see `voicerefs.maxRefClipSeconds`/
`speakerattr.maxRefLineWords`) turned out to be an oversized reference
clip inflating the *prefill* graph, not a runaway *decode* loop this
option guards against; audio.cpp's own compiled-in default (`4.0`) still
applies unconfigured. **Also only in the local checkout, uncommitted** -
touches `include/engine/models/
higgs_audio_tts/generator.h`, `src/models/higgs_audio_tts/generator.cpp`,
`include/engine/models/higgs_audio_tts/session.h`, and `src/models/
higgs_audio_tts/session.cpp`; a fresh clone/reset of `/home/rhino/audio.cpp`
loses this too, same as the `evict_cuda_graph_cache` patch above - both
need reapplying together before trusting a rebuilt `libaudiocpp.so`.

Rather than let that leak eventually OOM-kill the whole backend, TTS
generation is isolated into its own disposable process (kept in place as
cheap insurance regardless of the patch above - it's unverified over a long
soak, and this audit didn't rule out leaks in other model families) - and
the speaker-attribution LLM (llama.cpp, wrapped by `../llamacpp-go`) lives in
that same process too, even though llama.cpp carries none of audio.cpp's
leak: the two native model families share one process's VRAM budget there
and can be idle-unloaded independently of each other
(`internal/audioworker.Config.IdleUnloadAfter`/`internal/llmworker.
Config.IdleUnloadAfter`), which only works if they're actually in the same
process. An earlier version kept the LLM directly in `cmd/server` before
this moved it here specifically for the VRAM-sharing/idle-unload reason:

- **`cmd/server`** (the main binary) must **never** import `audiocpp-go`
  or `llamacpp-go`, even transitively - the hard invariant that keeps it
  immune to the audio.cpp leak (and free of every other native dependency
  besides). Enforce/verify with:
  ```
  go list -deps ./cmd/server | grep audiocpp-go   # must print nothing
  go list -deps ./cmd/server | grep llamacpp-go   # must print nothing
  ```
- **`internal/audioworker`** is the *only* package importing
  `audiocpp-go`. It loads/evicts clone-model instances (an LRU over
  `container/list`) and implements `/generate` (clone a preset's voice
  given a caller-supplied reference clip), `/design` (render a fresh
  reference clip via VoiceDesign), `/align` (forced word-level alignment -
  see `alignerSampleRate`'s comment for a real, easy-to-get-wrong gotcha
  about the sample rate audio.cpp reports timestamps in), and `/health`.
  Holds **no per-preset state** - the caller (backend) resends a preset's
  reference audio bytes on every call; audio.cpp's own session-level cache
  (`cloneFamily.sessionOptions`, 8 slots) transparently dedupes repeated
  (reference audio, reference text) pairs. `Config.IdleUnloadAfter`
  (wired from `MODEL_IDLE_UNLOAD_AFTER`, shared with `internal/llmworker`)
  frees *every* loaded clone model, including the otherwise-never-evicted
  default one, once none of `Generate`/`Design`/`Align` has been called
  for that long - timeout-based deliberately, not triggered eagerly on
  every switch away from TTS work, for the same reason `internal/jobs`'
  `poolLLM`/`poolGeneration` mutual-exclusion (below) avoids acting on
  every switch: a real, observed thrashing incident from an earlier,
  eager version of an analogous mechanism.
- **`internal/llmworker`** is the *only* package importing `llamacpp-go`.
  It loads the speaker-attribution GGUF model lazily (on first
  `LLMGenerate` call, or the first call after an idle unload) and serves
  up to `Config.MaxConcurrent` (default 2, `SPEAKER_LLM_MAX_CONCURRENT` -
  lowered from an original 4 after a real, observed "failed to allocate
  buffer for kv cache" VRAM exhaustion: the shared KV pool has to reserve
  room for every one of `MaxConcurrent` slots to independently reach
  `speakerattr.MaxOutputTokens()` at once, so raising this back up needs
  checking that math first - see `llmworker.Config.MaxConcurrent`'s own
  doc comment)
  concurrent generations against it via multi-sequence batching
  (`llamacpp.Scheduler` - see `../llamacpp-go/CLAUDE.md`) rather than
  serializing every call behind a mutex the way an earlier,
  single-sequence version did. `Config.SystemPrompts` (see
  `speakerattr.SystemPrompts`) is decoded once each into its own
  permanently-resident sequence at load time, so later calls sharing one
  of those exact strings skip redecoding it - `llamacpp.Context.CopySeq`
  copies that shared prefix into whichever generation slot picks up the
  call.
- **`cmd/ttsworker/main.go`** is a thin `main()` wiring `audioworker.
  Worker` and `llmworker.Worker` up to one loopback HTTP server (default
  `:8091`), including `/llm/generate` alongside the TTS routes.
- **`internal/ttsworker`** is the *client/supervisor* package `cmd/server`
  actually imports - zero dependency on `audiocpp-go`/`llamacpp-go`. Its
  `Manager` spawns the `ttsworker` binary, health-polls it, and runs a
  watchdog goroutine that kills and respawns it if its RSS crosses a
  threshold (default 11GiB, `RSSLimitBytes`) or if it dies on its own.
  Every proxied call (`Generate`/`Design`/`Align`/`LLMGenerate`) takes a
  `sync.RWMutex` `RLock` for its full round trip; a restart takes the
  exclusive `Lock`, which naturally blocks until every in-flight call has
  returned and blocks new calls until the restart finishes - no separate
  queue-depth logic needed. `LLMGenerate`'s RLock never limits how many
  calls run concurrently server-side beyond that - `llmworker.Config.
  MaxConcurrent` (worker-side batching) is the real concurrency bound.
- **`internal/ttsproto`** holds the plain (no-cgo) request/response
  structs both sides of the process boundary share, so they can't drift.

Build/run both binaries from `backend/` (needs a local audio.cpp checkout
with the C API built, and a local llama.cpp checkout - both linked into
`ttsworker` now, see `../audiocpp-go/README.md`/`../llamacpp-go/README.md`):

```
export CGO_LDFLAGS="-L/path/to/audio.cpp/build/bin -L/path/to/llama.cpp/build/bin"
export LD_LIBRARY_PATH="/path/to/audio.cpp/build/bin:/path/to/llama.cpp/build/bin:$LD_LIBRARY_PATH"
go build -o ttsworker ./cmd/ttsworker
go build -o server ./cmd/server
./server   # spawns ./ttsworker itself
```

`cmd/server`'s own build needs **neither** native library's
`CGO_LDFLAGS`/`LD_LIBRARY_PATH` set (per the hard invariant above) - only
building/running `ttsworker` does, since both now link into that binary.

## Layout

- `cmd/server/main.go` — wiring/startup; also owns spawning/watchdogging
  the ttsworker process via `internal/ttsworker.Manager`.
- `cmd/ttsworker/main.go` — the disposable worker binary (see above).
- `internal/audioworker/` — the worker's TTS model-loading/generation
  logic (imports `audiocpp-go`).
- `internal/llmworker/` — the worker's LLM model-loading/generation logic
  (imports `llamacpp-go`).
- `internal/ttsworker/` — the supervisor/client `cmd/server` uses to talk
  to the worker (no native deps - see above).
- `internal/ttsproto/` — shared wire-format structs for the two above.
- `internal/voices/` — the curated built-in narrator presets (a compiled-in
  Go slice) - never stored in the DB, just proxied.
- `internal/voicerefs/` — makes sure a voice preset's reference clip exists
  on disk, rendering it via the worker's `/design` + `internal/wsola`'s
  speed adjustment on first need, and caching design renders by their
  exact (instruct, seed, text, language) recipe so a "test this design"
  preview followed by "save as preset" doesn't pay for a second render.
  `EnsureFile`'s check-then-render is serialized per `presetID`
  (`presetLocks`, a `sync.Map` of `*sync.Mutex` - the same keyed-mutex
  shape `httpapi.Server.provisionLocks` uses for character-voice
  provisioning) with a second `os.Stat` once the lock is held: without it,
  several paragraphs of a chapter generating concurrently against a
  brand-new preset could each find no cached clip and race to render/write
  the same file. `Regenerate` (the explicit "always re-render" path for a
  preset edit) stays unlocked - a deliberate one-off action never called
  concurrently with itself.
- `internal/wsola/` — a from-scratch Go port of Chromium's WSOLA
  (Waveform Similarity Overlap-Add) time-domain pitch-preserving
  time-stretcher. Verified against a numpy reference port across the full
  supported speed range; differences are 16-bit PCM quantization noise
  only (~6e-5). Pure standard-library Go, no `audiocpp-go` dependency -
  unrelated to the leak/worker split above.
- `internal/epub/` — zip + XHTML parsing → `Book{Chapters[]Paragraph}`. No
  external epub library; container.xml/OPF are parsed by hand with
  `encoding/xml` token-walking (matches on `Name.Local` only, ignoring
  namespaces, since real-world epubs are inconsistent about `dc:`
  prefixes). Paragraph splitting walks the XHTML DOM
  (`golang.org/x/net/html`) looking for the innermost block-level element
  at each point; each block's text is then run through
  `splitQuoteSegments`, splitting on double-quote boundaries (straight `"`
  and curly `“ ”`, not single quotes - those are almost always an
  apostrophe) into alternating narration/dialogue segments, e.g. `"The
  bridge is out," Sam said.` → two segments. Every segment after the first
  in a source paragraph is flagged `Block.Inline` - a display hint meaning
  "part of the same visual paragraph as the previous one", not a real
  break - while still being its own row/idx/audio/speaker. This is what
  lets one mixed narration+dialogue sentence resolve to two different
  narration voices while reading on screen as one paragraph.
- `internal/store/` — DuckDB persistence (`books` → `chapters` →
  `paragraphs`, plus `characters`/`character_voices`, book-scoped). One
  `*sql.DB` connection (`SetMaxOpenConns(1)`) since DuckDB is single-writer
  and this app has no need for write concurrency. **Because of that single
  connection, one slow query stalls every other concurrent request
  app-wide** - a real, previously-hit bug: an earlier
  `ParagraphAudioStatuses` call built a SQL `IN (...)` with one placeholder
  per paragraph id, slow for the Narrator's entire multi-thousand-paragraph
  history on a long book, and every other request (Jobs page poll, chapter
  loads) would appear to hang behind it. Fixed by `CountReadyAudioForSpeaker`,
  which JOINs on `(book, speaker, voice)` instead of materializing an id
  list - general lesson: prefer a JOIN scoped by book/chapter over an
  `IN (...)` sized by paragraph count. **No FK `ON DELETE CASCADE`** —
  DuckDB's support for that isn't something to lean on; `DeleteBook`
  deletes paragraphs → chapters → book explicitly in a transaction. Column
  is `content` not `text` (avoids ambiguity with the SQL `TEXT` type
  name). **No migrations**: this app is pre-release with no deployed data
  to preserve, so schema changes just edit the `CREATE TABLE IF NOT
  EXISTS` statements in `Open` directly (this DuckDB version also rejects
  `ALTER TABLE ... ADD COLUMN` with a constraint outright, `IF NOT EXISTS`
  or not) — wipe the local `DATA_DIR` (`library.duckdb`) and start fresh
  whenever the schema changes.
- `internal/narration/` — resolves a paragraph's *effective* narration
  voice: `Resolver.BookVoice` (a book's own single voice) as the fallback,
  overridden per paragraph by `Resolver.CharacterVoice` when (a)
  `book.MultiVoice` is on, (b) the paragraph is attributed to a named
  character (not `""`/`"Narrator"`), and (c) that character has a voice
  assigned - see `ForParagraph`. Centralizes what used to be duplicated
  `resolveVoiceSeed`/`resolveVoiceContext` logic across `httpapi` and
  `jobs`. A book with `MultiVoice` off, or with no characters assigned
  voices, resolves every paragraph to the book's own voice. Character
  lookup is keyed by `store.SeriesScope(book)`, not `book.ID` - see
  "Speaker attribution" below.
- `internal/speakerattr/` — an LLM-driven pipeline, both stages prompting
  a general-purpose GGUF model. `Client` (this package) is a thin
  caller-side wrapper - batching, prompt-building, JSON-parsing,
  malformed-response retry - talking to the model over `llmBackend`
  (`LLMGenerate(ctx, systemPrompt, userPrompt, temp, maxTokens)`),
  satisfied by `*internal/ttsworker.Manager` in `cmd/server` (proxying to
  `internal/llmworker`, which actually runs the model) and directly by
  `*internal/llmworker.Worker` in `cmd/benchattr` (a standalone
  benchmarking tool with no ttsworker process of its own). Env vars
  `SPEAKER_LLM_MODEL_PATH`/`SPEAKER_LLM_GPU_LAYERS`/`SPEAKER_LLM_CTX`/
  `SPEAKER_LLM_MAX_CONCURRENT` are read by `cmd/ttsworker/main.go` (wired
  into `llmworker.Config`, since that's the process that loads the model)
  - `cmd/server` still reads `SPEAKER_LLM_MODEL_PATH` too, but only to
  decide whether the feature is enabled at all (`NewClientFromEnv`).
  `SPEAKER_LLM_MODEL_PATH` (a `.gguf` file - required for the feature at
  all), `SPEAKER_LLM_GPU_LAYERS` (default `-1`: offload everything a GPU
  backend can take), `SPEAKER_LLM_CTX` (default `speakerattr.DefaultNCtx`,
  `24576` - *not* `0`/"the model's own trained context size",
  deliberately: modern GGUF models increasingly train at a very large
  native context (Qwen3 trains at 262144), and a KV cache sized for that
  can fail to allocate alongside another already-VRAM-resident model, for
  a batch-of-40-paragraphs task that never needed more than a few thousand
  tokens; set `SPEAKER_LLM_CTX=0` explicitly if a given model genuinely
  needs more), `SPEAKER_LLM_MAX_CONCURRENT` (default `2`, see
  `llmworker.Config.MaxConcurrent`'s own doc comment for why), `SPEAKER_LLM_NO_THINK` (bool, default
  `false`) - appends Qwen3's `/no_think` marker to every attribution
  batch's user turn.
  **Recommended model: `Qwen3-4B-Instruct-2507`** (a genuinely
  non-thinking-only checkpoint, not a hybrid-thinking model with
  `/no_think` bolted on) - benchmarked on real book content against
  hybrid-thinking Qwen3-1.7B/0.6B (with and without
  `SPEAKER_LLM_NO_THINK`): a hybrid-thinking model's own `<think>...</think>`
  trace routinely burns through `attributeMaxTokens` before ever producing
  the JSON answer, outright losing entire batches on real chapter-length
  content (1.7B-thinking lost most of two of three test chapters,
  0.6B-thinking lost two of three entirely), and even completed batches
  drastically under-attribute real dialogue (defaulting almost everything
  to `"Narrator"`/`"Unknown"`). `SPEAKER_LLM_NO_THINK=true` on the same
  hybrid models fixes both (0 batch failures, far higher recall) but is
  still a workaround - Qwen3-4B-Instruct-2507 had zero batch failures, the
  best recall of everything tested, and correctly resolved a real
  "character's name mentioned inside someone else's quote" case that
  tripped up 1.7B+`/no_think`, for ~1.5x its wall time, an easy trade
  given attribution is an async, one-time preprocessing job. Deliberately
  *not* an encoder-based NLP pipeline (BookNLP/ModernBookNLP): every
  actively-maintained open-source audiobook generator surveyed (Alexandria,
  AudioBard, audiobook-creator, storycast, Castwright) converged on
  LLM-prompted attribution, and recent literary-NLP research agrees
  (prompted Llama-3 beats BookNLP's own quote-attribution baseline - NAACL
  2025).
    1. **Identify** (`Client.AttributeChapter`) - labels each paragraph
       `"Narrator"`, `"Unknown"`, or a character name, given the chapter
       text and names already known for that book's whole series (below)
       so a recurring character gets one consistent name. `"Narrator"` is
       reserved exclusively for actual narration/description - never
       spoken dialogue, including a first-person narrator's own line about
       themselves (an earlier version special-cased that as `"Narrator"`;
       dropped in favor of treating every dialogue line the same: a real
       name, or `"Unknown"`). The system prompt also asks the model to
       reuse one exact name for a recurring character even *within* its
       own reply; belt-and-suspenders on that, `canonicalizeSpeakerNames`
       runs afterward, folding any name that's a whole-word run inside a
       longer name already seen into that longer name (e.g. "Vesna" +
       "Captain Vesna Thorne" collapse to the latter) - heuristic, not
       exact (two characters sharing a surname would also collapse), but
       reader-correctable. Two more checks run over each batch's raw
       result: `normalizeBarePronoun` maps a raw speaker value that's just
       a bare pronoun ("I"/"he"/"they"/etc.) to `"Unknown"` (a real,
       observed model failure mode); `disallowNarratorForDialogue` maps
       `"Narrator"` to `"Unknown"` for any paragraph that's actual quoted
       dialogue (`ParagraphInput.IsQuote`), enforcing the "never Narrator
       for dialogue" rule in Go rather than trusting the prompt alone.
    2. **Describe** (`Client.DescribeChapter`) - a separate, single-purpose
       call (own system prompt, own smaller batch size - `5` vs.
       attribution's `40`) tagging which *narration* paragraphs describe a
       character's appearance/personality
       (`Store.SetParagraphDescriptions`), gathered later for
       characterization alongside a character's own dialogue. Deliberately
       its own call rather than a `"describes"` field folded into
       `AttributeChapter`: a folded version measurably hurt precision -
       judging two things per line across a 40-paragraph batch made the
       model lapse into blanket-tagging nearly every line touching a
       salient character (Qwen3-4B-Instruct-2507 tagged 30-52% of
       paragraphs, overwhelmingly mere mentions, not descriptions), even
       after tightening the prompt with explicit negative examples.
       Isolating the judgment and shrinking the batch fixed it - precision
       rose from ~6% (batch 40) to ~25-37% (batch 10) to ~80% (batch 5),
       since a smaller batch gives "streak" bias far less room before a
       fresh batch boundary forces independent judgment again. Runs only
       once attribution finishes a chapter in one uninterrupted pass,
       best-effort and decoupled from attribution's own success/failure.
       Can discover a brand-new character (one described before they ever
       speak).
    3. **Characterize** (`Client.CharacterizeVoice`) - given a sample of
       *only* a character's own attributed dialogue (not narration about
       them, except what Describe tagged) plus their description
       paragraphs, asks the LLM for a short "Speak as a..." voice-casting
       instruction (gender, age, tone, speaking style) - saved as
       `store.Character.Summary`, shared across every clone model's own
       voice preset for this character (see "Per-clone-model voice
       assignment" below). Can characterize from descriptions alone, with
       no dialogue yet.

  **Characterization and voice provisioning happen lazily, not during
  attribution.** An earlier version ran both eagerly, inline, as part of
  `attributeChapter` itself - this had a real, since-fixed deadlock (a
  `KindSpeakerCharacterization` task nested inside an already-running
  `KindSpeakerAttribution` task, both drawing from `internal/jobs`' single
  shared LLM slot, so the nested one could never dispatch) and made a
  chapter with many brand-new characters attribute much slower.
  `attributeChapter` now only runs LLM attribution and
  `Store.UpsertCharacter`s each new name, with no voice-related side
  effects; characterizing a character and auto-creating + assigning their
  voice preset (`Server.provisionCharacterVoice`, using `Instruct`
  verbatim on an auto-created custom preset) happens the first time that
  character's voice is actually *needed for generation* -
  `jobs.Manager.provisionMissingCharacterVoices`, called from
  `paragraphsNeedingGeneration`/`enqueueParagraphRegenerate` before
  resolving each paragraph's voice, via a `jobs.CharacterVoiceProvisioner`
  callback (`internal/jobs` has no `Store`/`Speaker` access of its own). A
  reader can still review/reassign a character's voice afterward through
  the ordinary custom voice preset endpoints - this just seeds a
  reasonable starting voice instead of leaving every character on a blind
  manual pick. `provisionCharacterVoice` also serializes itself per
  characterID (`Server.lockCharacterProvision`, a keyed mutex) across
  every caller and clone model - `RunCharacterization`'s own per-characterID
  dedup only covers the LLM call, not the preset-creation step after it,
  so without this lock two concurrent provision attempts for the same
  character could each create a duplicate preset.

  **Per-clone-model voice assignment**: a character's *identity* (name,
  `Summary`) is one row, shared series-wide (below) - but their *assigned
  voice* is tracked separately, one `voice_preset_id` per `(character,
  clone_model)` pair (`Store.CharacterVoiceForModel`/`CharacterVoicesForModel`/
  `SetCharacterVoice`, backed by `character_voices`), because a voice
  preset clones through one specific `clone_model` and isn't portable to
  another - a character can end up with a different auto-assigned preset
  under `audiocpp-qwen3-0.6b` than under `audiocpp-higgs-4b`. Every
  lookup/provisioning call is keyed by the book's own currently-resolved
  clone model (`narration.EffectiveCloneModel(bookVoice)`, defaulting to
  `voices.DefaultCloneModel` for a fully custom instruct with no preset
  backing it); `provisionMissingCharacterVoices` checks any character it
  touches - new or recurring - who doesn't yet have a voice for that
  model, so switching a book's (or series') narrator model doesn't
  silently leave recurring characters back on the book's own voice.
  `handleSetCharacterVoice` resolves the same book's clone model
  server-side. `httpapi.Server.Speaker` is `nil` (attribution endpoint
  reports 503, lazy characterization stays a no-op) if
  `SPEAKER_LLM_MODEL_PATH` is unconfigured - everything else works
  regardless. Attribution is explicitly triggered per chapter (`POST
  .../attribute-speakers`); characterization/voice-provisioning happens
  the first time it's needed, or explicitly via the Speakers page's
  "Regenerate" button. Every distinct system prompt this package generates
  against (`speakerattr.SystemPrompts` - attribution's, characterization's,
  and the direction/sfx/pronunciation/describe ones below, all fixed
  constants, byte-identical across every call) is decoded once, at
  model-load time, into its own permanently-resident sequence in
  `llmworker.Worker`'s shared `Context` (see above) - priming every known
  prompt up front is what makes it safe for several concurrent
  `LLMGenerate` calls to each copy from the same already-decoded prefix
  into their own slot without racing to prime it first.

  **Malformed-response retry**: every batch call in this package
  (`attributeBatch`, `describeBatch`, `directionBatch`, `sfxBatch`,
  `CharacterizeVoice`) goes through `generateAndParse`/`retryParse`
  instead of calling `Client.generate` directly - a malformed response (an
  unparseable JSON array, or `CharacterizeVoice`'s own
  too-short-looks-truncated check) retries the *whole* round trip, a
  fresh generation each time, up to `maxGenerateRetries` (2, so 3 attempts
  total). Added after a real production incident: a single malformed JSON
  response used to fail the *entire* remaining chapter outright, silently
  abandoning dozens of not-yet-processed paragraphs with no automatic
  retry, requiring a reader to manually re-trigger the whole action
  (redoing already-succeeded batches too, since there's no partial-progress
  tracking below the chapter level). A malformed response is usually a
  one-off sampling hiccup, not a systemic failure. `retryParse` never
  retries a `generate` error itself (context cancellation, a genuine
  inference failure) - only a parse/validate failure on an otherwise-
  successful generation. `pronunciationBatch` is the one exception -
  `parsePronunciationChoices` never itself returns an error (a bad
  response there just defaults to each term's more common reading) - so
  there's nothing for a retry to fix.

  **Series-wide character scope**: characters (identity, `Summary`, voice
  assignment) are keyed by `store.SeriesScope(book)`, not by book - books
  sharing a non-empty `SeriesName` share one character roster across the
  series, so a recurring character keeps the same name and voice in book
  2 that they got in book 1; a book with no series keeps its own private
  roster. Both stages draw on the whole series: `attributeChapter`'s
  known-character list and `characterizeVoice`'s quote sample are gathered
  via `httpapi.booksInScope`/`Store.ListSeriesBooks` (series order) +
  `Store.QuotesForBooks`, across every book in the series attributed so
  far. Lazy characterization still only ever runs once per character ever
  (gated on `Character.Summary` being empty) - no automatic
  re-characterization later as more dialogue accumulates. A reader who
  wants a fuller-sample redo can explicitly force one via the Speakers
  page's "Regenerate" button (`POST .../characterize`), which also
  invalidates *every* clone model's currently-assigned voice preset's
  cached reference clip for that character so each picks up the new
  characterization on next use.

  **Speech-direction tagging**: two independent, sibling LLM passes
  (`Client.DirectChapter`/`direction.go` and `Client.TagSfx`/`sfx.go`)
  annotate a chapter's paragraphs with Higgs Audio v3 TTS's own native
  inline control-token vocabulary (`<|emotion:anger|>`, `<|sfx:laughter|>`,
  etc.), confirmed directly against `Higgs-Audio-v3-TTS-4B-GGUF`'s own
  embedded `tokenizer.json`: each is a dedicated `"special": true`
  `added_tokens` entry, so audio.cpp's plain tokenizer encodes it as one
  atomic conditioning token - no audio.cpp/audiocpp-go changes needed,
  only a Go-side prompt/storage/wiring addition. Split along the same
  "sentence-level" vs "inline" line the model's own embedded
  `PROMPTING.md` draws:
    - `Client.DirectChapter`/`validSentenceTags` (24: 14 `emotion` (of the
      model's own 21 - `<|emotion:longing|>`, `<|emotion:arousal|>`,
      `<|emotion:affection|>`, `<|emotion:fear|>`, `<|emotion:contentment|>`,
      `<|emotion:confusion|>`, and `<|emotion:sadness|>` are deliberately
      excluded from this app's own valid set, found unreliable enough in
      practice to do more harm than good) + 2 `style` (of the model's own 3
      - `<|style:whispering|>` is deliberately excluded too, same
      reasoning) + 8 `prosody` speed/pitch/expressive) - each colors a whole
      sentence/clause from wherever it's inserted; more than one can land
      in a single paragraph if the tone genuinely shifts partway through.
      `<|prosody:expressive_low|>` also has a second, non-emotional
      trigger: non-narrative front/back-matter a chapter can contain (a
      copyright notice, dedication, TOC/index entry, epigraph attribution)
      gets it too, for a flatter delivery - folded into this same pass
      rather than a separate classify-and-skip feature.

      A narration (non-quote) paragraph is far more tag-restricted than a
      quoted one - only `<|sfx:humming/sigh/laughter/cough/sniff|>`,
      `<|prosody:pause/long_pause|>`, or
      `<|prosody:expressive_low/high|>` - never `<|emotion:*|>`,
      `<|style:*|>`, `<|prosody:speed_*/pitch_*|>`, or
      `<|sfx:crying/screaming/burping/sneeze|>`, since those alter the
      narrator's own consistent voice too much. `store.Paragraph.IsQuote`
      is what distinguishes the two, enforced in Go, not just asked for in
      either prompt: `restrictNonQuoteTags` (shared by both passes) strips
      any disallowed insertion from a non-quote paragraph after each
      pass's own heuristic backstop has already run. A paragraph left with
      no insertions after stripping is dropped from the batch result
      entirely.
    - `Client.TagSfx`/`validInlineTags` (11: 9 `sfx` + `prosody`
      `pause`/`long_pause`) - each marks an exact point within a line: an
      `sfx` tag immediately before an onomatopoeia word the line already
      spells out (no space - `<|sfx:laughter|>Haha`, per `PROMPTING.md`),
      never an invented word; a pause between existing words/phrases,
      triggered either by content (the narration describes a real beat of
      silence) or by a sentence's own length/structure (a "breath pause" -
      one `<|prosody:pause|>` at the clearest clause boundary in a
      genuinely long, unbroken sentence with no comma/semicolon/dash
      already giving a natural break). Both triggers stay one call/tag
      family - the same judgment ("does a narrator's voice need a beat of
      silence here"), just two different reasons to reach for it.
  Kept as two separate LLM calls rather than one combined judgment, the
  same lesson `DescribeChapter` was split from `AttributeChapter` over - a
  sentence-level judgment and a positional one are different enough in
  kind that one call doing both risks the same precision loss.

  **Regex heuristics layered on top of both passes** (`sfxheuristics.go`/
  `directionheuristics.go`): a handful of tags have such a small, closed
  set of real-world spellings (or an unambiguous typographic trigger) that
  a plain regex recognizes them deterministically and for free - a
  prompt-thinning move, not just a backstop. `addHeuristicSfxTags`/
  `addHeuristicSentenceTags` run after each LLM call returns for a batch,
  over every paragraph regardless of whether the LLM tagged it, merging in
  any new match by offset (skipping one that would collide with an
  LLM-placed tag at the same point):
    - `sfxheuristics.go`: `<|sfx:laughter|>` (ha/he/ho syllables, careful
      to exclude bare "he"/"ho"), `<|sfx:humming|>` (h's + optional r +
      m's), `<|sfx:sneeze|>` ("Achoo"/"Atchoo"), `<|sfx:cough|>` ("Ahem"),
      `<|sfx:sigh|>` ("Sigh", excluding "sight"/"sighed"/"sighing"), and a
      trailing-off `<|prosody:pause|>` before any run of 3+ periods or a
      real `…` glyph - `sfxSystemPrompt` correspondingly only lists
      `<|sfx:crying/screaming/burping|>` as valid (no fixed spelling to
      regex-match) and drops "an ellipsis" from its content-pause examples.
    - `directionheuristics.go`: `<|style:shouting|>` for doubled-or-more
      `!!`, `<|emotion:surprise|>` for a `?!`/`!?` combo - both inserted at
      the line's own start, stacking correctly with anything the LLM
      already placed there.

  A tag inserted at `len(orig)` - the very end of a line, with nothing
  left to color - is dropped rather than kept, for both passes regardless
  of source: a trailing delivery tag renders as an audible stray artifact
  since delivery tags condition the speech that *follows* them. Enforced
  once, centrally, in `parseTaggedText` (shared by both LLM passes).

  `directionSystemPrompt` documents that a single position may carry more
  than one tag of different kinds at once (e.g.
  `<|emotion:awe|><|style:singing|>`) - never two from the same
  category. `deliverytags.Merge`'s stable sort keeps insertions at the
  same offset in whatever order they were appended, so callers append in
  Emotion-then-Style-then-Pacing order to get that convention right;
  `addHeuristicSentenceTags` is a known, low-stakes exception to that
  ordering guarantee.

  Both passes share one hard safety invariant, enforced by
  `internal/deliverytags` (a small, dependency-free leaf package used by
  both `speakerattr`, to validate an LLM's own output, and `store`, to
  resolve a paragraph's final generation text): whatever text a pass
  returns must be the paragraph's real text with **only** tag markup
  inserted - `deliverytags.ExtractInsertions` diffs the returned text
  against the original and rejects (silently drops) anything that adds,
  removes, reorders, or rewords even one real word. This is what makes
  multi-position insertion safe to trust from an LLM at all: forced
  alignment and the reader's own on-screen text both stay keyed to the
  paragraph's real, untouched content regardless of what either pass
  returns.

  Deliberately its own separate, explicitly-triggered action (`POST
  .../tag-directions`, running *both* passes) rather than chained after
  attribution the way Describe is: it's optional/stylistic, nothing else
  depends on its output, and neither prompt has been benchmarked against
  real chapters yet the way attribution/description were. Only runs for
  Higgs's own clone model (`voices.DefaultCloneModel`, `"audiocpp-higgs-4b"`
  today) - checked synchronously (400 for any other clone model) since the
  tag vocabulary means nothing to another family's tokenizer.

  `directChapter` threads `jobs.Manager.HasHigherPriorityLLMWork` through
  as both passes' own `shouldPause`, the same pause/resume shape
  `attributeChapter` uses - SpeakersPage's own bulk "Tag all"/"Tag
  undirected" buttons can queue a whole book's chapters at
  `TierBackground` at once, and without pausing that could make a
  just-arrived, more urgent `poolLLM` task wait out however many
  chapters/batches are left. `directChapter` takes an `onlyIdx map[int]bool`
  restricting which paragraphs it sends to the model; when either pass
  pauses, it requeues a follow-up `KindSpeechDirection` task scoped to the
  union of both passes' own remaining paragraphs - unlike
  `attributeChapter`'s `onlyUnattributed`, this can't be re-derived from
  stored state later (a paragraph getting no tag is indistinguishable from
  one never checked), so the continuation carries the index set forward
  explicitly. If only one pass paused, the other still re-runs on the
  continuation over the narrower `onlyIdx` set - a few wasted-but-harmless
  calls, traded for not tracking each pass's own completion separately
  across a chain of continuations. Only marks `Chapter.Directed` once
  neither pass has anything left, possibly after several continuations.

  Each pass's output is stored independently per paragraph, per clone
  model - `paragraphs.tts_tags`, a JSON object keyed by `clone_model` id
  (the same per-`(x, clone_model)` shape `character_voices` uses, since a
  paragraph's resolved clone model can differ across books/characters and
  a tag vocabulary for one model means nothing to another's), each value a
  `store.ParagraphDirection{SentenceText, InlineText}` - two
  independently-annotated full copies of the paragraph's text, one per
  pass (`Store.SetParagraphSentenceTags`/`SetParagraphInlineTags`, a
  shared read-modify-write helper so setting one field never clobbers the
  other's value). Composed back into one final generation string only at
  generation time - `store.Paragraph.ResolveGenerationText` re-extracts
  each field's own insertions against the paragraph's real text and
  `deliverytags.Merge`s them (sentence-level insertions ordered first at a
  shared offset, matching `PROMPTING.md`'s own tag-stacking example) -
  `jobs.Manager.generate` calls this for the text sent to `generateClone`,
  **never** for forced alignment/word-timing, which always uses
  `paragraph.Text` untouched: a tag token has no corresponding audio, so
  including it there would surface as a spurious "word" with no real
  timing. Most paragraphs get no annotation at all by design (both system
  prompts ask for a short, sparse list), and a missing/empty field in
  `Tags` is a no-op at generation time, not an error.

  Whenever a run tags at least one paragraph (from either pass),
  `directChapter` invalidates that whole chapter's already-generated audio
  (`Store.DeleteChapterAudio` + removing its on-disk directory) so every
  paragraph regenerates and picks up its annotations - blunt (the whole
  chapter, not just the paragraphs actually tagged this run) rather than
  tracking which paragraphs' annotations actually changed. Skipped when a
  run tags nothing.

  **Pronunciation resolution** (`Client.ResolvePronunciation`/
  `pronunciation.go`) is a third sibling pass `directChapter` runs
  alongside DirectChapter/TagSfx, sharing the same per-chapter
  call/requeue-on-pause plumbing but solving a genuinely different
  problem: fixing a mispronounced abbreviation ("St." read as neither
  "Street" nor "Saint" correctly) means actually changing what word gets
  spoken, not inserting a control token - a narrow, deliberate exception
  to `deliverytags`' own "never reword real content" invariant. Rather
  than weaken that invariant (or trust an LLM to freely respell text - the
  "full free-form respelling" alternative considered and rejected), the
  LLM here is never shown a vocabulary and never returns any text at all:
  `pronunciationCandidates` (a small, hand-authored regex table) finds
  every pronunciation-worthy span and its own closed set of possible
  readings *before* the model is involved (`findPronunciationTerms`, pure
  regex, no LLM call) - today:
    - `Dr.` → Doctor/Drive, `St.` → Street/Saint, `Ft.` → Fort/Feet, `Mt.`
      → Mount/Mountain - each exact-case (never `(?i)`) to avoid colliding
      with ordinary lowercase words.
    - `No.` → Number/No, but only when immediately followed by a number
      ("No. 5") - RE2 has no lookahead, so the digit(s) have to be part of
      the regex match itself; requiring a following number is what keeps
      this from matching an ordinary sentence-ending "No."
    - `C'mon`/`c'mon` (straight or curly apostrophe, case-insensitive) -
      always returns exactly one reading ("Come on"/"come on"), so it
      never actually reaches the LLM at all.
    - A bare `M/D` number pair ("3/4") - parses the month/day digits and
      discards the match entirely for anything that isn't a plausible
      calendar date (month outside 1-12, day outside 1-31); otherwise a
      fully spelled-out date reading ("March the fourth") as the default,
      original text unchanged as the other option - a deliberately cheap
      fallback sidestepping any fraction-reading logic for the
      "actually a fraction, not a date" case. A full `M/D/YYYY` is left
      alone entirely - unambiguously a date already.

  Within a batch, a term with only one candidate is resolved immediately -
  no LLM call spent confirming a foregone conclusion; only a term with 2+
  candidates is actually sent to the model. For those, the model's only
  job is picking an index into that pre-computed list per span (reply
  shape `[{"id", "choice"}, ...]`), so it can never invent replacement
  text - the safety property that makes this safe to change what's spoken
  at all. A response entry that's missing, malformed, or out-of-range
  defaults silently to index 0 (always the more common reading) rather
  than erroring, since a term was already positively identified as needing
  *some* resolution before reaching the model. A batch with no
  multi-candidate terms (most paragraphs) skips the LLM call entirely.

  The result is stored as `internal/pronounce.Substitution` (`{Offset,
  Length, Replacement}`, byte-indexed into the paragraph's own real
  `Text`, the same coordinate space `deliverytags.Insertion.Offset` uses)
  in `paragraphs.pronunciation` - deliberately *not* `clone_model`-keyed
  like `tts_tags`: a pronunciation fix is plain word substitution, correct
  under any TTS model, unlike a delivery tag's model-specific vocabulary.
  `pronounce.Apply` (used by `Paragraph.ResolveGenerationText`) supersedes
  calling `deliverytags.Merge` directly for a paragraph with both kinds of
  edit: both are offset-anchored into the same original `Text`, so
  composing them in one single offset-ordered pass - rather than
  substituting first and re-targeting insertion offsets against the
  now-different-length result - is what keeps every offset meaningful.
  Forced alignment and the reader's own on-screen text always use `Text`
  untouched here too.

  Bundled behind `directChapter`'s own Higgs-only clone-model gate for v1
  even though pronunciation resolution doesn't actually need it -
  simplicity, since every book in practice currently resolves to
  `voices.DefaultCloneModel` anyway; move it out from behind that gate if
  a non-Higgs clone model ever wants pronunciation fixes without the
  Higgs-specific tag passes.
- `internal/jobs/` — a single priority queue shared by every
  chapter/paragraph/character-scoped background job, generalized over a
  `Kind` (`KindVoiceClone`, `KindVoiceDesign`, `KindSpeakerAttribution`,
  `KindSpeakerCharacterization`, `KindSpeechDirection` so far) rather than
  one mechanism per feature. Every kind sorts through the same
  tier-ordered heap (`TierUrgent`/`TierLookahead`/`TierBackground`), but
  dispatches into one of *two separate* worker slot pools by resource, not
  one shared pool: `poolGeneration` (`maxInFlight` slots, GPU-bound TTS
  work) and `poolLLM` (`maxAttributionInFlight` - `2`, matching
  `llmworker.Config`'s own default `MaxConcurrent` - slots, LLM-bound
  work). Deliberate, not incidental: an earlier version shared one pool
  across every kind, and a burst of attribution/characterization requests
  could fill every slot with LLM work, leaving zero for TTS paragraph
  generation until it drained. `maxAttributionInFlight` used to be `1`
  (rather than matching `maxInFlight`) for a second reason: the old,
  single-sequence `speakerattr.Client` fully serialized every
  attribution/characterization call behind its own mutex regardless of
  slot count, so a second `poolLLM` slot could only ever sit blocked. Not
  true any more now that `speakerattr.Client` proxies to
  `internal/llmworker.Worker`'s real multi-sequence batching, so
  `maxAttributionInFlight` was raised to match - keep the two in step if
  either default changes, or slots go unused / tasks queue despite a free
  model slot. `poolGeneration`'s own single worker is intentional
  independent of that: the ttsworker process serializes actual GPU
  generation anyway, so more Go-side concurrency wouldn't help.

  The two pools are also mutually exclusive at runtime, not just
  separately slotted: `worker`'s `fill`/`tryFill` closures never let
  `poolGeneration` and `poolLLM` be active at the same time in either
  direction (both are GPU-bound; running concurrently pushes VRAM past
  what either alone needs on hardware with no headroom) - starting one
  pool's first task for a pass immediately blocks the other from starting
  in the same pass. `fill` decides which pool gets first crack via
  `Manager.bestPool`: the single highest-priority queued task in the whole
  heap, found via the same `taskHeap.Less` ordering (tier, then
  `kindPriority`, then FIFO) used everywhere else, entirely without regard
  to pool - not a kind-based special case (an earlier version singled out
  attribution/characterization specifically; missed tier entirely, so a
  queued background `KindSpeechDirection` task could still make `poolLLM`
  go first ahead of a more urgent clone/design task in `poolGeneration`).
  Under `bestPool`, an urgent clone task always outranks a background
  direction-tagging task, while a background clone task still loses to
  background attribution/characterization at equal tier.

  Unlike a queued task (which a more urgent one simply overtakes in
  `bestPool`'s ordering before either starts), an already-dispatched task
  used to never be preempted at all. Two mechanisms now keep a genuinely
  urgent task from waiting out in-flight *background* work, shaped
  differently because of a real constraint: a `poolGeneration` task's
  `ttsworker` HTTP call is ctx-aware and aborts promptly; a `poolLLM`
  task's synchronous LLM decode loop is not, and can only pause itself
  between batches.
    - poolLLM yielding to urgent poolGeneration work: cooperative and
      cross-pool. `Manager.HasHigherPriorityWork` (checked between batches
      by attribution/direction implementations) reports true for a queued
      task at a tier more urgent than `TierBackground` in *either* pool -
      so a long background run pauses itself and requeues the remainder
      the moment a reader's own urgent paragraph shows up. Safe to fire
      often, since it never cancels in-progress GPU work.
    - Urgent `poolGeneration` work jumping its own queue: hard
      cancellation, same-pool only, `TierUrgent` only (not
      `TierLookahead`). `Manager.preemptBackgroundGeneration` cancels just
      enough in-flight `TierBackground` `poolGeneration` tasks to cover
      genuinely urgent `poolGeneration` work queued right now that a free
      slot can't already absorb - e.g. a reader jumping mid-read while a
      whole-book background run has claimed every slot. `TierLookahead`
      deliberately does not trigger this: `EnqueueLookahead` continuously
      re-enqueues a whole window of upcoming paragraphs as a reader
      scrolls, which is steady read-ahead buffering, not something the
      reader is blocked on right now - triggering on it fired constantly
      for very little benefit. Each canceled task is marked
      `task.preempted` first, so `handleResult` pushes a fresh copy back
      onto the queue instead of marking it `AudioError`.  Fire-and-forget
      - never tries to dispatch the waiting task in the same pass; the
      freed slot shows up on `tryFill`'s next pass once `handleResult`
      observes it.

      Also subtracts in-flight background tasks a *previous* call already
      marked `preempted` (asked to cancel, not yet observed) from how many
      new victims it picks - without that, repeated calls while waiting
      for the first cancellation to take effect would each wrongly
      conclude a fresh victim is needed, evicting far more background work
      than the number of urgent tasks actually waiting. Confirmed live,
      twice: an earlier cross-pool version thrashed constantly against
      routine `KindSpeakerCharacterization` calls; the first same-pool
      version (missing this dedup and free-slot accounting) still
      thrashed - the same paragraph preempted several times within one
      second, purely from calling itself redundantly while its own first
      cancellation was still in flight.

      An even earlier version triggered cross-pool - canceling background
      `poolGeneration` work whenever `poolLLM` had *any* urgent task
      queued. Removed after real production thrashing:
      `KindSpeakerCharacterization` defaults to `TierUrgent` for every
      caller (because a characterization call is short, not because it's
      more business-urgent), including `provisionMissingCharacterVoices`'s
      own lazy calls that happen routinely as part of ordinary bulk
      background generation - so on a book with many characters, nuking up
      to `maxInFlight` in-progress GPU renders every time generation
      crossed into a new character wasted far more than the brief wait it
      was meant to avoid (observed: thousands of cancellations within
      seconds, queue depth exploding into the thousands).
      `preemptBackgroundGeneration`'s own doc comment has the full
      history - worth reading before changing this again.

  Both mechanisms are independent of, and don't affect,
  `taskBlockedBySpeechDirection`/`popTask`'s own separate *per-chapter*
  hard block (a clone/design task for the exact chapter currently being
  direction-tagged is skipped entirely until that tagging finishes, and
  the blocking task's own tier is boosted to match whatever it's
  blocking) - a chapter actively being read still gets its direction tags
  applied before its audio generates, regardless of either mechanism above.
    - `KindVoiceClone`/`KindVoiceDesign` (`poolGeneration`) walk a
      chapter's not-yet-ready paragraphs and generate each one (via
      `internal/voicerefs` + `internal/ttsworker`), writing `.wav` files
      and updating paragraph status, then kicking off forced alignment as
      a detached follow-up. Each paragraph resolves its own voice via
      `internal/narration.Resolver` before being queued, so a chapter's
      paragraphs can dispatch to, and cache audio under, several different
      `voice_id`s in one generation run.
    - `KindSpeakerAttribution` (`poolLLM`) runs one chapter's speaker
      attribution, triggered via `Manager.EnqueueAttribution` - unlike the
      generation kinds, this package has no `Store`/`Speaker` access of
      its own, so the actual work is a caller-supplied `AttributionFunc`
      (`httpapi.Server.attributeChapter`). Fire-and-forget, defaulting to
      `TierBackground` - attribution never blocks anything the reader is
      looking at, and SpeakersPage's "Attribute all" can enqueue a whole
      book at once; that bulk work shouldn't preempt a more urgent
      `poolLLM` task (`RunCharacterization`, still `TierUrgent`) queued
      behind it. Also doesn't block its caller (`202` returned
      immediately) on a `poolLLM` slot actually dispatching it. An earlier
      version did block the caller; changed because attribution runs a
      whole chapter through the LLM in batches and can take minutes, and
      "Attribute all" fires one request per unattributed chapter at once -
      for a long book that meant dozens of HTTP requests sitting open
      behind `poolLLM`'s single slot for as long as the whole book took, a
      page reload aborting every not-yet-sent request outright (so that
      chapter's attribution never ran at all, despite the dispatched task
      itself never actually being tied to the request's context - it
      always ran via the worker loop's own long-lived context). Progress
      is observable via `GET /api/jobs` rather than the enqueuing
      response. `attributeChapter`'s own closure never calls
      `RunCharacterization` - it used to, and since `poolLLM` only had 1
      slot, that nested call could never dispatch, hanging both it and
      every other attribution/characterization request server-wide until
      the original request's context was cancelled. Fixed by moving
      characterization out of attribution entirely - `poolLLM`'s 1-slot
      size (as it was then) was safe once nothing dispatched into it ever
      called back into it.
    - `KindSpeakerCharacterization` (`poolLLM`) runs one character's
      voice characterization (`Client.CharacterizeVoice`, triggered via
      `Manager.RunCharacterization`, which - unlike `EnqueueAttribution` -
      still blocks its caller until a `poolLLM` slot dispatches it: a
      single character's characterization is one short LLM call, so
      holding a request open for it is fine), same join/`TierUrgent`
      behavior, keyed by characterID instead of chapterID, reporting only
      an error. Its only callers are `httpapi.provisionCharacterVoice`
      (the lazy path, called from `jobs.Manager`'s own background
      goroutines, never from inside a worker slot) and
      `httpapi.handleCharacterizeSpeaker` (Speakers page's "Regenerate"
      button), so it shows up on the Jobs dashboard and a reader can
      explicitly retrigger it even if already characterized.
      `handleCharacterizeSpeaker` then invalidates (updates `Instruct`,
      deletes the cached reference clip, does *not* eagerly re-render)
      that book's resolved clone model's assigned voice preset for the
      character, so it's lazily rebuilt next time it's actually needed.
- `internal/deliverytags/` — dependency-free leaf package behind speech-
  direction tagging's core safety invariant: `ExtractInsertions` diffs an
  LLM-annotated variant of a paragraph's text against the original and
  returns exactly which `<|category:value|>` tags it inserted (and at what
  offset), rejecting anything that changes a real word; `Merge` composes
  insertions from more than one independently-annotated variant of the
  same text into one final string. Used by `internal/speakerattr` and
  `internal/pronounce` - standalone specifically so it can sit underneath
  both without either importing the other.
- `internal/pronounce/` — the narrower, complementary mechanism behind
  pronunciation resolution: `Substitution` replaces a real span of text
  rather than only inserting alongside it, a genuine exception to
  `deliverytags`' invariant that's safe here specifically because a
  `Substitution`'s `Replacement` never comes from free generation - always
  from a small, pre-computed candidate list. `Apply` is what `Paragraph.
  ResolveGenerationText` calls to build a paragraph's final generation
  text, composing zero-width tag insertions and span-replacing
  substitutions together in one offset-ordered pass so a substitution's
  length change never throws off another edit's offset.
- `internal/audiopath/` — computes where a paragraph's audio file lives
  from IDs alone (`data/audio/<bookID>/<chapterID>/<idx>.wav`); never
  stored in the DB, just derived.
- `internal/wav/` — a minimal RIFF/WAVE codec: `Duration` (header-only, for
  reporting chunk length to the frontend) plus `Decode`/`Encode` (full
  16-bit PCM in-memory codec, shared by `internal/voicerefs` and
  `internal/audioworker` for WSOLA re-speed and worker request/response
  bodies).
- `internal/httpapi/` — handlers, using Go 1.22+'s stdlib `http.ServeMux`
  method+path-parameter routing (`"GET /api/books/{id}"`). No router
  dependency.
- `internal/mdnsadvert/` — advertises the backend on the LAN via mDNS/DNS-SD
  (`_lectable._tcp`, using `github.com/hashicorp/mdns`) so `android` can
  find it instead of requiring the user to type in an IP. Started from
  `cmd/server/main.go` alongside the HTTP server and shut down (sending an
  mDNS goodbye packet) on the same signal-triggered shutdown path. Pure
  advertisement — the backend never browses for anything itself.

## Toolchain note

This module's `go.mod` needs a newer Go than the box's default
`/usr/bin/go` (1.22.2) provides — `go-duckdb`'s dependency graph pulled in
a `go 1.26` requirement. A Go 1.27.1 toolchain is installed locally at
`/home/rhino/go-toolchains/go1.27.1/` for this purpose. Automatic
toolchain fetching via `GOTOOLCHAIN=auto` does not work in this
environment (fails with "toolchain not available" even though the Go
download servers are reachable directly) — don't spend time debugging that
path again; just build with:

```
export PATH=/home/rhino/go-toolchains/go1.27.1/bin:$PATH
export GOTOOLCHAIN=local
go build ./...
```

`Makefile` (this directory) wraps all of the above - the toolchain
`PATH`/`GOTOOLCHAIN` override *and* both native libraries'
`CGO_LDFLAGS`/`LD_LIBRARY_PATH` (defaulting to the same
`/home/rhino/audio.cpp/build/bin`/
`/home/rhino/llama-cpp-py-sync/vendor/llama.cpp/build/bin` paths used
elsewhere in this doc, overridable per-invocation, e.g. `make build
AUDIOCPP_DIR=/some/other/path`) - so day-to-day commands don't need any of
these exported by hand:

```
make build   # go build -o ttsworker ./cmd/ttsworker && go build -o server ./cmd/server
make test    # go test ./...
make vet     # go vet ./...
make tidy    # go mod tidy
make run     # builds then launches via run.sh - see "Run" below
```

## Run

```
go build -o ttsworker ./cmd/ttsworker   # needs CGO_LDFLAGS/LD_LIBRARY_PATH (audio.cpp *and* llama.cpp now), see above
go run ./cmd/server                     # needs neither - see the hard invariant above
```

`run.sh` (this directory) wraps the above plus every env var below with
the same defaults `Makefile` uses (override any of them by exporting
first, e.g. `AUDIOCPP_DIR`/`LLAMACPP_DIR`/`DATA_DIR`/
`SPEAKER_LLM_MODEL_PATH`): `./run.sh --build` builds both binaries then
launches `server` backgrounded (logging to `/tmp/lectable-server.log` by
default, `$LOG_FILE` to override) - `server` spawns `ttsworker` itself, as
always. Drop `--build` to relaunch without rebuilding, or add `--fg`/
`--foreground` to run in the foreground instead of backgrounding it. This
is the easiest way to get both binaries' env vars right at once - prefer
it (or `make run`, identical) over reconstructing `CGO_LDFLAGS`/
`LD_LIBRARY_PATH` by hand.

Env vars (all optional): `PORT` (8080), `DATA_DIR` (`./data`),
`ALLOW_ORIGIN` (`http://localhost:5173`), `TTS_WORKER_BIN` (`./ttsworker`),
`TTS_WORKER_PORT` (8091), `AUDIOCPP_LIB_DIR`
(`/home/rhino/audio.cpp/build/bin`), `SPEAKER_LLM_MODEL_PATH`/
`SPEAKER_LLM_GPU_LAYERS`/`SPEAKER_LLM_CTX`/`SPEAKER_LLM_MAX_CONCURRENT`/
`SPEAKER_LLM_NO_THINK`/`MODEL_IDLE_UNLOAD_AFTER` (speaker attribution - see
"ttsworker / audioworker / llmworker" and `internal/speakerattr` above;
attribution is disabled, not fatal, if `SPEAKER_LLM_MODEL_PATH` is unset).
Set these on whatever environment runs `./server` (or `go run ./cmd/
server`), not `./ttsworker` directly - `cmd/server`'s own `ttsworker.
Manager` spawns the worker with `os.Environ()` (its own environment) as
the child process's base, so any of these already set for `cmd/server`
flow through to `ttsworker` automatically; only `LD_LIBRARY_PATH` (and
`QWEN_TTS_DEFAULT_CLONE_MODEL`) get explicitly overridden on top of that.
Building `cmd/server` itself needs neither native library's shared
libraries on `CGO_LDFLAGS`/`LD_LIBRARY_PATH` any more - only `ttsworker`'s
build does, since both now link into that one binary.

## API surface

- `POST /api/books` (multipart `file`) / `GET /api/books` / `GET|DELETE /api/books/{id}`
- `GET /api/books/{id}/cover`
- `GET|PUT /api/books/{id}/voice` — changing voice resets that book's paragraph audio to `pending`; `multiVoice` toggles whether character voice assignments (see below) override this book's own voice at all (default off)
- `GET|PUT /api/books/{id}/position`
- `GET /api/books/{id}/chapters/{idx}` — paragraphs with `audioStatus`/`audioUrl`/`speaker`/`inline`
- `POST /api/books/{id}/generate` — enqueues background generation for every chapter in the book at once (library page's hover "Generate audio" button) - just loops `Jobs.EnqueueChapter` per chapter and returns `202` immediately; no background goroutine or dedup needed since `EnqueueChapter` is already fire-and-forget and idempotent per chapter
- `POST /api/books/{id}/chapters/{idx}/generate` — enqueue background generation (idempotent per chapter while in flight)
- `POST /api/books/{id}/lookahead` — `{"chapterIdx", "paragraphIdx"}`; enqueues generation starting at that paragraph for whatever isn't already `AudioReady` (pending/error) - readers scrolling/seeking ahead, not a regenerate
- `POST /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/regenerate` — unconditionally resets one already-`AudioReady` paragraph's audio to `pending` and re-enqueues it at urgent priority, even though `lookahead` above would skip it - the "the reader didn't like this line" action
- `PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/speaker` — `{"speaker": "..."}`; corrects one paragraph's speaker attribution directly, without touching its already-generated audio - a caller wanting the new voice actually narrated still needs a separate `regenerate` call above. `""`/`"Narrator"` both mean "no character"; any other name is registered as a real character if it wasn't one already. 400s if the target paragraph isn't quoted dialogue (`IsQuote`) - narration/description can't be attributed to a speaker
- `POST /api/books/{id}/chapters/{idx}/attribute-speakers` — enqueues LLM speaker attribution for one chapter and returns `202 {"queued": true}` immediately, not the result (503 if `SPEAKER_LLM_MODEL_PATH` unset) - fire-and-forget; registers any newly-discovered character with no voice yet - characterization/voice assignment happens lazily later, the first time that character's voice is actually needed for generation
- `POST /api/books/{id}/chapters/{idx}/tag-directions` — enqueues speech-direction tagging (Higgs's own inline delivery tags) *and* pronunciation resolution for one chapter, `202 {"queued": true}` immediately (503 if `SPEAKER_LLM_MODEL_PATH` unset, 400 if the book's resolved clone model isn't `audiocpp-higgs-4b`) - fire-and-forget
- `POST /api/books/{id}/preprocess` — the "run everything" meta-task: attribution, characterization, voice provisioning, and direction-tagging for every chapter/character in the book, without needing a reader to click SpeakersPage's four individual "all" buttons in sequence and wait for each. Only three of the four are actually chained - attribution before characterization (needs attribution's own discovered characters) before voice provisioning (needs characterization's own Summary); direction-tagging depends on none of them (`directChapter` only needs the book's own already-resolved voice and each paragraph's own `IsQuote`/`Text`, none of which the other three touch), so it dispatches immediately alongside attribution rather than waiting on the other three to clear first - see `jobs.pipelineResolver`'s own doc comment. `202 {"queued": true}` immediately (503 if `SPEAKER_LLM_MODEL_PATH` unset, `409` if a run is already in progress for this book) - the four phases are real, dependency-ordered tasks (`jobs.Manager.EnqueuePipeline`, one `pipelineTask` per phase, `httpapi`'s `preprocess*Phase` closures supplying each phase's actual work), each phase task blocking on its own chapter's/character's real per-item task (`RunAttribution`/`RunCharacterization` via `ensureCharacterized`/`RunVoiceProvision`/`RunDirection`) exactly the way a single-item button dispatch already does - so no nested-queue-call deadlock risk (a phase task never occupies the `poolLLM`/`poolGeneration`/`poolDesign` slot it's waiting on). The four phase tasks themselves dispatch through `poolPipeline`, on a second, independent `taskqueue.Queue` (`jobs.Manager.pipelineQueue`) separate from the one `poolGeneration`/`poolDesign`/`poolLLM` share - see `internal/jobs/pipeline.go`'s own doc comment: this is what lets book preprocessing actually run concurrently with ordinary generation/attribution instead of being subject to those three pools' own "one pool active at a time" mutual exclusion, since a phase task does no GPU/LLM work itself. Progress observable exactly like the individual buttons' own. Best-effort and idempotent per item, so rerunning it (or an individual button) after a partial failure only redoes what didn't finish
- `GET /api/books/{id}/speakers` — per-book speaker table: Narrator + every attributed character (shared across the whole series), each with its `summary` characterization, paragraph/ready-audio counts, and a sample clip URL
- `DELETE /api/books/{id}/speakers` — clear this book's own paragraph attribution and delete the whole series scope's character roster (identity, voice assignments, auto-created voice presets + cached clips) - the Speakers page's own "Delete speaker data" button, for starting over from scratch
- `DELETE /api/books/{id}/characters/{characterId}` — remove one character: this book's own paragraphs attributed to them revert to Unknown (still real dialogue, just unattributed - "Narrator" is reserved for actual narration, never dialogue), their identity/voice assignments/auto-created voice presets are deleted for their whole series scope - a scalpel next to the above, for cleaning up a single bad attribution without resetting the whole roster
- `POST /api/books/{id}/characters/{characterId}/merge` — `{"targetName": "..."}`; folds one character into another (or into `"Narrator"`, same effect as the DELETE above): this book's own paragraphs attributed to them are reattributed to targetName instead of blanked, and their own identity/voice/presets are deleted for their whole series scope
- `POST /api/books/{id}/characters/{characterId}/generate-voice` — force-provisions a voice for one character right now instead of waiting for the lazy path; 409 if there still isn't enough attributed dialogue to characterize them from
- `PUT /api/books/{id}/characters/{characterId}/voice` — assign (or clear, with `""`) a character's own narration voice, overriding the auto-assigned one
- `GET /api/books/{id}/characters/{characterId}/appearances` — every paragraph that character speaks, across every book in their series - a "where does this character show up" list; each one's `audioUrl` is set once its audio is ready under whatever voice it currently resolves to
- `POST /api/books/{id}/characters/{characterId}/characterize` — force re-run LLM voice characterization for one character (503 if `SPEAKER_LLM_MODEL_PATH` unset), even if already characterized; invalidates every clone model's assigned voice preset for the character, if any
- `GET /api/paragraphs/{id}/audio` — Range-request-capable, via `http.ServeFile`
- `GET /api/voices/presets`, `GET /api/voices/languages` — served in-process from `internal/voices` (no worker call needed)

## Testing changes

There's no test suite yet. To sanity-check end to end: build a synthetic
epub (zip with `mimetype` + `META-INF/container.xml` + OPF + XHTML
chapters — see conversation history / regenerate similarly) and run it
through upload → generate → audio fetch via curl. Both `server` and
`ttsworker` binaries must be built first (see "Run" above) - `server`
spawns `ttsworker` itself, so there's nothing separate to start.
