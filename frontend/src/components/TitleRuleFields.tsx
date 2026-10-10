import type { SourceTitleRule } from '../api/types'
import { compileTitleRule } from '../utils/titleRule'

// The fields of a source's title rule (utils/titleRule.ts): a regular
// expression that says which of a source's entries are chapters, and a
// template that names each from the expression's capture groups. Kept
// with the source, so every later check lists what it finds the same way.
export function TitleRuleFields(props: {
  value: SourceTitleRule
  onChange: (rule: SourceTitleRule) => void
  disabled?: boolean
}) {
  const { value, onChange, disabled } = props
  const invalid = compileTitleRule(value) === 'invalid'
  return (
    <div className="import-preview-tools title-rule">
      <label className="import-preview-range import-preview-filter">
        Chapter titles match
        <input
          type="text"
          placeholder="regex, e.g. ^TWI (\d+\.\d+)"
          spellCheck={false}
          value={value.pattern}
          onChange={(e) => onChange({ ...value, pattern: e.target.value })}
          disabled={disabled}
        />
      </label>
      <label className="import-preview-range import-preview-filter">
        name them
        <input
          type="text"
          placeholder="as they are - or e.g. Chapter $1"
          title="A template over the pattern's capture groups: $1, $2, ${name}. Empty keeps each title."
          spellCheck={false}
          value={value.output}
          onChange={(e) => onChange({ ...value, output: e.target.value })}
          disabled={disabled}
        />
      </label>
      <label
        className="import-preview-range"
        title="Entries whose title doesn't match are neither imported nor offered for review - now, and on every later check of this source"
      >
        <input
          type="checkbox"
          checked={value.only}
          onChange={(e) => onChange({ ...value, only: e.target.checked })}
          disabled={disabled}
        />
        leave out the rest
      </label>
      {invalid && <span className="error-text">Not a regular expression</span>}
    </div>
  )
}
