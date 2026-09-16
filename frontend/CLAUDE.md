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

- `src/api/types.ts` — hand-written types mirroring the Go backend's JSON
  DTOs exactly (see `../backend/internal/httpapi/*.go`). Keep these in
  sync by hand if a backend DTO changes shape.
- `src/api/client.ts` — thin `fetch` wrappers, one per backend endpoint.
- `src/api/queries.ts` — TanStack Query hooks built on `client.ts`.
  `useChapter`/`chapterQueryOptions` poll every 1.5s while any paragraph in
  that chapter is still pending/generating, and stop once everything is
  settled. `useChapterRange(bookId, start, end)` fetches a contiguous,
  inclusive range of chapters via `useQueries` — this backs the
  infinite-scroll reader, which can have several chapters loaded (and
  polling) at once.
- `src/hooks/usePlayback.ts` — the sequential-chunk audio player. A book
  has one `.wav` per paragraph, not one per chapter; this hook owns a
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
  whole chapter object, specifically so the 1.5s poll doesn't restart
  playback on every refetch.
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
