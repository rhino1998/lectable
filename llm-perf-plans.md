# LLM performance: pending benchmarks and ROCm update

**Done 2026-09-28.** Quick llama-bench checks (results in
`~/llm-bench/results/vibe-*`), then shipped: ROCm 7.2.4 (user-space) for
both native libraries, latest llama.cpp (526c43b), MTP speculative decoding
in `llamacpp-go`'s Scheduler, ubatch 2048. Windows was skipped (its ROCm
build fell back to CPU). See `backend/CLAUDE.md`'s GPU toolchain note and
`llamacpp-go/CLAUDE.md`'s "MTP speculative decoding". The rest of this file
is the original plan, kept for reference.

These benchmarks were left unfinished on 2026-09-28, when the Windows AMD
driver was updated in the middle of the MTP run. Run them in order. The
first one tells you whether the rest can run as-is.

All scripts, and every result so far, live in `~/llm-bench/`:

- `bench.sh <label> <gguf>`: the quality and speed suites, using
  `zz_modelbench_test.go` against a llama-server on :8095.
  - Environment: `SUITES` (default `attrib identity cast setting`),
    `SERVER_ARGS` (extra llama-server flags), and `SKIP_LLAMABENCH=1`.
  - It pauses the job queue, waits for in-flight GPU work to drain,
    unloads the worker's models, and puts the queue back as it found it.
- `gpu-compare.sh`: `llama-bench` on Linux/WSL ROCm, Windows ROCm and
  Windows Vulkan, at ubatch 512/1024/2048. It pauses and drains the queue
  the same way.
- `results/`: every earlier run. The baseline to compare against is
  `llamabench-qwen3.6-35b-a3b.txt`:
  - pp4096: 3371 tok/s;
  - tg128: 110 tok/s;
  - suite `result-qwen3.6-35b-a3b.txt`: 482 s total, attribution 95.4%.

Only one GPU job can run at a time. Never start one while another is still
running, and check `curl -s localhost:8080/api/jobs` shows `"paused": false`
once you're done.

## 0. After the driver update: is WSL ROCm still working?

The ROCm 6.4.4 WSL runtime (`hsa-runtime-rocr4wsl-amdgpu` 25.10) was built
for the older Adrenalin driver, so a newer Windows driver can break it.
Check before anything else:

1. Restart WSL: `wsl --shutdown` from Windows, then reopen.
2. `/opt/rocm/bin/rocminfo | grep -E 'gfx1100|Marketing'` should list the
   RX 7900 XTX as `gfx1100`.
3. `make deploy-backend`, then `curl -s localhost:8091/health`.
4. Generate a paragraph or two from the reader. Check the worker log for
   HIP errors, and that one attribution/summary task finishes.
5. Re-run the baseline speed test and compare it with the numbers above:

   ```
   LB=~/llama-cpp-py-sync/vendor/llama.cpp/build-bench/bin
   curl -s -X POST localhost:8080/api/jobs/pause; curl -s -X POST localhost:8091/unload; curl -s -X POST localhost:8091/llm/unload
   LD_LIBRARY_PATH=$LB $LB/llama-bench -m ~/llm-models/Qwen3.6-35B-A3B-UD-IQ4_XS.gguf -ngl 99 -fa 1 -p 4096 -n 128 -r 3
   curl -s -X POST localhost:8080/api/jobs/resume
   ```

   The driver alone may change these numbers, so every later comparison
   uses the new baseline, not the old one.

If `rocminfo` no longer sees the GPU, 6.4.4 is dead on this driver. Go to
step 2, which then becomes required rather than an experiment.

## 1. MTP (multi-token prediction) on Qwen3.6-35B-A3B

**Question.** Does MTP speculative decoding make our LLM passes faster
enough to justify porting it into `llamacpp-go`?
- Writing replies was over half the model's time in the attribution
  benchmark: 179 s per slot writing vs 127 s reading prompts.
- MTP speeds up writing only.

**Model.** `~/llm-models/Qwen3.6-35B-A3B-MTP-UD-IQ4_XS.gguf` (Unsloth,
18.2 GB, already downloaded). It has the same weights as the live model
plus the MTP head.

**Run.** Both runs use the same file, so MTP is the only difference, about
10 minutes each:

```
~/llm-bench/bench.sh q36mtp-off ~/llm-models/Qwen3.6-35B-A3B-MTP-UD-IQ4_XS.gguf
SERVER_ARGS="--spec-type draft-mtp" SKIP_LLAMABENCH=1 ~/llm-bench/bench.sh q36mtp-on ~/llm-models/Qwen3.6-35B-A3B-MTP-UD-IQ4_XS.gguf
```

**Compare** `results/result-q36mtp-{off,on}.txt`:
- the `SPEED` lines: wall time per suite, and gen tok/s;
- the `ATTRIB` line: accuracy should be identical, since MTP must not
  change the output;
- `results/server-q36mtp-on.log`: the draft acceptance rate.

The interrupted run got through attribution and identity with MTP off
(`results/misses-q36mtp-off.txt`, `identity-q36mtp-off.txt`) but never
wrote its summary, so both runs need redoing.

**Decision.** Port only if the passes get about 15% or more faster.
- The port means implementing MTP drafting in `llamacpp-go`'s `Scheduler`.
  llama.cpp only has it in `common/speculative.cpp` (the
  `draft-mtp` implementation), on top of the staging API in
  `src/llama-ext.h`, which may change between llama.cpp versions.
- The live worker's llama.cpp build must also load the MTP head. Check how
  `common.cpp` sets `mparams.load_mtp` and `LLAMA_CONTEXT_TYPE_MTP`.

## 2. ROCm 7.2.4

**Question.** Is ROCm 7 faster than 6.4.4 for llama.cpp (and later
audio.cpp)? It's also the fallback if the new driver broke 6.4.4.

**Already done.** ROCm 7.2.4 is downloaded and unpacked into
`~/rocm7-user/`, with nothing installed system-wide. It has no `sudo` and
leaves `/opt/rocm` on 6.4.4.
- `debs/`: the 23 ROCm packages llama.cpp needs (hip runtime, rocblas,
  hipblas(lt), rocm-llvm, rocminfo, ...), plus `rocdxg-roct_1.2.2`.
- `root/opt/rocm-7.2.4/`: the unpacked tree. `librocdxg.so` and
  `share/rocdxg/dids.conf` are copied into it.
- `apt/`: a private apt config for re-resolving packages
  (`APT_CONFIG=~/rocm7-user/apt/apt.conf apt-get ...`).

ROCm 7 runs under WSL through AMD's `librocdxg`, which bridges ROCm to the
Windows driver. ROCm 7.x doesn't ship its own WSL runtime; the old
`hsa-runtime-rocr4wsl` package stopped at 6.4.

**Step 2a: see whether the user-space copy can find the GPU.** Unverified:
`librocdxg` hardcodes `/opt/rocm/lib` and looks for `dids.conf` under
`/share/rocdxg/`, so it may insist on system paths.

```
R=~/rocm7-user/root/opt/rocm-7.2.4
export HSA_ENABLE_DXG_DETECTION=1 LD_LIBRARY_PATH=$R/lib:$R/llvm/lib:/usr/lib/wsl/lib
$R/bin/rocminfo | grep -E 'gfx|Marketing'
```

If it can't find the GPU because of paths, install it properly instead,
side by side with 6.4.4:

```
wget https://repo.radeon.com/amdgpu-install/7.2.4/ubuntu/noble/amdgpu-install_7.2.4.70204-1_all.deb
sudo apt install ./amdgpu-install_7.2.4.70204-1_all.deb
sudo apt update && sudo apt install rocm-hip-runtime rocm-hip-sdk hipblas-dev rocblas-dev hipblaslt-dev
sudo dpkg -i ~/rocm7-user/debs/rocdxg-roct_1.2.2_amd64.deb
```

Before that system install:
- `ls -l /opt/rocm` and note where it points. Installing 7.x may repoint
  the `/opt/rocm` symlink (or the alternatives entry) at 7.2.4.
- The live `libaudiocpp.so`/llama.cpp builds link against 6.4.4 (for
  example `libamdhip64.so.6`). Make sure they still resolve afterwards (run
  `ldd` on `backend/ttsworker` and the two libraries), or point
  `run.sh`'s `LD_LIBRARY_PATH` at `/opt/rocm-6.4.4/lib` explicitly.

**Step 2b: build a separate llama.cpp against ROCm 7.** Never touch
`build/` (live) or `build-bench/`:

```
cd ~/llama-cpp-py-sync/vendor/llama.cpp
HIPCXX=$R/llvm/bin/clang++ HIP_PATH=$R ROCM_PATH=$R \
  cmake -B build-rocm7 -DGGML_HIP=ON -DGPU_TARGETS=gfx1100 -DCMAKE_BUILD_TYPE=Release -DCMAKE_PREFIX_PATH=$R
cmake --build build-rocm7 -j 32 --target llama-bench llama-server
```

**Step 2c: benchmark it.** Use the same `llama-bench` line as step 0, with
`LD_LIBRARY_PATH=build-rocm7/bin:$R/lib:$R/llvm/lib` and
`HSA_ENABLE_DXG_DETECTION=1`, and compare it with the step 0 baseline. If
it's clearly faster, also run `bench.sh` with `LB` pointed at
`build-rocm7/bin`, so the whole suite confirms the gain.

**If ROCm 7 wins,** switching the live worker also means:
- rebuilding the live llama.cpp (`build/`);
- rebuilding audio.cpp's `libaudiocpp.so` against 7.2.4, with every local
  patch still applied. Check `git diff --stat` in `~/audio.cpp` and see
  `lectable-local-patches.diff`;
- re-verifying Higgs/PocketTTS/Stable Audio output, and the latents
  bit-exactness tests in `audiocpp-go`;
- updating `backend/CLAUDE.md`'s toolchain notes.

## 3. ubatch sizes and native Windows

**Question.** Does a larger ubatch speed up prompt reading? And is running
outside WSL faster, especially for writing replies, where WSL's
per-GPU-call overhead hurts most?

The Windows builds are llama.cpp release b11242, unpacked in
`~/llm-bench/win/{rocm-10.0,vulkan}`. They run straight from WSL and read
the model from the WSL filesystem, so nothing gets copied to the 95%-full
`C:`.

**Run:** `~/llm-bench/gpu-compare.sh` (writes `results/gpu-compare.txt`).
It runs, in order:
1. Linux (WSL ROCm, the `build-bench` build from 16 Sep);
2. Windows ROCm;
3. Windows Vulkan.

Each is `-p 4096 -n 128 -ub 512,1024,2048`.

**Reading it:**
- **ubatch:** a bigger ubatch on Linux that reads prompts faster is a
  one-line change (`LECTABLE_SPEAKER_LLM_UBATCH`). Check that the KV and
  compute buffers still fit beside the 16.5 GB model at 2 slots.
- **Windows:** the Linux build is 12 days older than the Windows builds.
  If Windows wins, first rebuild `build-bench` at b11242 and re-run
  Linux, so the gap isn't just the llama.cpp version.
- **What a Windows win would mean:** moving the ttsworker to Windows is a
  multi-day port (audio.cpp and llama.cpp built for Windows with every
  local patch, supervising a Windows process from WSL, model paths), so it
  needs a large speedup to be worth it.
- **ROCm 7 too:** if step 2 produced a ROCm 7 build, add it as a fourth
  row.

## Already decided (don't re-run)

- **Qwen3.8-27B** (dense) reads prompts 3.4× slower (987 vs 3371 tok/s)
  and writes replies 3× slower (36 vs 110 tok/s) than Qwen3.6-35B-A3B.
  Rejected on speed.
  - `~/llm-models/Qwen3.8-27B-UD-Q4_K_XL.gguf` (17.6 GB) can be deleted.
- **JetBrains Qwen3.8-3.6-27B-blend:** the same dense 27B architecture,
  so the same speed. Rejected.
