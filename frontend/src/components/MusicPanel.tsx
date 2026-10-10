import { useUpdateVoice, useVoice } from '../api/queries'
import { DEFAULT_MUSIC_VOLUME } from '../hooks/useMusicVolume'

// PlayerBar's "Music" popover: the book's background-music toggle (the
// same book-wide setting as Book Settings' "Background music") and how
// loud it plays under narration on this device (useMusicVolume).
export function MusicPanel(props: {
  bookId: string
  volume: number
  onVolumeChange: (volume: number) => void
}) {
  const voiceQuery = useVoice(props.bookId)
  const updateVoice = useUpdateVoice(props.bookId)
  const settings = voiceQuery.data
  const percent = Math.round(props.volume * 100)

  return (
    <div className="music-panel">
      <label className="checkbox-label">
        <input
          type="checkbox"
          checked={settings?.musicEnabled ?? false}
          disabled={!settings || updateVoice.isPending}
          onChange={(e) => {
            // cloneModel "" leaves the book's clone model as it is.
            if (settings)
              updateVoice.mutate({ ...settings, cloneModel: '', musicEnabled: e.target.checked })
          }}
        />
        Background music
      </label>
      <label className="music-volume">
        Volume
        <input
          type="range"
          min={0}
          max={100}
          value={percent}
          onChange={(e) => props.onVolumeChange(Number(e.target.value) / 100)}
        />
        <span className="music-volume-value">{percent}%</span>
      </label>
      <div className="music-panel-foot">
        <span className="muted">Relative to narration, on this device.</span>
        <button
          type="button"
          className="text-button"
          onClick={() => props.onVolumeChange(DEFAULT_MUSIC_VOLUME)}
          disabled={props.volume === DEFAULT_MUSIC_VOLUME}
        >
          Reset
        </button>
      </div>
      {updateVoice.isError && <p className="error-text">Could not change the setting.</p>}
    </div>
  )
}
