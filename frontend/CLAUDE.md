# frontend

React + TypeScript + Vite app. Routing is TanStack Router, data fetching
is TanStack Query. Talks only to `../backend`'s HTTP API.

## Toolchain note

Node isn't on the system `PATH` in this environment. A Node LTS build is
installed locally at `/home/rhino/node-toolchains/v24.21.0/` for this
purpose (not a system install). Prefix commands with:

```
export PATH=/home/rhino/node-toolchains/v24.21.0/bin:$PATH
```

## Formatting

Prettier (`.prettierrc.json`: no semicolons, single quotes, 100 columns).
`npm run format` rewrites, `npm run format:check` only reports.
`.prettierignore` skips Markdown (hand-wrapped) and `src/api/generated.ts` -
its layout is owned by `backend/cmd/apigen`, and a backend test compares it
byte-for-byte.

## Layout

- `src/api/generated.ts` — **generated** from the backend by
  `backend/cmd/apigen` (never edit it: change the Go side and run
  `make generate` from `backend/`; a backend test fails while it's
  stale). Holds every wire type, the backend's enums (`JOB_TIERS`/
  `JobTier`, `BULK_ACTIONS`, `CHARACTER_VOICE_MODES`, ...), the catalog
  (`CLONE_MODELS`, `DESIGN_MODELS`, `EMOTIONS`, `DEFAULT_CLONE_MODEL`,
  ...), the `LiveTopics` table (each topic's params and value type), and
  the `Routes` table (each `"<METHOD> <path>"` route's path/query params,
  body, response, and error codes) plus `ROUTE_RESPONSE_KINDS`.
- `src/api/types.ts` — re-exports `generated.ts` plus the frontend-only
  pieces: UI copy keyed by the generated unions (`SFX_ENGINE_LABELS`,
  `CHARACTER_VOICE_MODE_LABELS`, ... - `Record<Union, ...>`, so a new
  backend value is a compile error until it gets a label), lookup tables
  derived from the catalog (`CLONE_MODEL_LABELS`, temperature/guidance
  defaults), the `ContentItem` discriminated union, and `LiveOp`.
- `src/api/client.ts` — `call(route, { path, query, body })`, a `fetch`
  wrapper typed end to end by the generated `Routes` table (the route key
  fixes the params, body, and response type; 204s resolve to `undefined`,
  file routes to a `Blob`; non-2xx throws `ApiError` with the typed
  `ErrorCode`), and the `api` object's named mutations built on it.
  Server *state* is not fetched here - see `live.ts`.
- `src/api/live.ts` — the client for the backend's live-state WebSocket
  (`GET /api/events`, backend `internal/live`). All displayed server state
  (books, a book, each chapter, chapter music, voice settings, speakers,
  character appearances/descriptions, bookmarks, the job queue, voice
  presets, default voice) is a *topic*: subscribing yields a full
  `snapshot`, then a `patch` (JSON ops, applied with structural sharing -
  unchanged paragraphs keep object identity, so memoized components don't
  re-render) whenever the backend's value changes, whatever changed it.
  One socket per page; subscriptions are ref-counted per (topic, params),
  linger 5s after the last unmount, and are all resent on reconnect.
  `useLive(topic, params | null)` / `useLiveMany(topic, paramsList)` return
  `{data, error, isLoading, isError}` (the subset of TanStack's result
  shape pages read), typed by the generated `LiveTopics` table - the
  topic name fixes its params and value type. There is no polling and no cache invalidation
  anywhere - a mutation's write is itself what produces the update.
- `src/api/queries.ts` — hooks over both: `useBooks`/`useChapter`/
  `useChapterRange`/... are thin `useLive` wrappers (`useChapterRange`
  backs the infinite-scroll reader, one subscription per loaded chapter);
  mutations are plain TanStack `useMutation`s with no `onSuccess`
  invalidation. "Is chapter N still attributing/directing/generating"
  hooks (`useAttributingChapters` etc.) are pure derivations from the live
  jobs topic.
- `src/hooks/usePlayback.ts` — the sequential-chunk audio player. A book
  has one clip (Ogg Opus) per paragraph, not one per chapter; this hook owns a
  single `<audio>` element and advances to the next paragraph's URL on
  `ended`, reporting `{chapterIdx, paragraphIdx, seconds}` back
  (throttled) for position persistence. It's keyed by `(chapterIdx,
  paragraphIdx)` pairs looked up in a `Map<number, ChapterDetail>` of
  currently-loaded chapters — deliberately *not* a flat array index, since
  infinite scroll can prepend an earlier chapter at any time and a flat
  index would shift out from under whatever was mid-playback. If the next
  chapter isn't loaded yet when a chapter ends, it calls `onNeedChapter`
  and parks in a "pending advance" ref until that chapter's data shows up.
  Effects are keyed on the *resolved ready audio URL* (a string), not the
  whole chapter object, specifically so a live chapter patch (another
  paragraph finishing) doesn't restart playback.
- `src/lighting/` + `src/hooks/useLighting.ts` + `src/components/LightingPanel.tsx`
  — Philips Hue room lighting from the ambience pass's per-region
  `lighting` cues (backend `store.Lighting`). The browser talks to the
  bridge directly over its local **v1** API (`hue.ts`: cloud discovery via
  discovery.meethue.com, link-button pairing, rooms/lights, light/group
  PUTs) **over HTTPS** - current bridge firmware 301-redirects plain HTTP
  there. Deliberately direct from the browser, not relayed: the bridge is on
  the listener's own network, which only their browser can reach wherever
  the app is served from. Two one-time, per-browser steps, both surfacing
  as the same bare fetch failure (`HUE_UNREACHABLE`): accepting the
  bridge's certificate (Philips' own "root-bridge" CA - no browser trusts
  it, page code can't pin it) at `https://<bridge>/api/config`, and, for a
  page served from the internet, Chrome's Local Network Access permission
  prompt. `LightingPanel` walks through both (open bridge → Test
  connection → Pair); `explainUnreachable` reads the
  `local-network-access` permission to tell a blocked/pending permission
  from a certificate problem; probe and pair wait 60s, since the request
  hangs on that prompt (routine calls keep 4s). CORS is `*`.
  `engine.ts` sends a new cue as one group command (fast into a `cut`
  region, slow into a `continuation`) and animates flicker/pulse/shimmer
  one light per 125ms tick round-robin (inside Hue's ~10 light cmds/s,
  ~1 group cmd/s limits); lightning is an occasional flash-and-back group
  pair; effects run only while playing. `controller.ts` is a module
  singleton holding the per-browser settings (localStorage
  `lectable.lighting`: bridge, username, room, brightness cap, restore on
  stop) and the engine lifecycle; it snapshots the room's lights on start
  and restores them on stop. `useLighting` (ReaderPage) picks the region
  under the current paragraph from the `chapterMusic` topic; the reader
  also lazily runs the ambience pass when lights are on, even with music
  off. PlayerBar's "Lights" button opens `LightingPanel`. Mirrored by
  android's `playback/lighting/` - keep the two in step.
- `src/hooks/useSleepTimer.ts` — sleep timer for `PlayerBar`'s
  `SleepTimerButton.tsx`: either a real-wall-clock countdown (5/10/15/30/
  45/60 min, ticks regardless of play/pause, same as e.g. Audible's) that
  fires `onExpire` once and resets to `'off'`, or `'end-of-chapter'`,
  which instead fires the moment `chapterIdx` changes away from whatever
  it was when that option was picked. The Android analogue is
  `ParagraphPlayer.kt`'s `setSleepTimer`/`sleepTimer`
  (`SleepTimerOption.Countdown`/`.EndOfChapter`) — see `android/CLAUDE.md`.
- `src/hooks/useInView.ts` — thin IntersectionObserver wrapper used to grow
  the reader's loaded chapter range as sentinel divs above/below the
  content scroll into view. Takes a `deps` array so it re-observes (and
  therefore re-checks intersection) whenever the loaded range changes —
  necessary because IntersectionObserver only reports *changes* in
  visibility, and a short chapter on a tall screen might not actually
  leave the viewport between one expansion and the next.
- `src/pages/ExportPage.tsx` (`/books/$bookId/export`, from the library
  card's download icon) — book exports: builds (whole book or selected
  chapters, optionally split into N parts; paragraph or word/phrase sync
  tuned for a top playback speed; render missing audio first or export
  as-is) queue as Jobs-page tasks, and the list downloads, rebuilds or
  deletes each export (`bookExports` live topic).
- `src/routes.tsx` — code-based TanStack Router route tree (not
  file-based routing / no router-plugin codegen — deliberate, to keep
  the build simple for a two-page app). Add new routes here directly.
- `src/pages/LibraryPage.tsx`, `src/pages/ReaderPage.tsx` — the two
  screens. `ReaderPage` is an infinite-scroll reader: it keeps a `{start,
  end}` chapter-index window in state, renders every loaded chapter's
  paragraphs as one continuous flow, and grows the window via
  `useInView` sentinels rather than requiring a chapter to be picked. A
  "Jump to chapter" `<select>` remains as a fast way to skip far ahead —
  it resets the window to `{start: idx, end: idx}` and jumps playback
  there — but normal reading never needs it. Generation is auto-triggered
  per loaded chapter (not just "the current one") whenever it has pending
  paragraphs and isn't already generating (`backend`'s `EnqueueChapter` is
  idempotent per chapter, so this is safe to call opportunistically).
  Its self-contained pieces live beside it: `hooks/useChapterActions`
  (per-chapter pass buttons + the shared action error),
  `useChapterWindowing` (most-visible chapter + measured heights for
  windowing), `useAutoFollow` (scroll-follow of the playing paragraph),
  `useSFXOverlay`, `useReaderShortcuts`, `useStableCallback`,
  `useRemotePosition` (the "continued on another device" notice: the
  book's saved position moved by someone else while this reader sat
  paused - own saves are remembered so their echoes don't trigger it;
  mirrored in android's `ui/reader/RemotePosition.kt`), and
  `components/ChapterNotesBlock` (annotations view's chapter summary under
  the heading, editable in place: summary, cast with role/pronouns/what the
  text calls them, scenes with who is present as toggle chips - saved via
  `PUT .../notes`, marked edited, with "Re-run attribution now" after a
  save and a regenerate button that replaces the edits),
  the chapter header's "Re-attribute around my fixes" button
  (`useAttributeAroundEdits`, `?scope=edits`),
  `components/ParagraphContextMenu` (the annotations-view right-click menu,
  opened imperatively like `AnnotationTooltip`; its reassign lists are
  "This chapter" then "All characters", alphabetical - `utils/speakerOrder`,
  shared with the Speakers page's Narrator-first alphabetical roster).
  The annotations-view toggle is remembered in `localStorage`
  (`lectable.annotationsView`).
- `src/pages/SpeakersPage.tsx` — the roster and its handlers; the rest is
  in `src/components/speakers/`: `SpeakerSettings` (book-wide switches),
  `ChapterPassTable` (per-chapter pass table + whole-book Rest/All/Reset
  rows; its Summary column's eye icon expands a row with the chapter's
  summary notes - `chapterNotes` live topic - and its Ambience column's
  eye icon a per-region ambience preview, `AmbiencePreview`: mood,
  lighting swatch, soundscape, music prompt and a play button for
  generated regions), `SpeakerRow` (+ aliases/pronouns/emotions/merge),
  `SpeakerEvidence` (appearance/description rows), `CharacterVoiceEditor`,
  `PassStatus` (status cells, bulk button), `SettingPanel` (the book's
  setting profile - `bookSetting` live topic, editable, with a re-read
  button; see backend `httpapi/setting.go`), `LexiconPanel` (the series
  pronunciation lexicon - `lexicon` live topic; see backend
  `internal/pronounce/lexicon.go`). The Jobs page's "Narration left" line
  is `utils/narrationEta` (queued narration chars / the backend's measured
  `narrationCharsPerSecond`; android `ui/jobs/NarrationEta.kt`), which
  also shows that speed as a multiple of realtime
  (`narrationRealtimeFactor`).

## Dev server

```
npm run dev
```

`vite.config.ts` proxies `/api/*` to `http://127.0.0.1:8080` (the Go
backend), so the app fetches relative paths like `/api/books` and never
needs CORS in dev. If the backend runs on a different port, update the
proxy target here rather than hardcoding a backend origin in `client.ts`.

## Verifying changes

`npm test` runs Vitest over `src/**/*.test.ts` - pure modules only
(`lighting/` color math and `LightingEngine` against a fake bridge with
fake timers, `utils/`); there's no component/DOM test setup. `npx tsc -b`
for type errors, `npm run build` for a production build sanity check, and
— since this is a UI —
actually load it in a browser and click through upload → pick a chapter →
play, rather than trusting the build alone.
