import { useChapterMusic } from '../../api/queries'
import { LIGHT_EFFECT_LABELS } from '../../api/types'
import { LightingSwatch } from '../LightingSwatch'
import { MusicRegionPreviewButton } from '../MusicRegionPreviewButton'

// AmbiencePreview is the Book settings page's look at one chapter's ambience pass
// (backend run-ambience): each music region's paragraph range and
// transition, its mood, lighting cue, soundscape and music prompt, and - once
// generated - its audio to play. The same regions the reader's annotations
// view marks inline (ChapterSection's MusicRegionBoundary), in one list.
export function AmbiencePreview({ bookId, chapterIdx }: { bookId: string; chapterIdx: number }) {
  const music = useChapterMusic(bookId, chapterIdx, true).data
  if (!music) return <p className="muted">Loading…</p>
  if (music.regions.length === 0) return <p className="muted">No music regions yet.</p>
  return (
    <table className="ambience-preview">
      <thead>
        <tr>
          <th>¶</th>
          <th>Mood</th>
          <th>Lighting</th>
          <th>Soundscape</th>
          <th>Music</th>
          <th />
        </tr>
      </thead>
      <tbody>
        {music.regions.map((r) => (
          <tr key={r.id} className={r.transition === 'cut' ? 'ambience-preview-cut' : undefined}>
            <td className="muted" title={r.transition === 'cut' ? 'New scene (cut)' : 'Continues'}>
              {r.startIdx}–{r.endIdx}
              {r.transition === 'cut' && ' ✂'}
            </td>
            <td>{r.mood}</td>
            <td>
              {r.lighting ? (
                <span className="ambience-preview-light">
                  <LightingSwatch lighting={r.lighting} />
                  <span>
                    {r.lighting.description}
                    <span className="muted">
                      {' '}
                      · {LIGHT_EFFECT_LABELS[r.lighting.effect]}, {r.lighting.brightness}%
                    </span>
                  </span>
                </span>
              ) : (
                <span className="muted">not lit</span>
              )}
            </td>
            <td>{r.ambience || <span className="muted">none</span>}</td>
            <td className="ambience-preview-prompt">{r.prompt}</td>
            <td>
              {r.status === 'ready' && r.audioUrl ? (
                <MusicRegionPreviewButton audioUrl={r.audioUrl} />
              ) : (
                <span className="muted">{r.status}</span>
              )}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
