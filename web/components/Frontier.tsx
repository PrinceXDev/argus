'use client'

import type { FrontierClaim } from '@/lib/api'

/**
 * The abstention panel.
 *
 * A refusal that says only "I don't know" is not useful. This shows how far the
 * evidence actually reaches and where it stops, which turns the refusal into a
 * research lead: these are the claims nearest the question that the corpus does
 * support, and the missing document is the one that would connect them.
 */
export function Frontier({ claims, reason }: { claims: FrontierClaim[]; reason?: string }) {
  return (
    <div className="rounded-lg border border-abstain/40 bg-abstain/5 p-4">
      <h3 className="font-mono text-[11px] uppercase tracking-wider text-abstain">
        the evidence stops here
      </h3>
      {reason && <p className="mt-2 text-[14px] text-fg/80">{reason}.</p>}

      {claims.length > 0 ? (
        <>
          <ul className="mt-3 space-y-2">
            {claims.map((c) => (
              <li
                key={c.claimId}
                className="rounded border border-line bg-panel p-2.5 text-[13px]"
              >
                <p className="text-fg/85">{c.text}</p>
                <p className="mt-1 font-mono text-[11px] text-muted">
                  {(c.confidence * 100).toFixed(0)}% · {c.hops} hop
                  {c.hops === 1 ? '' : 's'} from the anchor
                </p>
              </li>
            ))}
          </ul>
          <p className="mt-3 text-[13px] leading-relaxed text-muted">
            An answer would need a document connecting one of these to the question.
            None is present in the corpus.
          </p>
        </>
      ) : (
        <p className="mt-2 text-[13px] text-muted">
          Nothing in the graph anchors this question.
        </p>
      )}
    </div>
  )
}
