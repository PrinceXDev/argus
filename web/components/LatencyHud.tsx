"use client";

import type { Timing } from "@/lib/api";

/**
 * The latency panel.
 *
 * This exists to make one claim checkable rather than asserted: the reasoning
 * is milliseconds of database work, and everything else the user waits for is
 * the language model producing text. Splitting the bar makes that visible at a
 * glance, and the graph-operation count shows the work was real rather than a
 * single lucky lookup.
 */
/** A sub-millisecond phase should read as such, not as a bare zero. */
function ms(v: number): string {
	return v < 1 ? "<1 ms" : `${v} ms`;
}

export function LatencyHud({
	timing,
	totalMs,
}: {
	timing: Timing;
	totalMs?: number;
}) {
	const total = Math.max(totalMs ?? timing.totalMs, 1);
	const graph = timing.graphMs;
	const model = Math.max(total - graph, 0);
	const graphPct = Math.max((graph / total) * 100, 0.6);

	return (
		<div className="rounded-lg border border-line bg-panel p-3.5">
			<div className="flex items-baseline justify-between">
				<h3 className="font-mono text-[11px] uppercase tracking-wider text-muted">
					where the time went
				</h3>
				<span className="font-mono text-[11px] tabular-nums text-muted">
					{timing.graphCalls} graph ops
				</span>
			</div>

			<div className="mt-3 flex h-2 w-full overflow-hidden rounded-full bg-line">
				<div
					className="h-full bg-accent transition-[width] duration-500"
					style={{ width: `${graphPct}%` }}
					title={`graph ${graph}ms`}
				/>
				<div className="h-full flex-1 bg-line" title={`model ${model}ms`} />
			</div>

			<div className="mt-2.5 grid grid-cols-2 gap-3 font-mono text-[12px] tabular-nums">
				<div>
					<div className="text-accent">{ms(graph)}</div>
					<div className="text-[11px] text-muted">graph — the reasoning</div>
				</div>
				<div className="text-right">
					<div className="text-fg/70">{ms(model)}</div>
					<div className="text-[11px] text-muted">model — reading it out</div>
				</div>
			</div>
		</div>
	);
}
