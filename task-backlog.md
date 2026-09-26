## Done
10. Fix music player crossfade. `BBBBBB(Ba)|(bA)AAAAAAAAAAAAAAAAAA(Ac)|(aC)CCCCCC`
1. UI: put the generate all/ungenerated buttons above the per-chapter execution buttons also as icons
2. Improve the pronunication pass: it is frequently incorrect
12. Fix reader autoscroll button not working if scrolled too far
17. Fully expanded pronunciation for numbers/fractions
13. New emotion system (most higgs tags are pretty broken) (keep higgs pauses as a plain charater-based go pass) -- allow saving breezetts CloneInstructed voice variants (with different ref text) for emotions. Each speaker should have a set of emotions. Investigate/create a reasonable set of emotions/speaking modes. Is this a reasonable ask?
    - Listening test done (`~/emotion-test/`): direct Breeze sounds best; Higgs cloned from a Breeze emotion variant is the practical pick (Higgs ~0.26 RTF vs IndexTTS2 ~0.46). Higgs has no guidance/instruction option, so the ref clip is its only emotion lever.
    - Possible future measure (skipped for now): strengthen variants for Higgs - render Breeze variants at higher guidance (5-7 vs the current 4), several seeds per variant with audition/lock, and a per-emotion fallback route to direct Breeze for emotions Higgs flattens (likely shout/whisper).
18. Incomplete voice generation detection (based on alignment) and retry with different text-chunking settings
    - `2d86181`: inline alignment completeness check, retries at half/quarter `text_chunk_size`, keeps the most complete attempt.
3. Add pipeline tasks for any tasks that don't have one
    - `0ac12fd`: every Speakers-page whole-book action is now one cancelable/promotable `pipeline_bulk_<action>` job group.
14. Attribution refinement passes with an eye for performance/throughput. Maybe an alternate model?
    - `825c39c`: compact `N: Name` replies, two batches in flight, trimmed known-characters roster, rule-based speech-tag refinement, aliases, group-speaker guards.
    - Not done: trying an alternate model.
15. Improved music generation prompts
    - `226e93d`/`d4dce82`: structured describe pass (genres/instruments/BPM tags), separate ambience layer shared per place.
4. Optimize everything: Too many adhoc/small DB reads
    - `9d75217`: in-memory read cache behind the `store.Store` interface, invalidated by write notifications; coalesced lookahead; paragraph lists reused instead of reloaded.

---------------------

5. Consider switching to sqlite
6. Redo task cancellation cascade logic: tasks should cascade cancel if all parents canceled (never if individually queued)
7. Task prioritization should be dynamic (if a high priority parent is canceled (and no others of that priority exist) then the task should have a lower effective priority)
8. Unify task ordering/display logic
9. Simplify the queue substantially (too many adhoc rules). Tasks should be evaluated at enqueue/preflight and otherwise be idle. Recompute effective priorities on task enqueue/cancel. Tasks should track their own exclusivity using a function on the set of in-progress tasks. If the tasks don't exclude, they can become inflight. Write this logic as a new fromt-scratch package. Also context switches are expensive, try to prioritize not doing that. Ask many questions and work through a full design spec.
16. Prompt output rating w/ experiments. The ability to mark some output as good/especially appropriate/bad (track the input to the prompt as well)
19. Generalized performance
    - Progress so far: Higgs continuous batching + resident reference KV (`142c685`), one resident clone model / model-kind residency gate (`5f3df73`, `147d1ba`), per-task timing logs (`ef7763d`), DB read cache (`9d75217`).
20. Content-defined audio file storage: key clips by a hash of what generated them (generation text after substitutions, clone model, reference clip bytes + transcript, instruct, language, sampling options, worker/model version) instead of `<bookID>/<chapterID>/<voiceID>/<idx>.opus`
    - Wins: invalidation becomes automatic (new inputs = new key, old file is garbage for the sweeper), clips survive re-import idx shifts, free dedup of repeated lines, immutable cacheable URLs; Android sync `audioHash` could just be the key.
    - Needs: a nonce/generation counter in the key for "regenerate" with unchanged inputs (generation is unseeded); merge groups keyed by merged text (members keep pointers); rows store the key; sweeper GCs unreferenced keys after a grace period; old clips served by path until regenerated. Latents sidecars (`<clip>.lat`) follow the clip's name for free.
21. Music seam crossfade: inpainting re-renders the kept seed window (~20 dB off the real seed), so every seeded splice in `musicgen` is slightly imperfect - most visible within a region (4-5x spectral jump at in-region chunk joins in the A/B). Add a short crossfade at the splice or clamp the seed frames during sampling. See `lac/latent-storage-report.md` "Seam A/B".
22. Latents over the wire: send Higgs codes (~2 kbps) / PocketTTS latents (~0.8 KB/s f16) to Android instead of Opus and decode on-device (audio.cpp/ggml via NDK + JNI, decoder weights ~20-45 MB per family). Per-clip format negotiation (latents when a `.lat` sidecar exists and the app has a matching `codec_model` decoder, Opus otherwise), chunk-aware decode/seeking (`chunk_frames`). Music stays Opus (SA3 latents aren't smaller). Measure phone decode speed/battery first.
23. Lightweight lectable->lectable export using latents instead of audio: narration ~66-420 MB and seeds ~170 MB vs ~16 GB today; importer decodes on its GPU (~15-25 min narration, ~1-2 h music for a book) and re-encodes Opus. Needs matching checkpoints (`codec_model` stamps; Opus fallback), a music recipe (chunk + ambience-loop latents, loop crossfade + mix params, rebuilt in Go), and mixed latents/Opus exports since only clips rendered after dual-write have sidecars.
24. Voice-ref caching for PocketTTS: Higgs reference codes are cached (`voice-refs/cache/refcodes/`), but audio.cpp's PocketTTS has no input to pass a precomputed voice state/encoding back in, so it re-encodes the reference after each worker restart. Needs an audio.cpp patch like Higgs' `reference_codes`.
25. Compress the remaining WAVs: ambience loops (~5.8 GB for Spire's Spite) with FLAC (lac was rejected); voice refs can stay WAV (676 MB total; see `lac/mastering-format-report.md`).
26. Upstream candidates in the local audio.cpp patch set: the SAME encode out-of-bounds fix for windows under 128 latent frames (a real upstream bug), and the latents/codec artifact API (`lectable-latents-codec.diff`).
27. Reference-code cache GC: `voice-refs/cache/refcodes/` is content-addressed and never pruned (~5 KB per voice x reference); sweep entries for references no preset uses anymore.
