// A derived voice preset id is its base preset's id plus what it is of
// that voice (backend narration.memberPresetID / stagePresetID):
// "<id>~2-<hash>" is the second of several people sharing a role's voice,
// "<id>~a20-<hash>" the voice a character has from about twenty.
const DERIVED = /^(.+)~(a?)(\d+)-[0-9a-f]+$/

// presetDisplayName names a voice preset for display: its name from names
// (preset id -> name), with a derived preset named after its base ("Guard
// 2", "Will Oh, about 20"). An id that is in neither is shown as it is.
export function presetDisplayName(presetId: string, names: Map<string, string>): string {
  const known = names.get(presetId)
  if (known !== undefined) return known
  const m = DERIVED.exec(presetId)
  if (!m) return presetId
  const base = presetDisplayName(m[1], names)
  return m[2] ? `${base}, about ${m[3]}` : `${base} ${m[3]}`
}
