'use client'

import type { Status } from '@/lib/api'

const STYLES: Record<Status, { label: string; cls: string }> = {
  proven: {
    label: 'Proven',
    cls: 'border-proven/50 bg-proven/10 text-proven',
  },
  contested: {
    label: 'Contested',
    cls: 'border-contested/50 bg-contested/10 text-contested',
  },
  insufficient_evidence: {
    label: 'Insufficient evidence',
    cls: 'border-abstain/50 bg-abstain/10 text-abstain',
  },
}

export function VerdictBadge({ status, confidence }: { status: Status; confidence?: number }) {
  const s = STYLES[status]
  return (
    <span
      className={`inline-flex items-center gap-2 rounded-full border px-3 py-1
                  font-mono text-[12px] ${s.cls}`}
    >
      {s.label}
      {confidence !== undefined && status !== 'insufficient_evidence' && (
        <span className="tabular-nums opacity-80">{(confidence * 100).toFixed(0)}%</span>
      )}
    </span>
  )
}
