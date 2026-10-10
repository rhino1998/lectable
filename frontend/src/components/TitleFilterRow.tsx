import { useState } from 'react'
import { selectMatching, titlePattern } from '../utils/importSelection'
import { foldTitle } from '../utils/titleRule'

// A row for ticking or unticking a list's entries by a regular expression
// over their titles ("^chapter \d", "poll|announcement") - shared by the
// import form's chapter picker and the review of entries a check found.
export function TitleFilterRow(props: {
  items: { id: string; title: string }[]
  selected: Set<string>
  onSelect: (selected: Set<string>) => void
}) {
  const { items, selected, onSelect } = props
  const [filter, setFilter] = useState('')
  const pattern = titlePattern(filter)
  const matching = pattern ? items.filter((c) => pattern.test(foldTitle(c.title))).length : 0
  return (
    <div className="import-preview-tools">
      <label className="import-preview-range import-preview-filter">
        Titles matching
        <input
          type="text"
          placeholder="regex, e.g. ^chapter \d|interlude"
          spellCheck={false}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
      </label>
      <button
        type="button"
        className="option-chip"
        title="Tick every entry whose title matches; the others stay as they are"
        onClick={() => pattern && onSelect(selectMatching(items, selected, pattern, true))}
        disabled={!pattern}
      >
        Tick
      </button>
      <button
        type="button"
        className="option-chip"
        title="Untick every entry whose title matches; the others stay as they are"
        onClick={() => pattern && onSelect(selectMatching(items, selected, pattern, false))}
        disabled={!pattern}
      >
        Untick
      </button>
      <button
        type="button"
        className="option-chip"
        title="Tick exactly the entries whose title matches"
        onClick={() => pattern && onSelect(selectMatching(items, new Set(), pattern, true))}
        disabled={!pattern}
      >
        Only these
      </button>
      {filter.trim() !== '' && (
        <span className={pattern ? 'muted' : 'error-text'}>
          {pattern ? `${matching} of ${items.length} match` : 'Not a regular expression'}
        </span>
      )}
    </div>
  )
}
