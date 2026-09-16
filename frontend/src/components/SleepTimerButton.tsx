import { useRef, useState } from 'react'
import { RiMoonLine } from 'react-icons/ri'
import { formatCountdown, type SleepTimerOption } from '../hooks/useSleepTimer'
import { useClickOutside } from '../hooks/useClickOutside'

const DURATION_OPTIONS: { value: SleepTimerOption; label: string }[] = [
  { value: 5, label: '5 min' },
  { value: 10, label: '10 min' },
  { value: 15, label: '15 min' },
  { value: 30, label: '30 min' },
  { value: 45, label: '45 min' },
  { value: 60, label: '60 min' },
  { value: 'end-of-chapter', label: 'End of chapter' },
]

// Toggle button + dropdown for picking (or cancelling) a sleep timer.
// Purely presentational - the countdown itself lives in useSleepTimer, one
// level up in PlayerBar, so its remaining time can also be shown on the
// progress bar without two independent timers drifting apart.
export function SleepTimerButton({
  option,
  remainingSeconds,
  onChange,
}: {
  option: SleepTimerOption
  remainingSeconds: number
  onChange: (option: SleepTimerOption) => void
}) {
  const [open, setOpen] = useState(false)
  const active = option !== 'off'
  const containerRef = useRef<HTMLDivElement>(null)

  useClickOutside([containerRef], () => setOpen(false), open)

  return (
    <div className="sleep-timer" ref={containerRef}>
      <button
        className={'text-button-icon' + (active ? ' text-button-icon-active' : '')}
        onClick={() => setOpen((v) => !v)}
        title="Sleep timer"
      >
        <RiMoonLine />
        {active ? (option === 'end-of-chapter' ? 'End of chapter' : formatCountdown(remainingSeconds)) : 'Sleep'}
      </button>

      {open && (
        <div className="popover-panel popover-panel-above sleep-timer-panel">
          {active && (
            <button
              className="text-button sleep-timer-cancel"
              onClick={() => {
                onChange('off')
                setOpen(false)
              }}
            >
              Cancel timer
            </button>
          )}
          <div className="sleep-timer-options">
            {DURATION_OPTIONS.map((opt) => (
              <button
                key={String(opt.value)}
                className={'option-chip' + (option === opt.value ? ' option-chip-active' : '')}
                onClick={() => {
                  onChange(opt.value)
                  setOpen(false)
                }}
              >
                {opt.label}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
