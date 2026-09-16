import { forwardRef, useImperativeHandle, useState } from 'react'

export interface AnnotationTooltipHandle {
  show: (text: string, rect: DOMRect) => void
  hide: () => void
}

// The annotations-view hover tooltip, split out from ReaderPage into its
// own component with an imperative show/hide API instead of state owned by
// ReaderPage itself. Hovering a word fires on every mousemove, and
// ReaderPage's own render covers every loaded chapter's full paragraph
// list - routing that through ReaderPage state would re-render the entire
// loaded list on every pixel of mouse movement. Exposing show/hide via
// useImperativeHandle instead means only this small component re-renders
// on hover; see ReaderPage's tooltipRef for the caller side.
export const AnnotationTooltip = forwardRef<AnnotationTooltipHandle>((_props, ref) => {
  const [tooltip, setTooltip] = useState<{ text: string; left: number; top: number } | null>(null)

  useImperativeHandle(
    ref,
    () => ({
      show: (text, rect) => setTooltip({ text, left: rect.left + rect.width / 2, top: rect.top }),
      hide: () => setTooltip(null),
    }),
    [],
  )

  if (!tooltip) return null
  return (
    <div className="annotation-tooltip" style={{ left: tooltip.left, top: tooltip.top }}>
      {tooltip.text}
    </div>
  )
})
AnnotationTooltip.displayName = 'AnnotationTooltip'
