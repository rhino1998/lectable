# internal/speakerattr - the LLM passes

Moved out of `../CLAUDE.md`. Every LLM pass and what was learned tuning
it: the model and its benchmarks, speaker attribution, scare quotes,
chapter summaries and the cast, the setting profile, characterization,
emotions, pauses, pronunciation, ambience (music and
lighting) and SFX.

`internal/speakerattr/` is an LLM-driven pipeline, both stages prompting
a general-purpose GGUF model. `Client` (this package) is a thin
caller-side wrapper - batching, prompt-building, JSON-parsing,
malformed-response retry - talking to the model over `llmBackend`
(`LLMGenerate(ctx, systemPrompt, userPrompt, temp, maxTokens)`),
satisfied by `*internal/ttsworker.Manager` in `cmd/server` (proxying to
`internal/llmworker`, which actually runs the model) and directly by
`*internal/llmworker.Worker` in `cmd/benchattr` (a standalone
benchmarking tool with no ttsworker process of its own). Env vars
`LECTABLE_SPEAKER_LLM_MODEL_PATH`/`LECTABLE_SPEAKER_LLM_GPU_LAYERS`/`LECTABLE_SPEAKER_LLM_CTX`/
`LECTABLE_SPEAKER_LLM_MAX_CONCURRENT` are read by `cmd/ttsworker/main.go` (wired
into `llmworker.Config`, since that's the process that loads the model)
- `cmd/server` still resolves the model path too
(`speakerattr.ModelPathFromEnv`), but only to decide whether the feature
is enabled at all (`NewClientFromEnv`: disabled if the file doesn't exist).
`LECTABLE_SPEAKER_LLM_MODEL_PATH` (a `.gguf` file, default
`~/llm-models/qwen3-4b-instruct-2507-q4_k_m.gguf`, see
`speakerattr.DefaultModelPath`), `LECTABLE_SPEAKER_LLM_GPU_LAYERS` (default `-1`: offload everything a GPU
backend can take), `LECTABLE_SPEAKER_LLM_CTX` (each generation slot's share of
the shared KV pool; default `speakerattr.SlotNCtx()` - the largest reply
any pass asks for (8192, the emotion pass), never below 12288 so the
prompt fits too. It was 24576 while attribution replied with a JSON
object for every line, reserving ~2x the KV cache (several GB at 2
slots) for replies that never came close. *Not* `0`/"the model's own
trained context size", deliberately: modern GGUF models increasingly
train at a very large native context (Qwen3 trains at 262144), and a KV
cache sized for that can fail to allocate alongside another
already-VRAM-resident model; set `LECTABLE_SPEAKER_LLM_CTX=0` explicitly if a
given model genuinely needs more), `LECTABLE_SPEAKER_LLM_MAX_CONCURRENT` (default `2`, see
`llmworker.Config.MaxConcurrent`'s own doc comment for why),
`LECTABLE_SPEAKER_LLM_PRIME_STATE` (bool, default `false`, but `run.sh` - so
`make deploy`/`make run` - sets `true`; keeps primed system
prompts as restored KV snapshots in a non-unified cache instead of
resident sequences; ~28% faster attribution, +1.4GB worker RSS, see
`llmworker.Config.PrimeAsState` and `../../llamacpp-go/CLAUDE.md`),
`LECTABLE_LLM_LOG_PROMPTS` (`1` logs every LLM call's full prompt and output
text; off by default, when each call logs a one-line summary - the
full dumps grew the server log past a gigabyte; `run.sh` also starts a
fresh `LECTABLE_LOG_FILE` per run, keeping the previous one as `LECTABLE_LOG_FILE.1`),
`LECTABLE_SPEAKER_LLM_UBATCH` (physical batch size, default 2048 - ~35%
faster prompt reading than llama.cpp's 512; 4096 adds ~1%),
`LECTABLE_SPEAKER_LLM_MTP` (MTP draft length, default 3, `0` off - see
`llmworker.Config.MTPDraft` and `../../llamacpp-go/CLAUDE.md`'s "MTP
speculative decoding"), `LECTABLE_SPEAKER_LLM_NO_THINK` (bool, default
`false`) - appends Qwen3's `/no_think` marker to every attribution
batch's user turn.
**Default model since 2026-09-28: `Qwen3.6-35B-A3B`** (Unsloth
`UD-IQ4_XS`, 16.5GB; a mixture-of-experts with ~3B active parameters,
`speakerattr.DefaultModelFile` - now the `-MTP-` file, 18.2GB, the same
weights plus the MTP head that drafts for speculative decoding: ~190 vs
~105 tok/s generation on a live summary pass). Benchmarked against Qwen3-4B and Gemma 4
26B-A4B through `llama-server` on the same pipeline code (harness kept
out of the repo): attribution on the hand-corrected Spire's Spite ch.
40-58 was 95.4% vs Qwen3-4B's 96.5% (the prompt and speech-tag rules
were tuned on those chapters; the extra misses are Fritz/Bert/Sid
turn-taking - 12 or 60 lines of batch context didn't help); invented
descriptor characters on 10 uncorrected chapters 0 vs 10 lines; a
per-chapter LLM cast list clean (Qwen3-4B's invented references and
pronouns); setting 5/5; emotion labels plausibly better (Qwen3-4B tags
many neutral lines warm/cold); pronunciation and scare quotes identical.
Whole-suite wall time 482s vs Qwen3-4B's 498s (it skips fewer lines, so
fewer follow-up calls); raw prompt/generation 3370/110 tok/s vs 5370/166.
The scare-quote pass's many tiny calls run ~5x slower. It's a hybrid
(Gated DeltaNet + attention) thinking model: `llmworker.disableThinking`
pre-fills an empty think block when a template has `enable_thinking`
(the C-API template renderer can't pass the switch); priming and the
Scheduler work unchanged (checked in-process, both PrimeAsState modes).
Too big to share the 24GB card: `cmd/ttsworker`'s `llmExclusive` (model
file > 8GiB, or `LECTABLE_SPEAKER_LLM_EXCLUSIVE=true/false`) makes every
audio model load unload it first (`audioworker.Config.BeforeLoad`, called
outside the audio worker's own locks so it can wait on in-flight LLM
calls without a lock-order cycle) and its own load unload every audio
model (`UnloadAll`). A load takes ~15s, so the job scheduler keeps a
pool through equal-priority ties until it runs dry, and holds off
non-urgent switches for a minute after each one (`internal/sched`,
`jobs.contextDwell`) instead of ping-ponging. Qwen3-4B
still works via `LECTABLE_SPEAKER_LLM_MODEL_PATH`.

**Previously recommended: `Qwen3-4B-Instruct-2507`** (a genuinely
non-thinking-only checkpoint, not a hybrid-thinking model with
`/no_think` bolted on) - benchmarked on real book content against
hybrid-thinking Qwen3-1.7B/0.6B (with and without
`LECTABLE_SPEAKER_LLM_NO_THINK`): a hybrid-thinking model's own `<think>...</think>`
trace routinely burns through `attributeMaxTokens` before ever producing
the JSON answer, outright losing entire batches on real chapter-length
content (1.7B-thinking lost most of two of three test chapters,
0.6B-thinking lost two of three entirely), and even completed batches
drastically under-attribute real dialogue (defaulting almost everything
to `"Narrator"`/`"Unknown"`). `LECTABLE_SPEAKER_LLM_NO_THINK=true` on the same
hybrid models fixes both (0 batch failures, far higher recall) but is
still a workaround - Qwen3-4B-Instruct-2507 had zero batch failures, the
best recall of everything tested, and correctly resolved a real
"character's name mentioned inside someone else's quote" case that
tripped up 1.7B+`/no_think`, for ~1.5x its wall time, an easy trade
given attribution is an async, one-time preprocessing job. Deliberately
*not* an encoder-based NLP pipeline (BookNLP/ModernBookNLP): every
actively-maintained open-source audiobook generator surveyed (Alexandria,
AudioBard, audiobook-creator, storycast, Castwright) converged on
LLM-prompted attribution, and recent literary-NLP research agrees
(prompted Llama-3 beats BookNLP's own quote-attribution baseline - NAACL
2025).
  1. **Identify** (`Client.AttributeChapter`) - labels each dialogue
     line with a character name or `"Unknown"`, given the chapter text
     and names already known for that book's whole series (below) so a
     recurring character gets one consistent name. Batches of 40
     paragraphs (plus the 3 before each, as unanswered context) mark
     only their dialogue lines `N (who?): …`, and the model answers
     with one plain `N: Name` line each (`N: Guard [role]` for a role
     label), plus ` | he`/`she`/`they`/`it` when the text establishes
     the speaker's pronouns (`splitPronouns`/`NormalizePronouns`,
     `pronouns.go`) - narration is never asked about (it's always Narrator), and
     a narration-only batch makes no call at all. Two batches are in
     flight at a time (`attributeParallelBatches`, matching the
     worker's 2 slots, which the one-attribution-task-at-a-time job
     queue otherwise left half idle), a rolling window so each batch
     starts with every name found so far. Each prompt lists only the
     known characters its own lines mention (a name, alias, or
     distinctive word of either) plus the book's 12 most frequent
     speakers (`Roster.Prominent`, `httpapi.prominentSpeakers`). Measured
     from the server log before this change: a 40-line batch replied
     with ~2KB of JSON (~650 tokens, a JSON object for every line) in a
     median 10.6s, and its known-characters list (median 8.7K chars on
     Spire's Spite) was more than twice the size of the lines themselves. `"Narrator"` is
     reserved exclusively for actual narration/description - never
     spoken dialogue, including a first-person narrator's own line about
     themselves (an earlier version special-cased that as `"Narrator"`;
     dropped in favor of treating every dialogue line the same: a real
     name, or `"Unknown"`). The system prompt also asks the model to
     reuse one exact name for a recurring character even *within* its
     own reply; belt-and-suspenders on that, `canonicalizeSpeakerNames`
     runs afterward, folding any name that's a whole-word run inside a
     longer name already seen into that longer name (e.g. "Vesna" +
     "Captain Vesna Thorne" collapse to the latter) - heuristic, not
     exact (two characters sharing a surname would also collapse), but
     reader-correctable. Two more checks run over each batch's raw
     result: `normalizeBarePronoun` maps a raw speaker value that's just
     a bare pronoun ("I"/"he"/"they"/etc.) to `"Unknown"` (a real,
     observed model failure mode); `disallowNarratorForDialogue` maps
     `"Narrator"` to `"Unknown"` for any paragraph that's actual quoted
     dialogue (`ParagraphInput.IsQuote`), enforcing the "never Narrator
     for dialogue" rule in Go rather than trusting the prompt alone.
     Every raw answer first goes through `sanitizeSpeaker`
     (`refine.go`): a group ("Fritz and Bert", "Bert & Sid", "the two
     clerks" - `IsGroupSpeaker`), a non-answer ("None", "No one",
     "Yes"), a possessive description ("his foe") all become
     `"Unknown"`; prompt markup leaking back out ("Thug [role]", "Name
     (description)") is stripped; a role label loses its article ("the
     guard" -> "Guard"). Each surviving name is folded onto a known
     character when it's one of their **aliases** (`store.Character.
     Aliases`, a JSON column - other names they go by, shown in the
     prompt as "Bert (also called Albert, Al)") or a case variant.
     **Pronouns** (`store.Character.Pronouns`: `he/him`, `she/her`,
     `they/them`, `it/its`, or `""`) are listed in "Known characters"
     too ("Mira (she/her; ...)"), so a "she said" tag can't go to a
     `he/him` character. `AttributeChapter` returns each speaker's
     strict-majority answer across the chapter's lines (votes go to the
     model's own speaker, before speech-tag refinement; never for a
     role label), and `attributeChapter` stores it only while the
     character's pronouns are still `""` - an earlier chapter's value or
     a reader's edit (Speakers page, `PUT .../characters/{id}/pronouns`)
     always wins. Auto Split's widening pass reads them but doesn't
     write them. Known, accepted limitation: first-sight-sticky means a
     mid-book reveal (Spire's Spite's Sid reads "he" until ch. 11/20)
     keeps the early chapters' pronouns - fixed by hand on the
     Speakers page rather than letting attribution overwrite.

     Last, after the speech-tag refinement described next,
     **`resolveIdentities`** (`identity.go`) keeps
     attribution to *picking* a speaker rather than inventing one: an
     answer that isn't a known name or role label but describes someone
     ("One of the thugs", "the older man", "Guard who grabbed Fritz",
     "Woman with pink hair net") becomes a role label from its head noun
     (`RoleLabel`: Thug, Man, Guard, Woman - a title like King/Count stays
     one non-role person), and one with no usable head noun ("the
     other", "one argued") becomes Unknown; a proper name still passes
     through as a new character. Auto Split's widening path applies it
     too. Measured on 10 uncorrected Spire's Spite chapters (999 lines,
     roster stripped of descriptor names): 43 invented descriptor
     characters on 137 lines -> 5 on 13; unchanged accuracy on the
     hand-corrected ch. 40-58. A separate LLM identification pass (a
     per-chapter cast list for batches to pick from) was tried first
     and dropped: no gain on top of this, extra errors (invented
     references, leaked prompt examples, wrong pronouns), a call per
     chapter. The live roster's existing descriptor characters (149 of
     367 on Spire's Spite) predate this and aren't touched by it.

     **Numbered roles** (`roleindex.go`): several unnamed people of one
     type talking in the same conversation are answered `Guard 1 [role]`,
     `Guard 2 [role]`, and stored as speaker `Guard` with the number in
     `paragraphs.speaker_index` (`store.Paragraph.SpeakerIndex`, 0 for
     none; `AttributeChapter`'s `indexes`) - kept beside the name, not in
     it, so renames, merges, the Speakers page and the evals still key on
     `Guard`. The numbers are slots within a conversation, not
     identities: "Guard 2" in the next scene is someone else. Each
     number from 2 up has its own voice (`narration.MemberVoice`): the
     role's own casting - same instruct, reference line and design model
     - rendered under another seed, which listening tests (2026-10-02,
     BreezeTTS design cloned through Higgs) showed comes out as a
     different person; blending reference latents and pitch/formant
     scaling of the role's clip didn't. Number 1 and unnumbered lines
     keep the role's voice. A sibling is never stored: its preset id is
     derived (`<presetID>~<n>-<hash of the casting>`), so its reference
     clip, emotion variants, audio and voice_id key on it like any
     preset's, a recast gives it a new id, and nothing lists it on the
     Voices page. A role voiced from a bare instruct or through the
     instructed-narrator modes has no siblings. Every per-paragraph
     resolution takes the number (`ForParagraph`/`ForSpeaker`,
     generation, the chapter view, exports, the used-voices bulk
     action), and the readiness joins carry one `SpeakerVoice` row per
     number (`speaker_voice.speaker_index`). `roleNumber`
     takes a number off an answer only when it is a role (marked, a known
     role, or a description like "the second guard"; "Unit 7" stays a
     name, as does "the Old One" - a trailing "One" needs a "Two"
     sibling), and a role name that carries its own number - a chapter's
     cast lists "Thug 1"/"Thug 2", and older rosters hold such characters
     - comes out as that number of the plain label. A number is dropped
     when a tag refinement moves the line to someone else, is stored only
     on a line whose character is a role, is cleared by every other write
     of the speaker, and is shared by the quotes of one source paragraph
     (`shareParagraphNumbers`). Batches run side by side, so the one after
     a seam restarted at 1 for the guard who was 2 just before it (Arise:
     Alpha ch. 38, the same dying thug as #2 then #1): where numbered role
     lines end a batch and that role speaks early in the next, the next
     batch is asked again with the earlier lines shown as settled
     (`seamContextLines`), and only its answers for that role are taken.
     Auto Split's widening path drops numbers. The model mostly numbers
     what the chapter's cast already numbers, so recall follows the
     summary notes. Measured with the preview on 23 chapters (2026-10-02,
     against the build before it): 1857 vs 1823 of 2025 lines matching the
     stored speakers, hand-corrected lines 28 vs 22 of 58, 29 numbered
     lines - nearly all of the gain from one fix found on the way: a role
     label no longer folds into a longer name (`canonMapFromPool`;
     "Guard" was folding into the roster's "The storm guard sergeant", who
     took all 21 guard lines of Spire's Spite ch. 62).

     Then **`RefineWithSpeechTags`** corrects the whole chapter's result
     against the text itself, with no LLM call: (1) a quote with an
     explicit named tag - right after it ("…,” Bert said" / "said
     Bert"), else a lead-in before it in the same paragraph ("Bert
     said, “…”", "Sid smirked and Bert grinned saying, “…”" - the last
     *subject*, not the last name: objects, lists after an object,
     possessives, and names followed by "and" don't count; a name
     followed by "who" does) - is that character's, when the name (or
     one word of it, or an alias) resolves to exactly one valid known
     character (`SpeakerNameResolver`); (2) an untagged quote continues
     the previous quote's speaker within the same paragraph, unless a
     pronoun/joint tag or another character starting a sentence in the
     narration between them ("Bert rounded on Fritz.") says otherwise;
     (3) an untagged quote opening a paragraph right after one whose last
     quote never closed continues that speech and keeps its speaker (the
     model took each paragraph of a multi-paragraph speech as a new line);
     (1b, after (1)) an untagged quote whose paragraph has an action beat
     right next to it with a named subject ("“Let’s get it done.” Elijah
     nodded.", "Staavo nodded, then offered up a loop of cable. “Tie
     it…”") is that character's (`BeatTag`, `beattags.go`) - not with a
     pronoun tag of its own, a possessive subject ("Jason’s companion
     said"), a perception verb ("Jake heard Minaga yell"), or, before the
     quote, a verb of speaking (that's the previous quote's own tag). Nor
     in a first-person book (the narrator's untagged lines outnumber
     anyone's; a beat there is as often the listener), on a common
     sentence-opening word or a "The …" epithet ("One thing was certain"
     is not "Old One"'s, "The Feather" is no one Feather), when the
     sentence also has "I"/"me"/"my" or another character in it, or on a
     state, thought or beat that didn't happen ("was", "knew", "didn't").
     Those were added after an audit: the first retag run over the library
     (`POST .../speakers/retag`, with the looser rule) changed 6.4k named
     lines in books nobody had corrected, and of 200 sampled 83 were right,
     64 broke a right line, 42 only moved between two entries of one
     person, 9 stayed wrong - far below what the corrected books promised.
     Replayed on those 200 the tightened rule still makes 29 of the 83 and
     4 of the 64; the other changes were restored
     (`POST .../speakers/restore`, listed by `cmd/tageval -undo-from`).
     `cmd/tageval` measures these rules against readers' corrections:
     on 96k dialogue lines of 20 fully corrected books (2026-10-15, the
     stored attribution wrong on 9.3%) the whole rule set run over the
     stored answers changes 1,788 lines for +1,077 net with the first,
     looser beat rule - beat +352 (tightened: +325 of 498 changes, 88%),
     named tag +462, continuation +268, continued speech -5 - so those
     books' stored attribution predates or missed the older rules too. A
     lowercase role-noun subject ("the operator said" -> Operator) was
     tried and rejected there: right 45% of the time.
     Measured on Spire's Spite before adding it: of 624 quotes with an
     explicit named trailing tag the model credited 42 to someone else,
     and 25% of multi-quote paragraphs had mixed speakers. Tuned against
     chapters a reader had already hand-corrected (whose remaining
     changes were overwhelmingly still-wrong lines) and spot-checked on
     uncorrected ones (~29/32 continuation changes right; misses were
     verbs missing from `speechVerbs`). Only ever changes lines the call
     itself attributed. **`RefinePronounTags`** (`pronountags.go`) runs
     right after it: a line whose own trailing tag is "he said"/"she
     asked" but whose answer's known pronouns (roster, then this call's
     votes) are the other ones goes to the most recent matching
     character before it - named in narration or speaking, never a
     first-person narrator - else Unknown, with untagged quotes after it
     in the paragraph. Replayed over ten saved eval runs
     (`cmd/attrreplay`, which re-scores `data/attreval-runs/*.json`
     offline in seconds, no GPU): 45 lines changed, none for the worse.
     A turn-taking rule (a third character answered inside an A, B, A
     exchange goes to B) was tried the same way and dropped: 39 fixed,
     179 broken - three-person scenes, and exchanges whose earlier turns
     were already wrong. Its pronouns come from the roster, overridden per chapter by the
     summary notes' pronouns for named (non-role) cast members, keyed by
     the roster's spelling (`Roster.WithCast`): the roster keeps one
     value per character, and Spire's Spite's Sid ("he" until a reveal,
     then "she", roster `she/her`) had three of five early "he said" lines
     moved to someone else (`attrreplay -stored spire:1-39`, read against
     the text). Notes and roster agree on 500 of 504 named characters;
     the override is neutral on the ten eval runs (`attrreplay
     -castpronouns`). Roles only fill gaps (a role label can be several
     people), and filling gaps adds nothing measurable - no tagged
     contradiction in the eval set involves a role. The chapters Sid's
     case came from predate the summary pass (no notes), so it applies
     once they're re-attributed. **Rejected: a scene-presence check**
     (drop a scene's present member never named, aliased or referenced in
     its paragraphs - `cmd/notescheck` audits notes this way): on the 19
     eval chapters 12 of its 19 would-be drops speak in that very scene
     (pronoun-only narration, "I" narrators), and the case that motivated
     it (Kev given Dave's lines in Arise ch. 11) is a scene that is too
     coarse, not a wrong one - Kev is on the phone for its first half. Group names are also refused outright by the
     manual speaker PUT and merge, and forced to `"Unknown"` before any
     attribution result is persisted.
  2. **Describe** - retired 2026-10-04, with its job kind, routes, UI
     and the `describes_characters` column (migration 00005). It was a
     separate call tagging which narration paragraphs describe a
     character, 5 paragraphs at a time (a folded `"describes"` field in
     `AttributeChapter` had the model blanket-tagging every line touching
     a salient character; precision rose from ~6% at batch 40 to ~80% at
     batch 5). At that size a 60K-paragraph book took ~12,000 calls, 159
     of 254 speakers with 20+ lines got two tags or fewer, and many tags
     named whoever the line mentioned rather than who it described. See
     "Does anything still need the description pass?" below for what
     removing it cost.

**Scare-quote tagging** (`Client.ScareQuoteChapter`) marks each candidate
"(?)" in the batch and asks for a "spoken"/"not spoken" verdict on every
one (`parseScareQuoteVerdicts`; a verdict with a miscopied line number is
taken by position when the reply has one verdict per asked line, in order -
Qwen3.6 answered a batch's only candidate, 214, as "14" at every
temperature up to 1.0, so retries couldn't fix it and the chapter's pass
failed forever) - the earlier prompt asked the model to
list the "very rare" scare quotes in an unmarked batch, written for
Qwen3-4B's over-tagging, and with the candidate filter already ruling out
dialogue that caution only cost recall. The prompt defines "not spoken" by
kind (terms/labels/signs/menu choices, sarcasm, thought/held-back/mouthed
words, reported or paraphrased words, translations) and makes "spoken"
need the sentence to show the words said aloud - the model otherwise
assumed a menu choice ("selecting “lethal”") was said. On Arise: Alpha ch.
0-37's reviewed candidates (`attreval.py sq`): one run found 48/112 before,
98/112 after, 0 of 8 embedded real-speech lines flagged either way, and no
new flags on 11 Spire's Spite / Jake's Magical Market control chapters.
It only asks about
quoted spans shaped like a scare quote (`scareQuoteCandidates`: an
Inline segment whose preceding narration ends mid-sentence on a word, or
a short quote with no closing punctuation that the sentence continues
past) - 1.6% of the library's 39K quoted spans, holding every flag any
run produced except a spoken line with a nested quote inside it, and no
spoken dialogue (which opens its paragraph or follows a sentence end,
comma or colon). Batches are 40 paragraphs, 2 in flight; a batch with no
candidate makes no call, and flags on non-candidates are dropped. On
Qwen3.6 that's Qwen3-4B's old speed (5-paragraph batches, one at a time)
with ~8x the scare quotes found - see `scareQuoteBatchParagraphs`. The
library's stored flags (30) came from the old setting and undercount.
A run is additive (`httpapi.scareQuoteChapter`): it only adds flags, so a
re-run never hands a scare quote it happens to miss back to attribution;
a manual edit or the pass's Reset clears.
It is its own
`jobs.KindScareQuote` task per chapter (sets `Passes.ScareQuote`), and
attribution for a chapter depends on it
(`jobs.Manager.scareQuoteDependency`, which lazily creates one if the
chapter isn't tagged yet). A failed scare-quote run keeps blocking them -
it's recreated on the next dependency check rather than letting
attribution run against unflagged scare quotes; the pass is sampled, so a
retry is expected to succeed.
  3. **Characterize** (`Client.CharacterizeVoice`) - given a sample of
     a character's own attributed dialogue plus narration about them
     found by search (see "Retrieved evidence" below), asks the LLM for a short "Speak as a..." voice-casting
     instruction (gender, age, tone, speaking style), taking the
     character's `Pronouns` as the authoritative gender signal
     (they/them and it/its commit to no gender word) - saved as
     `store.Character.Summary`, shared across every clone model's own
     voice preset for this character (see "Per-clone-model voice
     assignment" below). The prompt (rewritten 2026-09-30) casts the
     character's *resting* voice in audible terms only (gender + age
     group, pitch/pace/timbre, one accent, one or two usual traits) -
     never their emotional range, since every line clones from the one
     reference clip and emotion variants are rendered from it - and has
     the model write an `evidence` field (ignored by the parser) before
     casting. Accent order: an explicit textual signal, then the setting
     accent, then the dialogue's idiom; slang/roughness is a register,
     not an accent. The refLine is a calm, in-character line with no
     story names, exclamations or apostrophe respellings (the design
     model misreads "'ere"/"yer"). `Store.QuotesForBooks` spreads its 20
     lines across the character's whole run, preferring lines of 4+
     words (it took the first 20: Spire's Spite's Fritz, 8000 lines,
     was cast from his introduction scene). Measured on 20 characters
     across three books against the old prompt (a harness calling the
     worker's `/llm/generate` directly with the queue drained): 0 vs 3
     unparseable replies; no leaked archetype rules (the old one gave
     "the Guard" a pirate's "seafaring drawl" and an orc's Cockney); no
     visual detail or emotional range in the instruct. A character's own
     recurring national idiom (British "mate"/"bloody"/"arse", American
     "dude"/"gotten", Southern "y'all") outranks the setting accent - Arise's
     cast is mixed, and Steve is British whatever the setting says.
     **Co-stars**: the user turn lists "Already cast" - up to 5 characters
     sharing the most chapters with this one (`Store.CoSpeakersForBooks`,
     one SQL query) who already have a `Summary`, roles and invalid ones
     left out (`httpapi.characterizedCostars`) - and the prompt asks for a
     voice listeners can tell apart from each by pitch, pace or texture.
     Measured: Spire's Spite's Fritz and Bert, both "gruff, gravelly,
     middle-aged working-class British" when cast alone, came apart
     (Bert "lighter, higher, brisk, bright"). Shown whole instructs, the
     model once returned Mizhu's casting word for word for Sessi (a god,
     cast as "a young male swampkin"), so a reply sharing more than 0.6
     of its words with a co-star's (`wordOverlap`, Jaccard;
     `maxCostarOverlap`) is retried - honest castings topped out at 0.44
     over 276 pairs. The retry drops the copied co-star from "Already
     cast" and the last attempt shows none, checking only the co-stars
     shown: re-asked with the same prompt the model copied again
     (in one bulk run Underverse's Captain/Doctor/Dwarf/Heph each failed
     permanently, at 0.8-1.0 overlap with Captain West/Mick/Bob/Oren),
     and it can't copy wording it isn't shown.
     **Voice clues** (2026-10-04, `VoiceClues`, `voiceclues.go`): the user
     turn lists up to 8 narration sentences on how the character sounds or
     how old they are, found without the model - the narration following
     their own lines (`Store.SpeechTagsForBooks`; a first-person narrator's
     tags never name them) and narration naming them up to 120 characters
     before a voice word or stated age. Scored: a statement of their usual
     voice ("his normally steady voice") +4, a lasting quality (an
     adjective on the voice, an accent, an age) +2, one moment's delivery
     (lowered, "in his voice", cracked, whispered) -2; kept at 2 or more.
     A sentence giving the voice to another name or to the other gender's
     pronoun ("her voice", for a he/him character; "its" always) is
     dropped. None of this reached casting from a 20-line sample: of 163 narration paragraphs naming Sid
     beside a voice word, 0 were tagged, and the same held for Bert (73) and
     Fritz (268, 2 tagged). Found on the live library: Sid's "gruff, manly voice" put on
     as a disguise, Fritz's "slight noble-born accent" (he was cast
     working-class), Steve's "English accent... push my northern". It finds
     nothing for characters the text never describes by ear (Bert, Jake).
     A casting that uses the other gender's words than `Pronouns`, or
     none of its own (`genderContradicts`), is retried with a reminder;
     the last attempt is kept either way, since the pronouns can be
     what's wrong. A mixed setting accent is shown as its first with the
     rest aside (`castingLines`): shown "mixed: British, General
     American", Jax came back with "a mixed British-General American
     accent".
     **Stated ages** (2026-10-06, `StatedAges`, `voiceclues.go`): the user
     turn lists up to 3 of the character's own lines giving their age
     ("Their age, in their own words"), each with the line it answered -
     replies to someone asking how old they are (`ageQuestion`, whatever
     the reply says: "Old enough that I am unsure of your calendars"),
     then lines stating it unasked (`selfAge`: "I'm nineteen,"; the number
     must end its clause or be followed by "years", and "I was twelve" is
     left out). Read from every line they speak with the dialogue line
     before it (`Store.SpokenLinesForBooks`); roles get none. Underverse's
     Yen says "I'm seventy-six, this past spring" once in 393 lines:
     rarely in the quote sample, naming nobody, so no search for her name
     found it, and dialogue, which voice clues don't read for ages - she
     was cast "a young woman... high pitch... small, ethereal elf" and
     sounded like a child. 7 lines match across the library (76839 lines
     of dialogue), all real ages. The prompt places a long-lived kind by
     their life among their own people rather than the number (an elf of
     76 serving beside younger adults is an adult in her prime; the
     400-year-old Hans kept coming out "elderly"), gives a grown adult an
     adult's pitch unless the material describes a high one, and takes
     size from the material, never from the kind's name.
     **Retrieved evidence** (2026-10-04, `httpapi.castCharacter`): the
     material is found by search, not only sampled.
     - *First mentions*: the first 3 narration paragraphs naming the
       character, and *statements about them*: narration where the name is
       followed by a trait phrase ("Bert was a", "Zac, a", "Sid's voice" -
       `TraitPhrases`/`TraitStatements`), 14 spread over up to 400 matches.
       Both come from the `paragraphs_fts` trigram index
       (`Store.NarrationMatching`); it matches substrings in any case
       ("Albert was a"), so `TraitStatements` re-checks each hit. Looser
       phrases ("was the", "wasn't", "had a") mostly found plot and were
       dropped.
     - *Idiom words across all their lines* (`IdiomCounts` over every line
       they speak): of Jax's 3786 lines 99 say "mate" and 7 "aye", and a
       sample that happened to hold an "aye" cast him Scottish.
     Previewed on 12 characters (`attreval/casteval.py`, `POST
     .../characterize/preview`, which stores nothing): Fritz "young man,
     refined upper-class British" (was middle-aged, working-class), Sid a
     high, bright voice with the gruff one named as affected, Bert young
     and stocky, Steve Northern British, Jax an ordinary young British man,
     Zac Australian, Jonas Southern, Ingrid Danish. Still wrong: Scylla (a
     "Greek demigoddess" only in someone's dialogue) and Anders (Danish
     only by his family) come out British, and Hans, centuries old but
     young-looking, "elderly".
     **Dossier - tried, removed.** A first call distilled the same
     evidence into a profile (gender, age, origin, kind, build, voice,
     manner, temporary - each of the middle five a value with a quote that
     had to be found in the material), and casting then read only the
     profile and the lines. On the same 12 it fixed two (Jax, Steve - both
     since fixed in the single call) and broke four: Bert got Fritz's
     accent from "Bert said, copying Fritz's slightly aristocratic
     accent" (with a prompt rule naming that exact case), Fritz's age came
     from "thirty feet from the front door", Anders lost his accent and
     Hans turned elderly. A second call told to trust the first compounds
     its misreadings, where the single call's `evidence` field does the
     same job in view of the material. Migrations 00004 and 00006 add and
     drop its column.

**Temperature** (2026-10-04): casting is greedy (`characterizeTemp` 0,
was 0.3; a retry still samples, `retryTempBump`). The same 12 characters
cast three times each from identical material (`casteval.py --temp`):
at 0.3 the age group, accent or pitch changed between runs for 10 of 12
(Bert Southern American once, Ingrid British once, Hans's accent three
ways) and castings shared 0.48 of their words; at 0, 4 of 12 and 0.75,
with Scylla's Greek accent and Fritz's aristocratic one in every run. Not
fully repeatable: Sid still came out once with the gruff voice she puts
on.

**The accent step** (2026-10-11, `accent.go`, `Client.decideAccent`): the
accent is decided in a call of its own before the casting call, from the
same material plus what the lookups found, and the casting is held to it
(`accentSection`; a casting in another accent is retried with a reminder,
`castsAccent`). The reply must quote the one sentence the accent rests on
and say whose it is before naming the accent; one resting on a sentence
about someone else is refused, and a step that fails casts without a
decided accent. Why: asked for everything at once, the casting call did
not hold to its accent rules - shown "Ingrid, a Danish archeologist" it
cast her British in 4 of 6 runs, and Scylla ("the insane Greek woman")
British in 6 of 6. With the step, over 10 characters x 3 runs: Ingrid
Danish 3/3, Scylla Greek 3/3, Jonas Southern 3/3 (was 3 of 6), and every
casting used the decided accent. Still wrong 1 run in 3: Miryam, whose
"there was Miryam's Italian accent" is a stranger's accent she had
described, read as her own. Age group still varies between runs.
`LECTABLE_CAST_ACCENT_STEP=false` turns the step off; the preview route's
`accent=0`/`1` does it for one call and returns what was decided.

**Lookups** (2026-10-04, `Client.lookupSection`; every characterization
runs them, `.../characterize/preview?lookup=0` / `casteval.py --no-lookup`
cast without): the model searches the book for what the
gathered material doesn't state, then casts with what it found added to
the turn. Three designs, each on the same 12 characters at temperature 0:
- *Lookups offered inside the casting call* (reply `{"lookup": ...}` or
  the casting, 2 rounds, re-sending the growing turn): it searched
  categories - "age", "origin", "size" - which the substring index
  answered with "page" and "original"; fixed nothing and broke two
  (Scylla searched "Greek", got 5 hits, and was cast British).
- *The same with whole-word matching and a prompt demanding concrete
  words*: across 60 castings it never searched once. The casting
  instructions outweigh an optional detour.
- *Plan, then cast* (adopted): one call whose whole job is the search
  list (`lookupPlanSystemPrompt`: the character's name plus one concrete
  word the book would print - "lad", "accent", "Danish", "rumbled" - up
  to 6), each answered with up to 6 excerpts where both appear as whole
  words in one paragraph (`httpapi.lookupExcerpts` over
  `Store.ParagraphsContainingAll`; the index matches substrings, so hits
  are re-checked), then the ordinary casting call with a "Searched in the
  book" section. A dialogue hit that is a question is shown with the
  next line of dialogue when someone else speaks it within 3 paragraphs
  (`store.LookupMatch.Reply`, "[Yen: I'm seventy-six...]"): the question
  holds the name and the word, the answer neither. Since 2026-10-11 each
  lookup is also searched **by meaning** (`httpapi.semanticExcerpts`,
  `internal/semantic`): up to 3 more excerpts, from the 60 paragraphs of
  the series closest to the lookup's words that name the character as a
  whole word, are closer to the lookup's words than to the name alone
  by `lookupSemanticLift` (most of what is closest to "Loth old" is just
  short lines naming Loth), and that the exact search didn't already show
  - the exact search needs the book to print the very word asked for. Not yet
  measured on casting results; on retrieval alone, "Loth's voice" found
  "Loth's raspy voice" three times where the trait phrases found only
  "Loth's voice called from...". Nothing is added for a book that hasn't
  been embedded. Two runs: Anders Danish in both (British in every run
  without lookups), Scylla Greek, Steve Northern, Jonas, Fritz, Bert,
  Ingrid and Zac as without; 2 of 12 changed between runs (4 of 12
  without). Jax came out General American in one of the two, and Hans
  still "elderly". One extra LLM call per character.

**Does anything still need the description pass?** (measured
2026-10-04, before removing it) Casting: 15 characters previewed with and
without the tags differed no more than two runs of the same prompt do -
one real loss (Dave's "barrel-chested"), one gain (Scylla's Greek accent).
Attribution, whose "Known characters" list carried each one's first tagged
description as a blurb: Arise 1107 -> 1102 of 1157, Spire's Spite 198 ->
197 of 201, hand-corrected lines 79 -> 79 of 114, against a re-run of the
baseline at 1106/197/79. Blurbs taken from search instead (each
character's first trait statement) were worse than none - 1092, and 74
hand-corrected: "Shotgun Man" took six of Mikhail Fedorov's lines - and
cost ~90s a chapter in Arise (one index search per known character). So
the pass was removed outright and attribution's roster carries no blurbs
beyond the summary notes' own.

**Characterization and voice provisioning happen lazily, not during
attribution.** An earlier version ran both eagerly, inline, as part of
`attributeChapter` itself - this had a real, since-fixed deadlock (a
`KindSpeakerCharacterization` task nested inside an already-running
`KindSpeakerAttribution` task, both drawing from `internal/jobs`' single
shared LLM slot, so the nested one could never dispatch) and made a
chapter with many brand-new characters attribute much slower.
`attributeChapter` now only runs LLM attribution and
`Store.UpsertCharacter`s each new name, with no voice-related side
effects; characterizing a character and auto-creating + assigning their
voice preset (`Server.provisionCharacterVoice`, using `Instruct`
verbatim on an auto-created custom preset) happens the first time that
character's voice is actually *needed for generation* -
`jobs.Manager.provisionMissingCharacterVoices`, called from
`paragraphsNeedingGeneration`/`enqueueParagraphRegenerate` before
resolving each paragraph's voice, via a `jobs.CharacterVoiceProvisioner`
callback (`internal/jobs` has no `Store`/`Speaker` access of its own). A
reader can still review/reassign a character's voice afterward through
the ordinary custom voice preset endpoints - this just seeds a
reasonable starting voice instead of leaving every character on a blind
manual pick. `provisionCharacterVoice` also serializes itself per
characterID (`Server.lockCharacterProvision`, a keyed mutex) across
every caller and clone model - `RunCharacterization`'s own per-characterID
dedup only covers the LLM call, not the preset-creation step after it,
so without this lock two concurrent provision attempts for the same
character could each create a duplicate preset.

**Per-clone-model voice assignment**: a character's *identity* (name,
`Summary`) is one row, shared series-wide (below) - but their *assigned
voice* is tracked separately, one `voice_preset_id` per `(character,
clone_model)` pair (`Store.CharacterVoiceForModel`/`CharacterVoicesForModel`/
`SetCharacterVoice`, backed by `character_voices`), so a character can
be given a different preset under `audiocpp-qwen3-0.6b` than under
`audiocpp-higgs-4b`. A model with no preset of its own for a character
borrows the one they have under another model (the default clone
model's first): a preset is a reference clip any model clones from, and
switching a book to Drama 3 used to render every character a new voice
from the same casting, which sounded like someone else (2026-10-15).
`voices.InstructedCloneModel` doesn't borrow - its unassigned characters
read in the narrator's voice by design. Every lookup/provisioning call is keyed
by the book's own clone model (`store.Book.CloneModel`, via
`narration.BookCloneModel`/`EffectiveCloneModel`); `provisionMissingCharacterVoices` checks any character it
touches - new or recurring - who doesn't yet have a voice for that
model, so switching a book's (or series') narrator model doesn't
silently leave recurring characters back on the book's own voice.
`handleSetCharacterVoice` resolves the same book's clone model
server-side. `httpapi.Server.Speaker` is `nil` (attribution endpoint
reports 503, lazy characterization stays a no-op) if
`LECTABLE_SPEAKER_LLM_MODEL_PATH` is unconfigured - everything else works
regardless. Attribution is explicitly triggered per chapter (`POST
.../attribute-speakers`); characterization/voice-provisioning happens
the first time it's needed, or explicitly via the Speakers page's
"Regenerate" button. Every distinct system prompt this package generates
against (`speakerattr.SystemPrompts` - attribution's, characterization's,
and the emotion/pronunciation ones below, all fixed
constants, byte-identical across every call) is decoded once, at
model-load time, into its own permanently-resident sequence in
`llmworker.Worker`'s shared `Context` (see above) - priming every known
prompt up front is what makes it safe for several concurrent
`LLMGenerate` calls to each copy from the same already-decoded prefix
into their own slot without racing to prime it first.

**Roster review** (2026-10-08, `rosterreview.go`, `httpapi/roster_review.go`,
`jobs.KindRosterReview`): a pass over a whole roster that proposes fixes
for the reader to accept or skip - it never changes the roster, since a
merge deletes the merged name's voice and can't be undone. Audited first
on the live library (17 rosters of 30+ characters, 3,511 characters): 407
pairs of non-role names sharing a distinctive word, 23 aliases that are
another character's name, 99 lower-case names not marked as roles, and a
handful of kind words that had taken several people's names as aliases
(Underverse's "leader": Balthazar, Darius, Dwarrow, Musiah, Vectus).
Chapter co-occurrence doesn't tell duplicates from different people -
"Centurion Primus Restun" and "Primus Restun" both speak in 61 chapters
and are one man, Darakin and "Legionnaire Daralen" share 15 and aren't -
so pairs need the text read.
- *Rules* (`RosterRuleFixes`, no model): a name that names nobody is
  invalidated; a description is marked a role; a single word the book
  mostly writes in lower case (`WordCaseCounts` over the whole text) that
  holds two or more aliases loses them and becomes a role; an alias that
  is another character's name is removed when their pronouns differ or the
  holder is a bare title ("Baron", also called "Joshua"). The lower-case
  test counts uses: with a plain "does the book ever write it lower case"
  (`LowercaseWords`), Underverse's protagonist Jack (27,200 lines; the
  book has a "jack" somewhere) was proposed as a role.
- *One of a kind* (2026-10-14, `OneOfKind`): "One of the [Knights]",
  "One of the Gnolls" - names attribution kept from before `RoleLabel`
  turned such answers into role labels, so they never went away (TWI had
  ~300, "One of the [Knights]" 123 lines). Each is proposed merged into
  the role it is one of - a role first, singular spelling first, brackets
  or not ("Knight", "[Knight]", "Knights") - or, with none, renamed to it
  and marked a role (`rename_role`, "One of the Fraerlings" ->
  "Fraerling"; qualifiers kept: "Summer Knight", "Ruler of Khelt").
  "One of the others"/"the crowd" name no kind and are left alone.
- *Brackets* (`Unbracketed`): LitRPGs write classes in brackets, and
  attribution sometimes kept them ("[Mage]", "Gnoll [Druid]"). A
  bracketed name is proposed merged into the entry spelled without them
  (or its singular - "[alchemists]" into "Alchemist"), else renamed
  (`rename`, role flag unchanged).
- *Aliases the summary pass records* (`httpapi.recordCastAliases`, from
  `ResolveCastIdentities`'s check of a chapter's cast against the
  roster) are written straight onto the roster, not proposed - and on
  TWI overnight (2026-10-14) about half of 73 were wrong, many a named
  person filed under a kind ("Poinr" under "Gnolls", "Miihey" under
  "Elf"). So nothing is recorded onto a role or a name for a kind of
  person, and an alias a roster review took off (an accepted
  `remove_alias`/`split_alias`) is never put back - "Sprigaena" had
  returned to "Elf" that way.
- *First names* (`shortNameTarget`, dedupe's rule 4) are proposed, not
  merged - except a rank ("Baron" into "Baron Sanguis") or a name inside
  "X of Y" ("Issa" into "Priest of Issa", a god and his priest).
- *Pairs* (`RosterPairs`): one has the other's name as an alias, or one's
  distinctive words (titles and ranks dropped) are all among the other's.
  Possessives ("Daana's mother") and numbered names are never paired. Up
  to `maxRosterPairsPerRun` (40) a run, most lines first; a pair decided
  once is never asked again.
- *Unwritten names* (`RosterPair.UnwrittenName`): when the book never
  writes one of a pair's names on its own ("Centurion Restun" not at all,
  "Legionnaire Daralen" only as "Legionnaire Primus Daralen"), attribution
  made the name up - a merge is proposed without asking the model.
- *Verdict* (`Client.JudgeSamePerson`, temperature 0): `PairPassages` finds
  up to 3 paragraphs naming both and 3 naming each alone (a name inside
  the other's counts only as the longer one; the alias in question is
  taken off its holder first, or every passage naming one would name
  both), and the model answers same / different / unclear with the words
  that show it. "Same" stands only when those words are in a passage that
  names both: shown "I am Othair, my lord" the model called Othair, a
  butler, the same as the god Darakin whose alias he was. "Different" on
  an alias pair proposes splitting the alias off (`split_alias`: removed,
  made a character, the holder's lines re-judged for theirs); anything else is stored
  `ruled_out`.
- *Names used alike* (2026-10-11, `httpapi/roster_usage.go`, needs the
  semantic index): for a pair made by its names (`RosterPair.SharesName`,
  not an alias pair), the embeddings of the paragraphs writing only the
  one name are compared with those writing only the other
  (`semantic.Alike`: cosine of the two mean vectors, each less the
  series' mean). At `rosterAlikeMin` (0.6) or more, with 3+ paragraphs a
  name, the model's "same" stands without a passage naming both - two
  spellings of a name ("Dr. Zheng", "Doctor Zheng") never share a
  paragraph, and the model answers many such pairs with two passages run
  together or with its reasons, which the word-for-word check threw out.
  The proposal then shows a passage for each name. A name that could be
  short for several entries counts only toward the one it reads closest
  to ("Sanguis": Baron Sanguis 0.70, his son Prince Sanguis 0.61;
  "Zodiac": Lord Zodiac 0.84, his daughter Alicia Zodiac 0.61) unless
  those entries read alike themselves; a pair that loses that way is left
  unsettled, not ruled out, and asked about again next run. Not used for
  alias pairs: names sharing no word read 0.04 to 0.43 alike whether one
  person (Oracan/Eto 0.16) or two.

Measured on a copy of Underverse (530 characters, 12 books;
`cmd/rosterreview`, the live worker shared with a running queue): 3s
without the model; with it 14 calls and 39s in all. 63 proposals: 19 rule
merges (15 first names, 3 unwritten names, 1 markup), 21 alias removals,
17 roles, 1 invalid name, 3 model merges and 2 model alias removals
(Darakin's "Othair", Oracle's "Hellenica"); 11 pairs ruled out. Read by
hand, not scored against labels. Of the model's three merges two are
right (Restun; "Sanguis" into "Baron Sanguis" on "Baron Sanguis—no, just
Sanguis now") and one is wrong: "Lord Barabarattas" into "Mal", on "City
Lord Barabarattas was going to go apeshit..., and Mal loved it" - a
passage naming both that shows two people. The run before had called the
same pair unclear, so verdicts aren't repeatable across runs either.
Doubtful among the rules': "Grey" into "Grey Rat", and "Centurion Restun"
proposed into both Restuns (one person - accepting either makes the other
moot). Not found: anything needing more than six passages or knowledge of
the plot - what the Claude subagent (`.claude/agents/roster-fixer.md`)
files through `POST .../roster-review/proposals`. Its prompt is not in
`SystemPrompts`.

Names used alike, measured 2026-10-11 on a library backup from before any
merge (Underverse, Arise, Expeditionary Force, The Legend of William Oh;
`cmd/rosterreview -embeddings`, one run each with and without the index,
the same model): pairs the reader had gone on to merge read 0.46 to 0.91
alike (under 0.5 only a first name and its full name, which the rules
propose), different people sharing a rank or surname up to 0.68 with
most under 0.5 ("Mark Wyrd"/"Frederick Wyrd" the 0.68 - never a pair,
each has a word the other lacks), people sharing nothing under 0.35. The
model's merges went from 14 to 20, all 20 right on reading, and four more
pairs were left for the next run instead of ruled out; ruled-out pairs
fell from 53 to 43. Still missed: pairs the model itself calls unclear or
different ("Zodiac"/"Lord Zodiac" reads 0.84 and the model says two
people), and names written in fewer than three embedded paragraphs ("Lt
Charles"). Loading a 19-book series' vectors cold adds ~30s to a run.

**Front and back matter** (2026-10-11; `matter.go`, `Client.FindMatter`,
`httpapi/matter.go`): one call per book over its chapter list - each
section's number, title, length and the start of its text - answering with
the sections that aren't story, each as its number and title. Applied at
once (a mark is reversible and deletes nothing); see the `.../matter`
entry in `api.md` for what reads it.
- **Why a model**: the fixed title list (`httpapi.notStory`) matched 223
  of the 707 matter sections in this library's 86 epubs. The rest is each
  publisher's own ("Don't Miss Out", "Join the Conversation", "LitRPG!",
  "Backyard Starship Wiki" - 139 distinct titles).
- **What the prompt shows** (each measured with the guard below, against
  hand labels: 707 matter sections, 64 synopses of earlier books, 3,629
  story chapters): titles alone skipped 685 of 707 - the misses all title
  pages named for the book or series ("Expeditionary Force", "Citadel"),
  which read as a named chapter. Titles and lengths skipped 703 but kept
  24 synopses for being long and lost four chapters whose titles are epub
  ids ("cA10", opening "Chapter 84: Home Visit"). With the opening text
  too: 703 of 707, 61 of 64 synopses, 2 story chapters lost.
- **Title beside each number**: answering with numbers alone, the model
  once named the section before the one it meant (a play's last scene for
  the Gutenberg licence after it). Asked to copy each title beside its
  number, it didn't in any book, and `parseMatter` drops an entry whose
  title isn't its section's. The model sometimes copies the rest of the
  line too ("Thanks [1985 chars] - Hi everyone..."), which is cut before
  comparing. With this: 702 of 707, 63 of 64, 1 lost.
- **The guard** (`matterGuard`, code, after the answer): in a book with
  numbered chapters (85 of 86) a section is never matter if its title
  says chapter, prologue or epilogue, or it sits between two that do.
  Unguarded the model skipped 21-23 story sections a run: two epilogues,
  a "Chapter 45", three very short numbered chapters, 14 character sheets
  printed between chapters. A book with none (a play) has only the
  model's answer, so matter there is the run it named from the start and
  the run it named to the end.
- **"The End" pages**: a section under 60 characters titled "The End..."
  after the last chapter is matter whatever the model says - it left five
  of them in on one run, two on another. With it: 707 of 707.
- **Still wrong**: a "Post-Credits Scene" after the last chapter is
  skipped (the prompt names after-credits scenes as story; the guard
  can't help, nothing numbered follows it), and one "Book One:" synopsis
  is kept. Both are one toggle to fix.
- **Limits of the measurement**: one run per variant; the library is a
  few series deep (33 of the 86 books are one series), so nearer a dozen
  publishers' habits than 86 books; mid-book chapters were labeled story
  after a spot check of the short ones.
- A book of more than 160 sections is shown its first and last 60.

**Malformed-response retry**: every batch call in this package
(`attributeBatch`, `describeBatch`, `emotionBatch`,
`CharacterizeVoice`) goes through `generateAndParse`/`retryParse`
instead of calling `Client.generate` directly - a malformed response (an
unparseable JSON array, or `CharacterizeVoice`'s own
too-short-looks-truncated check) retries the *whole* round trip, a
fresh generation each time, up to `maxGenerateRetries` (2, so 3 attempts
total). Added after a real production incident: a single malformed JSON
response used to fail the *entire* remaining chapter outright, silently
abandoning dozens of not-yet-processed paragraphs with no automatic
retry, requiring a reader to manually re-trigger the whole action
(redoing already-succeeded batches too, since there's no partial-progress
tracking below the chapter level). A malformed response is usually a
one-off sampling hiccup, not a systemic failure. `retryParse` never
retries a `generate` error itself (context cancellation, a genuine
inference failure) - only a parse/validate failure on an otherwise-
successful generation. `pronunciationBatch` is the one exception -
`parsePronunciationChoices` never itself returns an error (a bad
response there just defaults to each term's more common reading) - so
there's nothing for a retry to fix.

**World notes** (2026-10-10; `store.WorldFact`, `speakerattr/worldnotes.go`,
`httpapi/facts.go`): keyed facts the reader records - "kobold: a
short-lived kind, thirteen is old", "dwarf: Scottish accent" - per series
or for every book, a series' fact replacing the global one under the same
word (`store.BookWorldFacts`). Casting is shown the ones whose word
appears in the character's names or gathered evidence
(`MatchWorldNotes`, whole words, at most 8), as a "World notes" section
the prompt ranks above its genre defaults.
- *Why*: Loth (The Legend of William Oh) was cast "a young kobold boy".
  The book says kobolds are short-lived only in dialogue about kobolds in
  general (chapters 172, 211), which no search keyed to his name finds.
- *Measured* (casting preview with `?notes=`, 2026-10-10): the note
  "Kobolds are a short-lived kind: thirteen years is old for a kobold"
  turned Loth into "an elderly kobold man", accent and manner unchanged -
  but did the same to Grak, another kobold, and the text gives neither an
  age. The prompt's rule now says a note on how a kind ages only says how
  to read an age the material gives, and changes nothing without one;
  **that wording has not been run yet.** A character's own age belongs in
  a fact keyed to them ("Loth: thirteen years old"), on the book's Facts
  tab.
- Not done: facts in the age-stage calls (`ageByCastingSystemPrompt` still
  says a non-human never changes), the summary/ambience prompts, an anchor
  chapter so the recap can't read a fact from ahead of the reader.
  (Since done: the age-stage calls read facts, and facts are proposed -
  "Fact finding" below.)
- *Baseline facts* (migration 00026, an upsert; 28 of them): the
  genre-standard voices the casting prompt lists (dwarf - Scottish, elf -
  RP, orc/goblin - Cockney, giant/troll, dragon/demon, fairy,
  robot/AI/construct, ghost), more kinds beside them (kobold, vampire,
  werewolf, lizardfolk, gnome, halfling, ogre, undead, angel, elemental,
  spirit, dungeon core, insectoid, merfolk), which kinds are long-lived,
  short-lived or ageless, and three rules that aren't kinds (royalty,
  pirate, telepathy), seeded as global facts. The casting prompt still
  lists its own defaults, so deleting one of those facts changes nothing
  yet; overriding one for a series does, since a note outranks the
  prompt's default. Common words among the keys ("giant", "spirit",
  "king", "construct") bring their fact up for characters who aren't one;
  the prompt's "only if the material establishes the character as one" is
  what holds that back, and hasn't been measured.

**World words** (2026-10-12; `speakerattr/titles.go`, `wordclass.go`,
`store.WorldFact.Class`, `Server.titles`): a world fact can say what sort
of word it is filed under - a **title** ("Prognugator"), a **role** (a job
or class: "Baker"), a **race** ("Drake"), a **nationality** ("Pallassian")
or a **place** ("Pallass") - for a series or for every book, with or
without fact text (a race keeps its lifespan and accent in the same fact).
The roster rules read these beside their fixed lists (`titleWords`,
`rosterRankWords`), which only know English titles.
- *Why*: The Wandering Inn had a character "Prognugator" (he/him) holding
  Tersk, Wrymvr and Xrn as aliases, beside "Prognugator Tersk",
  "Prognugator Dekass" and "Prognugator Xrn" (she/her). To the rules
  "Prognugator" was a first name: `shortNameTarget` folds it into the one
  full name that has it while there is only one, and no review rule could
  call it a title. Races did the same ("Drake Trader", "Drake Tesy").
- *What any class changes*: `shortNameTarget` never merges the word into a
  name that has it (attribution's `absorbShortNames`, dedupe's rule 4, the
  review's first-name proposals); `rosterNameWords` drops it, so
  "Prognugator Tersk" pairs with "Tersk" and not with "Prognugator
  Dekass", and the bare word is `IsBareTitle`; `RosterRuleFixes` proposes
  removing a bare title's alias that is another entry's name - now also
  when it is that name without its titles ("Tersk" for "Prognugator
  Tersk", only when one entry has those words);
  `SpeakerNameResolver.WithTitles` stops the word alone resolving to the
  one name that has it.
- *Roles, races and nationalities* also make a description: a name made
  only of them (`WorldWords.IsPeopleDescription`: "Drake", "Pallassian
  Gnoll", "Drake Trader", "One of the Gnolls") is proposed as a role and
  loses its aliases. A title among the words doesn't count ("Gnoll
  Chieftain" may be one person), nor does a place.
- Nothing is changed in a roster when a class is saved: the next review
  proposes the fixes.
- *A speaker's own race and nationality* (2026-10-12): set on the
  Speakers tab, or accepted from fact finding's character notes (kind
  only; no pass proposes a nationality for a character yet). They are
  character notes, so casting reads them through
  `Server.characterNoteLines` - which the first casting (`worldNotes`) now
  calls too; before, only the age-stage rewrites were shown a character's
  accepted notes, and a recast after accepting a kind never saw it. Not
  measured: what the extra lines do to a casting.
- *Not done*: `RoleLabel` still decides "the prognugator" (lower case) is a
  role and "The Prognugator" a name from the fixed `uniqueTitles` alone;
  the baseline global kinds (dwarf, elf, ...) carry no class.
- *Several words* ("Wall Lord", "Staff Sergeant", "Drowned Folk"): an
  entry of `WorldWords` can be a phrase, taken out of a name whole
  (`withoutPhrases`, a plain plural too) before its words are read - so
  "Wall Lord Ilvriss" pairs with "Ilvriss" while "Wall" alone stays an
  ordinary word. `WordCandidates` proposes a phrase of two or three
  capitalized words that leads two or more names going on differently
  (not "Lady Magnolia", which leads only "Lady Magnolia Reinhart"), and
  then doesn't count those names toward its single words. On the live
  rosters (2026-10-12, candidates only, not classed): The Wandering Inn -
  Wall Lord (8 names), Watch Captain, Wall Lady, Chief Warrior, Goblin
  Lord, High Mage, Grand Magus, and "King Itorin" (one man under two
  entries, left to the model to call a name); Expeditionary Force - Staff
  Sergeant (13), Gunnery Sergeant, Fleet Captain.
- *Peoples and places of several words*: a phrase is also a candidate
  when several entries use it anywhere and one of them is a role or a
  description ("Drowned Folk" of "Drowned Folk Captain" and "One of the
  Drowned Folk" - "Magnolia Reinhart" is in two entries too, but both are
  names), or when it is all that follows "of"/"from" in even one name
  ("King of the Blighted Lands"); a single word after "of" needs one name
  as well ("Healer of Tenbault"). The series' own unclassed facts are
  asked about whatever the roster says ("drowned folk", "half-elf").
  The Wandering Inn then gives 314 candidates, 32 of them phrases (House
  Tellis, Gaarh Marsh, Thirsting Veil, Five Winds, Loquea Dree, Wild
  Wastes Gnolls...) and 42 single words after "of". Not classed by the
  model yet: several are a nationality and a race together ("Ekhtouch
  Gnolls", "Redfang Warriors"), which add nothing once both words are
  classed.
- *Finding them* (`Server.findWorldWords`, in the fact-finding run):
  `WordCandidates` takes each capitalized word that two or more entries'
  names use (roles included, a plural as its singular); `TitleUses` keeps
  the ones the text writes after an article, possessive or preposition of
  place ("the Prognugator", "his Drakes", "in Pallass") in at least
  `MinTitleUses` (5) paragraphs, with three sample sentences;
  `Client.ClassifyWords` (25 words a call, each with five names and its
  sentences) answers title / role / race / nationality / place / name /
  other. A new word becomes a pending fact with its class; a word the
  series has a fact in force for gets the class proposed on that fact
  (`ClassProposed`), since the kind facts fact finding already made
  (drake, gnoll, antinium) are the races. A word skipped as one class may
  be proposed as another.
- *Measured* (2026-10-12, The Wandering Inn, read-only over the live
  library with the live model; 775k paragraphs, ~2,000 entries): 258
  candidate words, 230 used 5+ times, 137 classed in 2m10s (10 calls) -
  17 titles (Chieftain, Ser, Dame, Emir, Archmage, Magus, Prognugator,
  Guildmaster...), 68 roles (Mage, Guardsman, Witch, Soldier, Baker...),
  30 races (Gnoll, Drake, Garuda, Dullahan, Antinium, Selphid...), 9
  nationalities (Silverfang, Redfang, Pallassian, Ekhtouch...), 13 places
  (Izril, Pallass, Ailendamus, Khelt...). The 93 left out are family and
  first names (Shivertail, Reinhart, Tekshia) and ordinary words. Wrong
  ones seen: "Wall", "City", "Street", "Guild" as places, "Eater", "Crow"
  as races, plurals of fixed titles ("Lords", "Guards" - since dropped
  from the candidates). Before the role class existed the same words came
  back as 83 titles; splitting "held and addressed by" from "what someone
  does" is what made the title list usable. Not measured: the same on
  another series, or how many of the pairs and role proposals a review
  then makes are right.
- *By rule, without a model* (`findTitlesByRule`): titles only -
  `TitleCandidates` (a word leading two or more proper names),
  `MinTitleUses`, and `NamedHolders` dropping holders whose next word the
  text mostly writes in lower case ("Drake Trader"). On The Wandering Inn
  nearly 30 words passed, a few not titles (Drake, Pallassian);
  Expeditionary Force gave Specialist, Commodore, Ensign, Klasta, Minister
  and Cadet and no first names ("a Margaret" three times is why the
  threshold is 5).

**Fact finding** (2026-10-11; `speakerattr/factfinding.go`,
`httpapi/fact_finding.go`, a `jobs.KindFactFinding` task started from a
book's Facts tab): proposes world facts from a series' own text, each
stored `pending` with the sentence it rests on until the reader accepts
or skips it. Two sorts of fact (`FactSorts`, each its own search phrases
and prompts): how long a kind of being lives and how it ages - what
casting and age stages need to read an age - and how a whole people
speaks. A kind with both gets one fact holding both sentences, a quote
each. The steps and measurements below are the lifespan sort's; speech
has its own entry after them.
- *How*: (1) each of the sort's three fixed phrases (`FactSort.Queries`) is searched
  by meaning across the series (`semantic.Service.Search`), the 40
  closest paragraphs each; (2) `FindFacts` reads them 30 at a
  time and names kinds, each with a quote; (3) per kind,
  `SettleFact` reads the passages that named it plus the 8
  closest to "How long do <kind>s live?" and writes one fact or none.
  Go checks every quote against the passages shown (`QuotedIn`), drops
  what is no kind (`FactKind`: humans, "unknown species", a description
  of more than three words), and skips a kind the series already has a
  fact for.
- *Measured* (2026-10-11, the Python prototype of exactly this, 13
  series, each 50-90% embedded at the time): 64 facts kept; read against
  their quotes about 4 in 5 are sound, and three spot-checked against
  the surrounding text held ("Kobolds only live to fifteen or so"; orcs
  "usually reach adulthood in two or three years"; "Seventeen years is
  considered near death for us"). 10-25s a series, 4-14 calls. Step 3
  dropped 27 of 91 named kinds and merged repeats (four elf facts in The
  Primal Hunter). Cat's Cradle proposed nothing. On The Legend of
  William Oh one phrase ranked all six known statements in the top 35 of
  46,430 paragraphs, including two that a word search for lifespan
  wording (the retrieval tried first: "lifespan", "only live to",
  "long-lived") missed - "Fae don't really age, we just grow bigger".
- *Typical wrong proposals*: the oldest individual's age given as the
  kind's lifespan; a fact about ageing that isn't a lifespan; one fact
  filed under a neighbouring kind too. Progression stories state
  lifespan by rank ("a C-grade can live up to one hundred thousand
  years") - true, but casting only brings up a fact whose word a
  character's material uses.
- *Speech sort* (`SpeechFacts`; prototype over 10 series, 2026-10-11):
  18 facts kept, about 7 sound - "the Drakes elongated their 's' and 'l'
  sounds" (The Wandering Inn), "The Dread had a stilted way of speaking"
  (Unbound), a Drowned Folk voice "a deep, bubbling growl ... as did much
  of the Drowned Folk", "the heavy Othersider accent harsh to the ear"
  (12 Miles Below). The rest: one member's voice given to the kind
  ("Another Gnoll spoke, voice a growl"; "the giant ... deep words with a
  heavy accent"), or nothing a voice can perform (a language that is
  mostly scent). One or two proposals a series, so it is kept and left to
  the reader. No series named a real-world accent for a people, so the
  accent a monocultural people shares is still the reader's to write.
- *Tried and left out - culture in general*: asked for a people's culture
  and way of life (7 series, ~50 notes), most of what came back was true
  and of no use to a voice ("Minotaurs do throw themselves into what
  they do with a passion"), and whether a people is one culture or
  several came back wrong (faeries whose "accents seem to vary" marked
  as shared).
- *What castings already do* (tallied 2026-10-11): kinds with a baseline
  fact are uniform (6 of 6 Unbound dwarves Scottish, 12 of 12 Primal
  Hunter elves RP); a story's own peoples get the book's main accent with
  stragglers (Wandering Inn drakes: 36 General American, 4 British, 1
  RP; gnolls 12/3/1). The Facts tab's "Accents by kind" lists that
  spread and lets the reader pick one accent as the kind's fact, which
  recasts only the outliers (`httpapi/kind_accents.go`; the
  `facts/accents` entry in `docs/api.md`). Whether casting then follows
  the fact's accent sentence has not been measured.
- *Accepting recasts*: the characters whose name, alias or casting's
  opening sentence uses the fact's word (`recastForFact`) - not everyone
  whose evidence mentions the kind.
- Not done: running it unasked (a preprocessing phase), waiting for a
  series to finish embedding (it reads whatever is indexed - a run
  during embedding found five facts once and six the next time), a
  second proposal for a kind already accepted or skipped (one fact a
  key: the speech sort can't add to an accepted lifespan fact), Android.

**Time jumps and character notes** (2026-10-11): the same "Find facts"
run (`httpapi.findStory`, `httpapi/dossier.go`) then reads the series'
text once (`sweepSeries`) for two more things, each proposed for the
reader like a fact. Their passes, prompts and first measurements are in
the header comments of `speakerattr/timegaps.go` and
`speakerattr/dossier.go`; in short:
- *Where time passes* -> time-jump proposals (`store.TimeJumpProposal`,
  table `time_jump_proposals`, migration 00029; listed with the book's
  time jumps as `proposed`). Paragraphs with a phrase of elapsed time
  (`TimePhrase`, exact) plus the closest by meaning; `FindTimeGaps` keeps
  the statements that story time has gone by; runs of them in nearby
  chapters (`timeGapRuns`) are each put to `PlaceTimeJump` with the
  chapter summaries around their start. A run near a jump the book has or
  the reader skipped, or one that only looks back no further than an
  earlier jump reaches, is not proposed (`explained`). On William Oh the
  hand-set jump (5 years before chapter 201) was found as "5 years before
  chapter 202" - a chapter late, so accepting takes the reader's chapter
  and years, not the proposal's. Unbound (1,096 chapters): none proposed.
- *A character's dossier* -> notes (`store.CharacterNote`, table
  `character_notes`): one stated thing each, with its sentence - their
  **kind** ("goblin") or their **age** at a chapter. This is the second
  dossier; why it differs from the removed one is in
  `speakerattr/dossier.go`. Kinds come from how the text names someone
  beside a kind ("Kikri, the Elf", `KindMentions`), with `NameKinds`
  choosing which words are kinds and `VerifyKinds` naming whom each
  sentence is about; ages from an exact search for age phrases
  (`AgePhrase`), `FindAges`, then `SettleAges` a character at a time and
  a never-younger rule. Live runs: Unbound 21 kinds, 17 right (wrong:
  a kind that belongs to the other person in the sentence, a list) and 5
  ages, 4 right; William Oh 3 kinds, 3 right, and 5 ages, 5 right.
- *What an accepted note does*: `castingWorldNotes` adds it to the World
  notes of that character's casting and age-stage calls as a line of its
  own ("Jason's age: 12 years old as of "Chapter 3" - the text says
  ..."), and an accepted kind counts as the character being of that kind
  wherever that is asked (`acceptedKinds`: the facts shown to their
  casting, who a fact's acceptance recasts, the accents list). Accepting
  a kind recasts the character only when a fact in force now reaches them
  that did not before. A pending or skipped note does nothing.
- Not done: a jump's ageing does not yet read "12 at chapter 3" as an
  exact starting age in code (the model is shown the line and left to use
  it - unmeasured); notes are not found during characterization, only by
  this run; a merged character's notes are dropped with it; Android.

**Age stages** (2026-10-10; `store.CharacterStage`, `narration/stages.go`,
`httpapi/stages.go`): a character who grows up or grows old over a book
or series can change voice. A stage says "from this chapter on, about N
years old" and carries its own casting instruction; a character's lines
resolve to the last stage their chapter has reached
(`narration.Stages.At`), across the rest of that book and every later
book of the series (`Resolver.BookStages` - a stage set in an earlier
book is in force from chapter 0, one set in a later book doesn't apply).
Before the first stage, and for a character with none, it is the
character's own voice as always.
- *The voice* (`narration.StageVoice`): the character's own casting
  recipe - seed, reference line, design model - rendered from the stage's
  instruction instead of `Summary`. Not stored: the preset id is derived
  (`<presetID>~a<age>-<hash of the casting>`), like a numbered role's
  sibling, so its reference clip, emotion variants, clip keys and
  `voice_id` need nothing of their own, and an edited instruction is a new
  id. Roles have no stages; a character with no voice of their own (still
  on the narrator's) is left alone; in the instructed-narrator modes the
  stage's instruction replaces `Summary` as the clone instruction.
- *Why a redesign* - listening test on Will Oh (The Legend of William Oh:
  "a fifteen-year-old" in chapter 116, "I'm twenty" in 324, cast "a
  teenage boy" for all 6,109 lines), his clip rendered at 10/20/40/70
  three ways and two real lines cloned from each through Higgs. BreezeTTS
  instructed cloning of his clip (how emotion variants are made) kept the
  voice and barely aged it (212 Hz -> 203 at "twenty", and *up* at
  "seventy"); Praat pitch/formant shifting moved the pitch where told.
  By ear neither sounded good; redesigning from an age-edited casting
  under the same seed did. A speaker-embedding score ranked
  them the other way round (redesign 0.49-0.73 against his own clip,
  where other characters score ~0.6 and his emotion variants 0.84-0.92) -
  it measures the wrong thing for this.
- *The casting* (`Client.AgeCasting`, `agecasting.go`): the character's
  `Summary` rewritten for the age by one small call that changes only the
  opening age clause and the wording on pitch, timbre and pace, keeping
  accent and manner word for word - what the castings in the test
  differed by. The reader can edit it.
- *Readiness*: `store.SpeakerVoice` rows carry a chapter range
  (`FromIdx`/`ToIdx`), one row per stretch, so `ListChapterSummaries`/
  `ChapterNarration` count a clip only under the voice of its own
  chapter's stage. `Cached.ListCharacterStages` reads `chapters` (a stage
  is placed by its chapter's current index), which is written constantly,
  so a roster with no stages is answered from `CharacterStageScopes`.
- *Time jumps* (`store.TimeJump`, table `book_time_jumps`,
  `httpapi/timejumps.go`): "N years pass before this chapter", for the
  whole cast - at a book's first chapter, the whole book is set that long
  after the one before it in its series. Applying one needs no ages:
  every characterized, non-role character who speaks on both sides of it
  (`Store.SpeakersInChapters` over the series) is asked about in a
  background characterization task (`Client.AgeCastingBy`), which
  estimates their age from the casting's opening clause, adds the years,
  and rewrites the casting only when the age *group* changes (child,
  teenager, young adult, adult, elderly) - so an adult five years on keeps
  their voice and makes no stage. The casting aged is the one in force
  going into the jump (their latest earlier stage, else `Summary`), so
  jumps compound. Each stage made carries the jump's id
  (`character_stages.jump_id`); removing the jump removes them, and a
  stage the reader had already set at that chapter is left alone. Not yet
  run against the real model.
- *Following the character's own voice* (2026-10-10): a recast
  (`recharacterizeAndInvalidate`) rewrites every stage's instruction from
  the new casting (`Server.recastStages`, one `AgeCasting` call a stage,
  hand-edited instructions included; a failed rewrite leaves the stage
  with none, which plays the character's own voice) - before that a stage
  kept the casting the character had when it was set. And whenever a
  voice's reference clip is rendered anew or deleted, the clips and
  emotion variants of every voice derived from it - stages and a role's
  numbered siblings, ids `<presetID>~...` - are deleted with its own
  variants (`voicerefs.DeleteDerived`), to render again when next needed.
- *Detection must propose, not apply* (the reader's instruction,
  2026-10-10): when time jumps or stages are found automatically they
  wait for the reader to accept or skip, the way roster review's
  proposals do - a wrong one re-voices half a book.
- *Finding them* (2026-10-11): fact finding proposes time jumps and
  stated ages - "Time jumps and character notes" above. Stages themselves
  are still only made by hand or by an accepted jump. What this note said
  before that existed: finding them is two missing pieces - an approximate age per character (castings
  carry only a group word: 8 of ~5,300 named characters have a number),
  and where time passes: the summary pass cuts scenes at a time jump but
  records only time of day, and a gap is often never announced (Will's
  falls at the chapter titled "Epilogue"; the evidence is "over the last
  few years" two chapters later). Flashbacks (a stage for one scene) aren't handled; a
  younger self the text names apart ("Young Roland") is still its own
  roster character.

**Series-wide character scope**: characters (identity, `Summary`, voice
assignment) are keyed by `store.SeriesScope(book)`, not by book - books
sharing a non-empty `SeriesName` share one character roster across the
series, so a recurring character keeps the same name and voice in book
2 that they got in book 1; a book with no series keeps its own private
roster. Both stages draw on the whole series: `attributeChapter`'s
known-character list and `characterizeVoice`'s quote sample are gathered
via `httpapi.booksInScope`/`Store.ListSeriesBooks` (series order) +
`Store.QuotesForBooks`, across every book in the series attributed so
far. Lazy characterization still only ever runs once per character ever
(gated on `Character.Summary` being empty) - no automatic
re-characterization later as more dialogue accumulates. A reader who
wants a fuller-sample redo can explicitly force one via the Speakers
page's "Regenerate" button (`POST .../characterize`), which also
invalidates *every* clone model's currently-assigned voice preset's
cached reference clip for that character so each picks up the new
characterization on next use.

**Emotions** (`internal/emotions`, `Client.EmotionChapter`/`emotion.go`):
every real dialogue line (`IsQuote && !ScareQuote`) can carry one
delivery emotion or speaking mode from a small fixed set - `warm`,
`excited`, `sad`, `angry`, `afraid`, `cold`, `whisper`, `shout`,
`weary` - or stay neutral (`""`, the overwhelming default). Narration
always stays neutral so the narrator keeps one consistent delivery
(`store.Paragraph.EffectiveEmotion` enforces this at read time, so a
stale label or a later scare-quote flag can never leak through).

An emotion never reaches a TTS model as markup. Instead each voice
preset gets one extra **reference-clip variant** per emotion its lines
actually use (`voicerefs.EnsureVariantFile`, stored at
`voice-refs/variants/<presetID>/<emotion>.wav`): BreezeTTS instructed
cloning (`voices.InstructedCloneModel`) of the preset's own base clip,
speaking that emotion's `RefLine` under its `Instruction`, capped at
`maxRefClipSeconds` and loudness-bounded against the base clip
(`voicerefs.variantBand`, taking +10 LU as twice as loud): most emotions
may sound 0.8x to 1.5x as loud (-3.2..+5.85 LU), a shout 1.3-1.75x
(forced louder - Breeze's shouts often landed no louder than the base),
a whisper 0.8-1x. The floor was half as loud (-10 LU) until 2026-10-01:
Breeze renders warm/sad/weary 5-7 LU under the base and whispers as low
as -42 LUFS, and since paragraph audio isn't normalized and inherits
its reference's level, mostly-emotional characters sat 4-8 LU under the
narration. A raise is capped at +24 dB (`maxVariantRaise`, not
`clampGain`'s +12 dB, which left the quietest whispers under their
floor) and never pushes the peak past ~-1 dBFS. `NormalizeAll` re-bounds
existing variants at startup (`ReboundVariants`), so a band change
reaches them without a re-render - but lines already cloned from them
keep their old level until regenerated. Only
base clips are fully normalized (to `voicerefs.TargetLUFS`). An emotional
line then clones from the variant (with its `RefLine` as the reference
transcript) instead of the base clip - `jobs.Manager.generate`, via
`emotionVariant`, which re-reads the paragraph's emotion from the store
at dispatch so a manual override between enqueue and dispatch wins.
Zero-shot cloners copy a reference's delivery almost as strongly as its
timbre, so this works through every clone model, not just one with its
own emotion vocabulary. Replaced Higgs's own inline `<|emotion:*|>`/
`<|style:*|>`/`<|sfx:*|>` tag passes after a listening test
(task-backlog.md item 13): most of those tags pulled the voice away from
its reference entirely, while Higgs cloned from a Breeze variant stayed
on-voice (IndexTTS2's emotion vectors were the other candidate - about
1.75x slower than Higgs).

Variants are provisioned **lazily**: `jobs.Manager.variantClipDependency`
(a sibling of `referenceClipDependency`) makes an emotional clone task
depend on a `KindVoiceProvision` task keyed `variant:<presetID>:<emotion>`
whenever that variant isn't on disk yet, so only emotions a voice's
lines actually use ever get rendered. A render that fails on its final
attempt writes a `.failed` marker (`voicerefs.MarkVariantFailed`), which
clears the dependency - those lines clone from the base clip instead of
blocking forever. Every base-clip write or delete (`voicerefs.save`/
`Delete`) wipes that preset's variants, since each was
cloned from the old clip. Skipped for a scare-quote merge group (one
call renders narration and quote together).

**The emotion pass** (`httpapi.directChapter` - the `KindSpeechDirection`
task, `Passes.Direction`, `POST .../tag-directions` and the preprocess
direction phase all keep their old "direction" names) sends a chapter
in batches of `emotionBatchParagraphs` (30), narration included as
context with dialogue lines marked `(dialogue)`, and gets a label for
the clearly emotional dialogue lines (how it asks is below). Both prompts
are built from `emotions.All`, so the offered labels can't drift from
what the app renders. Runs under any clone model.
Rewrites every processed line's stored label (clearing stale ones) but
only invalidates audio for paragraphs whose *effective* emotion changed
(`Server.invalidateParagraphAudio`). A paused/failed run requeues just
the unreached paragraphs via `onlyIdx`, the same shape as before; with
`Book.SpeechDirection` on, `speechDirectionDependency` makes a chapter's
generation wait for it under any clone model.

**Decided, not generated** (since 2026-10-08): with a backend that can
decide (`llmDecider` - the live worker), `emotionBatch` asks
`emotionDenseSystemPrompt` - the same prompt wanting a verdict on *every*
dialogue line, "neutral" included - and reads each verdict from the
model's logits over neutral and the labels (`emotionSteps`, one decode
round a line, each conditioned on the verdicts before it); a line is
labeled when an emotion outscores neutral. The generated list of only the
emotional lines (`emotionSystemPrompt`, `parseEmotionLines`) is now the
fallback for a backend with no decide call (test fakes) or a tokenizer
that starts two labels with one token. Chapters keep the labels they have
until their pass is re-run.

Measured on Qwen3.6-35B-A3B over 11 chapters of Arise: Alpha, Spire's
Spite, Jake's Magical Market 3 and The Wandering Inn, 981 dialogue lines
(`Client.EmotionProbe`, `cmd/sqdecide -emotion`). No reviewed labels
exist, so quality is a read of 54 sampled disagreements, not a score:
- The generated list labeled a third of all dialogue (327 lines; 75 of
  117 in The Wandering Inn) and wasn't stable - a second run agreed on 893
  of 981 lines. The dense prompt labels half as many (162 generated, 163
  decided, agreeing on 941 lines; decided twice, 952).
- Of 24 sampled lines only the list labeled and the decide call put at
  0.98+ neutral, about 20 read as wrong or unearned ("Blue juice?"
  whisper, "Greetings. May we come in?" cold, "Shit." cold); of 16 only
  the dense prompt labeled, about 12 read right, one with "anger edging
  his voice" in its own tag.
- The decide call's P(neutral) (`EmotionVerdict.Neutral`) is a dial the
  pass doesn't use yet - it labels at the argmax: labelling under 0.3 /
  0.5 / 0.7 would give 128 / 174 / 224 lines. Picking one needs reviewed
  labels.
- Cost, same chapters: list 33s, dense generated 82s, dense decided 57s
  (35s with MTP off). The extra is per decide step with MTP on, and not
  the draft context's feed: keeping decide slots out of it changed nothing
  (57s). Unverified guess: `n_rs_seq` > 0, which MTP rollback needs, makes
  llama.cpp keep each sequence's trailing tokens in one ubatch, so a round
  of several slots' short step chunks splits into several computes.
- Rejected: reading the whole batch in one pass, each line's verdict
  written as neutral before the next is read. 27s, but a transcript of
  neutrals talks the model out of emotional runs - 83 labels instead of
  157, a villain's "Kill them as well." going from 1% neutral to 98%.

Stored in `paragraphs.emotion` (`Store.SetParagraphEmotions`). A reader
overrides one line via `PUT .../paragraphs/{pidx}/emotion`
(`handleSetParagraphEmotion` - invalidates and re-enqueues that line
when its effective emotion changes). The Speakers page lists each
speaker's emotions with line counts and variant status
(`speakerDTO.Emotions` - keyed by whichever preset their lines
actually generate under, so the book's own when they have none; the
`speakers` live topic also depends on `DepJobs` so a finished render
shows up without a DB write), previews a variant via
`GET /api/voices/variants/{presetId}/{emotion}/audio`, and re-renders
one via `POST .../speakers/variants/regenerate`, which resets that
speaker's lines in that emotion (this book only) and queues the render
right away (`jobs.Manager.EnqueueEmotionVariant`).

**Free-form direction** (2026-10-14, for Fish Drama 3; `internal/direction`,
`speakerattr/direction.go`, `httpapi/delivery.go`; design and open questions
in `drama3-direction-plan.md`). A clone model whose catalog entry says
`voices.DeliveryDirected` reads delivery as text - a style prefix
(`[too bright, covering the hurt]`), point tags (`[sighs]`), spans
(`<whisper>…</whisper>`) - so for it the emotion pass gets a **second
stage** (`Passes.Delivery`): after the labels, `httpapi.deliverChapter` picks
each dialogue line that has a label or narration beside it saying how it is
delivered (`speakerattr.DeliveryCue` - laughed, sighed, "voice breaking",
under his breath), and `Client.DirectLines` asks for each one alone (4 at a
time, `directionSystemPrompt`, primed) with its speaker and casting summary,
the 3 paragraphs before and 1 after, its scene and the book's setting, and
the label as a hint. The model writes the line back with direction in it;
`direction.Parse` keeps it only if the words are unchanged, and a one-word
style (`[weary]`) is sent back once for a phrase. Every other dialogue line's
direction is cleared, and lines the reader directed (`store.EditDirection`)
are left alone. Stored in `paragraphs.direction` (JSON, rune offsets plus a
hash of the text they were written for - a changed text drops it). The
labels stay: every other clone model renders from them, and under a
directed model a labeled line with no direction of its own is read with the
label's style (`emotions.Emotion.Direction`, via
`Paragraph.EffectiveDirection`), so a book moved to the model is directed
before stage 2 runs; a chapter labeled earlier runs just stage 2
(`speechDirectionDependency`, `directChapterRange`). Generation under a
directed model: no emotion variants are rendered or used anywhere. That
covers the lazy dependency and the clip a line clones from
(`task.variantEmotion`, `emotionVariant`), the "used_voices" bulk action, and
the Speakers tab, whose emotion chips show `directed` with no play or
regenerate and whose regenerate route answers 400 for an emotion. the book's own
brackets removed from the text (`direction.LiteralBracketSubs`), the
direction written in around substitutions (`Direction.Insertions`), a bisect
retry repeats the style and closes/reopens a cut span (`CarryAcross`), and
the extra-words check compares against the text with the direction stripped
and the transcript without "ha ha"/"ugh" (`dropVocalizations`), so a tag
read aloud is caught; after the chunk-size retries, the line is retried
without its tags, then without its style (`retryAttempts`). The annotations
view draws a directed line from `paragraphDTO.delivery` (structured; only
under a directed model): the style as a script's parenthetical before the
line (dimmer when it is only the label's - `styleFromLabel`; clicking it opens
the editor), point tags as small pills where they happen, and spans as
styling on their words (whisper faded and dotted, slow letter-spaced,
emphasis underlined) - web `ParagraphText`/`ChapterSection`, Android
`ReaderParagraph` (pills are inserted into the shown text, with word offsets
shifted past them), and Android's offline copies keep it. **Tuned 2026-10-15** (`cmd/directprobe`, Qwen3.6-35B-A3B, 174 labeled or
cued lines from 9 chapters of 7 books; text only, nothing listened to yet).
Every variant directed 98-100% of the picked lines, and changed a line's
words at most once.

- *Rules only* (the first prompt): the label hint, which then showed the
  label's LLM description, was pasted in as a style ("exhausted, weak,
  drained, sleepy, worn out" ×8). Without the hint, the prompt's own
  examples came back instead ("dry, barely holding back a laugh" ×7).
- *Concrete voice words* ("clipped, breathy, hoarse"): reused its own
  example phrases ×8-10.
- *Worked examples* (four passage → reply pairs): styles were 97-99%
  distinct and none were copied from the prompt; emphasis on 25-36% of
  lines. Some styles described gestures ("leaning in").
- *Worked examples plus "only what can be heard"*, with a note on the feeling
  under the line and a point tag never inside the style: gestures were
  gone, emphasis was on 67-77% of lines and mostly on the word that carries
  the line ("Are you heading back to your <emphasis>Hive</emphasis>?"), and
  point tags were on 15-21% of lines, mostly backed by the passage ("He
  sighed," → `[sighs]`; a few unearned). This is the shipped prompt.
  Telling it to avoid "sharp" didn't work (~30 lines).

Other choices from the same runs:

- The label hint is now just the label's name, marked as a rough guess.
  With or without it, results were similar.
- The first attempt runs at temperature 0.4, which gave 174 distinct styles
  out of 174, against 169-170 greedy.
- Seen once in ~1,400 calls: a reply of `[sharp Pisces.”`, the tail of a
  *different* line being generated at the same time. That suggests bleed
  between sequences in the batched LLM. The retry recovered it; it hasn't
  been investigated.

**Pauses** are the one Higgs inline tag kept: `deliverytags.
PauseInsertions` places `<|prosody:pause|>` before each ellipsis/em dash
that still has something speakable after it - a deterministic,
character-based pass applied by `Paragraph.ResolveGenerationText` only
when the clone model is `voices.HiggsCloneModel` (any other tokenizer
would read the tag as literal text), and reported to clients as
`directionMarks`. Computed, not stored - nothing to persist or
invalidate. The legacy `paragraphs.tts_tags` column from the old LLM tag
passes is never applied any more: re-labeling a chapter clears it
(`Store.ClearLegacyTags`) and invalidates those lines' audio (plus their
scare-quote merge groups), since it was generated with the old tags
baked in - a one-time migration per chapter, on top of the ordinary
"effective emotion changed" invalidation.

**Pronunciation resolution** (`Client.ResolvePronunciation`/
`pronunciation.go`) is its own chapter pass - `httpapi.pronounceChapter`,
a `jobs.KindPronunciation` task, `store.Passes.Pronunciation`, `POST
.../resolve-pronunciation`, and its own preprocess phase - with the same
per-chapter call/requeue-on-pause/invalidate-audio shape as
`directChapter` (which it used to run inside of, as a sub-pass).
It solves a genuinely different problem: fixing a mispronounced abbreviation ("St." read as neither
"Street" nor "Saint" correctly) means actually changing what word gets
spoken, not inserting a control token - a narrow, deliberate exception
to `deliverytags`' own "never reword real content" invariant. Rather
than weaken that invariant (or trust an LLM to freely respell text - the
"full free-form respelling" alternative considered and rejected), the
LLM here is never shown a vocabulary and never returns any text at all:
`pronunciationCandidates` (a small, hand-authored regex table) finds
every pronunciation-worthy span and its own closed set of possible
readings *before* the model is involved (`findPronunciationTerms`, pure
regex, no LLM call) - today:
  - `Dr.` → Doctor/Drive, `St.` → Street/Saint, `Ft.` → Fort/Feet, `Mt.`
    → Mount/Mountain - each exact-case (never `(?i)`) to avoid colliding
    with ordinary lowercase words.
  - `No.` → Number/No, but only when immediately followed by a number
    ("No. 5") - RE2 has no lookahead, so the digit(s) have to be part of
    the regex match itself; requiring a following number is what keeps
    this from matching an ordinary sentence-ending "No."
  - `C'mon`/`c'mon` (straight or curly apostrophe, case-insensitive) -
    always returns exactly one reading ("Come on"/"come on"), so it
    never actually reaches the LLM at all.
  - Homographs `read`/`lead`/`wind`/`tear`/`bow`/`close` and more, each with two
    respellings (reed/red, leed/led, winned/wined, tare/tier, beau/bough,
    clohss/cloze - near vs. shut). `close` is far more frequent than the
    rest, but its grammar rules settle about 92% of this library's 1,329
    uses (a 115-term spot check found no wrong call) and only the rest
    reach the LLM. `wound` (woond/wownd), `live` (liv/lyve), `lives`
    (lyves/livz), `minute` (minnit/my-newt) and `tears` (tiers/tares)
    follow the same shape; `wound`, `minute`, `tears` and (bar a
    sentence-initial one) `lives` always resolve by rule, falling back to
    the injury, the unit of time, teardrops and the plural of "life", and
    `live` leaves about 7% of its uses to the LLM.
    All of them match in any case, and the respelling takes the word's
    own ("Close" → "Cloze", "CLOSE" → "CLOZE").
  - A capitalized word + Roman numeral: a heading word
    (`romanNumeralNonNames`, matched case-insensitively) reads as a
    cardinal with no LLM call ("ACT II" → "Act two", "World War II" →
    "World War two"); anything else is a possible regnal number,
    unchanged vs. "Henry the eighth".
  - Numbers (`numbers.go`/`findNumberTerms`, a scanner rather than a
    table regex, since it needs neighbouring characters RE2 can't check):
    integers (plain or comma-grouped) spelled out British-style ("7456" →
    "seven thousand four hundred and fifty six"), fractions ("2/3" → "two
    thirds", "1/2" → "one half", "3/4" → "three quarters") - or a count
    ("six of six", "one of three") when the top is >= the bottom, after
    a "Label:", or ending a short label line ("Technique 1/3"), decimals
    ("three point five"), ordinals ("21st" → "twenty first"),
    multipliers ("2x"/"1.5x"/"3×" → "two times", "one point five
    times" - only when the mark ends the word, so "2x4" and "0x1F" are
    left alone), and years
    ("1782" → "seventeen eighty two") - a plain four-digit 1100-2099 in a
    year context (`yearContext`): before an era marker ("1614 AD"), after
    a month ("June 1922", "Month D,") or "the summer/War of", or after a
    weaker cue - "in"/"since"/"after"/"from"/sentence-initial "By", a
    copyright mark, or an earlier year it lists or ranges from ("1914,
    1915", "1914 to 1918") - provided what follows isn't a word it would
    be counting ("in 1782 who..." is a year, "since 1500 gold coins"
    isn't). Anything else stays a cardinal, deliberately: an uncued
    four-digit number in this library is usually a LitRPG stat ("1300",
    "increased by 1250") - except one directly before a capitalized
    word, where "a 1939 Chrysler" and "1500 Mana" look alike, so the
    year is a second, LLM-chosen candidate behind the cardinal
    (`possibleYearReading`; unbenchmarked). Skipped:
    a token glued to a letter ("A4", "3D") or currency sign, a leading
    zero ("007"), times ("3:45"), full dates ("3/4/2024"), and dotted
    labels ("1.2.3", "1.E.8"). A bare `M/D` that parses as a calendar
    date gets the date as a second, LLM-chosen candidate only after a
    date preposition ("on 3/4"); otherwise it's always the fraction - on
    real LitRPG books every `M/D` was a stat counter ("Capacity: 3/6").
    A number overlapping a table match (e.g. inside "No. 5"'s span) is
    dropped in favour of the table term.

Within a batch, a term with only one candidate is resolved immediately -
no LLM call spent confirming a foregone conclusion. A homograph is then
tried against `resolveHomographByGrammar` (`pronunciationrules.go`):
deterministic cues from the surrounding words - a modal/"to" before
"read" → present, "had/was/well" → past, a determiner before "tear" →
teardrop, "heavy as lead" → metal, "a slight bow" → gesture, and so on.
Only terms no rule settles reach the model, whose only job is picking a
lettered option per span (reply shape `{"0": "A", ...}`), so it can
never invent replacement text - the safety property that makes this safe
to change what's spoken at all. The reply isn't generated: the pass
writes everything but the letters itself and reads each letter from the
model's logits over that term's options (`pronunciationSteps`,
`llmDecider` - `ttsworker.Manager.LLMDecide`, the worker's `/llm/decide`),
one decode round a term in the same LLM slots as every other pass. A
backend with no decide call (test fakes) still generates and parses
(`pronunciationGenerate`). Each term is shown as its own short
passage with the word marked `⟦inline⟧` and each option described by
*meaning* (`pronunciationCandidate.senses`), not by respelling. A
response entry that's missing, malformed, or out-of-range defaults
silently to index 0 (always the more common reading).

**Benchmarked** (2026-09-24) against ~500 hand-labeled terms from this
library's own books (a 380-term gold set plus a 120-term held-out set
from chapters not used to write the grammar rules), Qwen3-4B-Instruct-
2507: the earlier prompt (whole numbered lines + a separate "term id in
line N -> 0=winned, 1=wined" list) scored 45% - *worse* than always
taking candidate 0 (72%): the model couldn't bind a bare respelling to
its sense, and a line containing the word twice made id→occurrence
ambiguous. The sense-labeled per-term prompt alone scored ~89%; rules +
that prompt score ~96% on both sets and resolve most terms with no LLM
call. Known remaining misses: "lead" misspelled for past-tense "led"
in narration, weapon-vs-gesture "bow" with no archery words nearby, and
"read" in present-tense narration (the rules assume past-tense
narration - `pronunciationNarrationPastTense`).

**Decided vs generated** (2026-10-08, Qwen3.6-35B-A3B, `cmd/sqdecide
-pron` over the same set - 447 of its terms are still two-way choices):
generated 425 right, decided 423, the two agreeing on 606 of 617 terms
asked; on the 91 no rule settles, 85 and 86. Decided is ~30% faster (51s
vs 71s; 9.8s vs 14.5s on the rule-unsettled terms) - the JSON reply was
6.3K generated tokens. One term per call scored 430 in 64s, and that with
both option orders averaged 422 at twice the cost, so neither is used.
Each decision carries the model's probability
(`ttsproto.LLMDecision.Probs`), which the pass doesn't keep yet: on that
run the 25 terms under 0.75 held 14 of the 24 errors and the 304 at 0.99
or more held one, but neither falling back to the default reading when
unsure nor letting a confident model overrule a rule changed the score.
The same call on the scare-quote pass (`Client.ScareQuoteDecide`, bench
only) matched generation in accuracy and time - its reply is a few
tokens - so that pass still generates.

Running the pass (`httpapi.pronounceChapter`) invalidates audio for
every paragraph it touches - one it finds a substitution in, or one
whose stale stored list it clears - even when the substitutions are
unchanged, plus their scare-quote merge groups
(`Store.DeleteParagraphAudioForIdxs`): existing audio can't be trusted
to have applied them (fixes nested inside emphasis were once silently
dropped - see `pronounce.Compose` below), so re-running the pass is how
to re-apply them. Paragraphs with nothing to substitute before or after
keep their audio.

The result is stored as `internal/pronounce.Substitution` (`{Offset,
Length, Replacement}`, byte-indexed into the paragraph's own real
`Text`, the same coordinate space `deliverytags.Insertion.Offset` uses)
in `paragraphs.pronunciation` - not `clone_model`-keyed: a pronunciation
fix is plain word substitution, correct under any TTS model.
`pronounce.Apply` (used by `Paragraph.ResolveGenerationText`) supersedes
calling `deliverytags.Merge` directly for a paragraph with both kinds of
edit: both are offset-anchored into the same original `Text`, so
composing them in one single offset-ordered pass - rather than
substituting first and re-targeting insertion offsets against the
now-different-length result - is what keeps every offset meaningful.
Emphasis spans routinely *contain* a pronunciation fix (a bold stat
line "Trait 2/3"), and `Apply` drops the second of two overlapping
edits, so `pronounce.Compose` nests each contained fix into its
emphasis span first ("TRAIT TWO OF THREE"); a fix straddling a span's
edge drops the emphasis instead.
Forced alignment and the reader's own on-screen text always use `Text`
untouched here too.

It runs for every clone model. With `store.Book.SpeechDirection` on, a
chapter's generation waits on both it and emotion labeling
(`jobs.Manager.pronunciationDependency`/`speechDirectionDependency`); chapters
directed before the split already carry `passes.pronunciation`, since
direction tagging used to resolve it too.
