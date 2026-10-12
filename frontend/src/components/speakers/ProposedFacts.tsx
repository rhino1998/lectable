import { useState } from 'react'
import { RiRestartLine, RiSearchLine } from 'react-icons/ri'
import { ApiError } from '../../api/client'
import {
  useFactFindingRunning,
  useFindBookFacts,
  useResetBookFacts,
  useReviewBookFact,
  useSkipBookFact,
  useUpsertBookFact,
} from '../../api/queries'
import { WORD_CLASSES, WORD_CLASS_LABELS, type WordClass, type WorldFact } from '../../api/types'

// Where in the series a proposed fact's quote is.
function where(f: WorldFact): string {
  if (f.evidenceChapterIdx == null) return ''
  const chapter = f.evidenceChapterTitle || `Chapter ${f.evidenceChapterIdx + 1}`
  return f.evidenceBookTitle ? `${f.evidenceBookTitle}, ${chapter}` : chapter
}

// The class waiting for the reader on f, if that is all that is proposed
// of it: a new word with a class and no text, or a class for a fact
// already in force. A proposed fact with text (a race and its lifespan)
// is reviewed as a fact, its class with it.
function wordProposal(f: WorldFact): WordClass | undefined {
  if (f.proposedClass) return f.proposedClass
  return f.proposed && f.fact === '' ? f.class : undefined
}

const CLASS_HINTS: Record<WordClass, string> = {
  title: 'ranks and offices people hold: "Prognugator Tersk" may be "Tersk"',
  role: 'jobs and classes: a name made only of these is a role, not a person',
  race: 'kinds of being: "Drake Tesy" may be "Tesy", and "Drake" alone is a role',
  nationality: 'peoples, tribes and factions: "Pallassian Gnoll" is a role',
  place: 'places: never short for someone whose name has it',
}

// Facts a search of this book's series proposed - how long each kind of
// being lives, how its people speak - each with the sentences it rests
// on, one a line. None is in
// force until accepted; the wording can be changed first. Accepting one
// recasts the characters it is plainly about (their name or casting says
// they are that kind); skipping one keeps it from being proposed again.
// Under them, the words character names share that the search took for a
// title, role, race, nationality or place (wordProposal), grouped by
// class: accepting one recasts nobody and only changes how the roster
// reads names.
export function ProposedFacts({
  bookId,
  proposals,
  skipped,
}: {
  bookId: string
  proposals: WorldFact[]
  // Facts the reader skipped and facts in force with a declined class:
  // listed so either can be put back for review.
  skipped: WorldFact[]
}) {
  const find = useFindBookFacts(bookId)
  const running = useFactFindingRunning(bookId)
  const accept = useUpsertBookFact(bookId)
  const skip = useSkipBookFact(bookId)
  const reset = useResetBookFacts(bookId)
  const review = useReviewBookFact(bookId)
  const [reviewingAll, setReviewingAll] = useState(false)
  // Wording the reader has changed, by fact id.
  const [edits, setEdits] = useState<Record<string, string>>({})
  // Classes the reader has changed, by fact id.
  const [classes, setClasses] = useState<Record<string, WordClass>>({})
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [acceptingAll, setAcceptingAll] = useState(false)
  const busy = accept.isPending || skip.isPending || acceptingAll
  const onError = (err: unknown) =>
    setError(err instanceof ApiError ? err.message : 'Could not save that')

  const facts = proposals.filter((f) => !wordProposal(f))
  const words = proposals.filter((f) => wordProposal(f))
  const classOf = (f: WorldFact) => classes[f.id] ?? wordProposal(f)!
  const wordBody = (f: WorldFact) => ({
    id: f.id,
    key: f.key,
    aliases: f.aliases,
    fact: f.fact,
    class: classOf(f),
    effect: f.effect,
  })

  const runAccept = (f: WorldFact) => {
    setError(null)
    setNotice(null)
    accept.mutate(
      {
        id: f.id,
        key: f.key,
        aliases: f.aliases,
        fact: edits[f.id] ?? f.fact,
        class: f.class,
        effect: f.effect,
      },
      {
        onSuccess: (saved) =>
          setNotice(
            saved.recast === 0
              ? `Accepted "${f.key}". No character's casting names that kind, so nobody was recast.`
              : `Accepted "${f.key}". ${saved.recast} ${saved.recast === 1 ? 'character is' : 'characters are'} being recast.`,
          ),
        onError,
      },
    )
  }
  const runSkip = (f: WorldFact) => {
    setError(null)
    setNotice(null)
    skip.mutate(f.id, { onError })
  }
  const runReset = () => {
    if (
      !confirm(
        'Remove every fact the search proposed for this series, including the ones you accepted or skipped? Facts you wrote yourself stay. Nobody is recast, and roster changes already made are not undone.',
      )
    )
      return
    setError(null)
    setNotice(null)
    reset.mutate(undefined, {
      onSuccess: (r) =>
        setNotice(`Removed ${r.removed} found ${r.removed === 1 ? 'fact' : 'facts'}.`),
      onError,
    })
  }
  const runReview = (f: WorldFact) => {
    setError(null)
    setNotice(null)
    review.mutate(f.id, { onError })
  }
  const runReviewAll = async () => {
    setError(null)
    setNotice(null)
    setReviewingAll(true)
    try {
      for (const f of skipped) await review.mutateAsync(f.id)
    } catch (err) {
      onError(err)
    } finally {
      setReviewingAll(false)
    }
  }
  const runAcceptWord = (f: WorldFact) => {
    setError(null)
    setNotice(null)
    accept.mutate(wordBody(f), { onError })
  }
  const runAcceptAll = async (group: WorldFact[], label: string) => {
    setError(null)
    setNotice(null)
    setAcceptingAll(true)
    try {
      for (const f of group) await accept.mutateAsync(wordBody(f))
      setNotice(
        `Accepted ${group.length} ${label.toLowerCase()} ${group.length === 1 ? 'word' : 'words'}. Review the roster from the Speakers tab to see what that changes.`,
      )
    } catch (err) {
      onError(err)
    } finally {
      setAcceptingAll(false)
    }
  }

  return (
    <section className="lexicon-panel">
      <h2>Proposed facts</h2>
      <p className="muted">
        A search of the text for what it says about each kind of being in its world: how long it
        lives and ages, and how its people speak. The second is often one character&rsquo;s voice
        mistaken for a rule, so read the quote. Nothing found is used until you accept it. Accepting
        a fact recasts the characters whose name or casting says they are that kind, which replaces
        their voice and the audio of their lines. The same search looks for where years pass in the
        story (proposed with the time jumps on the Speakers tab) and for what the text says of
        single characters (Character notes, below). It also sorts the words that character names
        share into titles, roles, races, nationalities and places (Words in names, below).
      </p>
      <button
        type="button"
        className="option-chip"
        onClick={() => {
          setError(null)
          find.mutate(undefined, { onError })
        }}
        disabled={running || find.isPending}
      >
        <RiSearchLine className={running ? 'spin' : undefined} />{' '}
        {running ? 'Searching…' : 'Find facts'}
      </button>{' '}
      <button
        type="button"
        className="option-chip"
        onClick={runReset}
        disabled={running || busy || reset.isPending}
        title="Remove every fact the search proposed for this series - accepted and skipped ones too - and forget the classes it proposed, so the next search starts afresh. Facts you wrote yourself stay"
      >
        <RiRestartLine className={reset.isPending ? 'spin' : undefined} /> Reset found facts
      </button>
      {error && <p className="error-text">{error}</p>}
      {notice && <p className="muted">{notice}</p>}
      {facts.length > 0 && (
        <details open>
          <summary>Facts ({facts.length})</summary>
          <ul className="roster-review-list">
            {facts.map((f) => (
              <li key={f.id}>
                <div className="roster-review-what">
                  <strong>{f.key}</strong>
                  {f.class && (
                    <span className="muted">{WORD_CLASS_LABELS[f.class].toLowerCase()}</span>
                  )}
                  <input
                    className="fact-proposal-text"
                    value={edits[f.id] ?? f.fact}
                    onChange={(e) => setEdits({ ...edits, [f.id]: e.target.value })}
                    aria-label={`Fact about ${f.key}`}
                  />
                </div>
                {f.evidence && (
                  <blockquote className="roster-review-evidence">
                    {f.evidence.split('\n').map((quote, i) => (
                      <div key={i}>{quote}</div>
                    ))}
                    {where(f) && <div className="muted">{where(f)}</div>}
                  </blockquote>
                )}
                {f.replaces && (
                  <div className="muted">
                    Accepting this replaces, in this series, the fact for every book: &ldquo;
                    {f.replaces}&rdquo; Carry over any of it you want to keep.
                  </div>
                )}
                <div className="roster-review-actions">
                  <button
                    type="button"
                    className="option-chip"
                    onClick={() => runAccept(f)}
                    disabled={busy || (edits[f.id] ?? f.fact).trim() === ''}
                  >
                    Accept
                  </button>
                  <button
                    type="button"
                    className="option-chip"
                    onClick={() => runSkip(f)}
                    disabled={busy}
                  >
                    Skip
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </details>
      )}
      {words.length > 0 && (
        <details open>
          <summary>Words in names ({words.length})</summary>
          <p className="muted">
            Words that several characters&rsquo; names use and that the text writes as a title, a
            role, a race, a nationality or a place rather than as anyone&rsquo;s name. Once accepted
            the roster never reads the word as a name, and the next roster review (Speakers tab)
            proposes what follows. Nobody is recast. Change the class where the guess is wrong, and
            skip a word that is someone&rsquo;s name after all. A word that already has a fact keeps
            it.
          </p>
          {WORD_CLASSES.map((c) => {
            const group = words.filter((f) => classOf(f) === c)
            if (group.length === 0) return null
            return (
              <div key={c}>
                <div className="roster-review-what">
                  <strong>
                    {WORD_CLASS_LABELS[c]} ({group.length})
                  </strong>
                  <span className="muted">{CLASS_HINTS[c]}</span>
                  <button
                    type="button"
                    className="option-chip"
                    onClick={() => runAcceptAll(group, WORD_CLASS_LABELS[c])}
                    disabled={busy}
                  >
                    {acceptingAll ? 'Accepting…' : `Accept all ${group.length}`}
                  </button>
                </div>
                <table className="lexicon-table">
                  <tbody>
                    {group.map((f) => (
                      <tr key={f.id}>
                        <td title={f.evidence || undefined}>
                          {f.key}
                          {f.fact && <div className="muted lexicon-matched-words">{f.fact}</div>}
                        </td>
                        <td
                          className="muted"
                          title={
                            f.usedBy?.length
                              ? `${f.usedBy.length} names use "${f.key}":\n${f.usedBy.join('\n')}`
                              : undefined
                          }
                        >
                          {(f.usedBy ?? []).slice(0, 3).join(', ')}
                          {(f.usedBy?.length ?? 0) > 3 && ` +${f.usedBy!.length - 3}`}
                        </td>
                        <td>
                          <select
                            value={c}
                            onChange={(e) =>
                              setClasses({ ...classes, [f.id]: e.target.value as WordClass })
                            }
                            aria-label={`What sort of word ${f.key} is`}
                          >
                            {WORD_CLASSES.map((o) => (
                              <option key={o} value={o}>
                                {WORD_CLASS_LABELS[o]}
                              </option>
                            ))}
                          </select>
                        </td>
                        <td>
                          <button
                            type="button"
                            className="text-button"
                            onClick={() => runAcceptWord(f)}
                            disabled={busy}
                          >
                            Accept
                          </button>{' '}
                          <button
                            type="button"
                            className="text-button"
                            onClick={() => runSkip(f)}
                            disabled={busy}
                          >
                            Skip
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )
          })}
        </details>
      )}
      {skipped.length > 0 && (
        <details>
          <summary>Skipped ({skipped.length})</summary>
          <p className="muted">
            What you turned down. It is kept so the search doesn&rsquo;t propose it again; put one
            back to rule on it afresh.{' '}
            <button
              type="button"
              className="text-button"
              onClick={runReviewAll}
              disabled={busy || review.isPending || reviewingAll}
            >
              {reviewingAll ? 'Putting back…' : `Review all ${skipped.length} again`}
            </button>
          </p>
          <table className="lexicon-table">
            <tbody>
              {skipped.map((f) => (
                <tr key={f.id}>
                  <td title={f.evidence || undefined}>{f.key}</td>
                  <td className="muted">
                    {f.skipped
                      ? [f.class && WORD_CLASS_LABELS[f.class], f.fact].filter(Boolean).join('. ')
                      : `${f.declinedClass ? WORD_CLASS_LABELS[f.declinedClass] : 'A class'} declined for this fact`}
                  </td>
                  <td>
                    <button
                      type="button"
                      className="text-button"
                      onClick={() => runReview(f)}
                      disabled={busy || review.isPending || reviewingAll}
                    >
                      Review again
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </details>
      )}
    </section>
  )
}
