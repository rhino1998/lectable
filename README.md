# lectable

A self-hosted epub reader that narrates books aloud using
[audio.cpp](https://github.com/0xShug0/audio.cpp)'s Qwen3-TTS/Higgs Audio
models. Upload an epub, pick a voice, and it generates narration
paragraph-by-paragraph in the background so you can start listening before
the rest of the chapter finishes.

Single-user, local-first: no auth, no accounts, everything runs on your own
machine.

## Components

- **`backend/`** (Go) — the API server. Owns the library (epub parsing,
  DuckDB storage, background TTS generation, audio/cover serving) and TTS/LLM
  orchestration (voice presets, reference-clip rendering, speed adjustment,
  speaker-attribution prompting).
- **`backend/cmd/ttsworker`** — a separate Go binary, spawned and
  watchdog-managed by the backend, that's the only process linking against
  `libaudiocpp.so`/`libllama.so`. Isolated into its own disposable process
  to contain a known native memory leak in audio.cpp.
- **`audiocpp-go/`** (Go, cgo) — Go bindings for audio.cpp.
- **`llamacpp-go/`** (Go, cgo) — Go bindings for llama.cpp, used for
  speaker-attribution LLM calls.
- **`frontend/`** (React + TypeScript + TanStack) — the web UI.
- **`android/`** (Kotlin + Jetpack Compose) — the Android client.

Each component has its own `CLAUDE.md` with implementation details. See the
top-level [`CLAUDE.md`](./CLAUDE.md) for how the pieces fit together and full
setup/run instructions.

## Quick start

1. **Backend**: from `backend/`, build both binaries
   (`go build -o ttsworker ./cmd/ttsworker && go build -o server ./cmd/server`),
   then run `./server` — it spawns `./ttsworker` itself.
2. **Frontend**: `npm run dev` (from `frontend/`), served at `:5173` and
   proxied to the backend at `:8080`.
3. **Android** (optional): `./gradlew assembleDebug` (from `android/`), then
   point the app at the backend's LAN address from its Settings screen.

See [`CLAUDE.md`](./CLAUDE.md) for full build prerequisites (audio.cpp /
llama.cpp linking) and default ports.

## License

No license specified yet.
