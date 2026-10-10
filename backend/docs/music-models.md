# Music model comparison

Region music stays on **Stable Audio 3 Medium** (`internal/musicgen`).
Decided 2026-10-07 after comparing it with ACE-Step, YuE2 and Magenta
RealTime 2 on real region prompts. This records what was measured, so the
comparison needn't be rerun to remember why.

## Speed

One 30 s clip, model already loaded, job queue paused, RX 7900 XTX (HIP).
RTF is render time / audio duration; lower is faster.

| Model | Run time | RTF | Load + 5 s warm-up clip |
|---|---|---|---|
| Stable Audio 3 Medium | 1.5 s | 0.05 | 5.1 s |
| ACE-Step 1.5 | 5.3 s | 0.18 | 13.2 s |
| YuE2, `cot=off`, instrumental LoRA | 8.1 s | 0.27 | 11.3 s |
| YuE2, `cot=full`, no LoRA | 14-21 s | 0.47-0.70 | not measured apart |
| Magenta RealTime 2 small (ONNX Runtime, CPU) - not indicative, see below | 85-94 s | 2.8-3.1 | ~3 s |

- The first three rows are seven prompts each, one request at a time
  through `POST /api/sfx/generate` at its default steps and guidance,
  seed 0. Run to run they varied by under 0.4 s.
- The YuE2 `cot=full` row is from four requests queued together, each
  run's time taken as the gap between completions; the first included the
  model load.
- The worker holds one YuE2 session, so parallel previews queue: seven
  took 57 s, the same as one after another.
- The YuE2 LoRA costs nothing at run time (8.1 s with it, ~9 s without).
  Planning (`cot=full`, the score-writing stage) is what roughly doubles
  YuE2's time.

## Parallelism

Measured 2026-10-09 (Stable Audio 3 Medium, RX 7900 XTX, job queue
paused, a separate worker). One render already keeps the GPU busy, so
VRAM headroom (a render uses ~4 GB) doesn't buy throughput:

| Setup | Result |
|---|---|
| One session, renders one after another | 5.0 s per 114 s clip; GPU compute engine 75-98% busy |
| Two sessions at once (`..._MEDIUM_POOL_SIZE=2`) | 9% more audio per second, GPU 82-100%; one request starved for 28 s |
| Four sessions at once | the worker segfaulted on the first batch |

So the worker stays at one session. What does help is on the server's
side: about a fifth of a region's time is spent between worker calls
(decoding, stitching, mixing, Opus encoding), and another region's call
can run then. Eight independent 200 s regions through the whole pipeline
(two chunks each, half with an ambience loop):

| Regions at once | Per region |
|---|---|
| 1 | 14.3 s |
| 2 | 11.6 s |
| 3 | 11.5 s |

Hence `jobs.musicChainConcurrency` = 2: a whole-chapter batch is split
where the music cuts, and two of those chains render side by side. A
region's ambience loop renders beside its music for the same reason. The
live path stays one region at a time - it already runs ~20x faster than
playback, and a second render would only delay the one the reader is
waiting for.

## The models

- **ACE-Step, YuE2**: run through the worker's existing aux engines. Both
  were given lyrics `[Instrumental]`.
- **YuE2 instrumental LoRA**:
  [Mothersuperior/YuE2-instrumental-cot-full-loras](https://huggingface.co/Mothersuperior/YuE2-instrumental-cot-full-loras)
  (`ar_lora_inst_v3abc.safetensors`, CC BY-NC 4.0), loaded when it sits in
  the YuE2 model root (`audioworker.yue2SessionOptions`). Its author
  documents it for `cot=full` with the lowercase lyric `[instrumental]`;
  it was run here with `cot=off`.
- **YuE2 continuation**: possible from its own semantic tokens only
  (`semantic_prefix`, with `export_semantic=true` to get them), never from
  audio. The worker's request doesn't expose either option. Not tried.
- **Magenta RealTime 2**
  ([google/magenta-realtime-2](https://huggingface.co/google/magenta-realtime-2)):
  not in audio.cpp. Google's runtimes are JAX (NVIDIA/CPU) and MLX (Apple
  Silicon), so nothing runs it on this GPU. Measured on CPU through the
  community ONNX export of the 230M model
  ([blanchon/magenta-realtime-2-onnx](https://huggingface.co/blanchon/magenta-realtime-2-onnx));
  the 2.4B model has no ONNX export.
  - **Its speed figure is not indicative** of the model: a CPU run of an
    unoptimized export, the only way to hear it here. On listening, its
    quality was low enough that a real benchmark (a GPU runtime, the 2.4B
    model) wasn't pursued.
  - About 60% of its temporal step is ONNX Runtime's `Einsum` operator, and
    thread count barely changes the time, so the export understates the
    model.
  - Its clips came out 9-13 dB quieter than Stable Audio's.
  - The library's tag-style prompts ("TrackType: Music, ...") and a short
    one ("satirical jazz") map to almost entirely different style tokens.
  - Supporting it properly means a native audio.cpp family: comparable
    music families there are 3,500-5,300 lines of C++.

## Where the samples are

`~/mrt2-eval/` on the dev box, outside the repo: `clips/` (eight library
prompts, 30 s each, per model), `src/mrt2.py` (the Magenta generation
loop, a Python port of the ONNX export's reference one), `onnx/` and
`lora/`.
