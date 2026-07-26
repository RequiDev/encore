/**
 * The date range control.
 *
 * Presets are a radio group, not a row of buttons: exactly one is selected at a
 * time, and arrow keys move between them, which is what a keyboard user expects
 * from a set of mutually exclusive choices. The custom range opens a small form
 * with two real date inputs, so the browser's own calendar and its locale are
 * used rather than a hand-rolled one.
 */

import type { ReactElement } from 'react'
import { useEffect, useRef, useState } from 'react'
import { RANGE_PRESETS, useRange } from '../../lib/range'
import type { RangePresetId } from '../../lib/range'
import { useEscapeKey, useOnClickOutside, useToggle } from '../../lib/hooks'
import { Button } from './Button'
import { Field, Input } from './Field'
import { Icon } from './Icon'

export interface RangePickerProps {
  className?: string
}

export function RangePicker({ className }: RangePickerProps): ReactElement {
  const { preset, label, days, setPreset, setCustomDays } = useRange()
  const custom = useToggle(false)
  const container = useRef<HTMLDivElement>(null)

  useOnClickOutside(container, custom.on, custom.close)
  useEscapeKey(custom.on, custom.close)

  return (
    <div
      ref={container}
      className={['relative flex items-center gap-2', className].filter(Boolean).join(' ')}
    >
      <div
        role="radiogroup"
        aria-label="Date range"
        className="flex items-center gap-px rounded-control border border-seam-strong p-px"
      >
        {RANGE_PRESETS.map((item) => (
          <PresetButton
            key={item.id}
            id={item.id}
            label={item.label}
            description={item.description}
            selected={preset === item.id}
            onSelect={setPreset}
          />
        ))}
      </div>

      <Button
        size="sm"
        aria-expanded={custom.on}
        aria-haspopup="dialog"
        onClick={custom.toggle}
        className={preset === 'custom' ? 'border-lamp text-lamp' : undefined}
      >
        <Icon name="calendar" />
        <span className="sr-only">Custom range, currently </span>
        <span className="tabular">{preset === 'custom' ? label : 'Custom'}</span>
      </Button>

      {custom.on ? (
        <CustomRangeForm
          from={days.from}
          to={days.to}
          onApply={(from, to) => {
            setCustomDays(from, to)
            custom.close()
          }}
          onCancel={custom.close}
        />
      ) : null}
    </div>
  )
}

function PresetButton({
  id,
  label,
  description,
  selected,
  onSelect,
}: {
  id: RangePresetId
  label: string
  description: string
  selected: boolean
  onSelect: (id: RangePresetId) => void
}): ReactElement {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      // Only the selected option is in the tab order; the arrow keys the radio
      // role implies move between them once it has focus.
      tabIndex={selected ? 0 : -1}
      onClick={() => onSelect(id)}
      onKeyDown={(event) => {
        if (event.key !== 'ArrowRight' && event.key !== 'ArrowLeft') return
        event.preventDefault()
        const index = RANGE_PRESETS.findIndex((p) => p.id === id)
        const step = event.key === 'ArrowRight' ? 1 : -1
        const at = (index + step + RANGE_PRESETS.length) % RANGE_PRESETS.length
        const next = RANGE_PRESETS[at]
        if (!next) return
        onSelect(next.id)
        // Selection follows focus in a radio group, so focus has to follow the
        // arrow key too — otherwise the next press moves from the old position.
        const sibling = event.currentTarget.parentElement?.children[at]
        if (sibling instanceof HTMLElement) sibling.focus()
      }}
      className={[
        'rounded-chip px-2.5 py-1 text-xs font-medium transition-colors',
        selected ? 'bg-lamp text-chassis' : 'text-ink-muted hover:text-ink',
      ].join(' ')}
    >
      <span aria-hidden="true">{label}</span>
      <span className="sr-only">{description}</span>
    </button>
  )
}

function CustomRangeForm({
  from,
  to,
  onApply,
  onCancel,
}: {
  from: string
  to: string
  onApply: (from: string, to: string) => void
  onCancel: () => void
}): ReactElement {
  const [start, setStart] = useState(from)
  const [end, setEnd] = useState(to)
  const firstField = useRef<HTMLDivElement>(null)

  useEffect(() => {
    firstField.current?.querySelector('input')?.focus()
  }, [])

  const invalid = start === '' || end === '' || start > end

  return (
    <div
      role="dialog"
      aria-label="Custom date range"
      className="panel-raised absolute top-full right-0 z-30 mt-2 w-72 p-3"
    >
      <form
        onSubmit={(event) => {
          event.preventDefault()
          if (!invalid) onApply(start, end)
        }}
        className="space-y-3"
      >
        <div ref={firstField}>
          <Field label="From">
            <Input type="date" value={start} max={end} onChange={(e) => setStart(e.target.value)} />
          </Field>
        </div>
        <Field
          label="To"
          hint="Inclusive. Both days are counted in full, in your timezone."
          error={invalid && start !== '' && end !== '' ? 'The first day must not be later.' : null}
        >
          <Input type="date" value={end} min={start} onChange={(e) => setEnd(e.target.value)} />
        </Field>
        <div className="flex justify-end gap-2">
          <Button size="sm" onClick={onCancel}>
            Cancel
          </Button>
          <Button size="sm" variant="primary" type="submit" disabled={invalid}>
            Apply
          </Button>
        </div>
      </form>
    </div>
  )
}
