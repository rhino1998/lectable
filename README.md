# Lectable

A self-hosted epub reader that narrates books aloud using
[audio.cpp](https://github.com/0xShug0/audio.cpp)'s Qwen3-TTS/Higgs Audio
models. Upload an epub, pick a voice, and it generates narration
paragraph-by-paragraph in the background so you can start listening before
the rest of the chapter finishes.

Single-user, local-first: no auth, no accounts, everything runs on your own
machine.


https://github.com/user-attachments/assets/9c1ec9c6-6b5c-41b9-adc1-40ff6035b324


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

## Models

All models run locally as GGUF checkpoints (q8_0 unless noted) inside
`ttsworker`. audio.cpp models are loaded from `~/audio.cpp/models` by
default (override the directory with `LECTABLE_AUDIOCPP_MODELS_DIR`), and
the LLM from `~/llm-models`. Each model's path can also be overridden
individually with the env var shown.

| Model | Used for | Env var |
| --- | --- | --- |
| **Higgs Audio v3 TTS 4B** | Default narration model: clones each voice preset's reference clip to speak every paragraph. Also the only model that supports inline speech-direction tags (emotion/SFX/prosody). | `LECTABLE_AUDIOCPP_HIGGS_MODEL_PATH` |
| **Breeze TTS 2** | Default VoiceDesign engine: renders a new voice's reference clip from its natural-language description. Also a selectable narration model, and the only one that takes a style instruction at clone time. | `LECTABLE_AUDIOCPP_BREEZE_DESIGN_MODEL_PATH` / `LECTABLE_AUDIOCPP_BREEZE_MODEL_PATH` |
| **Qwen3-TTS 12Hz 1.7B VoiceDesign** | VoiceDesign for the built-in `velvet-narrator` preset (the default book voice). Selectable for other presets too. | `LECTABLE_AUDIOCPP_QWEN3_DESIGN_MODEL_PATH` |
| **PocketTTS (English, 100M)** | Fast throwaway samples used to estimate speech length (chars/sec). | `LECTABLE_AUDIOCPP_POCKET_MODEL_PATH` |
| **Qwen3-ForcedAligner 0.6B** | Word-level timestamps for generated audio, which drive word highlighting during playback. | `LECTABLE_ALIGNER_MODEL_PATH` |
| **Parakeet TDT 0.6B v3** | Transcribes each generated clip to catch extra words the TTS model added (repeats, leaked reference lines), which triggers a regeneration. | `LECTABLE_TRANSCRIBER_MODEL_PATH` |
| **Qwen3-4B-Instruct-2507** (Q4_K_M, via llama.cpp) | Speaker attribution, character voice descriptions, speech-direction tagging, pronunciation resolution, and music scoring prompts. Optional: those features are turned off if the model file isn't there. | `SPEAKER_LLM_MODEL_PATH` |
| **Stable Audio 3 Medium** | Background music for chapters. | `LECTABLE_AUDIOCPP_STABLE_AUDIO_MEDIUM_MODEL_PATH` |
| **Stable Audio 3 Small SFX** | Sound effects. | `LECTABLE_AUDIOCPP_STABLE_AUDIO_SFX_MODEL_PATH` |

The design engine can be switched between `breeze_tts`, `qwen3_tts`,
`omnivoice`, `fireredtts3`, `firered_audio`, `auk`, and `auk_flash` with
`LECTABLE_AUDIOCPP_DESIGN_ENGINE`. These optional narration models can
also be picked per voice preset: Qwen3-TTS 12Hz 0.6B Base, OmniVoice,
Soprano 1.1 80M, FireRedTTS3 Instruct (`LECTABLE_AUDIOCPP_FIREREDTTS3_MODEL_PATH`),
FireRedAudio (`LECTABLE_AUDIOCPP_FIRERED_AUDIO_MODEL_PATH`), and AuK /
AuK-Flash (`LECTABLE_AUDIOCPP_AUK_MODEL_DIR`, a directory - audio.cpp
currently runs AuK on CUDA only, not HIP). FireRedTTS3, FireRedAudio, and
AuK each clone *and* design voices from one checkpoint. The standalone SFX/music test page can also use
ACE-Step 1.5 Turbo and Stable Audio 3 Small Music.

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

MIT - see [`LICENSE`](LICENSE). A few vendored headers and one ported
algorithm keep their upstream licenses; see
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
