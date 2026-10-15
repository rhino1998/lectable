# Latents in the backend

Moved out of `../CLAUDE.md`. How the backend stores and uses the model
latents/codes audio.cpp returns (see `audiocpp-patches.md` for the audio.cpp
side): narration sidecars, music seeds, the reference-encoding cache, and the
worker's wire format.

**Latents in the backend** (`internal/latents`: the `ttsproto.Latents`
wire type's sidecar format, `Slice`/`Concat`; `ttsworker.Audio{WAV,
Latents}`):
- **Narration dual-write.**
  - `jobs.generateClone*` return `ttsworker.Audio`, via
    `Manager.GenerateChunkedAudio`, which sends `returnLatents`.
  - `audiopath.WriteClipLatents` writes the `.opus`, then `<idx>.lat`
    beside it: Higgs codes bit-packed at 10 bits (`u10`, ~2 kbps),
    BreezeTTS codes at 11 bits (`u11`, ~2.2 kbps), PocketTTS latents as
    f32 (~12.8 kbps). All are exact, so decoding reproduces the pre-Opus
    WAV. Every clone model has latents now (`audioworker.familyHasLatents`;
    codec engines `higgs_codec`/`breeze_codec`/`pocket_codec`).
    - zstd/xz were measured and don't beat plain packing on codes (or help
      f32 latents).
    - Early sidecars stored as `i16` still read.
  - Split halves concatenate their latents as separate chunks
    (`concatCloneAudio`).
  - Anything that changes samples must drop `Latents`.
  - `WriteClip` and `RemoveClip` delete a stale sidecar, so a `.lat` never
    outlives or mismatches its clip.
  - Families without latents write no sidecar.
- **Music seeds as latents.**
  - `musicgen.GenerateRegion` takes a `LatentsBackend` path
    (`generateRegionLatents`), which seeds continuations from Stable
    Audio's own latents (`StableAudioRequest.InitLatents`) instead of
    re-encoding WAV tails.
  - Every non-final chunk is whole 4096-sample frames, so in-region seeds
    end exactly where the next chunk starts.
  - The cross-region seed (`Result.SeedLatents`, the served clip's last
    ~10 s to the nearest frame, ≤46 ms off, covered by the client
    crossfade) goes to `<region>.seed.lat` as f16 via
    `jobs.saveMusicSeed`, which removes the WAV.
  - `musicRegionSeeds` prefers `.seed.lat`, falling back to legacy
    `.seed.wav`, which is trimmed to whole frames.
  - Latents from another checkpoint fail with
    `ttsproto.ErrLatentsModelMismatch`, and the region starts unseeded.
  - `audiomaint.MigrateSeedLatents`, started from `cmd/server`, converts
    legacy seed WAVs through the worker's `/codec-encode` (aux engine
    `stable_audio_medium_codec`: the SAME autoencoder only). It runs only
    after 20 s of worker idleness (`Manager.Idle`), since loading the codec
    evicts generation models.
- **Reference-encoding cache** (`Config.RefCodesDir` =
  `voice-refs/cache/refcodes/`): Higgs, BreezeTTS and Drama 3 codes,
  PocketTTS latents (`audioworker.referenceArtifactID`). Drama 3's
  generated clips have no `.lat` sidecar - its session has no
  `return_latents`. Per reference, as `.lat`:
  Higgs `u10` ~250 B/s, BreezeTTS packed codes ~290 B/s, PocketTTS f32
  ~1.6 KB/s (a 12.5 s reference: 3.6 KB Breeze, 20 KB PocketTTS, vs
  600 KB WAV). Only paragraph generation
  (`GenerateChunkedAudio`) uses it; emotion-variant renders don't.
  - Keyed by the content of (clone model, transcript, reference bytes).
  - Cached codes go out with the reference audio. The worker uses them
    only if their `codec_model` stamp (`audioworker.modelIdentity`)
    matches the loaded checkpoint; otherwise it re-encodes and the reply
    replaces the entry.
- **Worker wire changes** (`ttsproto`):
  - `/generate` and `/stable-audio-medium` answer JSON
    (`GenerateResponse`/`StableAudioResponse`) only when `returnLatents`
    is set; raw WAV otherwise.
  - New `/codec-encode` endpoint.
  - The fake worker (`ttsworkertest`) mirrors this (`OnGenerateLatents`).
