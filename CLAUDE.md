# lectable

A self-hosted epub reader that narrates books aloud using audio.cpp's
Qwen3-TTS/Higgs Audio models. Independent components, each with its own
`CLAUDE.md`:

- `backend/` (Go) — owns the library (epub parsing, SQLite storage,
  background TTS generation, audio/cover serving) **and** TTS/LLM
  orchestration (voice presets, reference-clip rendering/caching, speed
  adjustment, speaker-attribution prompting/batching). All native model
  inference (both TTS and the speaker-attribution LLM) actually runs in
  `backend/cmd/ttsworker`, not here - `cmd/server` itself links against
  neither `libaudiocpp.so` nor `libllama.so`. See `backend/CLAUDE.md`.
- `backend/cmd/ttsworker` — a small, separate Go binary (built from the
  same module) that's the *only* thing in this repo linking against
  `libaudiocpp.so` **or** `libllama.so`. Spawned, health-checked, and
  watchdog-restarted by `cmd/server` (see `backend/internal/ttsworker`) -
  isolated into its own disposable process because audio.cpp has a
  confirmed native memory leak, root-caused to a specific upstream bug (see
  `backend/docs/audiocpp-patches.md`) and
  patched in this box's local `/home/rhino/audio.cpp` checkout - uncommitted
  there and not upstreamed, deliberately, so treat any fresh/reset checkout
  of that repo as still carrying the leak unless the patch has been
  reapplied. The disposable-process/watchdog isolation stays in place as
  cheap insurance either way (killing and relaunching just this process
  contains it without ever taking down the always-up backend). The
  speaker-attribution LLM shares this process too,
  even though llama.cpp itself carries no such leak, so both native model
  families share one process's VRAM budget and can be idle-unloaded
  independently instead of each sitting permanently resident. See
  `backend/CLAUDE.md`'s "ttsworker / audioworker" section.
- `audiocpp-go/` (Go, cgo) — Go bindings for the separate
  [audio.cpp](https://github.com/0xShug0/audio.cpp) C++ project, imported by
  `backend/internal/audioworker` (used only by the `ttsworker` binary, never
  by `cmd/server` itself). See `audiocpp-go/CLAUDE.md`.
- `llamacpp-go/` (Go, cgo) — Go bindings for the separate
  [llama.cpp](https://github.com/ggml-org/llama.cpp) C++ project, embedding
  a GGUF text model (e.g. Qwen) directly in a Go binary. Imported by
  `backend/internal/llmworker` (used only by `ttsworker`, the same shape
  `audiocpp-go` already has) to run speaker attribution's LLM calls;
  includes a `Scheduler` primitive (multi-sequence batching) so several
  concurrent generations share one loaded model via one batched decode
  call per round instead of queueing behind each other or each paying for
  their own loaded copy. See `llamacpp-go/CLAUDE.md`.
- `lac/` (Go, pure) — **rejected experiment**, kept for reference only: a
  lossless 16-bit PCM codec, 3-11% smaller than FLAC -8 on this app's
  audio, which wasn't enough to justify a private, frozen format. Don't
  wire it into `backend`; see `lac/CLAUDE.md` and
  `lac/mastering-format-report.md`. (`lac/latent-storage-report.md`, in
  the same directory, is unaffected and still current.)
- `frontend/` (React + TypeScript + TanStack) — the web UI. See
  `frontend/CLAUDE.md`.
- `android/` (Kotlin + Jetpack Compose) — the Android client, in daily
  use on a physical device (installed over wireless ADB). See
  `android/CLAUDE.md`.

## How the pieces fit together

```
frontend (Vite dev server, :5173)      android (Kotlin/Compose client)
   |  fetch /api/...                      |  http://<lan-ip>:8080/api/...
   v                                      v
backend/cmd/server (Go, :8080) <---------+
   |  parses epubs into books -> chapters -> paragraphs (SQLite)
   |  serves cover images and generated audio files
   |  owns voice presets, reference-clip rendering/caching, WSOLA speed
   |  adjustment, and speaker-attribution prompting/batching - all
   |  in-process, but every native model call proxies to the worker below
   |  spawns + health-checks + watchdog-restarts the worker below
   v
backend/cmd/ttsworker (Go, loopback HTTP, default :8091)
   the only process that links libaudiocpp.so or libllama.so - loads
   audio.cpp TTS models (generation/VoiceDesign/forced-alignment) and a
   GGUF LLM (speaker attribution), sharing one process's VRAM budget; each
   model family idle-unloads independently and reloads lazily on next use.
   Holds no per-preset state of its own. Disposable: killed and relaunched
   (transparently, via internal/ttsworker's Manager) whenever its RSS
   crosses a threshold, containing audio.cpp's own confirmed native leak.
```

The `frontend` reaches the backend through Vite's dev-server proxy at a
relative `/api/*` path; `android` has no such proxy, so it needs the
backend's actual LAN address entered in-app (see `android/CLAUDE.md`).

The **paragraph** is the unit of everything: it's what gets sent to the TTS
worker in one call, what gets cached as one audio clip (Ogg Opus), and what the
frontend highlights while its audio plays. Chapters are generated
paragraph-by-paragraph in the background so playback of paragraph 0 can
start before the rest of the chapter finishes.

Voice is controlled by a natural-language instruction string (VoiceDesign,
not a fixed speaker id) — see the presets in `backend/internal/voices`. A
book's voice is a per-book setting; changing it invalidates that book's
cached audio, since a book should keep one consistent narrator throughout.
The **clone model** (which TTS family clones every voice's reference clip
into paragraph audio - Higgs by default, or PocketTTS, BreezeTTS, etc.) is
also a per-book setting, never a property of a voice: a voice preset is
just a reference-clip recipe. New books start with the default clone model
set on the Settings page.

Dialogue lines can also carry an **emotion** (a small fixed set -
`backend/internal/emotions`: warm, excited, teasing, sad, pleading,
angry, afraid, nervous, cold, whisper, shout, weary, pained), labeled by an LLM pass or overridden per line in
the reader. An emotion isn't a TTS control token: each voice lazily gets
one reference-clip *variant* per emotion its lines actually use (rendered
by BreezeTTS instructed cloning of the base clip), and an emotional line
clones from that variant instead - so it works under every clone model.
The only Higgs inline tag still used is `<|prosody:pause|>`, inserted by a
plain Go pass before ellipses/em dashes. See "Emotions" in
`backend/docs/speakerattr.md`.

A **play** (speaker labels before each speech, no quote marks) is
detected at import and parsed deterministically: each speech is stored as
dialogue already carrying its speaker (`paragraphs.source_speaker`, which
attribution takes as given and never asks about) plus an unspoken display
`label` ("THESEUS.", with any direction on how the line is delivered);
stage directions stay narration. See "Plays" under `internal/epub` in
`backend/CLAUDE.md`.

A **table** in the source book is read row by row: each row is one
paragraph (its cells joined with " | ", plus where each cell sits in the
grid), so a stat block is a handful of clips rather than one per cell.
The clients lay a table's paragraphs back out as a grid; narration reads a
row as sentences and speaks each cell's column heading before it
("Rarity: Rare. Durability: 72/100."), the heading row itself staying
silent. See "Tables" under `internal/epub` in `backend/CLAUDE.md`.

The **ambience pass** (formerly "music scoring") splits each chapter into
tone regions and gives each one a background-music/soundscape prompt and,
in its **lighting sub-pass**, a room-lighting cue (color, accent,
brightness, effect). The backend only stores cues; the clients drive
Philips Hue lights themselves over the bridge's local v1 API (HTTPS)
(`frontend/src/lighting`, android `playback/lighting`), configured
per device. See the `run-ambience` entry in `backend/docs/api.md`.

**Speaker positions** are a client-side playback setting, per device:
each character is heard from a seat to the left or right of a centred
narrator, by plain stereo panning or binaurally for headphones. Seats are
worked out from each chapter's own speakers and applied as the clip
plays - clips stay mono and the backend knows nothing of it
(`frontend/src/utils/spatialAudio.ts`, android `playback/SpatialAudio.kt`).

A book can also be **imported from a website** by link instead of an
epub - a Royal Road fiction, The Wandering Inn (one volume per book,
as a series), or a creator's Patreon: the backend fetches it chapter by chapter (the book is
readable from its first chapter while the rest arrive), remembers where
it came from, and "Check for new chapters" later appends whatever has
been published since without touching what's there. A chapter the site
lists but won't serve is read from the Internet Archive's last public
copy of it, or from an epub of that stretch the reader uploads; with
neither, the book stops there rather than skip it. A Wayback Machine link
imports a work as archived, and an archive.org item link imports the
item's epub. Books from websites are checked for new entries daily (a
library-wide setting); what a check finds waits for the reader to say
import or skip - authors post notices in the chapter list - and marks the
book in the library meanwhile. A book can track **several sources** -
its advance chapters on Patreon beside Royal Road - each checked in turn,
a chapter posted in more than one taken once; Patreon posts for patrons
only are listed (locked) even when the library isn't signed in to
Patreon, and readable once it is (Settings, a browser's session cookie) -
which also unlocks The Wandering Inn's own patrons-only chapters. See the `.../sources` and `settings/patreon` entries in
`backend/docs/api.md`. An import can be previewed first to choose chapters, and -
for a serial whose early chapters the site has taken down - widened with
the Internet Archive's copies of the delisted ones or an epub of them. See the `import-url` entry in `backend/docs/api.md`.

**Front and back matter** - a title page, the copyright page, the table
of contents, "About the Author", adverts for other books, a synopsis of
the last one - is marked per chapter by a **matter pass**: one LLM call
over the book's chapter list at import, applied at once. Playback steps
over a marked chapter, and background narration, the passes, the recap and
reading progress leave it out; a book nobody has opened starts at its
first story chapter. The reader can mark or unmark any chapter (the web's
chapter heading and Chapters tab, Android's chapter picker), which the
pass then leaves alone, and a marked chapter is still read to a reader who
opens it. Books from websites aren't checked. See the `.../matter` entry
in `backend/docs/api.md` and "Front and back matter" in
`backend/docs/speakerattr.md`.

A per-book **setting profile** (era, technology, place, accent, genre)
is written once per book by one LLM call over excerpts from across it, the first time a pass needs it, and shown to the ambience,
lighting and characterization prompts so each stops guessing the world
from its own short span. See the `.../setting` entry in `backend/docs/api.md`.

A character can have **age stages**: from a chosen chapter on, through
the rest of the series, their lines are voiced as about that age - the
same casting recipe redesigned from an instruction rewritten for it. A
book **time jump** ("N years pass before this chapter", or for the whole
book) makes them for everyone whose age group changes. Both are set by
hand on the Speakers tab; fact finding (below) proposes time jumps it
finds in the text, and anything that finds jumps or stages must propose
them for the reader to accept, not apply them. See "Age stages"
in `backend/docs/speakerattr.md`.

**World facts** are things the reader records about a story's world,
each filed under the word that brings it up - "kobold: a short-lived
kind", "dwarf: Scottish accent" - for one series or for every book (the
Settings page), a series' fact replacing the general one. Casting is shown
the facts a character's material mentions. A fact about a kind can also name a **voice effect** - the
Antinium's insect buzz, the Drowned Folk's underwater gurgle - put on
every line of a character of that kind after it is generated (signal
processing, `backend/internal/voicefx`), since no casting makes a voice
model produce it. A **fact-finding pass**, run
from a book's Facts tab, searches the series by meaning for how long each
kind of being lives and how its people speak, and proposes what it finds, each with the sentence it
rests on; accepting one recasts the characters of that kind. The same tab
lists how each kind's characters are cast by accent, so one accent can be
made the kind's and the odd ones out recast. The pass also proposes
**character notes** - single things the text states of one character,
their kind of being or their age at a chapter, each with its sentence -
which, once accepted, are shown to that character's casting beside its
other material. A fact can also say what sort of word it is filed under - a **title**,
**role**, **race**, **nationality** or **place** of the world
("Prognugator", "Baker", "Drake", "Pallassian", "Pallass"), with or
without fact text: the roster then never reads the word as someone's name,
and the pass proposes a class for the words character names share. A
speaker's own race and nationality can be set on the Speakers tab; their
casting is shown both, with the facts about them.
Any pass that
proposes facts must leave them for the reader to accept. See the `/api/facts` entry in
`backend/docs/api.md` and "World notes" in `backend/docs/speakerattr.md`.

A **roster review** looks over a series' characters for what many
chapters of attribution left wrong - one person under two names, an alias
that is someone else's name, a description never marked as a role - and
proposes each fix for the reader to accept or skip on the Speakers tab;
nothing is merged on its own. See the `roster-review` entry in
`backend/docs/api.md`.

A **voice check**, run from the Speakers tab, has an audio-language
model (Audio Flamingo, in `ttsworker`) listen to each character's voice -
their own clip and each age stage's, not the emotion variants - and lists
the ones that don't sound like their casting (a man cast and a woman
heard, a child heard as an adult). It is a rough judge and optional:
nothing is re-rendered unless the reader asks. See the `voice-check`
entry in `backend/docs/api.md`.

**Search by meaning**: preprocessing also embeds every paragraph (an
embedding model in `ttsworker`), so a passage can be found by describing
it - what casting's lookups use beside their exact-word search, and
`GET /api/books/{id}/search?mode=meaning`. See `internal/semantic` in
`backend/CLAUDE.md`.

A per-chapter **summary pass** writes each chapter's summary, scenes
(where, when, who's there) and cast (with what the text calls them);
attribution reads the cast and ambience/lighting the scenes. See
the `.../summarize` entry in `backend/docs/api.md`.

A **"previously on" recap** is written and spoken in the narrator's voice
for where a reader stopped (from the chapter summaries behind them and the
part of the current chapter they heard - never anything ahead), prepared
once its reader has been away as long as the offer threshold so it is
ready when they come back; the clients offer it after that long away (one
library-wide setting, default a day). A book started right
after the one before it in its series opens with a recap of that book
instead. See the `.../recap` entry in `backend/docs/api.md`.

## Running everything locally

1. `backend`: from `backend/`, `make build` builds both binaries and
   `./run.sh` starts `./server`, which spawns `./ttsworker` itself (`make
   run` does both; see `backend/CLAUDE.md` for env vars). Both take their
   build environment from `env.sh` at the repo root: `CGO_LDFLAGS`/
   `LD_LIBRARY_PATH` pointed at local audio.cpp *and* llama.cpp builds
   (see `audiocpp-go/README.md`/`llamacpp-go/README.md`; only `ttsworker`
   links either) and the `sqlite_fts5` build tag, without which `server`
   builds but fails at startup. For a bare `go build`/`go run
   ./cmd/server`, `. ./env.sh` first.
2. `frontend`: `npm run dev` (from `frontend/`)
3. `android`: optional - `./gradlew assembleDebug` (from `android/`), needs
   `ANDROID_SDK_ROOT` set to a Linux-native Android SDK (see
   `android/CLAUDE.md` for the one installed at
   `/home/rhino/android-sdk-toolchains/` in this dev environment - this
   box can't accelerate an emulator, so it runs on a physical device over
   wireless ADB; `make install-android` builds and installs it). Point the app at the backend's address from
   its Settings screen; an emulator can reach a host-machine backend at the
   default `http://10.0.2.2:8080/`, a physical device needs the host's LAN IP.

To redeploy after changes, `make deploy` from the repo root rebuilds and
restarts the backend (`backend/deploy.sh`) and then builds and installs the
Android app on the connected device (`android/install.sh`; `make
deploy-backend`/`install-android` run either half).

Default ports: backend `8080`, ttsworker `8091` (loopback only, not meant
to be reached directly), frontend `5173`. The backend's `LECTABLE_ALLOW_ORIGIN` env
var (default `http://localhost:5173`) must match wherever the frontend dev
server actually runs. (`android` doesn't go through CORS - it's a native
client, not a browser.)

## Looking at a running backend (MCP)

`.mcp.json` registers `backend/cmd/lectable-mcp` with Claude Code as the
`lectable` server. Prefer its tools to `curl`/`sqlite3` when inspecting or
driving the live library - `books`, `book`, `read`, `search`, `speakers`,
`jobs`, `routes`, `api_get`/`api_call`, `sql` (read-only), `logs` - since
the raw API answers are sized for the clients, not a context window. See
the `cmd/lectable-mcp` entry in `backend/CLAUDE.md`.

## Conventions

- Lint with `make lint` from the repo root (or `lint-go` / `lint-frontend`
  / `lint-android`): golangci-lint v2 for all three Go modules via the
  shared root `.golangci.yml`, oxlint + `prettier --check` for
  `frontend`, AGP's built-in lint for `android`. All three should stay at
  zero errors.

- `make check` from the repo root runs every linter and test suite (`make
  test` for just the tests, or `test-go` / `test-frontend` /
  `test-android`): `go test ./...` in every Go module (`env.sh` at the repo
  root points `CGO_LDFLAGS`/`LD_LIBRARY_PATH` at the local audio.cpp/llama.cpp
  builds and picks the Go toolchain; both Makefiles and `backend`'s
  `run.sh`/`deploy.sh` load it, so a path changes in that one file), Vitest for `frontend`, and
  `testDebugUnitTest` for `android`. Run it before committing.

- The API clients are generated, not hand-mirrored: `backend/internal/httpapi`
  is the source of truth for wire types, enums, the model catalog, live
  topics, and the route table, and `make -C backend generate` rewrites
  `frontend/src/api/generated.ts` plus Android's `ApiTypes.kt`,
  `LectableApi.kt`, and `LiveTopics.kt` (see `backend/CLAUDE.md`'s
  `internal/httpapi` entry for what it reads and the response conventions).

- Live-topic builds are rebuilt on every relevant write, so a slow one
  costs a core for as long as generation runs; `internal/live` logs
  any build over `LECTABLE_LIVE_SLOW_BUILD_MS` (default 500) as `live: slow build`.

- No auth, no multi-user support — this is a single-user local app.
- All persistent state lives under the backend's `LECTABLE_DATA_DIR` (default
  `./data`): `library.sqlite`,
  `audio/<bookID>/<chapterID>/<voiceID>/<idx>.opus` (served clips - narration,
  plus `sfx/` and `music/` beside the voice dirs - are Ogg Opus; model inputs
  like legacy `music/<regionID>.seed.wav` stay WAV, and ambience loops are
  kept as latents, `audio/<bookID>/ambience/*.lat`; see `backend/CLAUDE.md`'s `internal/audiomaint`),
  `clips/<kk>/<key>.opus` (narration clips by content key, shared by every
  paragraph reading the same line the same way and named by their
  `paragraph_audio` rows - nearly all narration lives here rather than
  under `audio/`; see `backend/CLAUDE.md`'s `internal/clipkey`),
  latents sidecars beside clips whose model has them
  (`<idx>.lat`: Higgs/BreezeTTS codes, PocketTTS latents, exact - see
  `backend/docs/latents.md`) and music seeds as
  `music/<regionID>.seed.lat` (Stable Audio latents; legacy `.seed.wav` is
  migrated in the background),
  `embeddings.sqlite` (one embedding per paragraph, for search by meaning -
  derived, rebuilt by preprocessing; see `backend/CLAUDE.md`'s
  `internal/semantic`),
  `backups/library-<timestamp>.sqlite` (rolling daily copies of the
  database, see `backend/CLAUDE.md`'s `internal/dbbackup`),
  `audio/<bookID>/recap.opus` (the book's spoken recap),
  `covers/<bookID>.<ext>`, `epubs/<bookID>.epub` (the source epub, kept for
  single-chapter re-import; for a book imported from a website, an epub
  written from the fetched chapters and added to as new ones arrive), `sources/<bookID>/<chapter>.json.gz` (for a
  book imported from a website, each chapter's pages as fetched, so it can
  be re-read by newer import code without fetching again), `exports/<bookID>/<exportID>.epub` + `.json`
  (built book exports - EPUB 3 with read-aloud; a whole-book one also
  restores the book into another library when uploaded there, see
  `backend/CLAUDE.md`'s `internal/bookexport`), `voice-refs/<presetID>.wav`,
  `voice-refs/variants/<presetID>/<emotion>.wav`,
  `voice-refs/cache/refcodes/<hash>.lat` (cached Higgs/BreezeTTS/PocketTTS reference
  encodings). Nothing is stored
  in `ttsworker` or `frontend` - the worker is intentionally stateless
  aside from which model checkpoints happen to be loaded in VRAM.
