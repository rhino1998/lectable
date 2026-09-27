import { useEffect, useRef, useState } from 'react'
import { isConfigured, lightingController, useLightingState } from '../lighting/controller'
import { LIGHT_EFFECT_LABELS } from '../api/types'
import { LightingSwatch } from './LightingSwatch'
import {
  bridgeCertUrl,
  describeError,
  discoverBridges,
  HUE_LINK_BUTTON_NOT_PRESSED,
  HueError,
  pairBridge,
  probeBridge,
  type DiscoveredBridge,
  type HueGroup,
} from '../lighting/hue'

const PAIR_TIMEOUT_MS = 30000
const PAIR_POLL_MS = 1500

const STATUS_LABELS = {
  off: 'Off',
  unconfigured: 'Pick a bridge and room below',
  connecting: 'Connecting…',
  active: 'Lighting the room',
  error: 'Error',
} as const

// LightingPanel is PlayerBar's "Lights" popover: pairs a Philips Hue
// bridge, picks the room to light, and caps brightness. The reader itself
// feeds cues through useLighting; everything here is this browser's own
// setting (see lighting/controller.ts), never the backend's.
export function LightingPanel() {
  const { settings, status, message, cue } = useLightingState()
  const [ip, setIp] = useState(settings.bridgeIp)
  const [found, setFound] = useState<DiscoveredBridge[] | null>(null)
  const [busy, setBusy] = useState<null | 'discover' | 'pair' | 'test'>(null)
  const [reachable, setReachable] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [groups, setGroups] = useState<HueGroup[] | null>(null)
  const cancelPair = useRef(false)

  const paired = settings.bridgeIp !== '' && settings.username !== ''

  useEffect(() => {
    if (!paired) return
    let stale = false
    lightingController
      .groups()
      .then((g) => !stale && setGroups(g))
      .catch(async (err: unknown) => {
        const text = await describeError(err, settings.bridgeIp)
        if (!stale) setError(text)
      })
    return () => {
      stale = true
    }
  }, [paired, settings.bridgeIp, settings.username])

  useEffect(() => {
    lightingController.setPreviewing(true)
    return () => {
      cancelPair.current = true
      lightingController.setPreviewing(false)
    }
  }, [])

  const discover = async () => {
    setBusy('discover')
    setError(null)
    try {
      const list = await discoverBridges()
      setFound(list)
      if (list.length === 1) setIp(list[0].ip)
      if (list.length === 0) setError('No bridge found - enter its IP address instead.')
    } catch (err) {
      setError(`Discovery failed (${errorText(err)}) - enter the bridge's IP address instead.`)
    } finally {
      setBusy(null)
    }
  }

  // test checks the bridge answers this browser at all - the certificate
  // and local-network permission are one-time per browser (see hue.ts).
  const test = async () => {
    const target = ip.trim()
    if (!target) return
    setBusy('test')
    setError(null)
    const problem = await probeBridge(target)
    setBusy(null)
    setReachable(problem === null)
    setError(problem)
  }

  // pair polls the bridge until its link button is pressed (or we time out).
  const pair = async () => {
    const target = ip.trim()
    if (!target) return
    setBusy('pair')
    setError(null)
    cancelPair.current = false
    const deadline = Date.now() + PAIR_TIMEOUT_MS
    try {
      for (;;) {
        try {
          const username = await pairBridge(target, 'web')
          setGroups(null)
          lightingController.updateSettings({ bridgeIp: target, username, groupId: '' })
          return
        } catch (err) {
          const waiting = err instanceof HueError && err.type === HUE_LINK_BUTTON_NOT_PRESSED
          if (!waiting || cancelPair.current) throw err
          if (Date.now() > deadline) throw new Error('timed out waiting for the link button')
          await new Promise((r) => setTimeout(r, PAIR_POLL_MS))
        }
      }
    } catch (err) {
      setError(`Pairing failed: ${await describeError(err, target)}`)
    } finally {
      setBusy(null)
    }
  }

  const forget = () => {
    setGroups(null)
    lightingController.updateSettings({ username: '', groupId: '', enabled: false })
  }

  return (
    <div className="lighting-panel">
      <label className="lighting-row">
        <input
          type="checkbox"
          checked={settings.enabled}
          disabled={!isConfigured(settings)}
          onChange={(e) => lightingController.updateSettings({ enabled: e.target.checked })}
        />
        Drive Philips Hue lights from the book
      </label>
      <div className="lighting-status">
        <span>
          {STATUS_LABELS[status]}
          {status === 'error' && message ? `: ${message}` : ''}
        </span>
      </div>
      {/* Preview of the scene under the current paragraph, as the room gets
          it (brightness cap applied) - shown whether or not lights are on. */}
      <div className="lighting-preview">
        {cue ? (
          <>
            <LightingSwatch lighting={cue} cap={settings.brightnessCap} large />
            <div className="lighting-preview-text">
              <div>{cue.description || 'Current scene'}</div>
              <div className="lighting-hint">
                {LIGHT_EFFECT_LABELS[cue.effect]} ·{' '}
                {Math.round(cue.brightness * settings.brightnessCap)}% brightness
                {cue.accent && cue.effect !== 'steady' && cue.effect !== 'flicker'
                  ? ` · ${cue.color} → ${cue.accent}`
                  : ` · ${cue.color}`}
              </div>
            </div>
          </>
        ) : (
          <span className="lighting-hint">
            No lighting for this part of the book yet - it comes from the chapter's ambience pass.
          </span>
        )}
      </div>

      {!paired ? (
        <>
          <div className="reader-typography-row-label">Bridge</div>
          <div className="lighting-row">
            <input
              className="lighting-ip"
              placeholder="Bridge IP, e.g. 192.168.1.20"
              value={ip}
              onChange={(e) => {
                setIp(e.target.value)
                setReachable(false)
              }}
            />
            <button className="text-button" disabled={busy !== null} onClick={discover}>
              {busy === 'discover' ? 'Searching…' : 'Find'}
            </button>
          </div>
          {found && found.length > 1 && (
            <div className="lighting-row">
              {found.map((b) => (
                <button key={b.id} className="text-button" onClick={() => setIp(b.ip)}>
                  {b.ip}
                </button>
              ))}
            </div>
          )}
          {ip.trim() !== '' && (
            <ol className="lighting-steps">
              <li>
                Open{' '}
                <a href={bridgeCertUrl(ip.trim())} target="_blank" rel="noreferrer">
                  the bridge
                </a>{' '}
                and accept its certificate warning (it's signed by Philips, not a public authority).
              </li>
              <li>
                <button className="text-button" disabled={busy !== null} onClick={test}>
                  {busy === 'test'
                    ? 'Testing… (allow local network access if asked)'
                    : 'Test connection'}
                </button>
                {reachable && ' - reachable'}
              </li>
              <li>
                <button className="text-button" disabled={busy !== null} onClick={pair}>
                  {busy === 'pair' ? 'Press the link button on the bridge…' : 'Pair'}
                </button>
              </li>
            </ol>
          )}
        </>
      ) : (
        <>
          <div className="reader-typography-row-label">
            Room
            <button className="text-button" onClick={forget}>
              Forget bridge {settings.bridgeIp}
            </button>
          </div>
          <select
            value={settings.groupId}
            onChange={(e) => lightingController.updateSettings({ groupId: e.target.value })}
          >
            <option value="">{groups ? 'Choose a room…' : 'Loading rooms…'}</option>
            {groups?.map((g) => (
              <option key={g.id} value={g.id}>
                {g.name} ({g.lights.length} light{g.lights.length === 1 ? '' : 's'})
              </option>
            ))}
          </select>
          <div className="reader-typography-row-label">
            Brightness cap ({Math.round(settings.brightnessCap * 100)}%)
          </div>
          <div className="speed-slider">
            <input
              type="range"
              min={10}
              max={100}
              step={5}
              value={Math.round(settings.brightnessCap * 100)}
              onChange={(e) =>
                lightingController.updateSettings({ brightnessCap: Number(e.target.value) / 100 })
              }
            />
          </div>
          <label className="lighting-row">
            <input
              type="checkbox"
              checked={settings.restoreOnStop}
              onChange={(e) =>
                lightingController.updateSettings({ restoreOnStop: e.target.checked })
              }
            />
            Restore the lights when I leave the reader
          </label>
        </>
      )}
      {error && <p className="error-text">{error}</p>}
      <p className="lighting-hint">
        Cues come from each chapter's ambience pass (Speakers page). The bridge is reached directly
        from this browser.
      </p>
    </div>
  )
}

function errorText(err: unknown): string {
  return err instanceof Error ? err.message : String(err)
}
