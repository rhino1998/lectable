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

## Dev server

```
npm run dev
```

`vite.config.ts` proxies `/api/*` to `http://127.0.0.1:8080` (the Go
backend), so the app fetches relative paths like `/api/books` and never
needs CORS in dev. If the backend runs on a different port, update the
proxy target here rather than hardcoding a backend origin in `client.ts`.

## Verifying changes

There's no component test setup. `npx tsc -b` for type errors, `npm run
build` for a production build sanity check, and — since this is a UI —
actually load it in a browser and click through upload → pick a chapter →
play, rather than trusting the build alone.
