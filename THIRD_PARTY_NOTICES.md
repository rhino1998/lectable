# Third-party notices

Lectable itself is MIT-licensed (see `LICENSE`). The files below are copied
from, or derived from, other projects and remain under those projects'
licenses; each one's full license text sits next to it.

| Path | Origin | License | License text |
|------|--------|---------|--------------|
| `audiocpp-go/audiocpp/include/audiocpp.h` | [audio.cpp](https://github.com/0xShug0/audio.cpp) C API header, copied unmodified - Copyright 2026 ShugoAI LLC | Apache-2.0 | `audiocpp-go/audiocpp/include/LICENSE-audiocpp.txt` |
| `llamacpp-go/llamacpp/include/*.h` (`llama.h`, `ggml*.h`, `gguf.h`) | [llama.cpp](https://github.com/ggml-org/llama.cpp) public headers - Copyright (c) 2023-2026 The ggml authors | MIT | `llamacpp-go/llamacpp/include/LICENSE-llamacpp.txt` |
| `backend/internal/wsola/` | Go port of Chromium's WSOLA time-stretcher (`media/filters/audio_renderer_algorithm.cc`, `media/filters/wsola_internals.cc`) - Copyright 2013 The Chromium Authors | BSD-3-Clause | `backend/internal/wsola/LICENSE-chromium.txt` |

Dependencies pulled in by the Go modules, npm and Gradle (not vendored into
this repository) are all under permissive licenses - MIT, BSD, ISC or
Apache-2.0 - and carry their own license files in their distributions.

Model weights (Qwen3-TTS, Higgs Audio, the speaker-attribution GGUF, Stable
Audio, etc.) are not part of this repository; each is subject to its own
license terms, which apply when you download and run it.
