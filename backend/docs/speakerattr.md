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
     model took each paragraph of a multi-paragraph speech as a new line).
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
  book" section. Two runs: Anders Danish in both (British in every run
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
end up with a different auto-assigned preset under `audiocpp-qwen3-0.6b`
than under `audiocpp-higgs-4b`. Every lookup/provisioning call is keyed
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
context with dialogue lines marked `(dialogue)`, and asks for plain
`idx: label` lines only for clearly emotional dialogue.
`parseEmotionLines` drops anything that isn't a dialogue idx from the
batch with a known label, line by line - never failing the batch.
`emotionSystemPrompt` is built from `emotions.All`, so the offered labels
can't drift from what the app renders. Runs under any clone model.
Rewrites every processed line's stored label (clearing stale ones) but
only invalidates audio for paragraphs whose *effective* emotion changed
(`Server.invalidateParagraphAudio`). A paused/failed run requeues just
the unreached paragraphs via `onlyIdx`, the same shape as before; with
`Book.SpeechDirection` on, `speechDirectionDependency` makes a chapter's
generation wait for it under any clone model.

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
to change what's spoken at all. Each term is shown as its own short
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
