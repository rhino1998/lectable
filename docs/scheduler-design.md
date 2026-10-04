# Job scheduler design (task-backlog item 9; covers 6, 7, 8)

Replaces `backend/internal/taskqueue` and the two queues in
`backend/internal/jobs` with one package, `backend/internal/sched`,
written from scratch. Decisions below were made with the user on
2026-10-03.

## Why

The old queue grew one rule per incident:

- two queues and two worker loops (ordinary tasks; preprocessing phases and
  bulk groups, which waited on their children by polling);
- dependencies re-resolved for every queued task on every pop, creating
  tasks as a side effect;
- promotion was permanent, so a task stayed urgent after whatever made it
  urgent was cancelled;
- exclusivity spread over pools, "one pool at a time", a model-family
  unload hook, and a fake dependency capping attribution to one;
- the Jobs view sorted by a rule close to, but not the same as, dispatch.

## Model

One graph. A **task** has:

- **requests**: `source -> priority`. A source names who wants it (a
  reader's window for a book, a person on the Jobs page, a restart
  recovery). A source can withdraw its request.
- **needs**: other tasks, declared by the task's `Prepare`. Two kinds:
  - a **dependency** - the task needs its output. If it fails or is
    cancelled, the task fails too (always; there is no optional
    dependency. Whether a book's setting pass is required is a per-book
    setting, decided by the task's `Prepare`, not by the scheduler).
  - a **member** - the task is a group that asked for it. A member's
    failure doesn't fail the group; the group runs once every member has
    ended.

**Effective priority** is derived, never stored: the most urgent of a
task's own requests and the effective priorities of the tasks that need
it. It is recomputed on every change, so it drops when an urgent requester
goes away (item 7).

**Cancel cascade** (item 6): when a task ends for any reason, each of its
unfinished needs loses it; a need left with no request and nothing needing
it is cancelled, recursively. A task someone requested directly is never
cancelled this way. Cancelling a task by hand fails its dependents.

**Evaluation**: `Prepare` runs when a task is added, and once more each
time its outstanding needs have all ended (so a group can name its members
only once the phases before it are done, and a retry re-checks what it
needs). A need that already ended for this task is not asked for twice.
Nothing is polled in between.

**Retries** belong to the scheduler: a failed task stays the same node,
keeps its requesters and dependents, and goes back through `Prepare` with
its attempt count raised (3 attempts). Dependents fail only when attempts
run out. A task can also yield (`ErrYield`): back to ready, no attempt
counted.

## Exclusivity and contexts

A task names the **context** it needs - the model that must be resident -
or none (groups). At most one context has running tasks. Within a context
each task decides for itself with `CanStart(running)` (capacity, "one
attribution at a time").

## Order

One total order, used for dispatch and for display (item 8):

1. effective priority;
2. tasks in the current context (or needing none) first;
3. the task's own `Less` (reading position, kind rank, reference voice);
4. arrival, as the last tiebreak only.

Priority classes used by `jobs`: Live (a person is blocked on it now),
Lookahead (the reader's window and the prerequisites of their next
chapters), Promoted (bumped by hand), Background.

## Admission

After every change, walk the ready tasks in order:

- A task in the current context (or needing none) starts if `CanStart`
  allows.
- The first task needing another context decides whether to switch:
  - **stay on a tie**: no switch while the current context has a ready
    task at least as urgent;
  - **dwell**: within 60 s of the last switch, only Live work may force
    another, unless the current context has run out of work at the
    priority that caused the last switch;
  - otherwise **drain**: nothing more is admitted to the current context;
    once it has nothing running, switch.
- `ShouldYield(key)` tells a long-running task to stop at its next batch
  boundary: true when a strictly more urgent ready task is blocked by it,
  in its own context or by a pending switch.

## Rules that live in `jobs`, not the scheduler

All hold today, except that the Jobs page can add a bump but not yet remove one.

- Order that affects results is a dependency, never a priority
  (attribution in chapter order, summary before attribution, emotion and
  pronunciation before audio, voice before lines).
- Background clone work within a chapter groups by reference voice; Live
  and Lookahead keep reading order.
- **Prework**: entering chapter N requests the passes of chapters N+1 to
  N+3 at Lookahead, from the reader's source.
- **Windowed live passes**: a Live paragraph needing a pass that hasn't
  run gets the batch around the playhead first; the rest requeues at
  Lookahead.
- **Jump-ahead**, per book: `strict` makes narration depend on the
  order-dependent passes; `reader-first` (default) narrates with what is
  known and re-renders what the passes later change.
- The Jobs page's priority control adds or removes a request from the
  `bump` source; it can't push a task below what needs it.
- Pause is an admission gate (`SetGate`); a paused, drained queue admits
  only explicit previews.

## How `jobs` uses it

- A pool (`generation`, `design`, `sfx`, `llm`) is a context; `CanStart`
  holds each to its capacity and attribution to one at a time.
- Everything asked for outright is a request from the `direct` source.
- A preprocessing phase or bulk group is a task with no context whose run
  asks for its work through `Scheduler.Adopt`: a member edge made while the
  group runs, rather than one declared by `Prepare`, because the existing
  phase functions discover their work as they go. Paragraph audio a group
  queues isn't adopted yet.
- A paused LLM pass and a stale render return `ErrYield`.
- Tests step dispatch by hand (`Config.Manual`, `Pop`/`Finish`).

## Status

1. Done (2026-10-03): `internal/sched` with its own tests.
2. Done (2026-10-03): `internal/jobs` ported, `internal/taskqueue` deleted.
3. Done (2026-10-03): the reader rules (`jobs/reader.go`) - a request
   source per reader (`clientId` on the lookahead call), withdrawn as the
   reader moves; prework for the next 3 chapters; window passes for the
   emotion and pronunciation passes; per-book strict order and
   setting-required.
4. Not built (task-backlog.md item 31): removing a bump from the Jobs page,
   adopting paragraph audio, the two settings in the Android app, and
   persistence - store root requests (source, priority, kind, ids) and
   re-request them at startup; needs are rebuilt by `Prepare`; blocking
   one-off calls (previews) are not restored.
