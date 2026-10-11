# backend

Go service owning the library: epub parsing, storage, background TTS
generation, TTS/LLM orchestration (voice presets, reference-clip
rendering/caching, speed adjustment, speaker-attribution prompting/
batching), and the HTTP API the frontend talks to. This module also builds
`cmd/ttsworker` - a second, separate binary that's the only thing here
linking against `libaudiocpp.so` **or** `libllama.so`.

The long-form notes live in `docs/`, to be read when working on that area:
`docs/speakerattr.md` (every LLM pass), `docs/jobs.md` (the job queue),
`docs/api.md` (per-route notes), `docs/latents.md` (latents sidecars and
caches), `docs/audiocpp-patches.md` (the local audio.cpp patch set) and
`docs/music-models.md` (why region music is Stable Audio 3 Medium: the
measured comparison with ACE-Step, YuE2 and Magenta RealTime 2).

## ttsworker / audioworker / llmworker - why model inference is a separate process

audio.cpp (the C++ engine doing TTS inference, wrapped by `../audiocpp-go`)
has a confirmed native memory leak: Higgs rebuilds its prefill/decode graphs
on nearly every call and upstream never evicts the captured HIP graph cache
entries, so every paragraph leaves one resident. It is patched in this box's
local `/home/rhino/audio.cpp` checkout only - uncommitted there and not
upstreamed, deliberately - along with a guard against non-terminating Higgs
generations, several Higgs throughput patches, and the latents/codec
artifact API the backend depends on. **A fresh clone or reset of that
checkout loses all of them**; `docs/audiocpp-patches.md` has each patch,
how to reapply it, and the measurements behind it.

**Latents in the backend** (`internal/latents`): clips whose model has
them get a `<idx>.lat` sidecar (Higgs/BreezeTTS codes, PocketTTS latents,
exact), music seeds are stored as Stable Audio latents, and reference-clip
encodings are cached under `voice-refs/cache/refcodes/`. See
`docs/latents.md`.

Rather than let that leak eventually OOM-kill the whole backend, TTS
generation is isolated into its own disposable process (kept in place as
cheap insurance regardless of the leak patch - it's unverified over a long
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
  about the sample rate audio.cpp reports timestamps in), `/transcribe`
  (free ASR via Parakeet TDT, for the completeness check's extra-words
  half), and `/health`.
  Holds **no per-preset state** - the caller (backend) resends a preset's
  reference audio bytes on every call; audio.cpp's own session-level cache
  (`cloneFamily.sessionOptions`, 8 slots) transparently dedupes repeated
  (reference audio, reference text) pairs. `Config.IdleUnloadAfter`
  (wired from `LECTABLE_MODEL_IDLE_UNLOAD_AFTER`, shared with `internal/llmworker`)
  frees *every* loaded clone model, including the otherwise-never-evicted
  default one, once none of `Generate`/`Design`/`Align` has been called
  for that long - timeout-based deliberately, not triggered eagerly on
  every switch away from TTS work, for the same reason `internal/jobs`'
  `poolLLM`/`poolGeneration` mutual-exclusion (below) avoids acting on
  every switch: a real, observed thrashing incident from an earlier,
  eager version of an analogous mechanism. Model *kinds* (each clone
  model / each design engine / each aux engine like Stable Audio) are
  mutually exclusive via `residencyGate` (`residency.go`): a request of a
  different kind waits for every in-flight request of the current kind to
  finish before its own eviction + load runs, FIFO so a stream of clone
  calls can't starve it. Without this, eviction had to skip in-flight
  models and loaded on top of them - Higgs + Stable Audio + a Breeze clone
  resident at once, observed maxing VRAM and silently killing the worker.
  Only one clone model is ever resident (no default-plus-extras LRU): an
  emotion-variant render's Breeze clone used to load right beside the
  Higgs pool mid-chapter and max VRAM, so a different clone model now
  waits for the current one to drain and evicts it. The aligner, the
  transcriber, and the LLM aren't gated.
- **`internal/llmworker`** is the *only* package importing `llamacpp-go`.
  It loads the speaker-attribution GGUF model lazily (on first
  `LLMGenerate` call, or the first call after an idle unload) and serves
  up to `Config.MaxConcurrent` (default 2, `LECTABLE_SPEAKER_LLM_MAX_CONCURRENT`, which `run.sh` sets to 4 and `internal/jobs` sizes its LLM pool from too; measured 2026-10-07 with batches of identical recap calls: total generation ~110 tok/s at 2 slots, ~120 at 3, ~133 at 4, each call slowing from ~74 to ~55 to ~47 tok/s, ~0.9 GB of VRAM a slot - 4 is ~22 GB with the model on the 24.5 GB card, so 5 is not an option -
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
  call. With `LECTABLE_SPEAKER_LLM_PRIME_STATE=true` they're snapshots
  restored into (and then kept in) a slot's own KV stream instead. Each
  finished call logs its prompt/reused/generated tokens and prefill/decode
  time. `LLMDecide` shares those slots and primed prefixes for a reply
  made only of closed-set answers: each answer is read from the model's
  logits over its options instead of generated (`llamacpp.Scheduler.
  Decide`) and comes back with its probability - what the pronunciation
  and emotion passes use.
  The package also holds the **embedding model** (`embedder.go`,
  `Embedder`: Jina v5 text nano, the file `internal/semantic` names),
  loaded twice from one GGUF, each lazily: on the GPU for the chapters a
  `jobs.KindEmbedding` task embeds - LLM-pool work, so it is on the card
  only while that pool holds it, and `/llm/unload` and an exclusive LLM's
  audio load unload it too - and on the CPU (`llamacpp.ModelParams.NoGPU`)
  for a search phrase at any time. `POST /embed` picks by
  `ttsproto.EmbedRequest.Query`. `LECTABLE_EMBED_GPU=false` keeps the bulk
  copy on the CPU as well.
- **`cmd/ttsworker/main.go`** is a thin `main()` wiring `audioworker.
  Worker` and `llmworker.Worker` up to one loopback HTTP server (default
  `:8091`), including `/llm/generate` and `/llm/decide` alongside the TTS
  routes.
- **`internal/ttsworker`** is the *client/supervisor* package `cmd/server`
  actually imports - zero dependency on `audiocpp-go`/`llamacpp-go`. Its
  `Manager` spawns the `ttsworker` binary, health-polls it, and runs a
  watchdog goroutine that kills and respawns it if its RSS crosses a
  threshold (default 11GiB, `RSSLimitBytes`) or if it dies on its own.
  Every proxied call (`Generate`/`Design`/`Align`/`LLMGenerate`/`LLMDecide`) takes a
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
- `cmd/lectable-mcp/` — an MCP server (stdio) for agents working on this
  repo, registered with Claude Code by `../.mcp.json` as `lectable` and
  started through `mcp.sh` (builds, then runs it against
  `LECTABLE_URL`/`LECTABLE_DATA_DIR`/`LECTABLE_LOG_FILE`, the same
  defaults `run.sh` uses). Its tools answer in what fits a context
  window: `books`, `book`, `read` (a chapter as `idx [speaker, emotion]
  text`), `search`, `speakers` and `jobs` summarize the matching GET
  routes (`GET /api/jobs` alone lists every queued task); `routes`
  searches the route table; `api_get`/`api_call` call any route, reads and
  writes kept apart so reads can be allowed without a prompt; `sql` runs
  one statement on a read-only connection to `library.sqlite`; `logs`
  tails the server log. The route table (`routes_gen.go`) is written by
  `cmd/apigen` with the clients, so a new route shows up after `make
  generate`; a new *tool* is an entry in `tools.go`'s `tools` slice. Talks
  to the backend over HTTP only - it imports nothing from `internal/`.
- `cmd/llmproxy/` — a standalone OpenAI-compatible endpoint
  (`/v1/chat/completions`, `/v1/models`; default `127.0.0.1:8092`) in
  front of the backend's own LLM, for tools that only speak that format
  (DuckDB's `ai`/`flock` extensions). It calls `POST /api/llm/test`, so
  each completion is a pooled, `TierUrgent` job rather than a call behind
  the scheduler's back. No embeddings (501), no real streaming. Not built
  or started by `make build`/`run.sh`: `go run ./cmd/llmproxy`.
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
  concurrently with itself. Every base clip is normalized to
  `voicerefs.TargetLUFS` (-20 LUFS, BS.1770 integrated via
  `internal/loudness`) as it's saved (`save`), with the gain clamped to
  ±12 dB and a lookahead peak limiter (`limitPeaks`) holding peaks under
  ~-1 dBFS - cloning carries a reference's level into every paragraph, so
  this keeps narrators, custom presets and characters at one level.
  `NormalizeAll` runs at startup (`cmd/server`) to level clips saved before
  this or restored from an export: it skips clips within 1 LU of the
  target and shifts a rewritten clip's emotion variants by the same gain
  instead of deleting them. A rewritten clip's bytes change, so its cached
  reference encodings (`voice-refs/cache/refcodes`, keyed by content hash)
  miss and re-encode; the stale ones can just be deleted. Existing
  paragraph audio keeps its old level until regenerated.
  `variants.go` renders/caches each preset's
  emotion variants (`EnsureVariantFile`, its own per-(preset, emotion)
  lock) - see "Emotions" in `docs/speakerattr.md`.
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
  bridge is out," Sam said.` → two segments. A curly/guillemet opener left
  unclosed at the paragraph's end quotes the rest of it - a speech running
  over several paragraphs leaves each one's “ unclosed by convention, and
  those used to read as narration (58 paragraphs of Arise: Reclaimer, in
  the narrator's voice); a stray straight `"` still doesn't (inch marks).
  Books imported before that keep their rows - `PUT .../paragraphs/{pidx}/quote`
  flags one by hand. Every segment after the first
  in a source paragraph is flagged `Block.Inline` - a display hint meaning
  "part of the same visual paragraph as the previous one", not a real
  break - while still being its own row/idx/audio/speaker. This is what
  lets one mixed narration+dialogue sentence resolve to two different
  narration voices while reading on screen as one paragraph. A single `<br>` is a space, but two in a row (only whitespace
  between) split the block into separate paragraphs (`paragraphBreak`):
  some epubs put a whole scene in one `<p>` with `<br/><br/>` between its
  real paragraphs, which merged every turn of dialogue into one paragraph
  and threw off speaker attribution. `readZipFile` caps each entry at 64 MiB decompressed (`maxEntrySize`), so a malformed epub or zip bomb fails the upload instead of exhausting memory.
  Text loose beside a block's block children (`<div>Note <p>...</p></div>`)
  is a paragraph of its own (`walkChildren`), as are `<th>`, `<caption>`,
  `<dt>`/`<dd>` - all of which used to vanish; `<head>`, `<script>`,
  `<style>` and `<svg>` are skipped.
  **Tables** (`table.go`): a `<table>` is read row by row - a row's leaf
  cells are joined into one paragraph with `CellSeparator` (" | "), never
  quote-split, with `Block.Table` (`TablePos`: table, row, each cell's
  byte span of the text and its grid column/colspan/rowspan) saying how to
  lay it back out; a cell holding blocks of its own keeps them as separate
  paragraphs, each placed in that cell. One paragraph per cell (the old
  behavior) made a 5-column spell list 5 clips a row, each read with no
  context. A table of a single cell is a layout wrapper and reads as
  ordinary blocks. **Heading rows** (`table.headings`) are `<thead>`/all-
  `<th>` rows, or - most epubs use plain `<td>` - the first row of a table
  3+ columns wide that reads like headings (a few words a cell, no digits,
  no "Key:"), a row of "Key:" cells over a row laid out the same
  ("Rarity: | Durability:" over "Rare | 72/100"), or a heading-like row
  where the layout changes (a character sheet's "Stat | Points | Effect").
  Each cell under one carries its column heading as `TableCell.Label`,
  which `store.Paragraph.ResolveGenerationText` speaks before the cell
  ("Rarity: Rare. Durability: 72/100." - cell separators become sentence
  breaks either way), and the heading row itself (`TablePos.Heading`) is
  `Unvoiced`, a silent clip. The spoken headings aren't in the text:
  alignment still runs on the plain text (the aligner stretches a
  neighbouring word over them) and the completeness check already compares
  against the generation text too. Clients must not count the " | "
  separators as words - the aligner returns no timing for a bar on its
  own - so each tokenizes a row's cells only. Measured on the library:
  ~250 tables, 5,600 cells; two-column tables never get first-row headings
  ("Name | Jack" over "Class | None"). Stored as `paragraphs.table_pos`
  (JSON, migration 00008); books imported before it keep one paragraph per
  cell until a chapter is reimported.
  **Plays** (`play.go`): a play names the speaker before each speech
  instead of quoting it, so as prose it had no dialogue at all and the
  narrator read every label (A Midsummer Night's Dream: 0 quotes).
  `applyPlay` runs once over the parsed book, re-reading each source
  paragraph as `collectText` marked it up (`Block.src`: `<br>`s and
  small-caps/`charname` spans kept as sentinels, dropped again before
  `Parse` returns). It recognizes four label shapes - on its own line
  (`THESEUS.<br/>`), inline capitals (`FREDDY. There's...`, honorifics
  like `MRS. HIGGINS.` kept whole), a marked span (`<span
  class="smcap">Domin.</span>`), and alone in its paragraph with the
  speech in the ones after (Faust) - and reads the book as a play when
  at least 40 paragraphs and 15% of all of them start with a label
  recurring 3+ times (12 Gutenberg plays: all detected; 13 novels incl.
  LitRPG stat blocks: none, at most 1.5%). `?play=on|off` on upload and
  reimport overrides it (`ParseAs`). A speech becomes `IsQuote` blocks
  with `Block.Speaker` (the label's name in title case) and `Block.Label`
  (the label as set, shown by the clients, never spoken or aligned). A
  one-off speaker counts when its label looks like the others' and sits
  beside a recurring speaker's speech. An unlabelled paragraph continues
  the last speaker's speech when its class says speech (`drama`/
  `dialog`), or, in a book with no such classes whose labels stand apart
  from the speech, whenever it isn't a direction; a heading, the chapter
  title or "THE END"/`***` ends that. Directions inside a speech
  (`[...]`, and `(...)` when italic or when the book's whole-paragraph
  directions are parenthesized - Shakespeare's parentheses are spoken)
  split it: one saying what happens is an inline narration block, one
  saying how the line is delivered (opening the speech, or "aside"/"to
  X"/"-ing"/"-ly") joins the next speech block's `Label` and isn't
  narrated. Whole-paragraph directions, scene descriptions and front
  matter stay narration as parsed. Not handled: Standard Ebooks'
  table-based plays (their rows are table paragraphs, which the play pass
  leaves alone), merging name variants ("Rank"/"Doctor Rank"), and
  group labels ("All", "Both") are ordinary characters.
- `internal/websource/` — what importing a book from a website shares
  across sites: the `Source` interface a site implements (`Match` a link,
  `Resolve` it to a work id, `Work` = metadata + chapter list with the
  site's chapter ids, `Chapter` = the chapter's text as a cleaned XHTML
  fragment plus its images), `Client` (one request at a time, paced,
  retrying rate limits; its default HTTP client refuses loopback/private
  addresses, since chapter images are fetched from wherever the author
  linked), `WriteEPUB`, which lays fetched chapters out as the book's
  source epub and can add chapters to one it wrote, and `Wayback`, which
  reads the Internet Archive's copies of a site's pages (a `Fetcher` like
  `Client`: every `Source` method takes the fetcher to read through, so
  the same parser reads the live site or an archived one - and has to
  cope with the site's older layouts). A site that takes chapters off
  its lists also implements `Delister`, which `Wayback.Delisted` uses to
  find them in the archive's index. No store or job
  access: the orchestration (create the book, append chapters, check for
  new ones) is `httpapi/source.go` - see the `import-url` entry in
  `docs/api.md`. The sites: `internal/royalroad/` (a work is a fiction),
  `internal/wanderinginn/` (a work is one volume of the serial, in the
  series "The Wandering Inn") and `internal/patreon/` (a work is a
  creator's campaign or one collection of it, read through the site's own
  undocumented JSON API - see the `settings/patreon` entry in
  `docs/api.md`); each keeps everything that depends on its
  site's markup in `parse.go` (Patreon: `patreon.go`/`tiptap.go`), and has a `...test` package that is a local
  stand-in for the site. A book can track several sources at once
  (`httpapi/source_multi.go`, the `.../sources` entry in `docs/api.md`). Adding a site is a new package implementing
  `Source`, listed in `httpapi.defaultSources`, plus its `bookSourceKind`
  constant. `WEBSOURCE_LIVE=<link> go test ./internal/websource -run
  TestLive -v` checks a real site, for when its markup changes.
- `internal/store/` — SQLite persistence (`books` → `chapters` →
  `paragraphs`, plus `characters`/`character_voices`, book-scoped), through
  `mattn/go-sqlite3` (cgo, SQLite bundled). WAL mode with **one write
  connection and a pool of read-only ones**: `notifyDB` sends
  `Query`/`QueryRow` to the pool and `Exec`/`Begin` to the writer, so reads
  never wait on a write or on each other; a transaction's own reads go
  through its `tx`. Whole-book aggregates (narration stats, chapter
  summaries) cost tens of milliseconds on a long book - SQLite walks rows
  one at a time - so still prefer a JOIN scoped by book/chapter over an
  `IN (...)` sized by paragraph count, and keep them covered by an index
  (`idx_paragraphs_stats`, `idx_paragraph_audio_stats`). `paragraph_audio`
  carries copies of its paragraph's `chapter_id` and `length(content)`
  (`chars`), filled at insert - neither ever changes for a paragraph - so
  the single-voice aggregates and `AudioRefs` read
  `idx_paragraph_audio_chapter` alone, a few milliseconds for a whole
  book; the multi-voice variants pick each paragraph's voice by its
  speaker and still join per paragraph. A trigger deletes a paragraph's
  audio rows with it (`DeleteBook` used to leave them behind), which is
  what keeps those copies trustworthy; the live library has no rows left
  without one. Needs the
  **`sqlite_fts5` build tag** (`../env.sh`, which the Makefiles, `run.sh` and `deploy.sh`
  all load, exports `GOFLAGS=-tags=sqlite_fts5`; a bare `go build`/`go test` fails at
  `Open` with "no such module: fts5"): `paragraphs_fts` is a trigram
  full-text index over `paragraphs.content`, external-content (keyed by
  `paragraphs.rowid`) and kept current by triggers, which
  `SearchParagraphs` matches a query against as one phrase - any substring
  of three or more characters, case-insensitive beyond ASCII; shorter
  queries scan the book with the Go-registered `fold()`. Never run a plain
  `VACUUM`: it may renumber `paragraphs` rowids out from under the index
  (`INSERT INTO paragraphs_fts(paragraphs_fts) VALUES ('rebuild')` repairs
  it). `rowhash(...)`/`bit_xor(...)`, the row fingerprints' hash and
  aggregate (`SyncFingerprints`, `ExportFingerprints`), are C functions in
  `rowhash.go` registered through `sqlite3_auto_extension` - a Go callback
  per row made a manifest request take half a second. Column types are
  declared `JSON`/`BOOLEAN`/`DOUBLE`/`BIGINT` as before; SQLite stores JSON
  as text, and the declared type is what row export reads to nest JSON and
  write booleans as `true`/`false`. Data access goes through the
  `store.Store` interface (`iface.go`, every method); `SQLiteStore` is the
  implementation and `store.Cached` (`cache.go`, what `cmd/server`
  and the httpapi/jobs tests use) wraps any `Store` with a read cache for
  the hot lookups (book, chapter, paragraph list, character, character
  voices, voice presets). Each cached read declares the tables it reads;
  invalidation hangs off the inner store's own `OnChange` write
  notifications (synchronous, before the write returns, `AllTables`
  clearing everything), with a per-table generation check so a read that
  raced a write never caches its result. Values are cloned on the way in
  and out. Writes need nothing extra as long as they go through
  `s.db`; a new hot read gets cached by adding one `cachedRead` override
  with the right tables (`GetBook` also depends on `books.position`).
  Logs its hit rate every minute. `ChapterNarration` (per-chapter
  paragraph/ready counts plus narration stats - what every book summary
  and the `books` live topic are built from) is cached differently, per
  book and never dropped by a write: each call compares
  `ChapterFingerprints` (per chapter: its own row, `chapter_text.rev`, and
  count and duration/chars/rowid sums of its `paragraph_audio` rows per
  voice and status) and re-reads only the chapters that changed.
  `chapter_text` (migration 00020) is one row per chapter - paragraph
  count, chars, words, and a `rev` that is a new random value whenever a
  paragraph's text or speaker changes - kept by triggers on `paragraphs`,
  so the query itself never reads a paragraph that has no ready audio: the
  multi-voice form starts from the ready `paragraph_audio` rows and looks
  each one's speaker up in `idx_paragraphs_stats`. Before that (2026-10-08,
  74 books, 2.5M paragraphs, all multi-voice) it joined every paragraph to
  its audio and any `chapters`/`paragraphs` write dropped every book's
  result: a `books` rebuild was ~5 s after each such write, and is now
  ~0.6 s the first time after a restart, ~0.1-0.2 s after. The
  fingerprints themselves were most of that (108 books: 170 of 200 ms), so
  they are only read after a write to one of the tables they cover
  (`narrationTables`); a rebuild after anything else - a saved position, a
  job - is ~20 ms (2026-10-10). The per-clip narration writes
  (`SetParagraphGenerating`/`Ready`/`ReadyClip`/`ReadyPointer`/`Error`,
  `SetParagraphWordTimings`, `SetParagraphClipKey`) also report which book
  they touched (`store.BookScope`, a pseudo-table beside the real one in
  `OnChange`), so a clip finishing re-reads that one book's fingerprints.
  So do the per-chapter pass writes (`SetChapter*` flags, `SetChapterNotes`,
  `SetParagraphSpeakers*`/`ScareQuotes`/`Emotions`/`Pronunciation`/`Quote`/
  `Speaker`, `DeleteParagraphAudioForIdxs`, `DeleteChapterAudio`,
  `ResetParagraphAudio`; a transaction sets `notifyTx.scope`). Any other
  write to those tables - an import, a reset, a restructure - still
  re-reads every book's; scope a new one only if every row it changes
  that a fingerprint covers is in the one book.
  **No FK `ON DELETE CASCADE`** —
  `DeleteBook` deletes paragraphs → chapters → book explicitly in a
  transaction. Column
  is `content` not `text` (avoids ambiguity with the SQL `TEXT` type
  name). **Schema migrations** are [goose](https://github.com/pressly/goose)
  SQL files in `internal/store/migrations/` (`NNNNN_name.sql`, embedded
  in the binary), and `Open` applies whichever ones the library hasn't
  had (`migrate.go`), each in its own transaction and recorded in
  `goose_db_version`. A schema change is a new file with the next number
  (`-- +goose Up`, then the statements; wrap a trigger in `-- +goose
  StatementBegin`/`StatementEnd`); never edit one a library has applied.
  They are Up-only: to undo one, restore a backup (see
  `internal/dbbackup`). `00001_initial_schema.sql` is the schema as it
  stood when migrations were introduced, all `IF NOT EXISTS`, so a
  library from before then adopts it as a no-op; `00002` gave
  `sync_tree_nodes`, `book_settings` and `chapter_summaries` primary keys
  (their writers are upserts). Before applying anything to a library that
  already has tables, `OpenWith` copies it to
  `Options.BackupBeforeMigrate` - `cmd/server` passes a path in
  `backups/`, so the copy is pruned with the scheduled ones - and refuses
  to migrate if the copy fails.
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
  `docs/speakerattr.md`. Resolution also takes the paragraph's chapter
  index (`NoChapter` for a voice asked about apart from any line): a
  character with age stages (`stages.go`, `StageVoice`) has a different
  voice from each stage's chapter on - see "Age stages" there.
- `internal/speakerattr/` — every LLM pass, as a thin caller-side `Client`
  (batching, prompt-building, reply parsing, malformed-response retry) over
  a GGUF model that actually runs in `ttsworker` (`internal/llmworker`):
  speaker attribution, scare quotes, chapter summaries and cast, the
  per-book setting profile, characterization,
  emotions, pronunciation, ambience (music and lighting) and SFX. Disabled
  as a whole when the model file (`LECTABLE_SPEAKER_LLM_MODEL_PATH`,
  default `Qwen3.6-35B-A3B`) is missing. The prompts, the rules each pass
  applies around the model, and the benchmark history ("Speaker
  attribution", "Emotions", "SFX sound effects", ...) are in
  `docs/speakerattr.md` - read the relevant part before changing a pass.
- `internal/semantic/` — search by meaning: one embedding per paragraph
  of 20+ characters, kept in `LECTABLE_DATA_DIR/embeddings.sqlite` (its
  own file - derived data, ~0.9 KB a paragraph, never in the library's
  backups; an index made with another model is emptied on open), and
  `Service.Search` over it. Vectors are stored a byte a dimension and
  scanned in memory per book (cached, 1 GB at most) - a series is tens of
  milliseconds, so there is no ANN index. A chapter is embedded by a
  `jobs.KindEmbedding` task (`Service.EmbedChapter`, the "embedding"
  preprocessing phase, which characterization waits for); what is stale
  is decided by comparing `chapter_text.content_rev` (migration 00027:
  `rev` for a chapter's text alone, untouched by a speaker change), not a
  `store.Passes` flag, and only paragraphs whose text changed are embedded
  again. A book list's `preprocessed` also needs every chapter embedded
  (`Service.EmbeddedBooks`). `POST /api/library/embed` embeds a library
  preprocessed before the phase existed. Readers of
  it: casting's lookups (`httpapi.semanticExcerpts`) and
  `GET /api/books/{id}/search?mode=meaning`. Off, with everything doing
  without, when the model file (`LECTABLE_EMBED_MODEL_PATH`, default
  `~/llm-models/jina-embeddings-v5-text-nano-retrieval-Q8_0.gguf`) is
  missing. Why this model: `model.go`.
- `internal/jobs/` — one queue for every chapter/paragraph/character-scoped
  background job, generalized over a `Kind` rather than one mechanism per
  feature, running on `internal/sched`. The queue is in memory: queued
  work is lost across a restart. **Pause** (`POST /api/jobs/pause`,
  `?drain=1` to wait) stops new dispatch, lets in-flight work finish, then
  unloads resident models; explicit previews still run. Job kinds,
  enqueueing, ordering, retries, the reader rules and timing logs are in
  `docs/jobs.md`.
- `internal/sched/` — the job scheduler `internal/jobs` runs on: one graph
  of tasks, each wanted by named requesters and by the tasks that need it;
  effective priority derived from that, a task nobody wants cancelled, one
  total order for dispatch and display, one admission rule (one context at
  a time, stay on a tie, drain then switch for more urgent work, a dwell
  after each switch). No dependencies beyond the standard library. See
  `../docs/scheduler-design.md`.
- `internal/cron/` — the server's periodic background tasks: a
  `Scheduler` of named `Task`s, each a function run on a fixed interval
  (never overlapping itself, errors and panics logged, nothing persisted -
  a task works out what is due from stored state). `cmd/server` registers
  them; so far `idle-recaps` (`httpapi.Server.PrepareIdleRecaps`). New
  "check every so often" work goes here rather than in its own ticker
  loop; `dbbackup.Run` and `audiomaint.Run` predate it and still run their
  own.
- `internal/dbbackup/` — rolling copies of `library.sqlite` in
  `LECTABLE_DATA_DIR/backups/library-<YYYYMMDDTHHMMSS>.sqlite`. `Run`
  (started from `cmd/server`) checks at startup and hourly, takes one when
  the newest is at least `LECTABLE_BACKUP_INTERVAL` old, then prunes to the
  newest `LECTABLE_BACKUP_KEEP`. The copy itself is
  `store.SQLiteStore.Backup`: SQLite's online backup API on a pooled read
  connection (nothing else waits), page for page so the search index stays
  consistent with its table, written via a temp name. A backup is a plain
  SQLite file: to restore, stop the server, delete `library.sqlite-wal` and
  `library.sqlite-shm`, and copy the backup over `library.sqlite`.
  A backup older than the newest migration is brought up to date the
  next time the server opens it. `Path` names a backup; the copy `store`
  takes before a migration uses it, so it is listed and pruned like any
  other.
- `internal/deliverytags/` — dependency-free leaf package for Higgs inline
  `<|category:value|>` tags: `Insertion` (a tag at a byte offset, the type
  `internal/pronounce.Apply` composes with substitutions), the Go pause
  pass (`PauseInsertions` - see "Emotions" in `docs/speakerattr.md`), and
  `ExtractInsertions`/`Merge` (diffing an annotated copy of a text against
  the original, rejecting any changed word - used by one-off cmd tools
  over the legacy `tts_tags` column).
- `internal/emotions/` — leaf package holding the fixed emotion set
  (`All`: id, label, LLM description, BreezeTTS instruction, variant
  reference line) shared by `speakerattr`, `store`, `voicerefs`, `jobs`
  and `httpapi` - see "Emotions" in `docs/speakerattr.md`. Its ids/labels reach the clients
  through `cmd/apigen`'s catalog (`EMOTIONS`); the frontend only adds an
  icon per id (`frontend/src/utils/emotions.ts`, keyed by the generated
  union).
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
  `formatting.go` (`FormattingSubstitutions`) strips formatting
  characters a TTS model would try to say - list bullets, box/shape/
  dingbat glyphs, and `* _ | = # ~ ·` runs standing alone between spaces -
  computed at generation time like pauses, never stored; a run between
  two words leaves a comma ("HP 10 • MP 5").
  `lexicon.go` is the listener-authored half: `LexiconEntry` ("say Nyx as
  Nicks", optional case match) becomes whole-word `Substitution`s
  (`LexiconSubstitutions`, longest term first) that override any
  overlapping LLM fix (`MergeLexicon`); a `Prefix` rule also covers words
  starting with the term ("gaol" -> "gaoler"), and case-insensitive rules
  carry the original's capitalization onto the replacement. Rules live in
  the store's `pronunciation_lexicon` table, scoped per series like
  characters (`store.SeriesScope`) or to every book (`GlobalLexiconScope`;
  `store.BookLexicon` = series rules, then global); `ResolveGenerationText(cloneModel, lexicon)` and
  the chapter DTO's `pronunciationMarks` both go through
  `Paragraph.EffectivePronunciation`, and `jobs.Manager.lexiconFor`
  supplies the rules at generation time (and records them on the task, so a
  rule edited mid-render discards that clip). Editing a rule
  (`PUT/DELETE /api/books/{id}/lexicon`, `httpapi/lexicon.go`) resets audio
  for every paragraph in the series the old or new rule matches; the
  `lexicon` live topic lists rules with per-book match counts.
- `internal/audiopath/` — computes where generated audio lives from IDs
  alone (`data/audio/<bookID>/<chapterID>/<voiceID>/<idx>.opus` - or, for
  narration on a shared clip, from its row's key, see `internal/clipkey` - plus
  `sfx/`/`music/` beside the voice dirs and a per-book `ambience/`); never
  stored in the DB, just derived. Served clips (narration, SFX, music
  regions) are **Ogg Opus**, written only through `WriteClip` (encode +
  atomic rename); a clip from before the switch may still exist only as its
  `.wav` sibling until `internal/audiomaint` converts it, so reads go
  through `Resolve` and deletes through `RemoveClip`, and the handlers
  serve through `httpapi.serveClip` (sets `audio/ogg; codecs=opus`).
  Voice refs stay WAV. Ambience loops are kept as their latents alone
  (`ambience/<hash>.lat`, rebuilt by `jobs.ambienceLoop` when a region in
  that place renders; `audiomaint.DropRebuildableAmbienceLoops` removes a
  WAV that has latents beside it - only loops from before latents are
  still WAV), as is each music region's seed (only the music layer's last
  10s - all `musicgen.GenerateRegion` ever reads of one).
- `internal/clipkey/` + `jobs/sharedclips.go` — **shared narration
  clips**. Every narration clip is stored once, library-wide, under a
  content key (`clipkey.Recipe`: clone model, reference clip identity,
  language, clone instruct, the effective spoken text, the plain text it's
  aligned to) at `LECTABLE_DATA_DIR/clips/<kk>/<key>.opus` (+ `.lat`), and
  the `paragraph_audio` row of every paragraph it plays for names the key
  (`clip_key`); `audiopath.NarrationClipFile` is how anything finds a
  paragraph's clip. Nothing is written under `audio/.../<voice>/` any more
  (only as a fallback when the store can't be written); the sweep removes
  leftover files and empty voice directories there.
  - **Keys.** Clones key on the preset's reference clip + render version
    (`voicerefs.RefIdentity`, `.ver` files stamped on a re-render, not on
    loudness fixes) and emotion variant; preset-less voices on their
    instruct/seed/design model (`designRecipe`); scare-quote merge groups
    on the whole group's spoken text (members stay pointer rows into the
    anchor's clip, their parts also kept on the anchor row as
    `clip_parts`); unvoiced lines on spelling. The key carries a **take**
    (`clipkey.TakeAt`): identical lines within `RunWindow` (5) paragraphs
    of each other count 0, 1, 2..., further apart they share take 0.
  - **Reuse.** A paragraph whose clip exists is put on it instead of
    rendered: at enqueue when it waits on nothing (`resolveAtEnqueue` - no
    task is queued at all), else when its task runs (`claimSharedClip`,
    which also waits out another task rendering the same key). A clip
    outlives its last row: a trigger saves what that row knew to
    `clip_orphans` and the clip is still found for
    `clipkey.OrphanRetention` (7 days), so a voice switched back or a
    chapter reimported unchanged isn't rendered twice. Explicit "clear
    generation" and deleting a book call `Manager.DiscardOrphanedClips`,
    so those do re-render. `audiomaint.SweepSharedClips` drops clips no row
    names once past retention.
  - **Regenerate this paragraph** (`task.fresh`) re-renders the clip for
    every paragraph on it. A push that arrives while its paragraph's task
    is already running is looked at again when that task ends
    (`runAgain`), so it isn't dropped with it.
  - **Writes.** `SetParagraphReadyClip` sets status, key, timings and parts
    in one statement - a row is never ready without its clip. Word timings
    saved for one row reach every row on its clip.
  - **Old libraries.** `AdoptSharedClips` (once, marker `clips/.adopted`)
    keys pre-existing clips from their paragraph's current state and moves
    them in; `AdoptStaleClips` (every startup) drops clips still at their
    own path under a voice their paragraph no longer uses (their recipe
    isn't known) and retries any under the current voice;
    `DropUnsharedClips` (once, `clips/.unshared-dropped`) deletes the ones
    an earlier version kept under private keys instead;
    `Store.BackfillClipParts` fills `clip_parts` for older groups.
  - Exports carry shared clips under `clips/`; import keeps one the
    library already has. Clients play a shared clip from
    `/api/clips/{key}` (see `docs/api.md`).
- `internal/oggopus/` — pure-Go WAV -> Ogg Opus encoder (`EncodeWAV`,
  `Duration`): 48 kbps mono / 64 kbps stereo VBR (128 before 2026-10-06; older clips keep it), 44.1kHz (Stable Audio)
  resampled to 48kHz, 24kHz narration encoded natively, pre-skip and end
  trimming so decoded length matches the WAV exactly, ~1 page per second.
  The encoder is `github.com/kazzmir/opus-go` (transpiled libopus 1.6.1,
  no cgo), v1.6.0 or later - the first upstream release carrying the
  fixes this app used to take from a local fork. Two are load-bearing
  here, and both corrupted audio silently:
  (1) v1.4.0's libc shim zero-extends signed->unsigned casts, which
  garbles every SILK/hybrid frame - libopus's normal mode for speech
  (measured: SNR about -19 dB on real narration; `TestSpeechSurvivesSILK`
  fails against v1.4.0); (2) `Encode` hands the transpiled code
  `uintptr`s into caller buffers that may sit on a goroutine stack that
  moves mid-call (all-zero packets; `Encode` also pins its buffers as a
  second line of defence, and `TestDeterministic` guards it). With both
  fixed, the output is byte-identical to a plain-C build of libopus 1.6.1.
  **Never downgrade below v1.6.0** - both tests above fail on older
  versions.
- `internal/audiomaint/` — background housekeeping started by
  `cmd/server` (`Run`): sweeps orphaned audio, converts pre-Opus WAV clips
  (`MigrateLegacyWAV`, NumCPU/4 workers, idempotent - a restart just
  resumes), then sweeps again daily. A converted clip keeps its WAV's
  mtime, since the mtime is the `?v=` in audio URLs and part of Android's
  offline-sync `audioHash`: changing it would re-download every offline
  chapter. The .opus is `os.Link`ed into place (never overwriting a fresh
  render) and withdrawn if the WAV was deleted mid-conversion.
  `SweepOrphans` removes, per `Store.AudioRefs`, only entries older than an
  hour: deleted books/chapters, voice dirs with no `paragraph_audio` row
  (rows exist from "generating" on, before any file), music files of
  regions that no longer exist, ambience loops no region's prompt maps to,
  and stale `.tmp-` files. Names it doesn't know (e.g. a leftover `foley/`)
  are left alone.
- `internal/bookexport/` — book exports and imports. `Source.Collect`
  reads a book into a format-neutral `Book` (per chapter: display content
  plus a playback timeline of `Segment`s - a `[Begin, End)` range of a
  narration `Clip` with its word timings, under each paragraph's
  *currently resolved* voice via `narration.BookVoices`, so clips cached
  under other voices stay behind; a scare-quote merge group's members are
  consecutive ranges of the anchor's clip). `WriteEPUB` lays that out as an
  EPUB 3 with Media Overlays (generated XHTML with a `<span id="p-<paragraph
  id>">` per row, one SMIL per chapter, clips copied as-is since Ogg Opus is
  an EPUB 3.3 core type, legacy WAV clips transcoded, mtimes kept);
  `Options.WordLevel` adds per-word (or, with `PhraseSeconds`, per-phrase)
  overlay entries where the alignment matches the text word for word.
  Phrases exist because browser-engine readers (Thorium) see the audio
  clock only ~every 250ms: a unit shorter than 250ms x playback rate drifts,
  so the Export page's "synced up to N x" picks `PhraseSeconds = 0.3 x N`.
  The timeline is meant to feed an Opus .m4b writer too - `manager.go`'s
  `write` is the one place a format plugs in. A **full-book** export also
  carries lectable's own data under `META-INF/lectable/` (readers ignore
  it): `manifest.json` (maps every archive entry to its LECTABLE_DATA_DIR path + mtime),
  `db/<table>.jsonl` (`store.ExportBookRows` - one JSON object per row,
  paragraph_audio trimmed to the exported clips), and `data/...` (source
  epub, voice refs + emotion variants for every preset in play, SFX, music
  clips/seeds, ambience loops). `Source.Import` restores that into another
  library under the original ids (`store.ImportBookRows`: `read_json` into
  the intersection of export/current columns; shared rows - presets, series
  characters - kept if present, characters matched by (scope, name) and
  `character_voices` remapped), then files (only under the book's own dirs
  or `voice-refs/`, which is never overwritten); `409 book_exists` if the
  book is already there. Uploading a full export through `POST /api/books`
  imports it; a chapter-subset export is imported as a plain new book.
  `Manager` keeps built exports at `LECTABLE_DATA_DIR/exports/<book>/<id>.epub` +
  `<id>.json` (id = `Options.ID()`, so rebuilding the same selection
  replaces it) and tracks requested ones; staleness is computed on read
  from `store.ExportFingerprints` (SyncFingerprints widened to SFX, music
  regions, bookmarks and word timings), never invalidated on write.
  Measured on Spire's Spite (244 chapters, 73h): collect 12s, write 60s,
  16GB - of which narration is 1.5GB and background music ~14GB (ambience
  WAV loops 5.5GB, music seed WAVs 4.7GB, music clips 4GB); import 31s.
  `Options.ExcludeMusic` (full exports only) leaves those three out and
  exports the music regions back as `pending` (scoring kept), so an
  importing library re-renders the music instead of shipping it.
  `Options.Latents` is the **latent-only transfer** mode, for moving books
  between Lectable instances.
  - The EPUB part is text-only.
  - Each narration clip travels as its `.lat` sidecar where it has one,
    else as its audio; ambience loops likewise (their `.lat` beside the
    WAV); seeds as `.seed.lat`.
  - Always as-is: an in-progress book moves with whatever it has, and the
    rest stays pending.
  - The receiving library decodes a clip the first time it's served
    (`httpapi.serveMaterializedClip`, via the worker's `/codec-decode`) and
    rebuilds a loop the first time a region needs it (`jobs.ambienceLoop`).
  - With `Chapters` set, it imports as a standalone book of just those
    chapters (`collector.standalone`, `standalone.go`):
    - rows filtered to them;
    - every book-local id replaced with one derived (sha256) from the
      source id and the chapter set, so it can sit beside a full copy of
      the same book, and re-importing is refused like any existing book;
    - chapters renumbered from 0;
    - titled "Title (chapters a–b)";
    - reading position reset;
    - data paths rewritten to the new ids;
    - source epub dropped.
  - `Manifest.Latents` marks it, and import accepts a partial export only
    when it's set.
- `internal/wav/` — a minimal RIFF/WAVE codec: `Duration` (header-only, for
  reporting chunk length to the frontend) plus `Decode`/`Encode` (full
  16-bit PCM in-memory codec, shared by `internal/voicerefs` and
  `internal/audioworker` for WSOLA re-speed and worker request/response
  bodies).
- `internal/httpapi/` — handlers, using Go 1.22+'s stdlib `http.ServeMux`
  method+path-parameter routing (`"GET /api/books/{id}"`). No router
  dependency. **This package is the API contract**: `cmd/apigen` reads it
  and generates both clients' copies - the frontend's
  `src/api/generated.ts` and Android's `dto/ApiTypes.kt`, `LectableApi.kt`
  (the Retrofit interface), and `LiveTopics.kt` - plus the MCP server's
  route table (`cmd/lectable-mcp/routes_gen.go`). It derives:
  - **types** from every json-tagged struct (doc comments included) - so
    every request and response body must be a named struct, never a
    `map[string]any` or function-local struct (small shared ones live in
    `responses.go`);
  - **enums** from named string types with constants (`jobTier`,
    `bulkAction`, `audioStatus`, `store.CharacterVoiceMode`, `errorCode`,
    ...) - a TS union plus const array, a Kotlin typealias plus constants
    object;
  - **the catalog** (`voices.CloneModels`/`DesignModels`, `emotions.All`,
    a few default constants - `cmd/apigen/catalog.go`);
  - **live topics** from `registerLiveTopics` (the topic wrappers are
    generic over their value type; each wrapper's `apigen:topic-params`
    doc line names its params);
  - **routes** from every `mux.HandleFunc`, by reading the handler (and
    helpers it passes `w`/`r` to): path params (`strconv.Atoi(r.PathValue(
    ...))` makes one an int, `T(r.PathValue(...))` makes it enum `T`),
    `r.URL.Query().Get` params, the `json.NewDecoder(r.Body).Decode`
    target or a multipart `FormFile`, the response (`writeJSON`/
    `writeBuilt` type, `writeNoContent`, or a file via `http.ServeFile`/
    `w.Write`), and `writeErrorCode` codes. The generator fails on a
    handler that answers with more than one JSON type, or mixes JSON with
    204/binary.

  Response conventions (`responses.go`): a GET returns its resource;
  enqueueing background work answers `202 queuedResponse{Queued: n}`
  (a count, 1 for a single item); a create answers 201; a PUT replacing a
  whole resource may return it; a mutation whose result is a count or
  computed value returns that; every other mutation answers 204
  (`writeNoContent`) - clients see the effect through live topics. Errors
  are always `errorResponse` (`writeError`, or `writeErrorCode` with an
  `errorCode` constant for ones a client recovers from).
  After changing any of this, run `make generate`; `cmd/apigen`'s
  `TestGeneratedUpToDate` fails while the output is stale. Struct tags
  `tstype:"..."`/`kttype:"..."` override one field's client type, a
  type's `apigen:ts-hand` doc line leaves it to hand-written TS, and a
  handler's `apigen:skip` isn't a route (the WebSocket) - see the
  generator's own doc comment.
- `internal/live/` — the live-state push behind `GET /api/events` (the
  frontend's only source of server state - see `../frontend/CLAUDE.md`).
  Clients subscribe to *topics* (`{"type":"subscribe","id","topic",
  "params"}`; topics and params listed in `httpapi.registerLiveTopics`,
  each built by the same `build*` function as its REST GET handler) and
  get a `snapshot`, then `patch`es (`live.Op`: `set`/`del`, plus `arr`,
  which rebuilds an array from index runs of the previous one - id-keyed
  when elements have a unique `"id"` - so a job queue shifting by one
  costs a few bytes). Change detection: `store.Store.OnChange` wraps the
  store's single `*sql.DB` and reports the table of every committed write
  (`db:<table>`; `UpdatePosition` reports the pseudo-table
  `books.position` so 3s position saves don't rebuild everything that
  reads `books`), `jobs.Manager.SubscribeChanges` feeds `jobs`, and a 30s
  resync catches anything else. One refresh loop (100ms debounce) rebuilds
  only topics that someone is subscribed to *and* whose declared deps were
  invalidated, compares marshaled bytes with the last value, and fans out a
  patch only on a real change - computed once per topic regardless of
  subscriber count. Whole-book topics (`books`, `speakers`, appearances)
  carry a `MinInterval` so steady generation doesn't keep rebuilding them.
  `books` depends on `DepPipelines` rather than `jobs`: it reads only
  whether a book's preprocessing run or source check is going, so
  `httpapi.WatchPipelines` invalidates it when that set changes
  (`jobs.Manager.PipelineSignature`), not on every task. A subscriber whose outbound buffer fills is dropped
  (it reconnects and resnapshots) rather than risk a lost patch. **A new
  topic, or a builder reading a new table, must list that table in its
  deps** or it'll only update on the 30s resync. Every client (web, Android, `cmd/jobswatch` - which applies
  patches with `live.Apply`) uses only this; the older per-book and
  jobs-only WebSocket routes are gone.
- `internal/mdnsadvert/` — advertises the backend on the LAN via mDNS/DNS-SD
  (`_lectable._tcp`, using `github.com/hashicorp/mdns`) so `android` can
  find it instead of requiring the user to type in an IP. Started from
  `cmd/server/main.go` alongside the HTTP server and shut down (sending an
  mDNS goodbye packet) on the same signal-triggered shutdown path. Pure
  advertisement — the backend never browses for anything itself.

## Metrics

`GET /metrics` on the main server (`:8080`) serves Prometheus metrics
(`client_golang`, default registry - so also the server's own `go_*`/
`process_*`). All are recorded in `cmd/server`, never inside the worker:

- `lectable_worker_request_duration_seconds{endpoint,outcome}` (histogram),
  `lectable_worker_requests_in_flight`, `lectable_worker_restarts_total
  {reason=process_died|rss_limit|stuck|manual}` - `internal/ttsworker`.
  `outcome` is `ok`, `canceled`, `decode_budget` (Higgs never reached EOC -
  the "before EOC for this text chunk" error), or `error`.
- `lectable_ttsworker_process_*` - the worker process's CPU seconds, RSS,
  fds, via a process collector pointed at its current pid.
- `lectable_tts_generate_duration_seconds{clone_model,outcome}` and
  `lectable_tts_generated_audio_seconds_total{clone_model}` - per-call
  real-time factor is `rate(..._duration_seconds_sum{outcome="ok"}) /
  rate(..._audio_seconds_total)` (concurrent calls overlap, so throughput
  is audio seconds per wall second instead).
- `lectable_tts_completeness_retries_total{reason=missing|extra|both}`,
  `lectable_tts_completeness_results_total{result=clean|fixed|unfixed|
  unchecked}` - `jobs.generateCloneChecked`.
- `lectable_jobs_task_run_seconds{kind,outcome}`,
  `lectable_jobs_task_wait_seconds{kind,tier}` (histograms, from
  `logTaskTiming`), and `lectable_jobs_queued`/`lectable_jobs_in_flight
  {kind,tier}`/`lectable_jobs_paused`, read from `Manager.Snapshot` at
  scrape time.
- `lectable_http_request_duration_seconds{route,method,code}` - `route` is
  the matched ServeMux pattern (`GET /api/books/{id}`), never a raw path.
- `lectable_db_call_duration_seconds{op}` - every database call (`begin_wait` is time spent waiting for the
  write connection).

Useful queries: worker CPU cores
`rate(lectable_ttsworker_process_cpu_seconds_total[5m])`; generation
failure ratio by model `sum by (clone_model, outcome)
(rate(lectable_tts_generate_duration_seconds_count[1h]))`; p90 generation
latency `histogram_quantile(0.9, sum by (le, clone_model)
(rate(lectable_tts_generate_duration_seconds_bucket[15m])))`.

## Toolchain note

This module's `go.mod` needs a newer Go than the box's default
`/usr/bin/go` (1.22.2) provides (`go 1.26`). A Go 1.27.1 toolchain is installed locally at
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

`../env.sh` is the one place all of the above is set - the toolchain
`PATH`/`GOTOOLCHAIN` override *and* both native libraries'
`CGO_LDFLAGS`/`LD_LIBRARY_PATH` (defaulting to
`~/audio.cpp/build-rocm7/bin` and `~/llama.cpp-latest/build-live/bin`).
Both Makefiles load it into every recipe (as `BASH_ENV`), and `run.sh` and
`deploy.sh` source it, so day-to-day commands don't need any of these
exported by hand; override a location per invocation (`make build
AUDIOCPP_DIR=/some/other/path`) or by exporting it first. For a bare
`go build`/`go test`, `. ../env.sh` first. A path that moves is changed
there and nowhere else.

**GPU toolchain (since 2026-09-28): ROCm 7.2.4**, unpacked user-space in
`~/rocm7-user/root/opt/rocm-7.2.4` (`ROCM_DIR`; no system install - `/opt/rocm`
is still 6.4.4 and on `ld.so.conf`, and both share sonames like
`libhsa-runtime64.so.1`, so `ROCM_DIR/lib` must come first on
`LD_LIBRARY_PATH`). Under WSL it reaches the GPU through `librocdxg`, which
needs `HSA_ENABLE_DXG_DETECTION=1`; `../env.sh` sets both (the repo's `.envrc` loads that same file). llama.cpp is `~/llama.cpp-latest` (a worktree of
`llama-cpp-py-sync/vendor/llama.cpp`, no local patches) built in
`build-live` with `LLAMA_BUILD_COMMON=ON`; audio.cpp is the usual checkout,
local patches included, built in `build-rocm7`. The old ROCm 6.4.4 builds
(`~/audio.cpp/build`, `~/llama-cpp-py-sync/vendor/llama.cpp/build`) are
untouched, for rollback. Both libraries must use the same ROCm - they
share the ttsworker process, and two HIP runtimes in one process crashed it
("ROCm error"); `run.sh` exports `LECTABLE_AUDIOCPP_LIB_DIR` (which
`cmd/server` puts first on the worker's library path) from `AUDIOCPP_DIR`
for that reason. Measured with llama-bench on Qwen3.6-35B-A3B: ROCm 7 plus
the newer llama.cpp is ~5% faster reading prompts and ~15% faster
generating than 6.4.4. Day-to-day commands don't need any of this exported by hand:

```
make build   # go build -o ttsworker ./cmd/ttsworker && go build -o server ./cmd/server
make test    # go test ./...
make vet     # go vet ./...
make generate # regenerate the frontend/Android API clients (cmd/apigen)
make deploy   # rebuild both binaries + restart the running backend (deploy.sh)
make rollback # restart on the previous binaries (*.prev) instead
make tidy    # go mod tidy
make run     # builds then launches via run.sh - see "Run" below
```

## Run

To rebuild and restart a running backend, use `./deploy.sh` (`make
deploy`): it builds both binaries before stopping anything (a failed build
leaves the old server running), keeps the previous ones as `server.prev`/
`ttsworker.prev`, stops only the server running from this directory,
relaunches through `run.sh`, waits for `/api/instance`, and re-pauses the
job queue if it was paused - queued work itself is lost either way, since
the queue is in memory. `./deploy.sh --rollback` (`make rollback`)
restarts on the `.prev` binaries.

```
go build -o ttsworker ./cmd/ttsworker   # needs CGO_LDFLAGS/LD_LIBRARY_PATH (audio.cpp *and* llama.cpp now), see above
go run ./cmd/server                     # needs neither - see the hard invariant above
```

`run.sh` (this directory) wraps the above plus every env var below,
taking the toolchain locations from `../env.sh` (override any of them by exporting
first, e.g. `AUDIOCPP_DIR`/`LLAMACPP_DIR`/`LECTABLE_DATA_DIR`/
`LECTABLE_SPEAKER_LLM_MODEL_PATH`): `./run.sh --build` builds both binaries then
launches `server` backgrounded (logging to `/tmp/lectable-server.log` by
default, `$LECTABLE_LOG_FILE` to override) - `server` spawns `ttsworker` itself, as
always. Drop `--build` to relaunch without rebuilding, or add `--fg`/
`--foreground` to run in the foreground instead of backgrounding it. This
is the easiest way to get both binaries' env vars right at once - prefer
it (or `make run`, identical) over reconstructing `CGO_LDFLAGS`/
`LD_LIBRARY_PATH` by hand.

Env vars (all optional): `LECTABLE_PORT` (8080), `LECTABLE_DATA_DIR` (`./data`),
`LECTABLE_ALLOW_ORIGIN` (`http://localhost:5173`), `LECTABLE_TTS_WORKER_BIN` (`./ttsworker`),
`LECTABLE_TTS_WORKER_PORT` (8091), `LECTABLE_AUDIOCPP_LIB_DIR`
(`~/audio.cpp/build-rocm7/bin`; `run.sh` sets it from `AUDIOCPP_DIR`), `LECTABLE_AUDIOCPP_MODELS_DIR`
(`~/audio.cpp/models` - where every default audio.cpp GGUF path resolves), `LECTABLE_SPEAKER_LLM_MODEL_PATH`/
`LECTABLE_SPEAKER_LLM_GPU_LAYERS`/`LECTABLE_SPEAKER_LLM_CTX`/`LECTABLE_SPEAKER_LLM_MAX_CONCURRENT`/
`LECTABLE_SPEAKER_LLM_NO_THINK`/`LECTABLE_MODEL_IDLE_UNLOAD_AFTER`,
`LECTABLE_EMBED_MODEL_PATH`/`LECTABLE_EMBED_GPU`/`LECTABLE_EMBED_QUERY_THREADS`
(search by meaning - see `internal/semantic`) (speaker attribution - see
"ttsworker / audioworker / llmworker" and `internal/speakerattr` above;
attribution is disabled, not fatal, if the model file doesn't exist). `LECTABLE_BACKUP_INTERVAL`
(`24h`, a Go duration; `0` disables) and `LECTABLE_BACKUP_KEEP` (7) - see
`internal/dbbackup`. `LECTABLE_DB_SLOW_LOG_MS` (100) logs every DB call at or above that
many ms (`store: timing <method> <op> took ...`; `0` logs all, negative
disables) - with the single connection, that time includes waiting for
it, and `transaction held` lines show who held it. Every finished job
task logs `jobs: timing <kind> <key> ... waited=<queue+deps> ran=<run>`,
every lookahead request `jobs: timing lookahead ... took`, and every
worker call `ttsworker: timing <method> <path> took`.
Set these on whatever environment runs `./server` (or `go run ./cmd/
server`), not `./ttsworker` directly - `cmd/server`'s own `ttsworker.
Manager` spawns the worker with `os.Environ()` (its own environment) as
the child process's base, so any of these already set for `cmd/server`
flow through to `ttsworker` automatically; only `LD_LIBRARY_PATH` (and
`LECTABLE_DEFAULT_CLONE_MODEL`) get explicitly overridden on top of that.
Building `cmd/server` itself needs neither native library's shared
libraries on `CGO_LDFLAGS`/`LD_LIBRARY_PATH` any more - only `ttsworker`'s
build does, since both now link into that one binary.

## API surface

`internal/httpapi` is the source of truth: the route table, wire types and
enums are read from it by `cmd/apigen` (`make generate`) to write the
frontend and Android clients. The per-route notes - what each endpoint
does beyond its signature, including the `run-ambience`, `.../setting`,
`.../summarize` and manifest entries other docs point at - are in
`docs/api.md`. Add a route's note there when adding the route.

## Testing changes

`make test` (or `make check` from the repo root, which also lints) runs
every package's tests; most need no GPU, and the store's run against
temporary SQLite files, migrations included. To sanity-check end to end,
upload an epub and follow it through generate and audio fetch against a
running server (`./run.sh`) - `server` spawns `ttsworker` itself.
