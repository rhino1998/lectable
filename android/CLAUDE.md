# android

Native Android client (Kotlin + Jetpack Compose). Talks only to `../backend`'s
HTTP API, same as `../frontend`, but without a dev-server proxy to hide the
backend's origin, so the app itself has to know where the backend lives
(see "Server address" below).

`./gradlew assembleDebug` and `./gradlew test` both build and pass in this
dev environment - see "Toolchain note" for how that SDK got installed.
Verified extensively on a real physical device over wireless ADB (this box
has no accelerated emulator - see below).

## Toolchain note

This dev environment has no Android Studio and, until it was needed here,
no Android SDK - only a JVM (`openjdk 21`). A **Linux-native** SDK (just
what's needed to build: cmdline-tools, `platform-tools`,
`platforms;android-34`, `build-tools;34.0.0` - no emulator/system-image)
is installed at `/home/rhino/android-sdk-toolchains/`, same pattern as the
Go/Node toolchains in the root `CLAUDE.md`'s "Environment gotchas".
`android/local.properties` (gitignored) points `sdk.dir` at it.

```
export ANDROID_SDK_ROOT=/home/rhino/android-sdk-toolchains
cd android
./gradlew assembleDebug   # -> app/build/outputs/apk/debug/app-debug.apk
./gradlew test            # pure-JVM unit tests
```

Two real bugs surfaced and got fixed on the first build here (both now
correct in committed source, noted in case this drifts again): `themes.xml`'s
manifest theme referenced `android:Theme.Material.DayNight.NoActionBar`,
which doesn't exist - `DayNight` is an AppCompat/MaterialComponents feature
this project doesn't depend on (Compose's `isSystemInDarkTheme()` handles
light/dark instead); and `NetworkModule.kt` imported
`retrofit2.converter.kotlinx.serialization.asConverterFactory`, but that
symbol actually lives in `com.jakewharton.retrofit2.converter.kotlinx.serialization`.

**This dev environment cannot run an emulator** - WSL2 with no `/dev/kvm`
exposed. Building happens here; verification happens against a real
physical device over **wireless ADB**: pair once
(`adb pair <ip>:<port>`, code under Developer options > Wireless
debugging), then the device shows up in `adb devices` on the same LAN, no
cable needed. The Windows-side SDK's `platform-tools\adb.exe` is reachable
from WSL via Windows interop (full `/mnt/c/...` path, not on PATH by
default). Workflow: `./gradlew assembleDebug`, then from WSL:
`"/mnt/c/.../adb.exe" -s <device-id> install -r app/build/outputs/apk/debug/app-debug.apk`,
`adb.exe -s <device-id> shell am start -n com.lectable.app/.MainActivity`,
and `adb.exe -s <device-id> exec-out screencap -p -d <display-id> > file.png`
to see the result (a multi-display device may need the right `-d`, found
via `adb.exe shell dumpsys SurfaceFlinger --display-id`). A real device has
no `10.0.2.2` loopback alias (emulator-only) - point Settings at the
backend host's actual LAN IP.

## Server address

The backend is self-hosted on the LAN with no auth (root `CLAUDE.md`'s
Conventions) - unlike the web frontend, which gets `/api/*` for free from
Vite's dev proxy, the app needs to know where it lives.
`ui/settings/SettingsScreen.kt` persists this (via DataStore, see
`data/settings/ServerSettingsRepository.kt`) and
`data/remote/DynamicBaseUrlInterceptor.kt` rewrites every outgoing
Retrofit request's scheme/host/port to match at request time, so the
address can change without restarting the app. Defaults to
`http://10.0.2.2:8080/` (the emulator's alias for the host's localhost)
via `BuildConfig.DEFAULT_SERVER_URL` - a **real device** needs the host's
actual LAN IP entered in Settings instead. Plain HTTP is expected
(`usesCleartextTraffic="true"`).

While Settings is open, `data/discovery/NsdDiscoveryRepository.kt` browses
for the backend's `_lectable._tcp` mDNS advertisement (see
`../backend/internal/mdnsadvert`) via `NsdManager` and lists hits as
tappable chips that fill in the URL field - manual entry stays the
fallback for whenever mDNS doesn't reach the phone (different subnet,
client-isolated Wi-Fi, mobile data). Discovery starts/stops with the
Settings screen's lifecycle, not app-wide, since it's a standing
multicast listener.

**First launch** also tries this automatically, once, before Settings is
ever opened: `LibraryViewModel.autoConfigureServerIfNeeded` (gated on
`ServerSettingsRepository.isConfigured` - whether a URL has ever actually
been saved) runs the same discovery for up to `AUTO_DISCOVERY_TIMEOUT_MS`
(4s) and saves whichever backend answers first. Silently leaves the
compiled-in default in place if nothing answers in time.

## Layout

- `data/remote/dto/` - hand-written `@Serializable` DTOs mirroring the Go
  backend's JSON exactly (see `../backend/internal/httpapi/*.go`,
  cross-checked against `../frontend/src/api/types.ts`). Keep in sync by
  hand if a backend DTO changes shape. Note the inconsistent casing:
  `VoicePresetDto` has `ref_text`/`speed_multiplier` (curated built-in
  presets, proxied as-is from `backend/internal/voices`), while
  `CustomVoicePresetDto` has `refText`/`speedMultiplier` (backend's own
  DTO for user-created voices).
- `data/remote/LectableApi.kt` - Retrofit interface, one method per
  endpoint, paths relative (host attached per "Server address" above).
- `data/remote/MediaUrlResolver.kt` - resolves relative `coverUrl`/
  `audioUrl` DTO paths into absolute URLs for Coil/ExoPlayer.
- `data/repository/` - `LibraryRepository` (books/chapters/position/search)
  and `VoiceRepository` (presets/per-book voice), thin wrappers over
  `LectableApi`, mirroring `../frontend/src/api/client.ts`.
- `data/settings/` - one repository per preference, DataStore-backed:
  `ServerSettingsRepository` (backend URL), `ThemeSettingsRepository`
  (system/light/dark), `ReadingSettingsRepository` (reader font
  size/family - Android-only, no web equivalent), `PlaybackSettingsRepository`
  (last-picked speed).
- `data/discovery/NsdDiscoveryRepository.kt` - mDNS backend discovery, see
  "Server address" above.
- `data/remote/LiveClient.kt` - the Android counterpart of
  `../frontend/src/api/live.ts`: the app's only source of server *state*.
  One app-scoped OkHttp WebSocket to `GET /api/events` (backend
  `internal/live`); `observe<T>(topic, params)` returns a cold
  `Flow<LiveResult<T>>` that subscribes on collect and releases on cancel.
  The first value is a full snapshot, then a new one after every backend
  patch (JSON ops applied to immutable `JsonElement` trees in `applyLiveOps`,
  then decoded into the usual DTOs on `Dispatchers.Default`). Subscriptions
  are ref-counted per (topic, params) and linger 5s after the last
  collector goes. The socket is open only while something is subscribed,
  reconnects with backoff (resubscribing everything), and on a Settings
  server-address change drops every cached value and re-points. When the
  backend can't be reached before a topic ever got a value, that topic
  reports `LiveError.isNetwork` (status 0) - what `LibraryViewModel`/
  `ReaderViewModel`/`DownloadRepository` key their offline fallbacks on;
  values already received stay visible across a reconnect. Nothing reads
  `LiveClient` directly except `LiveStore` (below) - there is no polling and no
  "refetch after mutation" anywhere; a mutation's own write is what
  produces the update. REST (`LectableApi`) remains for mutations and
  one-off reads (search, pickers, the voice-language list). The shared
  `OkHttpClient`'s `pingInterval` keeps the socket from tripping its
  `readTimeout` during quiet stretches.
- `data/live/LiveStore.kt` - the app's single store of server state, on
  top of `LiveClient`: one shared `StateFlow<LiveResult<T>>` per topic
  instance (`books()`, `book(id)`, `chapter(id, idx)`, `jobs()`, ...), so
  each push is decoded once and reaches every reader at the same moment.
  Every screen and `DownloadRepository` read from it. Keeps the last-known
  value across reconnects/restarts instead of flashing empty (dropped only
  on a server-address change, `LiveResult.reset`). Library-wide topics
  (`books`, built-in/custom presets, default voice) are **eager**: kept
  subscribed while the app is in the foreground (`MainActivity.onStart`/
  `onStop` -> `setForeground`) whether or not a screen shows them;
  per-book/per-chapter topics and `jobs` are live only while read (each
  subscription costs a backend rebuild on every relevant write), then
  linger 5s; at most 64 non-eager instances are cached (LRU).
- `data/live/OfflineCacheWriter.kt` - `LiveStore`'s write-through into the
  Room offline cache: every `books` value refreshes downloaded books'
  title/author/cover/chapter count, and every `chapter` value that passes
  through the store refreshes that chapter's downloaded copy (title, text,
  speakers, annotations, content/image order) if it's COMPLETE - keeping
  each paragraph's stored `durationSeconds`/`audioPointerSeconds`, which
  describe the downloaded .wav, not whatever the backend has since
  re-generated. Only updates existing rows; what gets downloaded is still
  `DownloadRepository`'s call. Uses the DAOs directly (not
  `DownloadRepository`, which depends on `LiveStore` - a cycle).
- `playback/ParagraphPlayer.kt` - the Android analogue of
  `../frontend/src/hooks/usePlayback.ts`: one ExoPlayer instance advancing
  through a book's paragraphs one `.wav` at a time. **App-scoped
  (`@Singleton`), not tied to the Reader screen's ViewModel** - playback
  (and the `MediaSession` exposing it to the system) needs to survive
  leaving/backgrounding the reader; `ReaderViewModel.onCleared()`
  deliberately does not release it. Keyed by `(chapterIdx, paragraphIdx)`
  pairs looked up in a loaded-chapters map rather than a flat index, and
  parks on `pendingTarget` + fires `onNeedChapter` when the next
  paragraph's chapter/audio isn't ready yet - `ReaderViewModel` re-feeds it
  via `setChapter()` once that data arrives. Also owns the sleep timer
  (`setSleepTimer`/`sleepTimer` - countdown or "end of chapter"), for the
  same survive-backgrounding reason.
- `playback/BackgroundMusicPlayer.kt` - the Android analogue of
  `../frontend/src/hooks/useBackgroundMusic.ts`: mixes a chapter's own
  generated background-music regions (`GET .../chapters/{idx}/music`, see
  backend `store.MusicRegion`) in underneath narration as `ParagraphPlayer`
  advances, entirely client-side - the backend only ever hands over
  per-region clips, never a chapter-length stitched track. Runs two
  independent ExoPlayer "decks" so a region switch can crossfade the
  outgoing clip into the incoming one (a `MusicTransitionCut` region) or,
  for a genuine adjacent hand-off into a `MusicTransitionContinuation`
  region (seeded server-side from the preceding region's own tail), cut
  over instantly with no fade. Each deck's real ExoPlayer volume is two
  independently-ramped stages multiplied together (`Deck.ownLevel` ×
  `masterLevel`, see `applyVolume`) - mirroring the hook's own two-GainNode
  chain (a deck's own gain feeding into one shared master gain) - so a
  region crossfade (`CROSSFADE_SECONDS`, 1.5s) and the book-wide toggle's
  own fade (`MASTER_FADE_SECONDS`, 0.8s, see `setMusicEnabled`) never fight
  over one shared value. A region switch also overlaps its neighbor by
  `REGION_OVERLAP_FRACTION` (5%, matching the hook's own constant) of each
  clip's own duration on both ends: the *incoming* region starts fading in
  this fraction of its own length before the paragraph boundary that owns
  it actually arrives (pre-roll, in `tick()`), and the *outgoing* region
  symmetrically keeps playing at full volume this same fraction of its own
  length past that boundary before its own fade-out even starts
  (post-roll, `stopDeckAfter`'s own `delaySeconds`) - a genuine overlap
  window, not just a same-instant crossfade. This overlap works across a
  chapter boundary too, not just between two regions of the same chapter:
  `tick()` checks `chapterParagraphCounts` (fed by `ReaderViewModel
  .mergeChapter` unconditionally, same as `ParagraphPlayer
  .setChapter`) to tell "paragraphIdx is this chapter's real last
  paragraph" apart from merely "the last one some region happens to cover"
  (an unscored trailing paragraph must never trigger this early), and if
  so looks at the *next* chapter's own first region (already present in
  `chapterMusic` - every loaded chapter's regions are live regardless of
  which one is currently playing) for the pre-roll target instead of
  bailing out at the chapter's own edge. A region's own transition never
  actually spans chapters in practice (scoring has no cross-chapter
  context to seed a continuation from), so a cross-chapter switch always
  falls back to an ordinary crossfade, never the seamless cut - that falls
  out naturally rather than needing its own special case. **App-scoped (`@Singleton`) like
  `ParagraphPlayer`** - owns its own tick loop observing `ParagraphPlayer
  .state`/`.positionMs()`/`.durationMs()` directly (constructor-injects
  `ParagraphPlayer`) rather than requiring a ViewModel to push per-tick
  updates. `ReaderViewModel.syncMusicSubscriptions` keeps a live
  `chapterMusic` topic subscription per loaded chapter while
  `VoiceSettingsDto.musicEnabled` (the reader-facing, book-wide toggle -
  `VoicePickerSheet`'s "Background music" switch, alongside its existing
  multi-voice one) is on; `onChapterMusic` feeds each value to the player
  via `setChapterMusic` and lazily triggers `POST .../score-music` once
  per chapter (`scoringMusicChapters` plus a job-queue check guard against
  re-triggering it). The reader's own per-region "Generate"/
  "Regenerate" and preview-clip actions live in annotations mode's
  `MusicRegionBoundaryRow` (`ui/reader/ReaderScreen.kt`) - see below.
- `playback/PlaybackService.kt` - a `MediaSessionService` hosting
  `ParagraphPlayer`'s session as a foreground service, so playback
  survives backgrounding/screen-off and exposes system-level play/pause
  (notification, lock screen, headset/Bluetooth). Doesn't own the player
  itself; started via `ContextCompat.startForegroundService` the moment
  `ParagraphPlayer` starts playing, not unconditionally at app launch.
- `ui/reader/WordHighlight.kt` - karaoke-style word highlight timing. Uses
  real per-word forced-alignment timings from the worker's `POST /align`
  (`ParagraphDto.words`) when present and matching the paragraph's
  whitespace tokenization 1:1; falls back to a proportional
  character-position estimate otherwise (alignment runs as a detached
  follow-up, so a freshly-generated paragraph briefly uses the estimate
  until its alignment push arrives) - mirrors
  `../frontend/src/components/ParagraphText.tsx`'s `wordStartTimes`
  exactly, including never mixing real and estimated timings within one
  paragraph.
- `ui/library/` - `LibraryScreen` (list, cover, progress bar, upload,
  pull-to-refresh) + `LibraryViewModel`. A book row's long-press menu
  (mirroring frontend's `BookCard` icon row) has Preprocess/Speakers/
  Download book/Remove downloaded copy (only shown once
  `LibraryUiState.downloadedBookIds` says there's a local copy)/Delete.
  Preprocess kicks off the whole-book meta-task
  (`POST /api/books/{id}/preprocess`) and shows a dimmed spinner overlay
  while `BookSummaryDto.preprocessing` is set (live, via the `books`
  topic). Download book enqueues every chapter via
  `ChapterDownloadWorker` (pinned, `ExistingWorkPolicy.KEEP`), fire-and-forget.
- `ui/reader/` - `ReaderScreen`: an infinite-scroll reader like the web
  frontend's `ReaderPage.tsx` (a `{range}` chapter-index window grown via
  `LazyListState.layoutInfo` instead of `IntersectionObserver`), plus a
  "jump to chapter" sheet (each row itself long-press-able for a
  chapter-scoped menu: "Generate chapter audio", "Attribute & tag chapter"
  - `ReaderViewModel.generateChapter`/`attributeChapter` - and, once
  something's downloaded, "Delete downloaded data"), bookmarks/search
  sheets, and a long-press paragraph menu (regenerate/bookmark/copy/set
  speaker). Book-level
  settings (Narrator voice, Speakers, offline-download controls) live
  behind one `⋮` overflow menu rather than always-visible icons - occasional
  settings a reader dips into, unlike Play/Pause which stays in the bottom
  `PlaybackBar`. `ReaderViewModel` subscribes to the book, voice,
  bookmarks, every loaded chapter (`subscribeChapter`/`awaitChapter` -
  jumps cancel and resubscribe, which replays the lingering value) and,
  while music is on, every loaded chapter's music; it also owns throttled
  position sync (`POSITION_SAVE_MIN_INTERVAL_MS`) and injects the
  app-scoped `ParagraphPlayer`.
- `ui/settings/` - `SettingsScreen` for backend URL, theme, reader font
  size/family, an offline-storage summary + "Delete all downloads"
  (`DownloadRepository`, walked off the filesystem rather than summed from
  `DownloadedChapter.totalBytes` alone, since that column only ever
  tracked audio, not the cover images `downloadChapter` also saves), and
  links to Voices and Jobs.
- `ui/jobs/` - `JobsScreen`/`JobsViewModel`, the Android analogue of
  `../frontend/src/pages/JobsPage.tsx`: a live view of the backend's
  shared job queue (`backend/internal/jobs.Manager`), live via the `jobs`
  topic (`LiveClient`). `QueueTaskDto.kind` is a plain `String`, not a closed enum -
  `backend/internal/jobs.Kind` has grown before and an unrecognized value
  would fail to deserialize the whole snapshot; `kindLabel()` falls back
  to the raw string instead. Reachable from Settings, not a bottom-level
  nav destination - a diagnostics view.
- `ui/speakers/` - `SpeakersScreen`/`SpeakerViewModel`, the per-book
  character roster - see "Multivoice playback and character management"
  under Known scaffold limitations for scope.
- `ui/navigation/` - `Routes.kt` + `LectableNavHost.kt` (Library / Reader /
  Settings / Voices / Jobs / Speakers, Navigation-Compose, code-based like
  the frontend's `routes.tsx`).

## Known scaffold limitations (not yet implemented)

- **Voice picker only swaps curated presets**, using the preset's own
  `instruct` text and a hardcoded `"Auto"` language. No UI for *creating or
  editing* custom voice presets (`VoiceRepository.createCustomPreset`/etc.
  are wired but unused) or for editing `instruct`/`language`/`seed`
  directly - you can pick an existing custom preset, just not make one.
- **Multivoice playback and character management work, at reduced scope
  vs. `SpeakersPage.tsx`.** `ui/speakers/SpeakersScreen.kt` +
  `SpeakerViewModel` cover: the multi-voice toggle, a "Preprocess book"
  button, and the roster itself (ready/total counts, characterization
  summary, ref-clip playback, a `⋮` menu for
  Characterize/Generate-voice/Assign-voice/Merge/Delete). Tapping a row
  expands its appearances list for regenerate/reassign; the `⋮` menu's own
  "View descriptions" independently expands that same character's
  descriptions list (paragraphs whose narration describes them, not lines
  they speak) with its own "Reassign to…" (`SpeakerViewModel
  .reassignDescription`, `PUT .../paragraphs/{pidx}/description`) - moves
  the paragraph off this character's describes-list onto another's, same
  per-line correction shape as appearances' own reassign, mirroring
  frontend's own `DescriptionRow`. "Assign voice" opens a picker over
  built-in + custom presets with a hand-off into `VoicesScreen` to create a
  new one first - no separate voice-editor form was built here, since
  `VoicesScreen` already has one. Still web-only: per-chapter attribution/
  description/direction-tag status and individual buttons from
  `SpeakersPage.tsx` itself (the single "Preprocess" call covers the
  common path here instead, and the reader's own chapter picker long-press
  menu - see below - covers attribution/tagging one chapter directly), and
  every bulk endpoint (characterize-all/generate-voices-all/regenerate-
  voices-all) - each character's own menu action is the single-item call
  instead. A
  paragraph's own long-press "Set speaker" still lives directly in the
  reader - only shown when the block has at least one `isQuote` segment
  (the backend 400s on attributing narration to a character). Metadata-only,
  same as the web action - doesn't touch already-cached audio, so hearing
  the new voice still needs a follow-up regenerate.
- **Speech-direction tags and pronunciation fixes both display, but
  running either pass is web-only.** `ParagraphDto.directionMarks` mirrors
  backend's inline Higgs delivery tags (e.g. `<|emotion:anger|>`,
  `<|sfx:laughter|>`), each pinned to the exact rune offset it was
  inserted at. `ParagraphDto.pronunciationMarks` mirrors resolved
  pronunciation substitutions (e.g. "Dr." -> "Doctor") the same way, plus
  original/replacement text for display. Both are shown as inline
  annotations (direction tags: an up-arrow caret below the line;
  pronunciation fixes: a strikeout through the exact word - matching
  frontend's `DirectionCaret`/`PronunciationStrike` exactly) and as entries
  in the long-press menu. `POST .../chapters/{idx}/tag-directions` itself
  (web's "Tag directions" button, which also runs pronunciation resolution
  server-side) *is* reachable here too, just not from this screen - the
  reader's own chapter picker ("Jump to chapter" sheet) has a long-press
  menu per chapter with "Generate chapter audio" and "Attribute & tag
  chapter" (`ReaderViewModel.attributeChapter`, firing attribute-speakers/
  retag-descriptions/tag-directions together for that one chapter) - see
  `ui/reader/` below. Both survive offline downloads
  (`OfflineParagraph.directionMarks`/`.pronunciationMarks`).
- **Annotations mode**: a palette-icon toggle in the bottom playback bar
  (local Compose state, not persisted) drawing three independent kinds of
  marks via `drawBehind`/`wordLineBounds`, generalized from the active-word
  highlight box to a whole segment/offset/word's position:
    - An **underline** under dialogue/descriptive-narration segments
      (`isQuote`/`describesCharacters`), colored blue/yellow matching
      frontend's `--annotation-speaker`/`--annotation-description`.
    - A small open-caret ("^") below the line at each delivery tag's
      insertion point, colored per category (emotion/style/prosody/sfx)
      matching frontend's `--direction-*` variables - composable with the
      underline above.
    - A **strikeout** through each pronunciation fix's exact word,
      deliberately a different shape from the delivery-tag caret so the
      two stay distinguishable, matching frontend's `--pronunciation-caret`.
  Deliberately does not port frontend's fuller `annotationsView`: no
  background color wash per kind, no legend, no "select a speaker to
  highlight their lines." Not purely read-only, though: a block's
  long-press menu (available with or without annotations mode on) lists
  each distinct `Speaker`/`describesCharacters` name as its own tappable
  row - "Speaker: X" opens `SpeakerPickerSheet` (unchanged), and each
  "Describes: X" opens `DescriptionPickerSheet`, reassigning just that
  block's own segment(s) actually carrying that name to a different
  character (or "Remove", describing no one) via `ReaderViewModel
  .reassignDescription` - the reader's own inline counterpart to
  `ui/speakers/`'s per-character "descriptions" list, and to frontend
  ReaderPage's own annotations-view right-click "Reassign description"
  menu (see `frontend/CLAUDE.md`).

  A fourth kind of mark, background-music region boundaries, is a labeled
  divider row (`MusicRegionBoundaryRow`) rather than an inline mark like
  the three above - one row per region, inserted immediately before the
  paragraph block where it starts (`ReaderListEntry.MusicBoundary`, built
  by `groupContent`'s own `musicRegionsByStartIdx` map, only while
  annotations mode is on), showing mood, transition (`continues`/`new
  cue`)/generation status, duration once ready, the region's own full
  Stable Audio prompt, a preview-clip toggle (a standalone `MediaPlayer`,
  independent of narration and of `BackgroundMusicPlayer`'s own live mix -
  `ReaderViewModel.previewMusicRegion`/`.stopMusicPreview`), and a
  Generate/Regenerate button (`ReaderViewModel.regenerateMusicRegion`) -
  mirrors frontend's `ChapterSection.MusicRegionBoundary` exactly, minus
  its hover tooltip (the prompt is just shown inline instead, better
  suited to touch). See `playback/BackgroundMusicPlayer.kt` above for the
  actual audio mixing this annotates.

Infinite scroll, background/lock-screen playback (notification + system
play/pause), search, and bookmarks are all implemented (see "Layout"
above) - check here before trusting otherwise if this list drifts.

## Testing

`app/src/test/` has pure-JVM unit tests (`ServerSettingsRepositoryTest.kt`,
and `LiveOpsTest.kt` for `applyLiveOps` against the backend's patch wire
format) - run and pass via `./gradlew test`. `app/src/androidTest/` has one
Compose smoke test that launches `MainActivity` and checks the app title
renders - needs a device/emulator. `./gradlew connectedAndroidTest` run
from WSL finds and uses the real device over wireless ADB (the WSL-side
`adb` binary sees it independently, no need to route through `adb.exe`)
and passes.
