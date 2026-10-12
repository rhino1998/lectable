import {
  DEFAULT_SPATIAL_SETTINGS,
  MAX_SPATIAL_WIDTH,
  type SpatialMode,
  type SpatialSettings,
} from '../utils/spatialAudio'

const PLAYBACK_RATES = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 3.5, 4, 4.5, 5, 6, 8, 10]

const SPATIAL_MODE_LABELS: Record<SpatialMode, string> = {
  off: 'Off',
  stereo: 'Left and right',
  hrtf: 'Around you (headphones)',
  'hrtf-head': 'Around you, narrator in your head (headphones)',
}

// PlayerBar's "Playback" popover: narration speed and where each speaker
// is heard from (utils/spatialAudio) - both this device's own settings.
export function PlaybackPanel(props: {
  playbackRate: number
  onPlaybackRateChange: (rate: number) => void
  spatial: SpatialSettings
  onSpatialChange: (next: SpatialSettings) => void
}) {
  const { spatial } = props
  const percent = Math.round((spatial.width / MAX_SPATIAL_WIDTH) * 100)

  return (
    <div className="playback-panel">
      <label className="playback-panel-row">
        Speed
        <select
          value={props.playbackRate}
          onChange={(e) => props.onPlaybackRateChange(Number(e.target.value))}
        >
          {PLAYBACK_RATES.map((r) => (
            <option key={r} value={r}>
              {r}×
            </option>
          ))}
        </select>
      </label>
      <label className="playback-panel-row">
        Speaker positions
        <select
          value={spatial.mode}
          onChange={(e) =>
            props.onSpatialChange({ ...spatial, mode: e.target.value as SpatialMode })
          }
        >
          {Object.entries(SPATIAL_MODE_LABELS).map(([mode, label]) => (
            <option key={mode} value={mode}>
              {label}
            </option>
          ))}
        </select>
      </label>
      <label className="playback-panel-row">
        Spread
        <input
          type="range"
          min={0}
          max={100}
          value={percent}
          disabled={spatial.mode === 'off'}
          onChange={(e) =>
            props.onSpatialChange({
              ...spatial,
              width: (Number(e.target.value) / 100) * MAX_SPATIAL_WIDTH,
            })
          }
        />
        <span className="playback-panel-value">{percent}%</span>
      </label>
      <div className="playback-panel-foot">
        <span className="muted">
          Characters speak from either side of the narrator. On this device.
        </span>
        <button
          type="button"
          className="text-button"
          onClick={() =>
            props.onSpatialChange({ ...spatial, width: DEFAULT_SPATIAL_SETTINGS.width })
          }
          disabled={spatial.width === DEFAULT_SPATIAL_SETTINGS.width}
        >
          Reset
        </button>
      </div>
    </div>
  )
}
