import { useDeleteGlobalFact, useGlobalFacts, useUpsertGlobalFact } from '../../api/queries'
import { FactsEditor } from './FactsEditor'

// The Settings page's facts: things that hold in every book, each filed
// under the word that brings it up ("dwarf" -> "Scottish accent"). The
// casting prompt is shown a fact when its word appears in what was
// gathered about a character (backend speakerattr.MatchWorldNotes); a
// series with a fact of its own under the same word uses that instead
// (BookFactsPanel). Saving one recasts nobody - it applies the next time a
// voice is cast.
export function GlobalFactsPanel() {
  const factsQuery = useGlobalFacts()
  const upsert = useUpsertGlobalFact()
  const remove = useDeleteGlobalFact()
  return (
    <FactsEditor
      intro={
        <>
          Things that are true in every book, each filed under the word that brings it up: when that
          word (or its plural) appears in what the text says about a character, the fact is shown to
          the casting of their voice, and outranks its usual assumptions. A series can have its own
          fact under the same word (on a book's Facts tab), which then replaces this one for its
          books. Saving a fact recasts nobody; it applies the next time a voice is cast.
        </>
      }
      facts={factsQuery.data ?? []}
      loaded={!!factsQuery.data}
      loadError={factsQuery.isError}
      saving={upsert.isPending}
      removing={remove.isPending}
      onSave={(body, cb) => upsert.mutate(body, cb)}
      onDelete={(id, cb) => remove.mutate(id, cb)}
    />
  )
}
