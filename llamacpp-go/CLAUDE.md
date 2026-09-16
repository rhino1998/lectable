# llamacpp-go

Go (cgo) bindings for [llama.cpp](https://github.com/ggml-org/llama.cpp)'s
public C API -- a separate, external C++ inference project, not part of
this repo. Its own Go module
(`github.com/rhino1998/lectable/llamacpp-go`, not `.../backend`), the same
shape as [`audiocpp-go`](../audiocpp-go/CLAUDE.md) but for llama.cpp.

Imported by `backend/internal/llmworker` (used only by `cmd/ttsworker`,
never `cmd/server` - the same shape `audiocpp-go` has in
`backend/internal/audioworker`), wired into `backend/go.mod` via a local
`replace` directive the same way `audiocpp-go` is. Hosts the
general-purpose GGUF model `backend/internal/speakerattr` prompts for
speaker attribution/characterization/direction-tagging - see
`backend/CLAUDE.md`'s "ttsworker / audioworker / llmworker" section for the
full wiring. llama.cpp itself carries none of audio.cpp's confirmed native
leak, so an earlier version loaded this model directly inside `cmd/server`;
it moved into `ttsworker` alongside the TTS models specifically so both
native model families share one process's VRAM budget and can be
idle-unloaded independently (`llmworker.Config.IdleUnloadAfter`), which
only works with both living in the same process.

## Layout

- `llamacpp/` -- the package. One file per concern, mirroring
  `audiocpp-go/audiocpp`'s split: `llamacpp.go` (backend init/version),
  `log.go` (captures libllama's log lines, since its C API otherwise
  returns no error strings), `errors.go`, `model.go`, `vocab.go`
  (tokenize/detokenize/chat template), `context.go`, `batch.go`,
  `sampler.go`, `generate.go` (the high-level single-sequence decode loop),
  `scheduler.go` (multi-sequence batching across concurrent callers - see
  below).
- `llamacpp/include/` -- vendored copies of llama.cpp's `llama.h` and the
  ggml/gguf headers it `#include`s (`ggml.h`, `ggml-cpu.h`,
  `ggml-backend.h`, `ggml-alloc.h`, `ggml-opt.h`, `gguf.h`). Unlike
  audio.cpp's single dependency-free header, llama.cpp's API drags in
  ggml's headers, so all of these travel together -- if any need updating,
  copy the full set from the same llama.cpp checkout's `include/` and
  `ggml/include/`, verbatim, don't hand-edit.
- `llamacpp/smoke_test.go` -- an unconditional library-info test (still
  needs libllama linked) plus a generation test gated on
  `LLAMACPP_TEST_MODEL` pointing at a real local GGUF file (llama.cpp
  doesn't bundle a tiny test model the way audio.cpp bundles Silero VAD);
  see the package doc comment and README.md for setup.

## What's vendored vs. external

Only headers are vendored. llama.cpp's and ggml's C/C++ source, their
CMake build (produces `libllama.so` and its `libggml*.so` companions), and
any GGUF model weights are **not** vendored -- a large separate native
project plus multi-gigabyte assets. Anything that *links* against this
package needs `CGO_LDFLAGS`/`LD_LIBRARY_PATH` pointed at a local llama.cpp
build; see README.md.

## Scope: a subset, not a mirror

`audiocpp-go` mirrors `audiocpp.h` close to 1:1 because that header is a
small, clean binding surface. `llama.h` is llama.cpp's entire engine API
(LoRA adapters, full session state save/load, embeddings extraction,
quantization, model introspection, ...) -- this package wraps only what a
prompt-in/text-out chat-completion feature needs (load a GGUF model,
tokenize, apply a chat template, decode, sample), for either a single
caller reusing one sequence (`Context.Generate`/`GenerateFrom`) or several
concurrent callers sharing one loaded model via real multi-sequence
batching (`Scheduler`, see below) - plus specific slices of the
KV-cache/memory API each of those needs to reuse an already-decoded prompt
prefix without paying to redecode it every time:

- `Context.TrimSequence` (backed by `llama_memory_seq_rm`) rewinds a
  still-open `Context`'s cache back to an earlier prefix boundary in
  place - the single-sequence reuse path `GenerateFrom` describes, and
  what `Scheduler` uses (`p0=0`, a full wipe) to reset a generation slot
  between requests. Can't rewind every memory type this way - a
  hybrid/recurrent architecture (Gated-Delta-Network/Mamba-style) can
  decline it outright (see its own doc comment).
- `Context.CopySeq` (backed by `llama_memory_seq_cp`) copies one
  sequence's cached range into another, leaving the source untouched -
  what `Scheduler` uses to give each of several concurrent generations its
  own copy of one shared, already-primed prefix
  (`GenRequest.PrimedSeq`/`StartPos`), the multi-slot generalization of
  `TrimSequence`'s single-sequence in-place reuse. Requires
  `ContextParams.KVUnified: true` - llama.cpp's own default
  (unified-buffer-off) only supports `CopySeq`-ing a *whole* KV buffer, not
  a short prefix range, and asserts otherwise (hit and fixed while
  building `Scheduler` - see its tests).
- `Context.SaveSeq`/`RestoreSeq` (backed by `llama_state_seq_get/set_data`)
  snapshot/restore a sequence's entire state to a plain `[]byte` -
  llama.cpp's documented answer for the architectures `TrimSequence` can't
  handle. Not currently called by anything in this repo (an earlier,
  single-sequence version of `speakerattr`'s context-reuse logic used this
  as `TrimSequence`'s fallback; superseded by `llmworker`/
  `Scheduler`/`CopySeq` once concurrent callers were added) - kept as a
  tested primitive for a future single-sequence caller on a
  hybrid/recurrent architecture. `Context.StateSeqSize` is the exact-size
  query `SaveSeq` uses internally, exposed since it's independently useful.

`llama_state_get/set_data` and the `_file` full-state-serialization
variants (whole-`Context`, not one sequence, and/or to-disk) remain
unwrapped - nothing here has needed a KV cache to survive past the
`Context` that built it. `Batch` and `Context.Decode`/`LogitsIth` are
exposed directly (not just through `Generate`) so a future call site
needing more of llama.h's surface has something to build on.

## Scheduler: multi-sequence batching

`llama_decode` is not reentrant - only one call may be in flight against a
given `Context` at a time - so serving several concurrent generations
against one loaded model can't just mean calling `Generate`/`GenerateFrom`
from several goroutines at once. `Scheduler` (`scheduler.go`) is real
multi-sequence batching instead: a `Context` created with
`ContextParams.NSeqMax > 1` gets a dedicated pool of `nSlots` sequence
ids, and `Scheduler`'s own single goroutine (`run`) packs together
whatever every currently-active slot needs *this round* - a still-prefilling
slot's next chunk of prompt tokens, a still-generating slot's single next
token - into one shared `Batch`/`Context.Decode` call per round, so the
GPU/CPU work behind one decode is shared across every active request
instead of paid once per request. `Generate` (a `GenRequest` in, a
`string` out) is the public API; `GenRequest.StartPos`/`PrimedSeq` opt
into `CopySeq`-based prefix reuse from a separately-primed sequence (see
"Scope" above).

Two real bugs surfaced building this, both now covered by
`scheduler_test.go` (gated on `LLAMACPP_TEST_MODEL`, same as
`prefixcache_test.go`/`smoke_test.go`):

- **`llama_get_logits_ith`'s index is a raw batch position, not a count of
  logit-requesting tokens.** It's documented as "the ith token" but
  actually resolves via `output_ids[i]` - an index into *every* token
  submitted this round, output-requesting or not, not a running counter
  over only the tokens that set `logits=true`. A round mixing a
  multi-token prefill chunk (only its last token wants logits) ahead of
  another slot's single decode token needs the *raw* position of each
  logit-requesting token within the round's full submission order, or
  `Sample` looks up the wrong row - confirmed as a real
  `GGML_ASSERT(logits != nullptr)` crash before the fix (see `run`'s own
  `batchPos` doc comment).
- **`CopySeq` needs `ContextParams.KVUnified: true`.** llama.cpp defaults
  `kv_unified` to `false` - every sequence gets its own separate KV-cache
  stream, and copying a short prefix *range* between two different
  streams hits `GGML_ASSERT(is_full && "seq_cp() is only supported for
  full KV buffers")`; only a same-stream copy (`kv_unified: true`, or
  copying a seq's entire buffer) is cheap and range-capable. Any `Context`
  whose caller ever sets `GenRequest.PrimedSeq` must be created with
  `KVUnified: true` (see `NewScheduler`'s and `ContextParams.KVUnified`'s
  doc comments).

`TestSchedulerMatchesFreshGenerateConcurrently`/`TestSchedulerWithPrimedPrefix`
hold `Scheduler` to the same bar `prefixcache_test.go` set for
single-sequence reuse: every result, however many requests ran
concurrently through the shared `Context`, must exactly match what a
brand-new single-sequence `Context` given the same prompt produces under
deterministic (temp 0, greedy) sampling - not just "it doesn't crash."
`TestSchedulerMoreRequestsThanSlots` checks that a queue deeper than
`nSlots` drains correctly rather than dropping or corrupting a request.
