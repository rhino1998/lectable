# lectable

A self-hosted epub reader that narrates books aloud using audio.cpp's
Qwen3-TTS/Higgs Audio models. Independent components, each with its own
`CLAUDE.md`:

- `backend/` (Go) — owns the library (epub parsing, DuckDB storage,
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
  `backend/CLAUDE.md`'s "ttsworker / audioworker / llmworker" section) and
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
- `frontend/` (React + TypeScript + TanStack) — the web UI. See
  `frontend/CLAUDE.md`.
- `android/` (Kotlin + Jetpack Compose) — the Android client. Builds and
  unit-tests clean; running on-device/emulator not yet verified. See
  `android/CLAUDE.md`.

## How the pieces fit together

```
frontend (Vite dev server, :5173)      android (Kotlin/Compose client)
   |  fetch /api/...                      |  http://<lan-ip>:8080/api/...
   v                                      v
backend/cmd/server (Go, :8080) <---------+
   |  parses epubs into books -> chapters -> paragraphs (DuckDB)
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
into paragraph audio - PocketTTS by default, or Higgs, BreezeTTS, etc.) is
also a per-book setting, never a property of a voice: a voice preset is
just a reference-clip recipe. New books start with the default clone model
set on the Voices page.

Dialogue lines can also carry an **emotion** (a small fixed set -
`backend/internal/emotions`: warm, excited, sad, angry, afraid, cold,
whisper, shout, weary), labeled by an LLM pass or overridden per line in
the reader. An emotion isn't a TTS control token: each voice lazily gets
one reference-clip *variant* per emotion its lines actually use (rendered
by BreezeTTS instructed cloning of the base clip), and an emotional line
clones from that variant instead - so it works under every clone model.
The only Higgs inline tag still used is `<|prosody:pause|>`, inserted by a
plain Go pass before ellipses/em dashes. See `backend/CLAUDE.md`'s
"Emotions" section.

## Running everything locally

1. `backend`: from `backend/`, build both binaries once
   (`go build -o ttsworker ./cmd/ttsworker && go build -o server ./cmd/server`,
   with `CGO_LDFLAGS`/`LD_LIBRARY_PATH` pointed at local audio.cpp *and*
   llama.cpp builds - see `audiocpp-go/README.md`/`llamacpp-go/README.md`.
   Only the `ttsworker` build actually needs either linked; `cmd/server`
   needs neither, so those env vars are harmless-but-unused for its own
   build), then run `./server` (it spawns `./ttsworker` itself - see
   `backend/CLAUDE.md` for env vars). `go run ./cmd/server` also works as
   long as `./ttsworker` has been built into `backend/` first.
2. `frontend`: `npm run dev` (from `frontend/`)
3. `android`: optional - `./gradlew assembleDebug` (from `android/`), needs
   `ANDROID_SDK_ROOT` set to a Linux-native Android SDK (see
   `android/CLAUDE.md` for the one installed at
   `/home/rhino/android-sdk-toolchains/` in this dev environment - building
   works here, but running needs an emulator/device elsewhere, since this
   box can't accelerate one). Point the app at the backend's address from
   its Settings screen; an emulator can reach a host-machine backend at the
   default `http://10.0.2.2:8080/`, a physical device needs the host's LAN IP.

Default ports: backend `8080`, ttsworker `8091` (loopback only, not meant
to be reached directly), frontend `5173`. The backend's `ALLOW_ORIGIN` env
var (default `http://localhost:5173`) must match wherever the frontend dev
server actually runs. (`android` doesn't go through CORS - it's a native
client, not a browser.)

## Conventions

- Lint with `make lint` from the repo root (or `lint-go` / `lint-frontend`
  / `lint-android`): golangci-lint v2 for all three Go modules via the
  shared root `.golangci.yml`, oxlint for `frontend`, AGP's built-in lint
  for `android`. All three should stay at zero errors.

- No auth, no multi-user support — this is a single-user local app.
- All persistent state lives under the backend's `DATA_DIR` (default
  `./data`): `library.duckdb`,
  `audio/<bookID>/<chapterID>/<voiceID>/<idx>.opus` (served clips - narration,
  plus `sfx/` and `music/` beside the voice dirs - are Ogg Opus; model inputs
  like `music/<regionID>.seed.wav` and `audio/<bookID>/ambience/*.wav` stay
  WAV; see `backend/CLAUDE.md`'s `internal/audiomaint`),
  `covers/<bookID>.<ext>`, `epubs/<bookID>.epub` (the source epub, kept for
  single-chapter re-import), `voice-refs/<presetID>.wav`,
  `voice-refs/variants/<presetID>/<emotion>.wav`. Nothing is stored
  in `ttsworker` or `frontend` - the worker is intentionally stateless
  aside from which model checkpoints happen to be loaded in VRAM.
