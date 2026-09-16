import { useEffect, useRef, useState } from 'react'

export type SleepTimerOption = 'off' | 5 | 10 | 15 | 30 | 45 | 60 | 'end-of-chapter'

export function formatCountdown(seconds: number): string {
  const m = Math.floor(seconds / 60)
  const s = Math.floor(seconds % 60)
  return `${m}:${s.toString().padStart(2, '0')}`
}

// A standard audiobook-app sleep timer: either a real-wall-clock countdown
// (ticks regardless of play/pause, same as e.g. Audible's) that calls
// onExpire once and resets to 'off', or 'end-of-chapter', which instead
// fires onExpire the moment chapterIdx changes away from whatever it was
// when that option was picked.
export function useSleepTimer(chapterIdx: number, onExpire: () => void) {
  const [option, setOption] = useState<SleepTimerOption>('off')
  const [remainingSeconds, setRemainingSeconds] = useState(0)
  const startChapterRef = useRef(chapterIdx)
  const onExpireRef = useRef(onExpire)
  onExpireRef.current = onExpire

  useEffect(() => {
    if (typeof option !== 'number') return
    setRemainingSeconds(option * 60)
    const id = setInterval(() => {
      setRemainingSeconds((s) => {
        if (s <= 1) {
          onExpireRef.current()
          setOption('off')
          return 0
        }
        return s - 1
      })
    }, 1000)
    return () => clearInterval(id)
  }, [option])

  useEffect(() => {
    if (option === 'end-of-chapter') startChapterRef.current = chapterIdx
    // Only re-capture the starting chapter when the option itself is
    // (re)selected, not on every chapterIdx change - otherwise the ref
    // would just keep tracking the current chapter and never trip.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [option])

  useEffect(() => {
    if (option !== 'end-of-chapter') return
    if (chapterIdx !== startChapterRef.current) {
      onExpireRef.current()
      setOption('off')
    }
  }, [option, chapterIdx])

  return { option, setOption, remainingSeconds }
}
