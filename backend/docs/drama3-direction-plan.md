# Speech direction for Fish Drama 3: plan

Written 2026-10-14. Nothing here is built or run yet. This is the design for
the "Emotion" bullet of Phase 3 in `drama3-plan.md`. It assumes the port
and its `audiocpp-drama3` clone model come from the other session's work.
Phase 2's listening test still decides whether any of this gets built.

## What Drama 3 accepts

Source: the tech report (arXiv 2610.18010, Appendix C, Table 10) and
Fish's `chunking.py`. All of it goes in the user text; there are no
separate request fields.

| Control | Syntax | Scope |
|---|---|---|
| Style prefix | `[icy calm, as if you already accepted it] Fine.` | the whole line |
| Point tag | `[sighs]`, `[laughs]`, `[pause]`, `[gasps]`, `[clears throat]`, `[hesitates]`, `[coughs]` | an event at that spot |
| Scoped tag | `Promise me <whisper>you won't tell her</whisper>.` (also `<slow>`, `<emphasis>`) | the enclosed words |
| Director's note | `Direction: more amused than before; drop the hurt.` then `Line: ...` | the whole line (an alternative form) |
| Revision note | `[softer and warmer versus previous] line` | a retake, with the earlier take in history |
| Scene / cast | `Scene: ...` and `- Mr. Carter: steady male voice, about 40, formal.` | scene dubbing |

A control is a short span with no nested brackets and no line breaks.
Fish's chunker never splits inside `[...]`, `<x>...</x>` or `<|...|>`.

The report's own measurement (Expresso, section 5.2) is the risk Phase 2
already names. Speaker similarity to a default-style reference falls for
every styled reading: by 0.31 for whispering, by 0.02 for clear speech.
Some of that fall is the style working, not the voice drifting, so ears
decide. Similarity is not a gate.

The first port test (`~/drama3-eval/first-clips`) ran prefixes and point
tags with no tag spoken aloud. Nobody has listened to it yet.

## Design

### 1. One stored direction per line, with the label kept beside it

- Add a migration, `00035`: `paragraphs.direction JSON NOT NULL DEFAULT '{}'`.
  ```json
  {"style": "barely holding back a laugh",
   "marks": [{"at": 17, "tag": "sighs"}],
   "spans": [{"start": 30, "end": 52, "tag": "whisper"}],
   "for": "<sha1 of p.Text>"}
  ```
  Offsets are runes into `p.Text`, the same unit as the `directionMarks` DTO.
  `for` drops a direction once its text changes (an edit, a re-import), the
  same guard `EffectiveEmotion` gives labels.
- Add `Paragraph.EffectiveDirection()`. Like `EffectiveEmotion`, it is empty
  for narration, scare quotes, unvoiced lines and stale text.
- Keep `paragraphs.emotion`. It stays the label for every other clone model,
  so switching a book's clone model never needs a re-run. It is also the
  fallback under Drama 3 when a line has a label but no direction yet.
  Give `emotions.Emotion` a `Direction` field (whisper -> `whispering`,
  pained -> `through pain, strained`, and so on), so a Drama 3 book voices
  its existing labels on day one, before any new pass runs.
- Add the new column to the SELECT/Scan lists in `store/chapters.go`
  (:407, :431, :458), to `iface.go`, and to the column lists in
  `export.go`, `backup.go` and `restructure.go`.

### 2. A delivery capability on the clone model, not string comparisons

- Add `Delivery` to `voices.CloneModelInfo`:
  - `none`
  - `higgs-pauses` (today's `<|prosody:pause|>`)
  - `directed` (Drama 3)
- Replace the id comparisons with it:
  - `store/models.go:686` (`ResolveGenerationText`)
  - `httpapi/chapters.go:50` (`directionMarksFor`)
  - `httpapi/pipeline.go:807` (the preprocess emotion phase)
- `cmd/apigen` exports the capability, so the clients stop hard-coding
  `HIGGS_CLONE_MODEL` in `resolveGenerationText.ts`.
- **Something to look at now, whatever happens with Drama 3:**
  `preprocessDirectionPhase` returns early for any non-Higgs book. Its
  comment says `directChapter` no-ops for other models, but that hasn't
  been true since labels moved to variant clips, and `speakerattr.md` says
  the pass runs under any clone model. So whole-book preprocessing skips
  emotion labels for books on other clone models, and
  `speechDirectionDependency` catches up on them chapter by chapter at
  generation time. It's worth confirming whether that is intended.

### 3. Generation text

- When the model is `directed`, `ResolveGenerationText` writes
  `[style] ` + text, with each point tag and each scoped tag's open/close
  tags placed as `deliverytags.Insertion`s. Rune offsets become byte offsets
  first, and the insertions are composed with the pronunciation insertions
  through `pronounce.Apply`, which already merges insertions.
- Ellipses and em dashes get nothing. Drama 3 reads punctuation itself;
  Phase 2 can try `[pause]` as an option.
- **Chunking.** Most of the work belongs in the port's chunker, not
  `split.go`. Measured 2026-10-14:
  - `split.go` only acts after a failure, and rarely. Of ~13,500 worker
    `/generate` calls that day, 273 were bisects after running out of
    tokens (~2%), 23 were `text_chunk_size` retries and 3 were duration
    retries. Higgs's own chunker splits at 1024 characters, which only
    1,535 of 2.8M paragraphs exceed.
  - Fish chunks at ~300 bytes. 283k paragraphs (10%) are longer than that,
    45k of them dialogue, so the port's chunker will split directed lines
    routinely.
  - What the port already does (`Drama3Session::run`,
    `src/models/fish_drama3/session.cpp`):
    - Each chunk after the first gets the previous chunk as a history
      turn: its text, prefix included, plus its generated codes. So
      chunk 2 hears chunk 1's directed delivery, and the style likely
      carries without repeating the prefix.
    - It keeps only the last turn. Fish's `Drama3History` also pins the
      first chunk. So from chunk 3 on, the prompt no longer holds the
      prefix's text, only audio that followed it. On a long line the
      direction may fade.
    - The generic chunker (`framework/text/chunking.cpp`) treats each
      `[...]` and `<...>` as one token, so it never cuts inside a tag. It
      appears to cut between `<whisper>` and `</whisper>` freely, though,
      leaving an unclosed span in one chunk and a stray close in the next.
  - **Test before changing anything** (add to Phase 2): directed lines of
    one, two and four or more chunks, comparing
    - (a) the port as it is
    - (b) the first chunk pinned in history, as Fish does
    - (c) the prefix repeated on every chunk

    Listen for whether the delivery holds at chunk 3+. Add a `<whisper>`
    span forced across a cut.
  - **Leaning (b) as the default:**
    - It is Fish's own default (`Drama3History`: the first chunk pinned
      plus 1 recent turn), so the port's last-turn-only history is a
      departure from the reference, not a choice.
    - It anchors more than direction. By character count, ~17k paragraphs
      run to 3+ chunks (3.2k of them dialogue, 539 of those labelled), and
      narration's voice and pace hold better with chunk 1 in view too.
    - Its cost is one more turn in the prompt (up to ~300 bytes of text and
      ~20 s, ~430 rows, of codes). That is prefill time, plus KV room in
      the backbone and tower. With the plan to lower `cache_steps` to 2048,
      a long reference plus two full turns plus the output gets close to
      the limit.
    - So the port also needs Fish's fallback (`Drama3History.candidates`):
      when the prompt doesn't fit, drop the recent turn, then the pinned
      one, then all history.
    - **Status (2026-10-14):** the port session has queued it after its
      VRAM work. That covers the pin, the fallback, prefill timing, and
      paired tags kept together in chunking. The same work shrinks batch
      slot caches to likely 2048 rows. Rough budget:
      - reference ~260 + pinned ~430 + recent ~430 + output ~430 + text
        ~200 ≈ 1,750, so it fits on a typical full chunk
      - a long reference or a slow voice pushes it into the fallback
      Fish's order drops the recent turn before the pinned one, which is
      the right order for direction. The port session will report how
      often the fallback fires.
    - The test then shrinks to (a) vs (b), measuring prefill time per chunk
      as well. (c) is needed only if (b) still fades.
  - Change the chunker only as the results require: repeat the prefix if
    (b) still fades, and close and reopen a scoped tag
    at a cut. Never cutting inside a span would need Fish's regex,
    `<\|[^|>]*\|>|<([A-Za-z][\w-]*)\b[^>]*>.*?</\1>|\[[^\[\]]*\]`.
  - `bisectForGeneration` and the `retryChunkSizes` retries need the same
    rules, since they hand the port pieces of a directed line. But they
    are only a fallback.
- **Reference clip:** under `directed`, `emotionVariant` and
  `variantClipDependency` always return the base clip, and
  `clipRecipe`'s `RefIdentity` is called with no emotion. No Breeze variants
  are rendered, and the clip key doesn't depend on a variant's version. The
  direction reaches the key through `Recipe.Text`, so `clipkey` itself
  needs no change.

### 4. Check that no tag is spoken aloud

`extraWordsIn` (`split.go:267`) takes the minimum of the extra-word count
against the plain text and against the generation text. If the model *says*
"sighs" aloud, the generation-text comparison matches it and hides the
fault. So under `directed`:

- Compare against the plain text only.
- Add a second escalation step to `generateCloneChecked`'s retries: after a
  failed retry, drop the point and scoped tags and keep the prefix; after
  that, drop the prefix too. A line then degrades to plain cloning rather
  than failing.
- Log which tag was dropped, per tag. This is what grows or shrinks the
  allowlist in section 5.

Forced alignment is already safe, because a single line aligns on
`p.Text`. Merge groups align on the generation text (`generation.go:330`,
`alignMergedAudio`), but their members are all narration, so they carry no
direction. That only changes if narration ever gets tags (see the open
questions).

### 5. The pass: gate with labels, then write directions

The obvious approach is to ask an LLM for a free-form direction on every
line. The label measurements in `speakerattr.md` argue against it. The
generated "only the emotional lines" list labelled a third of all dialogue
and wasn't stable across runs. A free-form version would also add a
direction to every line: a mannered, over-acted book. Two stages instead:

1. **Gate (existing):** the dense decide pass, unchanged. It labels and
   gives P(neutral), the unused dial `speakerattr.md` describes.
   - A line goes to stage 2 if it has a label, or if a narration paragraph
     next to it holds a vocal-event or delivery verb (laughed, sighed,
     sobbed, gasped, coughed, whispered, hissed, muttered, snapped, "voice
     breaking"...).
   - That word list is plain Go, like `deliverytags`. It catches "He
     laughed. 'No,' he said, his voice breaking.", which can earn a point
     tag or a scoped `<whisper>` without being an "emotion".
   - Expect about 20-25% of dialogue to reach stage 2 (163 of 981 lines
     labelled in the measurement, plus the verb hits).
2. **Direct (new, generative):** one request per gated line, run
   concurrently through the llamacpp `Scheduler` rather than one batched
   prompt. Each line gets its own context:
   - the speaker's name and a short character summary (`ParagraphInput.Known`
     exists but the emotion pass never fills it)
   - the 3 paragraphs before and the 1 after (the speech tag often follows
     the line)
   - the scene from the summary pass, and the book's setting profile
   - the stage-1 label as a hint, not a constraint

   The model answers by **rewriting the line with its controls in place**:
   `[dry, amused] Oh, you didn't. [sighs] Fine.` Offsets would be harder for
   an LLM to get right. Parsing:
   - Remove the controls. What's left must equal `p.Text` after whitespace
     normalization. If it doesn't, keep only the prefix; if even that
     fails, write nothing.
   - Prefix: at most about 10 words. It describes *delivery*, not content
     or plot ("angry at Bob for lying" becomes "angry, accusing"). No names.
   - Point and scoped tags must be on an allowlist. Start from Table 10 plus
     what the first clips proved: laughs, sighs, gasps, pause, clears
     throat, hesitates, coughs, sobs, sniffs, chuckles, groans, and the
     scoped tags whisper, slow and emphasis. Drop any other tag, and widen
     the list from section 4's logs.
   - The prompt also allows answering a line with `neutral`. Stage 1 can be
     wrong.

**Where it lives:**

- `speakerattr/direction.go` (`Client.DirectLines`)
- `KindSpeechDirection` runs stage 2 after stage 1, but only when the book's
  clone model is `directed`. Add a pass flag `Passes.Delivery` beside
  `Passes.Direction`. Under a `directed` model,
  `speechDirectionDependency` waits for both.
- Switching a finished book to Drama 3 runs only stage 2.

**Invalidation:** compare the clip recipe (or `ResolveGenerationText` plus
the reference identity) before and after, not `EffectiveEmotion`. That
generalizes "effective emotion changed" for every clone model at once. Lines
edited by hand (`EditedParagraphIdx`) are left alone, under a new
`store.EditDirection` kind.

**Offline tool first:** add `cmd/sqdecide -direct`, or a sibling
`cmd/sqdirect`, to run stages 1 and 2 over chosen chapters and print the
directed lines with their context. Prompt tuning happens there, like the
emotion probe, before anything touches the store.

### 6. Clients

- **Per-line override:** `PUT .../paragraphs/{pidx}/direction` takes the
  annotated line as one string. The server validates it with the same
  parser as the pass, so typing `[`/`<` tags is the editor.
  - Web: `ParagraphContextMenu.tsx` gets a text field, with the 13 labels'
    `Direction` strings as preset chips.
  - Android: `Annotations.kt` / `ReaderParagraph.kt` get the same.
  - The emotion override stays for other clone models.
- **Display:** extend the `directionMarks` DTO with the style and spans.
  The reader shows the style as a small caption and the tags as inline
  markers, as it does pause marks today. The manifest hash already includes
  `directionMarks` (`manifest.go:96`).
- **Speakers tab:** under Drama 3, variant status means nothing. Show label
  counts only, and hide preview/re-render of variants.
- Run `make -C backend generate`, then update the docs `drama3-plan.md`
  lists.

## Add to Phase 2 before building any of this

Phase 2's conditions C and D use hand-written directions, which tests the
model but not this pass. Add:

- **C' / D':** directions written by the stage-2 prompt prototype
  (`cmd/sqdirect` on the test-set chapters). If C/D win but C'/D' don't, the
  pass needs work, not the model.
- **Prefix vs director's note** (`Direction: ... / Line: ...`) on the same
  lines.
- **Scene and cast context** in the prompt, from the summary pass, to see
  whether it helps delivery or drifts the voice.
- **Spoken-tag rate** per allowlisted tag, from Parakeet against the plain
  text.
- **Over-direction control:** the 4 neutral lines, given a prefix the
  prototype wrote anyway. Is the result worse than no prefix?
- **Direction across chunks:** the three-way test in section 3 (as is,
  pinned first chunk, prefix repeated).
- **Whisper level:** `<whisper>` and `[whispering]` against the base clip
  (the old variant loudness problem).

## Later, not in the first cut

- **Retake with a note:** a reader action, "softer", sends
  `[softer versus previous] line` with the old take as history. This needs
  `previous_takes`/history in the port and the clip's Drama 3 codes kept as
  a `.lat` sidecar (`latents.md` already does this for Higgs and Breeze).
- **Continuity across lines:** pass the previous line by the same speaker as
  a history turn. It costs prefill time and VRAM, which is tight.
- **Narration:** epub italics as `<emphasis>` in the narrator's lines.
  Narration stays neutral otherwise. This would make merge-group alignment
  strip tags.

## Open questions for the user

1. Keep the 13 labels as the gate and the fallback (recommended), or let
   Drama 3 books skip labels entirely?
2. Is the per-line override a raw tagged-text field good enough, or should
   it be a structured editor?
3. Is the preprocess phase skipping emotion labels for non-Higgs books
   (section 2) intended?

## Progress (2026-10-14, lectable side)

Built and covered by unit tests, with nothing run against the model yet.
`make check` passes.

- `voices.Delivery` on `CloneModelInfo` (`plain`, `higgs-pauses`,
  `directed`) and `voices.CloneDelivery`/`Directed`, used in place of the
  Higgs id comparisons.
- `voices.Drama3CloneModel = "audiocpp-drama3"` is a placeholder outside
  `CloneModels`, which `CloneDelivery` answers `directed` for. When the port
  session adds the catalog entry with `Delivery: DeliveryDirected`, delete
  that special case.
- `internal/direction` (parse, annotate, insertions, strip, carry across a
  bisect, the book's own brackets), migration `00035`
  (`paragraphs.direction`), `Paragraph.EffectiveDirection` (with the label's
  style as the fallback, `emotions.Emotion.Direction`), and
  `ResolveGenerationText`'s directed branch.
- Jobs:
  - no variants under a directed model (`task.variantEmotion`)
  - the extra-words check runs against the stripped text, with
    vocalizations dropped
  - a tags-then-style retry ladder
  - `CarryAcross` on bisect
  - `speechDirectionDependency` runs stage 2 for a labeled chapter
- The pass: `speakerattr.Client.DirectLines` (one call per line, 4 at a
  time, a one-word style re-asked once) and `DeliveryCue`. In httpapi,
  `deliverChapter`/`deliverAfterLabels` and `Passes.Delivery`. The
  preprocess phase now also runs for directed books.
- The reader's override, `PUT .../paragraphs/{pidx}/direction`, and
  `paragraphDTO.direction`. The direction editor is in the web context menu
  and Android's long-press menu ("Set direction"), shown only for a directed
  book.

Left for after the model is wired:

- Tune the prompt, `PointTags` and the cue words on the Phase 2 test set.
- First-attempt temperature: greedy may make the styles repetitive across
  lines.
- The bare-style retry count.
- Window runs (`directWindow`) label ahead but don't direct, so the first
  lines of a chapter can render with the label's style before stage 2
  re-renders them.
- `<emphasis>` for the epub's italics in dialogue (`Paragraph.Emphasis`
  wraps them in quotes today).
- The Speakers tab still lists variant status, which is meaningless under
  Drama 3.

## Direction across chunks: measured (2026-10-14, port session)

History pinning (Fish's `Drama3History`, now `fish_drama3.history_pinned`,
on by default) does **not** keep a style going. The test was a 1,029-char
passage opening with `[whispering, frightened]`, cut into 5 chunks. Chunk
loudness in dBFS:

| History | Chunk 1 | Chunk 2 | Chunk 3 | Chunk 4 | Chunk 5 |
|---|---|---|---|---|---|
| Pinned | -35.5 | -21.9 | -20.9 | -21.1 | -19.8 |
| Last turn only | -35.5 | -21.9 | -21.4 | -21.4 | -24.3 |

The whisper is gone from chunk 2 on either way. Clips are in
`~/drama3-eval/history/`.

So option (c) is the answer: the port's chunker will repeat a leading
`[style]` on every chunk, unless it's one of `direction.PointTags` (a copy of
that list lives in audio.cpp; keep the two in step). It will also keep
`<whisper>` spans whole, or close and reopen them at a cut. Lectable's
`CarryAcross` does the same for its own bisect retry.

Prefill per chunk from chunk 3 on:

| History | Prefill |
|---|---|
| Pinned | ~390 ms |
| Last turn only | ~297 ms |
| None | ~110 ms |

The port session was asked to check whether pinning still earns its cost on
voice and pace consistency in long undirected paragraphs, and to default it
off if not. They were also asked to confirm the repeated prefix holds the
whisper through chunks 2-5.

**Built in the port (2026-10-14, CPU-checked only):**

- The fish_drama3 chunker repeats a leading non-point-tag `[..]` on every
  chunk. Its tag list is `is_point_tag` in `session.cpp`, mirrored from
  `direction.PointTags`.
- A span cut at a chunk boundary is closed there and reopened in the next
  chunk; an empty reopen is dropped.

Two GPU checks are scripted on the port side and waiting for the GPU:

- the whisper rerun
- pinned vs last-only history on ~1,000 chars of undirected narration

**Decided (2026-10-14):** `fish_drama3.history_pinned` now defaults to
off. The user heard artifacts in the pinned clip, pinning doesn't carry a
style, and it costs extra prefill. Each chunk's history is the latest chunk
only, as before. The pinned-vs-last-only narration comparison was dropped.
The whisper rerun with the style repeated is still to run.

**Verified (2026-10-15):** repeating the style on every chunk holds it.
The same 5-chunk `[whispering, frightened]` passage, with latest-chunk-only
history, measured -35.5, -35.1, -34.5, -34.2 and -34.8 dBFS per chunk, where
chunks 2-5 had been about -21 without the repeat. It transcribes
word-perfect. Clip: `~/drama3-eval/history/style-carried.wav`.

## Direction prompt tuned (2026-10-15)

The tuning ran on the LLM only; nothing was listened to. Results are in
`speakerattr.md`, "Free-form direction". The shipped prompt is the
worked-examples prompt with "only what can be heard", at temperature 0.4.
Emphasis lands on most lines, which the user is fine with.

Still open:

- Listen to directed clips, which needs the clone model in the worker.
- Prune unearned point tags if they sound wrong.
- "sharp" is overused.
- The one cross-sequence bleed seen in the batched LLM.
