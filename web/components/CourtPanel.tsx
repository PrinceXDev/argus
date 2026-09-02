'use client'

import type { Ruling } from '@/lib/api'

/**
 * Dispute and corroboration.
 *
 * The bottleneck finding is deliberately given the most visual weight. "Six
 * sources assert this, but they share an upstream origin" is a real
 * investigative conclusion, and it is the one thing on the page that is
 * genuinely impossible to produce without a graph: it is a max-flow result, not
 * a count and not a similarity score.
 */
export function CourtPanel({ ruling }: { ruling: Ruling }) {
  const bottlenecked = ruling.corroboration.filter((c) => c.bottlenecked)

  // Notes repeat across claims that share a source, and the same sentence
  // three times reads as a bug rather than a finding. Deduplicate by text and
  // report how many claims each applies to.
  const noteCounts = new Map<string, number>()
  for (const c of ruling.corroboration) {
    if (!c.note || c.bottlenecked) continue
    noteCounts.set(c.note, (noteCounts.get(c.note) ?? 0) + 1)
  }
  const notes = [...noteCounts.entries()]

  if (ruling.disputes.length === 0 && ruling.corroboration.length === 0) return null

  return (
    <div className="rounded-lg border border-line bg-panel p-4">
      <h3 className="font-mono text-[11px] uppercase tracking-wider text-muted">
        who agrees, and are they independent?
      </h3>

      {bottlenecked.map((c) => (
        <p
          key={c.claimId}
          className="mt-3 rounded border border-contested/50 bg-contested/5 p-3 text-[14px] leading-relaxed"
        >
          <span className="font-medium text-contested">Corroboration is not independent.</span>{' '}
          <span className="text-fg/85">{c.note}</span>
          <span className="mt-1.5 block font-mono text-[11px] text-muted">
            {c.directSources} sources · {c.independentFlow} units of independent support ·{' '}
            {c.originCount} origin{c.originCount === 1 ? '' : 's'}
          </span>
        </p>
      ))}

      {ruling.disputes.length > 0 && (
        <div className="mt-3">
          <p className="font-mono text-[11px] text-contested">
            {ruling.disputes.length} disputed claim
            {ruling.disputes.length === 1 ? '' : 's'}
          </p>
          <ul className="mt-2 space-y-2">
            {ruling.disputes.map((d, i) => (
              <li key={i} className="rounded border border-line bg-panel-2 p-2.5 text-[13px]">
                <p className="text-fg/85">{d.counterText}</p>
                <p className="mt-1 font-mono text-[11px] text-muted">
                  {d.counterSource.name}
                  {d.counterSource.trust > 0 && (
                    <> · trust {(d.counterSource.trust * 100).toFixed(0)}%</>
                  )}
                  {d.detectedBy && <> · detected by {d.detectedBy}</>}
                </p>
              </li>
            ))}
          </ul>
        </div>
      )}

      {notes.length > 0 && (
        <ul className="mt-3 space-y-1">
          {notes.map(([note, n]) => (
            <li key={note} className="font-mono text-[11px] text-muted">
              {note}
              {n > 1 && <span className="text-muted/60"> ×{n} claims</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
