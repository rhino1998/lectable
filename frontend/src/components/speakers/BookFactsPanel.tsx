import { useBookFacts, useDeleteBookFact, useUpsertBookFact } from '../../api/queries'
import { FactsEditor } from '../settings/FactsEditor'
import { CharacterNotes } from './CharacterNotes'
import { KindAccents } from './KindAccents'
import { ProposedFacts } from './ProposedFacts'

// The Book settings page's facts: what is true in this book's series (or
// this book alone, outside one) - "kobold: short-lived", or one about a
// single character ("Loth: thirteen years old") - shared across the series
// like the roster. Under them, read-only, the facts for every book
// (GlobalFactsPanel), which one of the series' own under the same word
// replaces here. Above them, the facts a search of the series proposed
// (ProposedFacts), in force only once accepted; below, how each kind's
// characters are cast by accent (KindAccents) and what the text states of
// single characters (CharacterNotes).
export function BookFactsPanel({ bookId, seriesName }: { bookId: string; seriesName?: string }) {
  const factsQuery = useBookFacts(bookId)
  const upsert = useUpsertBookFact(bookId)
  const remove = useDeleteBookFact(bookId)
  const all = factsQuery.data ?? []
  return (
    <>
      <ProposedFacts
        bookId={bookId}
        proposals={all.filter((f) => f.proposed || f.proposedClass)}
        skipped={all.filter((f) => f.skipped || f.declinedClass)}
      />
      <FactsEditor
        intro={
          <>
            Things that are true in {seriesName ? `the "${seriesName}" series` : 'this book'}, each
            filed under the word that brings it up - a kind of being, a place, or one character's
            name. When that word (or its plural) appears in what the text says about a character,
            the fact is shown to the casting of their voice, and outranks its usual assumptions. A
            fact here replaces the one for every book under the same word. Saving a fact recasts
            nobody; it applies the next time a character's voice is cast. Say what sort of word it
            is - a title, role, race, nationality or place of this world (Prognugator, Baker, Drake,
            Pallassian, Pallass) - so the roster never takes it for someone's name; a fact can have
            both a class and text.
          </>
        }
        facts={all.filter((f) => !f.global && !f.proposed && !f.skipped)}
        inherited={all.filter((f) => f.global)}
        loaded={!!factsQuery.data}
        loadError={factsQuery.isError}
        saving={upsert.isPending}
        removing={remove.isPending}
        onSave={(body, cb) => upsert.mutate(body, cb)}
        onDelete={(id, cb) => remove.mutate(id, cb)}
      />
      <KindAccents bookId={bookId} />
      <CharacterNotes bookId={bookId} />
    </>
  )
}
