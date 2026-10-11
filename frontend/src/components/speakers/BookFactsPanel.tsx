import { useBookFacts, useDeleteBookFact, useUpsertBookFact } from '../../api/queries'
import { FactsEditor } from '../settings/FactsEditor'

// The Book settings page's facts: what is true in this book's series (or
// this book alone, outside one) - "kobold: short-lived", or one about a
// single character ("Loth: thirteen years old") - shared across the series
// like the roster. Under them, read-only, the facts for every book
// (GlobalFactsPanel), which one of the series' own under the same word
// replaces here.
export function BookFactsPanel({ bookId, seriesName }: { bookId: string; seriesName?: string }) {
  const factsQuery = useBookFacts(bookId)
  const upsert = useUpsertBookFact(bookId)
  const remove = useDeleteBookFact(bookId)
  const all = factsQuery.data ?? []
  return (
    <FactsEditor
      intro={
        <>
          Things that are true in {seriesName ? `the "${seriesName}" series` : 'this book'}, each
          filed under the word that brings it up - a kind of being, a place, or one character's
          name. When that word (or its plural) appears in what the text says about a character, the
          fact is shown to the casting of their voice, and outranks its usual assumptions. A fact
          here replaces the one for every book under the same word. Saving a fact recasts nobody; it
          applies the next time a character's voice is cast.
        </>
      }
      facts={all.filter((f) => !f.global)}
      inherited={all.filter((f) => f.global)}
      loaded={!!factsQuery.data}
      loadError={factsQuery.isError}
      saving={upsert.isPending}
      removing={remove.isPending}
      onSave={(body, cb) => upsert.mutate(body, cb)}
      onDelete={(id, cb) => remove.mutate(id, cb)}
    />
  )
}
