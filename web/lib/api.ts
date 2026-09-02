// Client for the ARGUS API.
//
// The ask endpoint is a Server-Sent Events stream, and the ordering of its
// events is the product: the proof chain arrives before the prose, so the user
// watches the reasoning land in milliseconds and then watches the model read it
// out. Rendering the two together at the end would hide exactly the thing this
// system exists to show.

export type Status = 'proven' | 'contested' | 'insufficient_evidence'

export interface Step {
  claimId: string
  text: string
  weight: number
  confidence: number
}

export interface Chain {
  confidence: number
  weight: number
  leaps: number
  hops: number
  steps: Step[]
}

export interface FrontierClaim {
  claimId: string
  text: string
  confidence: number
  distance: number
  hops: number
}

export interface Timing {
  embedMs: number
  anchorMs: number
  searchMs: number
  graphMs: number
  totalMs: number
  graphCalls: number
}

export interface Verdict {
  question: string
  status: Status
  asOf: string
  chains: Chain[]
  frontier?: FrontierClaim[]
  reason?: string
  timing: Timing
}

export interface Evidence {
  claimId: string
  text: string
  confidence: number
  sourceName: string
  sourceKind: string
  documentTitle: string
  documentUrl: string
  span: string
}

export interface Dispute {
  claimId: string
  counterText: string
  counterConfidence: number
  counterSource: { id: string; name: string; kind: string; trust: number }
  detectedBy: string
}

export interface Corroboration {
  claimId: string
  directSources: number
  independentFlow: number
  originCount: number
  bottlenecked: boolean
  note?: string
  sources: { id: string; name: string; kind: string; trust: number }[]
}

export interface Ruling {
  contested: boolean
  disputes: Dispute[]
  corroboration: Corroboration[]
  graphMs: number
}

export interface Retraction {
  claimId: string
  text: string
  baseConfidence: number
  confidence: number
  delta: number
  collapses: boolean
  impact: 'critical' | 'major' | 'minor' | 'none'
  alternativeHops: number
}

export interface Analysis {
  question: string
  baseConfidence: number
  retractions: Retraction[]
  loadBearing: string[]
  forksUsed: number
  graphMs: number
  llmCalls: number
}

export interface AskOptions {
  question: string
  leapBudget?: number
  minConfidence?: number
  asOf?: string
  adjudicate?: boolean
}

export interface AskHandlers {
  onVerdict?: (v: Verdict) => void
  onEvidence?: (e: Evidence[]) => void
  onRuling?: (r: Ruling) => void
  onAnswer?: (text: string, status: Status) => void
  onDone?: (d: { totalMs: number; graphMs: number; graphOps: number }) => void
  onError?: (message: string) => void
}

/**
 * Stream an answer.
 *
 * EventSource is not used because it cannot issue a POST, and the question
 * belongs in a body rather than a URL. Parsing the SSE framing by hand is a
 * dozen lines and avoids encoding a long question into a query string.
 */
export async function ask(
  opts: AskOptions,
  handlers: AskHandlers,
  signal?: AbortSignal,
): Promise<void> {
  const res = await fetch('/api/ask', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(opts),
    signal,
  })

  if (!res.ok) {
    const body = await res.text()
    handlers.onError?.(`HTTP ${res.status}: ${body}`)
    return
  }
  if (!res.body) {
    handlers.onError?.('the response carried no stream')
    return
  }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  while (true) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })

    // SSE frames are separated by a blank line. A partial frame stays in the
    // buffer until its terminator arrives, so a chunk boundary mid-event never
    // produces a truncated parse.
    let sep: number
    while ((sep = buffer.indexOf('\n\n')) !== -1) {
      const frame = buffer.slice(0, sep)
      buffer = buffer.slice(sep + 2)
      dispatch(frame, handlers)
    }
  }
}

function dispatch(frame: string, h: AskHandlers) {
  let event = 'message'
  let data = ''
  for (const line of frame.split('\n')) {
    if (line.startsWith('event: ')) event = line.slice(7)
    else if (line.startsWith('data: ')) data += line.slice(6)
  }
  if (!data) return

  let payload: unknown
  try {
    payload = JSON.parse(data)
  } catch {
    return
  }

  switch (event) {
    case 'verdict':
      h.onVerdict?.(payload as Verdict)
      break
    case 'evidence':
      h.onEvidence?.(payload as Evidence[])
      break
    case 'ruling':
      h.onRuling?.(payload as Ruling)
      break
    case 'answer': {
      const p = payload as { text: string; status: Status }
      h.onAnswer?.(p.text, p.status)
      break
    }
    case 'done':
      h.onDone?.(payload as { totalMs: number; graphMs: number; graphOps: number })
      break
    case 'error':
      h.onError?.((payload as { message: string }).message)
      break
  }
}

/** Run a counterfactual retraction sweep. */
export async function retract(opts: AskOptions, signal?: AbortSignal): Promise<Analysis> {
  const res = await fetch('/api/retract', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(opts),
    signal,
  })
  const body = await res.json()
  if (!res.ok) throw new Error(body.error ?? `HTTP ${res.status}`)
  return body as Analysis
}

export interface GraphStats {
  nodeCount: number
  relCount: number
  labelCount: number
  relTypeCount: number
}

export async function stats(): Promise<GraphStats | null> {
  try {
    const res = await fetch('/api/stats')
    if (!res.ok) return null
    return (await res.json()) as GraphStats
  } catch {
    return null
  }
}
